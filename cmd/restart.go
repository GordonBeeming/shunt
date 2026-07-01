package cmd

import (
	"fmt"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart [name]",
		Short: "Rebuild + restart the app in place, keeping the guest, deps, and data running",
		Long: "For when a hot-reload misses a change or a clean rebuild " +
			"is needed. Kills only the app process and re-runs it (full build), leaving the guest, " +
			"its Docker daemon, the dependency containers (SQL etc.), and their data up — no teardown, no pull.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			// restart can bounce the host too (it re-runs the native app), so the
			// bare picker includes `host` and starts on whatever's live — unlike
			// up/kill/logs, which are guest-only (sidingArg).
			var name string
			if len(args) > 0 {
				name = args[0]
			} else if name, err = pickSiding(app, true); err != nil {
				return err
			}
			if name == state.HostTarget {
				fmt.Println("• restarting your local app on the host…")
				siding.HostStop(ctx, app)
				if err := siding.HostStart(ctx, app); err != nil {
					return err
				}
				fmt.Println(tick() + " host app restarted")
				return nil
			}
			sd, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q — create it with `"+bin()+" new %s`", name, name)
			}
			st, _ := container.State(ctx, sd.Container)
			if st != "running" {
				return fmt.Errorf("the guest for %q isn't running (state=%s); start it with `"+bin()+" up %s`", name, st, name)
			}

			fmt.Printf("• stopping the AppHost in %q (keeping deps + data up)…\n", name)
			if err := siding.StopApp(ctx, app, sd); err != nil {
				return err
			}
			// Clear the old start marker so WaitStarted waits for the fresh run.
			_, _ = container.Exec(ctx, sd.Container, "sh", "-c", "> /var/log/apphost.log")

			fmt.Println("• rebuilding + restarting…")
			if err := siding.StartApp(ctx, app, sd); err != nil {
				return err
			}
			if err := siding.WaitReady(ctx, app, sd, 15*time.Minute); err != nil {
				return err
			}
			fmt.Printf("%s %q restarted — dashboard %s\n", tick(), name, siding.DashboardURL(app, sd))
			return nil
		},
	}
}
