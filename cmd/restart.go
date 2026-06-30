package cmd

import (
	"fmt"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/spf13/cobra"
)

func newRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart <name>",
		Short: "Rebuild + restart the AppHost in place, keeping the guest, deps, and data running",
		Long: "For when `dotnet watch` doesn't catch a change (it's best-effort) or a clean rebuild " +
			"is needed. Kills only the AppHost process and re-runs it (full build), leaving the guest, " +
			"its Docker daemon, the dependency containers (SQL etc.), and their data up — no teardown, no pull.",
		Args: cobra.ExactArgs(1),
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
				return fmt.Errorf("the guest for %q isn't running (state=%s); start it with `"+bin()+" up %s`", name, st, name)
			}

			fmt.Printf("• stopping the AppHost in %q (keeping deps + data up)…\n", name)
			if err := siding.StopApp(ctx, sd); err != nil {
				return err
			}
			// Clear the old start marker so WaitStarted waits for the fresh run.
			_, _ = container.Exec(ctx, sd.Container, "sh", "-c", "> /var/log/apphost.log")

			fmt.Println("• rebuilding + restarting…")
			if err := siding.StartApp(ctx, app, sd); err != nil {
				return err
			}
			if err := siding.WaitStarted(ctx, sd.Container, 15*time.Minute); err != nil {
				return err
			}
			fmt.Printf("✓ %q restarted — dashboard %s\n", name, siding.DashboardURL(sd))
			return nil
		},
	}
}
