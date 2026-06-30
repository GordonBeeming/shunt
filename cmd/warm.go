package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/spf13/cobra"
)

func newWarmCmd() *cobra.Command {
	var from string
	c := &cobra.Command{
		Use:   "warm",
		Short: "Capture a running siding's built + pulled dependency images so new sidings skip the rebuild/pull",
		Long: "A siding's guest starts with an empty Docker store, so it rebuilds custom images (e.g. a SQL+FTS " +
			"image) and re-pulls dependencies from scratch — the bulk of a cold start. `warm` saves those images " +
			"from a running siding into a per-project cache that `up` loads into each new siding, bringing first-run " +
			"time down toward a native warm start. Run `up` on one siding first, then `warm`.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}

			// Source guest: --from, else the live siding, else any running one.
			src := from
			if src == "" {
				src = app.LiveSiding
			}
			guest := ""
			if src != "" {
				sd, ok := app.Sidings[src]
				if !ok {
					return fmt.Errorf("no siding %q to warm from", src)
				}
				guest = sd.Container
			} else {
				for name, sd := range app.Sidings {
					if st, _ := container.State(ctx, sd.Container); st == "running" {
						guest, src = sd.Container, name
						break
					}
				}
			}
			if guest == "" {
				return fmt.Errorf("no running siding to warm from — `"+bin()+" up <name>` one first, then `"+bin()+" warm`")
			}
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

			tar := siding.WarmTarPath(app)
			if err := os.MkdirAll(filepath.Dir(tar), 0o755); err != nil {
				return err
			}
			fmt.Printf("• capturing %d image(s) from %q into the project warm cache:\n", len(imgs), src)
			for _, im := range imgs {
				fmt.Printf("    %s\n", im)
			}
			saveArgs := append([]string{"exec", guest, "docker", "save"}, imgs...)
			if err := proc.RunToFile(ctx, tar, "container", saveArgs...); err != nil {
				return fmt.Errorf("docker save → %s: %w", tar, err)
			}
			fi, _ := os.Stat(tar)
			fmt.Printf("✓ warmed: %s (%.1f GB). New sidings `docker load` this instead of rebuilding/pulling.\n",
				tar, float64(fi.Size())/1e9)
			return nil
		},
	}
	c.Flags().StringVar(&from, "from", "", "siding to capture images from (default: live, else any running)")
	return c
}
