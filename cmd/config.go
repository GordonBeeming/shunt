package cmd

import (
	"fmt"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/spf13/cobra"
)

// newConfigCmd groups shunt user-config commands. Config is per channel, stored
// at <global-dir>/config.json.
func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Get/set shunt user config",
	}
	c.AddCommand(newConfigBranchPrefixCmd())
	return c
}

// newConfigBranchPrefixCmd gets or sets the siding worktree branch prefix.
func newConfigBranchPrefixCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "branchPrefix [value]",
		Short: "Get or set the siding worktree branch prefix (default \"shunt/\")",
		Long: "With no value, prints the current prefix. With a value, sets it — e.g.\n" +
			"`gb/shunt/` so siding branches already follow your branch convention and\n" +
			"don't need renaming before push.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Println(config.BranchPrefix())
				return nil
			}
			uc := config.LoadUserConfig()
			uc.BranchPrefix = args[0]
			if err := config.SaveUserConfig(uc); err != nil {
				return err
			}
			fmt.Printf("✓ branchPrefix = %q\n", uc.BranchPrefix)
			fmt.Println("  applies to sidings created from now on; existing ones keep their branch")
			return nil
		},
	}
}
