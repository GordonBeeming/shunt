// Package cmd is shunt's Cobra command layer. Commands stay thin: they parse
// flags and delegate to the internal/* packages for real work.
package cmd

import (
	"fmt"
	"os"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	id := config.Current()
	root := &cobra.Command{
		Use:           id.BinaryName,
		Short:         "Run parallel app experiments and switch between them with no teardown",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(
		newVersionCmd(),
		newInitCmd(),
		newSkillCmd(),
		newCertCmd(),
		newConfigCmd(),
		newAppCmd(),
		newNewCmd(),
		newUpCmd(),
		newWarmCmd(),
		newRestartCmd(),
		newReapplyCmd(),
		newSwitchCmd(),
		newActiveCmd(),
		newKillCmd(),
		newRmCmd(),
		newLsCmd(),
		newLogsCmd(),
		newGitCmd(),
		newRunCmd(),
		newDashboardCmd(),
		newDebugDiscoverCmd(),
	)
	return root
}

// Execute runs the root command, printing errors and setting the exit code.
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
