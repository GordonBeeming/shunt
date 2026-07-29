package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newKillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kill [name]",
		Short: "Stop a siding's guest (keeps its clone + data to restart later)",
		Args:  cobra.MaximumNArgs(1),
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
			if name == state.HostTarget {
				return fmt.Errorf("%q is your local copy, not a siding — nothing for shunt to tear down", name)
			}
			result, err := siding.Stop(ctx, app, name)
			if err != nil {
				return err
			}
			if result.WasLive {
				gone := "stopped"
				if result.Forced {
					gone = "force-removed"
				}
				fmt.Printf("⚠ %q was live — the front door now points at a %s guest; switch to another siding\n", name, gone)
			}
			if result.Forced {
				fmt.Printf("%s %q was wedged on its cgroup — force-removed it; run `%s up %s` to recreate it (worktree + data are kept)\n", tick(), name, bin(), name)
			} else {
				fmt.Printf("%s stopped %q\n", tick(), name)
			}
			return nil
		},
	}
}

func newRmCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "rm [name]",
		Short: "Tear down a siding: stop + remove the guest and delete its clone/data",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			in := bufio.NewReader(os.Stdin)
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			name, err := sidingArgWithReader(ctx, app, args, in)
			if err != nil {
				return err
			}
			if name == state.HostTarget {
				return fmt.Errorf("%q is your local copy, not a siding — nothing for shunt to tear down", name)
			}
			_, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q", name)
			}
			if app.LiveSiding == name && !force {
				return fmt.Errorf("siding %q is live — switch away first, or pass --force", name)
			}
			if !force {
				dirty, err := sidingWorktreeHasChanges(ctx, app, name, []string{name})
				if err != nil {
					return err
				}
				if dirty {
					confirmed, err := confirmDirtyCleanup([]string{name}, in, os.Stdout)
					if err != nil {
						return err
					}
					if !confirmed {
						fmt.Println("removal cancelled")
						return nil
					}
				}
			}
			return removeSiding(ctx, &app, name)
		},
	}
	c.Flags().BoolVarP(&force, "force", "f", false, "remove even if the siding is live or its worktree has uncommitted changes")
	return c
}

func removeSiding(ctx context.Context, app *state.App, name string) error {
	return siding.WithSidingOperation(ctx, app.ConfigDir, name, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		*app = current
		return removeSidingLocked(ctx, app, name)
	})
}

func removeSidingLocked(ctx context.Context, app *state.App, name string) error {
	sd, ok := app.Sidings[name]
	if !ok {
		return fmt.Errorf("no siding %q", name)
	}

	src, _, err := siding.Paths(*app, name)
	if err != nil {
		return err
	}
	// Paths validates every filesystem target before the guest or worktree is
	// touched, so corrupt state cannot leave a partially removed siding.

	fmt.Printf("• removing guest %q…\n", sd.Container)
	if err := container.Remove(ctx, sd.Container); err != nil {
		return err
	}

	// Tear down the git worktree + its branch from the main repo first, so
	// removing the dir doesn't leave a dangling worktree registration.
	fmt.Println("• removing the worktree…")
	if err := fsclone.RemoveWorktree(ctx, app.RepoPath, src, sd.Branch); err != nil {
		return err
	}

	// Deleting the copy-on-write data clones is the slow part (a large SQL
	// volume can take a while), so say so instead of hanging silently.
	fmt.Println("• deleting siding data (a large data volume can take a while)…")
	if err := siding.RemoveFiles(*app, name); err != nil {
		return err
	}
	updated, err := siding.RemoveSidingState(ctx, app.ConfigDir, name)
	if err != nil {
		return err
	}
	*app = updated
	fmt.Printf("%s removed %q\n", tick(), name)
	return nil
}
