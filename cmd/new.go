package cmd

import (
	"fmt"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newNewCmd() *cobra.Command {
	var branch string
	var doSwitch bool
	c := &cobra.Command{
		Use:   "new <name>",
		Short: "Create a new siding: clone the repo and run the app in a guest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			if _, exists := app.Sidings[name]; exists {
				return fmt.Errorf("siding %q already exists", name)
			}
			if err := container.EnsureSystemStarted(ctx); err != nil {
				return err
			}

			fmt.Printf("• cloning repo + launching guest for %q…\n", name)
			sd, err := siding.Spin(ctx, app, name, branch)
			if err != nil {
				return err
			}
			sd.CreatedAt = time.Now().Format(time.RFC3339)
			// Record the siding before the (slow) wait so a failure still leaves a
			// trace for `shunt ls` / `shunt rm`.
			app.Sidings[name] = sd
			if err := state.SaveApp(app); err != nil {
				return err
			}

			fmt.Println("• waiting for the Aspire app to start (first run pulls images)…")
			if err := siding.WaitStarted(ctx, sd.Container, 6*time.Minute); err != nil {
				return err
			}
			fmt.Println("• discovering endpoints + bridging to the host…")
			if err := siding.Activate(ctx, app, &sd); err != nil {
				return err
			}
			app.Sidings[name] = sd
			if err := state.SaveApp(app); err != nil {
				return err
			}
			fmt.Printf("✓ siding %q ready — dashboard %s\n", name, siding.DashboardURL(sd))

			if doSwitch || app.LiveSiding == "" {
				return switchTo(ctx, &app, name)
			}
			fmt.Printf("  run `shunt switch %s` to make it live\n", name)
			return nil
		},
	}
	c.Flags().StringVar(&branch, "branch", "", "branch to check out in the clone")
	c.Flags().BoolVar(&doSwitch, "switch", false, "switch to this siding once it's ready")
	return c
}
