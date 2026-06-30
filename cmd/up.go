package cmd

import (
	"fmt"
	"time"

	"os"
	"path/filepath"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/hostdocker"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newUpCmd() *cobra.Command {
	var noSwitch bool
	c := &cobra.Command{
		Use:   "up <name>",
		Short: "Build + start Aspire in a siding's guest, then point the front door at it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			sd, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q — create it with `"+bin()+" new %s`", name, name)
			}
			st, _ := container.State(ctx, sd.Container)
			if st != "running" {
				return fmt.Errorf("the guest for %q isn't running (state=%s); recreate it with `"+bin()+" new`", name, st)
			}

			// Idempotent: only (re)launch the app if it isn't already running in the
			// guest. Checking the running app — not the log marker — means re-running
			// `up` on a live app (which HubX never marks "started") re-activates
			// instead of restarting and colliding with the live AppHost.
			if !siding.AppRunning(ctx, app, sd) {
				// Stop any stale orchestration first so we don't start a second
				// AppHost (port clash), and clear its log so WaitStarted waits fresh.
				_ = siding.StopApp(ctx, app, sd)
				_, _ = container.Exec(ctx, sd.Container, "sh", "-c", "> /var/log/apphost.log")

				// A restarted guest often has a dead dockerd (stale state); make sure
				// it's healthy before the warm-load and Aspire need it.
				fmt.Println("• checking the in-guest Docker daemon…")
				if e := siding.EnsureDockerd(ctx, sd); e != nil {
					return fmt.Errorf("%w — try a fresh guest: `%s rm %s && %s new %s`", e, bin(), name, bin(), name)
				}

				// Keep the host as the canonical cache: if dependency images are
				// declared and the project cache is missing, build it from the host
				// (pull only what the host lacks), then load it into this guest — so
				// the siding never pulls from the network itself.
				tar := siding.WarmTarPath(app)
				if len(app.PrebakeImages) > 0 && hostdocker.Available(ctx) {
					if _, statErr := os.Stat(tar); statErr != nil {
						fmt.Println("• warming the host image cache (one-time)…")
						if _, e := hostdocker.Ensure(ctx, app.PrebakeImages); e != nil {
							return fmt.Errorf("warm host cache: %w", e)
						}
						if e := os.MkdirAll(filepath.Dir(tar), 0o755); e != nil {
							return e
						}
						if e := hostdocker.Save(ctx, app.PrebakeImages, tar); e != nil {
							return e
						}
					}
				}
				if loaded, e := siding.LoadWarm(ctx, app, sd); e != nil {
					fmt.Printf("  (warm load failed: %v)\n", e)
				} else if loaded {
					fmt.Println("• loaded dependency images from cache (no pull)")
				} else {
					fmt.Printf("• no warm cache — declare prebakeImages + run `%s warm`, or it'll build/pull cold\n", bin())
				}

				fmt.Printf("• starting Aspire in %q…\n", name)
				if err := siding.StartApp(ctx, app, sd); err != nil {
					return err
				}
				fmt.Println("• waiting for the app to start…")
				if err := siding.WaitReady(ctx, app, sd, 25*time.Minute); err != nil {
					return err
				}
			} else {
				fmt.Printf("• Aspire is already running in %q\n", name)
			}

			fmt.Println("• discovering endpoints + bridging to the host…")
			if err := siding.Activate(ctx, app, &sd); err != nil {
				return err
			}
			app.Sidings[name] = sd
			if err := state.SaveApp(app); err != nil {
				return err
			}
			fmt.Printf("✓ %q is up\n", name)

			if noSwitch {
				fmt.Printf("  run `"+bin()+" switch %s` to point the stable ports at it\n", name)
				return nil
			}
			if err := switchTo(ctx, &app, name); err != nil {
				return err
			}
			printFrontDoor(app, app.Sidings[name])
			fmt.Printf("  dashboard (guest): %s\n", siding.DashboardURL(sd))
			return nil
		},
	}
	c.Flags().BoolVar(&noSwitch, "no-switch", false, "start it but don't point the front door at it")
	return c
}
