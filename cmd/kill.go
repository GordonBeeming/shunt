package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

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
			sd, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q", name)
			}
			// StopOrForce gracefully stops, and only when the graceful stop wedges on the
			// guest's cgroup.kill (errno 95) does it force-remove as the escape — the one
			// failure `stop` can't clear. A transient failure comes back as an error (no
			// force), so we never nuke a guest the user could have retried.
			forced, err := container.StopOrForce(ctx, sd.Container)
			if err != nil {
				return err
			}
			sd.Stopped = true
			if forced {
				// Force-removed: the guest is gone (not just stopped), so its bridges are
				// stale and `up` must recreate it rather than `start` it.
				sd.Bridges = nil // match markStopped: a cleared bridges map serializes as null
			}
			app.Sidings[name] = sd
			if app.LiveSiding == name {
				gone := "stopped"
				if forced {
					gone = "force-removed"
				}
				fmt.Printf("⚠ %q was live — the front door now points at a %s guest; switch to another siding\n", name, gone)
			}
			if err := state.SaveApp(app); err != nil {
				return err
			}
			if forced {
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
			_, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q", name)
			}
			if app.LiveSiding == name && !force {
				return fmt.Errorf("siding %q is live — switch away first, or pass --force", name)
			}
			if !force {
				dirty, err := sidingWorktreeHasChanges(ctx, app, name)
				if err != nil {
					return err
				}
				if dirty {
					confirmed, err := confirmDirtyCleanup([]string{name}, os.Stdin, os.Stdout)
					if err != nil {
						return err
					}
					if !confirmed {
						fmt.Println("cleanup cancelled")
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
	sd, ok := app.Sidings[name]
	if !ok {
		return fmt.Errorf("no siding %q", name)
	}

	src, _ := siding.Paths(*app, name)
	base := filepath.Dir(src) // <configDir>/<name>
	// Validate every filesystem target before touching the guest or worktree, so
	// a corrupt state file can't leave a partially removed siding.
	if !filepath.IsAbs(base) || base == "/" || base == "." || filepath.Dir(base) == base {
		return fmt.Errorf("refusing to remove unsafe siding dir %q (resolved from %q)", base, src)
	}

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
	if err := os.RemoveAll(base); err != nil {
		return fmt.Errorf("remove siding dir %s: %w", base, err)
	}
	delete(app.Sidings, name)
	if app.LiveSiding == name {
		app.LiveSiding = ""
	}
	if err := state.SaveApp(*app); err != nil {
		return err
	}
	fmt.Printf("%s removed %q\n", tick(), name)
	return nil
}

func sidingBase(app state.App, name string) (string, error) {
	configDir := filepath.Clean(app.ConfigDir)
	if !filepath.IsAbs(configDir) {
		return "", fmt.Errorf("refusing to remove siding %q with unsafe config dir %q", name, app.ConfigDir)
	}

	base := filepath.Clean(filepath.Join(configDir, name))
	rel, err := filepath.Rel(configDir, base)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing to remove unsafe siding dir %q (config dir %q)", base, configDir)
	}
	return base, nil
}
