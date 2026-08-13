// Package cmd is shunt's Cobra command layer. Commands stay thin: they parse
// flags and delegate to the internal/* packages for real work.
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

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
		newBaseCmd(),
		newNewCmd(),
		newUpCmd(),
		newWarmCmd(),
		newDataCmd(),
		newRestartCmd(),
		newReapplyCmd(),
		newSwitchCmd(),
		newActiveCmd(),
		newKillCmd(),
		newParkCmd(),
		newRmCmd(),
		newCleanupCmd(),
		newSpaceCmd(),
		newTrimCmd(),
		newLsCmd(),
		newStatusCmd(),
		newLogsCmd(),
		newGitCmd(),
		newSyncCmd(),
		newRunCmd(),
		newPlaywrightCmd(),
		newCdCmd(),
		newDashboardCmd(),
		newDebugDiscoverCmd(),
	)
	return root
}

// Execute runs the root command, printing errors and setting the exit code.
func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
