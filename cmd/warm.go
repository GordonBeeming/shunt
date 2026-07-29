package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/imagecache"
	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newWarmCmd() *cobra.Command {
	var from string
	c := &cobra.Command{
		Use:   "warm",
		Short: "Build the project's dependency-image cache so sidings never pull from the network",
		Long: "Refresh the daemon-free, immutable per-image cache declared in `.shunt.app.json` (prebakeImages and prebakeBuilds). " +
			"Sidings load only missing or changed cached images and never pull. Use --from <siding> to instead capture whatever a running " +
			"siding built/pulled (handy before prebakeImages are declared).",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			tar := siding.WarmTarPath(app)
			if err := os.MkdirAll(filepath.Dir(tar), 0o755); err != nil {
				return err
			}

			if from != "" {
				return warmFromGuest(ctx, app, from, tar, cmd.ErrOrStderr())
			}
			if len(app.PrebakeImages) == 0 && len(app.PrebakeBuilds) == 0 {
				return fmt.Errorf("no `prebakeImages` or `prebakeBuilds` in .shunt.app.json — declare dependency images, or use --from <siding> to capture from a running siding")
			}
			fmt.Printf("• refreshing %d dependency image(s)…\n", len(app.PrebakeImages)+len(app.PrebakeBuilds))
			changes, err := siding.RefreshImageCacheProgress(ctx, app, printWarmProgress)
			if err != nil {
				var cleanupErr *imagecache.CommittedCleanupError
				if !errors.As(err, &cleanupErr) {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", cleanupErr)
			}
			for _, change := range changes {
				switch change.Action {
				case "updated":
					fmt.Printf("  updated %s: %s → %s\n", change.Ref, change.PreviousDigest, change.Digest)
				default:
					fmt.Printf("  %s %s: %s\n", change.Action, change.Ref, change.Digest)
				}
			}
			fmt.Printf("%s warmed: %s. Sidings import only missing or changed images — no pull.\n", tick(), tar)
			return nil
		},
	}
	c.Flags().StringVar(&from, "from", "", "capture images from this running siding instead of the host cache")
	c.AddCommand(newWarmGCCmd())
	return c
}

func printWarmProgress(event imagecache.ProgressEvent) {
	line := "  " + event.Step + " " + event.Ref
	if event.Platform != "" {
		line += " (" + event.Platform
		if event.Fallback {
			line += ", fallback"
		}
		line += ")"
	}
	fmt.Println(line)
}

func newWarmGCCmd() *cobra.Command {
	var dryRun bool
	var maxBytes int64
	c := &cobra.Command{
		Use:   "gc",
		Short: "Collect unreachable image-cache generations, exports, and blobs",
		Long: "Collect content unreachable from the current, previous, or actively leased cache generation. " +
			"Protected content is never deleted; shunt warns if it alone exceeds the configured budget.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			result, err := imagecache.Collect(cmd.Context(), siding.WarmTarPath(app), imagecache.GCOptions{
				DryRun: dryRun, MaxBytes: maxBytes,
				Progress: func(line string) { fmt.Println("  " + line) },
			})
			if err != nil {
				return err
			}
			action := "collected"
			if dryRun {
				action = "would collect"
			}
			fmt.Printf("%s cache GC %s %d bytes across %d object(s)\n", tick(), action, result.ReclaimedBytes, len(result.Removed))
			if result.Warning != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", result.Warning)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "report reclaimable cache content without deleting it")
	c.Flags().Int64Var(&maxBytes, "max-bytes", imagecache.ConfiguredMaxBytes(), "cache budget in bytes; protected content is never deleted")
	return c
}

// warmFromGuest captures whatever images a running siding's Docker store holds —
// useful before prebakeImages are declared, or to grab a locally-built image.
func warmFromGuest(ctx context.Context, app state.App, src, tar string, warnings io.Writer) error {
	sd, ok := app.Sidings[src]
	if !ok {
		return fmt.Errorf("no siding %q to warm from", src)
	}
	guest := sd.Container
	if st, _ := container.State(ctx, guest); st != "running" {
		return fmt.Errorf("siding %q isn't running", src)
	}
	out, err := container.Exec(ctx, guest, "sh", "-c", "docker images --format '{{.Repository}}:{{.Tag}}'")
	if err != nil {
		return fmt.Errorf("list images in %q: %w", src, err)
	}
	var imgs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "<none>") {
			continue
		}
		imgs = append(imgs, line)
	}
	if len(imgs) == 0 {
		return fmt.Errorf("no images in siding %q yet — let `up` finish first", src)
	}
	fmt.Printf("• capturing %d image(s) from siding %q:\n", len(imgs), src)
	for _, im := range imgs {
		fmt.Printf("    %s\n", im)
	}
	saveArgs := append([]string{"exec", guest, "docker", "save"}, imgs...)
	if err := imagecache.CaptureContext(ctx, tar, func(temp string) error {
		return proc.RunToFileLimited(ctx, temp, imagecache.ConfiguredMaxBytes(), "container", saveArgs...)
	}); err != nil {
		var cleanupErr *imagecache.CommittedCleanupError
		if errors.As(err, &cleanupErr) {
			fmt.Fprintf(warnings, "warning: %v\n", cleanupErr)
		} else {
			return fmt.Errorf("docker save → %s: %w", tar, err)
		}
	}
	fmt.Printf("%s warmed from siding: %s.\n", tick(), tar)
	return nil
}
