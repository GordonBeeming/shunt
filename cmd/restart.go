package cmd

import (
	"fmt"

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
			} else if name, err = pickSiding(ctx, app, true); err != nil {
				return err
			}
			if name == state.HostTarget {
				fmt.Println("• restarting your local app on the host…")
				if err := siding.HostRestart(ctx, app); err != nil {
					return err
				}
				fmt.Println(tick() + " host app restarted")
				return nil
			}
			sd, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q — create it with `"+bin()+" new %s`", name, name)
			}
			fmt.Printf("• stopping the application process in %q (keeping deps + data up)…\n", name)
			fmt.Println("• rebuilding + restarting…")
			if err := siding.Restart(ctx, app, sd); err != nil {
				return err
			}
			fmt.Printf("%s %q restarted\n", tick(), name)
			if dashboard := siding.DashboardURL(app, sd); dashboard != "" {
				fmt.Printf("  dashboard (guest): %s\n", dashboard)
			}
			return nil
		},
	}
}
