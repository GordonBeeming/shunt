package cmd

import (
	"context"
	"fmt"
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
		Long: "Build a daemon-free cache from the images declared in `.shunt.app.json` (prebakeImages). " +
			"`warm` always refreshes configured refs from their registries, then writes a per-project Docker archive " +
			"that every siding loads. Use --from <siding> to instead capture whatever a running " +
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
				return warmFromGuest(ctx, app, from, tar)
			}
			if len(app.PrebakeImages) == 0 {
				return fmt.Errorf("no `prebakeImages` in .shunt.app.json — declare the dependency images, or use --from <siding> to capture from a running siding")
			}
			fmt.Printf("• refreshing %d dependency image(s)…\n", len(app.PrebakeImages))
			changes, err := imagecache.Refresh(ctx, tar, app.PrebakeImages)
			if err != nil {
				return err
			}
			for _, change := range changes {
				switch change.Action {
				case "updated":
					fmt.Printf("  updated %s: %s → %s\n", change.Ref, change.PreviousDigest, change.Digest)
				default:
					fmt.Printf("  %s %s: %s\n", change.Action, change.Ref, change.Digest)
				}
			}
			fi, _ := os.Stat(tar)
			fmt.Printf("%s warmed: %s (%.1f GB). New sidings `docker load` this — no pull.\n",
				tick(), tar, float64(fi.Size())/1e9)
			return nil
		},
	}
	c.Flags().StringVar(&from, "from", "", "capture images from this running siding instead of the host cache")
	return c
}

// warmFromGuest captures whatever images a running siding's Docker store holds —
// useful before prebakeImages are declared, or to grab a locally-built image.
func warmFromGuest(ctx context.Context, app state.App, src, tar string) error {
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
	if err := imagecache.Capture(tar, func(temp string) error {
		return proc.RunToFile(ctx, temp, "container", saveArgs...)
	}); err != nil {
		return fmt.Errorf("docker save → %s: %w", tar, err)
	}
	fi, _ := os.Stat(tar)
	fmt.Printf("%s warmed from siding: %s (%.1f GB).\n", tick(), tar, float64(fi.Size())/1e9)
	return nil
}
