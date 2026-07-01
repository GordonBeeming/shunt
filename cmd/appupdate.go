package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

// newAppUpdateCmd applies .shunt.app.json edits to the already-registered app.
// It's the front-door counterpart to `reapply` (which recreates the guest):
// `app add` = first registration, `app update` = apply contract edits in place.
func newAppUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Apply .shunt.app.json front-door edits to the registered app (in place)",
		Long: "Re-derives the front door from the contract for the app registered here and applies only\n" +
			"what changed: added routes are registered and bridged onto the live siding, removed ones are\n" +
			"dropped, unchanged ones are left untouched (no front-door blackout). Resolves the project\n" +
			"case-insensitively, so it works from a differently-cased cwd. Use `app add` for the first\n" +
			"registration, and `reapply` for guest settings (memory/cpus/mounts/env).",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			return applyContract(cmd.Context(), cwd, applyUpdate)
		},
	}
}
