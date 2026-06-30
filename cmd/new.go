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
	c := &cobra.Command{
		Use:   "new <name>",
		Short: "Create a siding: a worktree + an idle guest (does NOT start Aspire — use `"+bin()+" up`)",
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

			fmt.Printf("• creating worktree + launching idle guest for %q…\n", name)
			sd, err := siding.Spin(ctx, app, name, branch)
			if err != nil {
				return err
			}
			sd.CreatedAt = time.Now().Format(time.RFC3339)
			app.Sidings[name] = sd
			if err := state.SaveApp(app); err != nil {
				return err
			}

			src, _ := siding.Paths(app, name)
			fmt.Printf("✓ siding %q ready — guest is up, Aspire is NOT started yet.\n", name)
			fmt.Printf("  edit code here:  %s\n", src)
			fmt.Printf("  run it:          "+bin()+" up %s   (builds + starts Aspire, then points the front door at it)\n", name)
			return nil
		},
	}
	c.Flags().StringVar(&branch, "branch", "", "base branch/commit for the worktree (default: current HEAD)")
	return c
}
