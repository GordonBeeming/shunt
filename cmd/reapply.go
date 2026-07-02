package cmd

import (
	"fmt"

	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

// newReapplyCmd recreates a siding's guest with the current config (memory, cpus,
// mounts, env) — guest-creation settings can't change on a live guest, so this
// replaces the container while keeping the worktree, branch, and data.
func newReapplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reapply [name]",
		Short: "Recreate a siding's guest with the current config (memory/cpus/mounts), keeping its code + data",
		Long: "Guest resource caps and mounts are fixed when the guest is created, so changing them\n" +
			"(e.g. `shunt config memory` or a contract `memory`/`cpus`) needs the guest recreated.\n" +
			"This removes + recreates only the container — your worktree, branch, and data clones stay.\n" +
			"Run `up` afterwards to start the app (the fresh guest re-clones bind volumes + bridges).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			name, err := sidingArg(ctx, app, args)
			if err != nil {
				return err
			}
			sd, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q", name)
			}
			fmt.Printf("• recreating the guest for %q with current config (keeps code + data)…\n", name)
			newSd, err := siding.Recreate(ctx, app, sd)
			if err != nil {
				return err
			}
			app.Sidings[name] = newSd
			if app.LiveSiding == name {
				// The front door pointed at the old guest; it's gone now.
				app.LiveSiding = ""
			}
			if err := state.SaveApp(app); err != nil {
				return err
			}
			fmt.Printf("%s %q guest recreated — run `%s up %s` to start it\n", tick(), name, bin(), name)
			return nil
		},
	}
}
