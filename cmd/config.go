package cmd

import (
	"fmt"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/spf13/cobra"
)

// newConfigCmd groups shunt user-config commands. Config is per channel, stored
// at <global-dir>/config.json; a project's .shunt.app.json can override per app.
func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Get/set shunt user config (defaults the contract can override per app)",
	}
	c.AddCommand(
		configField("branchPrefix", "siding worktree branch prefix, e.g. gb/shunt/",
			func(u *config.UserConfig) *string { return &u.BranchPrefix }, config.BranchPrefix),
		configField("memory", "default per-guest RAM cap, e.g. 4g (bump heavy apps in their contract)",
			func(u *config.UserConfig) *string { return &u.Memory }, config.GuestMemory),
		configField("cpus", "default per-guest CPU cap, e.g. 4",
			func(u *config.UserConfig) *string { return &u.CPUs }, config.GuestCPUs),
	)
	return c
}

// configField builds a `shunt config <name> [value]` get/set command for one
// string field; `effective` prints the value-in-effect (with default applied).
func configField(name, desc string, field func(*config.UserConfig) *string, effective func() string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [value]",
		Short: "Get or set the " + desc,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Println(effective())
				return nil
			}
			uc := config.LoadUserConfig()
			*field(&uc) = args[0]
			if err := config.SaveUserConfig(uc); err != nil {
				return err
			}
			fmt.Printf("✓ %s = %q\n", name, args[0])
			fmt.Println("  applies to sidings created from now on")
			return nil
		},
	}
}
