package cmd

import (
	"fmt"

	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/spf13/cobra"
)

// newReapplyCmd recreates a siding's guest with the current config (memory, cpus,
// mounts, env) — guest-creation settings can't change on a live guest, so this
// replaces the container while keeping the worktree, branch, and data.
func newReapplyCmd() *cobra.Command {
	var freshData bool
	c := &cobra.Command{
		Use:   "reapply [name]",
		Short: "Recreate a siding's guest with the current config (memory/cpus/mounts), keeping its code + data",
		Long: "Guest resource caps and mounts are fixed when the guest is created, so changing them\n" +
			"(e.g. `shunt config memory` or a contract `memory`/`cpus`) needs the guest recreated.\n" +
			"This removes + recreates only the container — your worktree, branch, and data clones stay.\n" +
			"Run `up` afterwards to start the app (the fresh guest reattaches the existing data and rebuilds bridges).\n\n" +
			"With --fresh-data, each data volume is reset to the project baseline (the current\n" +
			"clone is dropped and cp -c re-cloned), so the siding restarts with the seeded data\n" +
			"while your worktree (code) is left untouched.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			if err := ensureNoRemovalInProgress(app, "reapply"); err != nil {
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
			if err := siding.RequireGuest(sd); err != nil {
				return err
			}
			kept := "keeps code + data"
			if freshData {
				kept = "keeps code, resets data to baseline"
			}
			fmt.Printf("• recreating the guest for %q with current config (%s)…\n", name, kept)
			_, err = siding.Recreate(ctx, app, sd, freshData)
			if err != nil {
				return err
			}
			fmt.Printf("%s %q guest recreated — run `%s up %s` to start it\n", tick(), name, bin(), name)
			return nil
		},
	}
	c.Flags().BoolVar(&freshData, "fresh-data", false, "reset each data volume to the project baseline (keeps the worktree; discards data changes)")
	return c
}
