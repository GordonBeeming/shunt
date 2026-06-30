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
	var noBridge bool
	c := &cobra.Command{
		Use:   "up [name]",
		Short: "Build + start the app in a siding's guest, then point the front door at it",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			name, err := sidingArg(app, args)
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

				// Point guest Docker volumes at this siding's copy-on-write data
				// clones (bind-mounted by `new`), so Aspire mounts the host's test
				// data — before the app starts.
				if e := siding.CreateBindVolumes(ctx, app, sd); e != nil {
					fmt.Printf("  (data volume bind failed: %v — continuing with empty volumes)\n", e)
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

				fmt.Printf("• starting the app in %q…\n", name)
				if err := siding.StartApp(ctx, app, sd); err != nil {
					return err
				}
				// Brief, non-fatal: the app keeps building in the background and the
				// eager bridges serve each route as it comes up — no blocking on a
				// readiness marker (shunt pins nothing now). Watch the dashboard.
				fmt.Println("• starting the app (it keeps building in the background)…")
				_ = siding.WaitReady(ctx, app, sd, 45*time.Second)
			} else {
				fmt.Printf("• the app is already running in %q\n", name)
			}

			// --no-bridge: start it in the guest but don't touch the host at all —
			// no socat bridges, no Caddy. Lets you confirm "would it work" on the
			// guest's own Aspire dashboard before bridging steals the shared ports
			// from whatever's currently live. Bridge later with a plain `up`/`switch`.
			if noBridge {
				if ip, e := container.IP(ctx, sd.Container); e == nil {
					sd.LastIP = ip
				}
				app.Sidings[name] = sd
				if err := state.SaveApp(app); err != nil {
					return err
				}
				fmt.Printf("✓ %q started in the guest — not bridged to the host (front door untouched)\n", name)
				if u := siding.DashboardURL(app, sd); u != "" {
					fmt.Printf("  check it on the guest's Aspire dashboard: %s\n", u)
				}
				fmt.Printf("  when it looks good: `%s up %s` to bridge + go live (or `%s switch %s`)\n", bin(), name, bin(), name)
				return nil
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
			fmt.Printf("  dashboard (guest): %s\n", siding.DashboardURL(app, sd))
			return nil
		},
	}
	c.Flags().BoolVar(&noSwitch, "no-switch", false, "start it + bridge, but don't point the front door at it")
	c.Flags().BoolVar(&noBridge, "no-bridge", false, "start it in the guest only — no host bridges, no front door (verify before going live)")
	return c
}
