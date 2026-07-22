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
	var branch, from string
	c := &cobra.Command{
		Use:   "new <name>",
		Short: "Create a siding: a worktree + an idle guest (does NOT start the app — use `" + bin() + " up`)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			if name == state.HostTarget {
				return fmt.Errorf("%q is reserved — it's the switch target for your local copy; pick another siding name", name)
			}
			if from != "" && branch != "" {
				return fmt.Errorf("--from and --branch are mutually exclusive: --from continues an existing branch, --branch forks a new siding branch off a start point")
			}
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

			// One-time per project: extract declared host data volumes to APFS
			// baselines so Spin can cp -c an instant copy-on-write clone per siding.
			if err := siding.EnsureVolumeBaselines(ctx, app); err != nil {
				return err
			}

			if from != "" {
				fmt.Printf("• creating worktree from existing branch %q + launching idle guest for %q…\n", from, name)
			} else {
				fmt.Printf("• creating worktree + launching idle guest for %q…\n", name)
			}
			sd, err := siding.Spin(ctx, app, name, branch, from)
			if err != nil {
				return err
			}
			sd.CreatedAt = time.Now().Format(time.RFC3339)
			app.Sidings[name] = sd
			if err := state.SaveApp(app); err != nil {
				return err
			}

			src, _ := siding.Paths(app, name)
			fmt.Printf("%s siding %q ready — guest is up, Aspire is NOT started yet.\n", tick(), name)
			fmt.Printf("  edit code here:  %s\n", src)
			fmt.Printf("  on branch:       %s\n", sd.Branch)
			fmt.Printf("  run it:          "+bin()+" up %s   (builds + starts Aspire, then points the front door at it)\n", name)
			return nil
		},
	}
	c.Flags().StringVar(&branch, "branch", "", "fork a new siding branch off this start point (branch/commit; default: current HEAD, or origin's default branch for GitButler workspaces)")
	c.Flags().StringVar(&from, "from", "", "create the siding ON an existing remote branch (fetched + tracked), so commits push back to it")
	return c
}
