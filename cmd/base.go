package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newBaseCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "base",
		Short: "Show or choose the siding that seeds new worktrees",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			if app.BaseSiding == "" {
				if app.BaseCommit == "" {
					fmt.Println("no source base is configured")
					return nil
				}
				// Detached: the seed is the commit itself, so no siding is held
				// open to carry it. Say which case this is, because "no siding is
				// base" reads as broken otherwise.
				if len(app.Sidings) > 0 {
					fmt.Printf("source base: detached at %s (no siding is the base)\n", app.BaseCommit)
					return nil
				}
				fmt.Printf("source base: saved commit %s (no sidings remain)\n", app.BaseCommit)
				return nil
			}
			fmt.Printf("source base: %s @ %s\n", app.BaseSiding, app.BaseCommit)
			return nil
		},
	}
	c.AddCommand(newBaseSetCmd())
	return c
}

func newBaseSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <siding>",
		Short: "Use a siding's committed HEAD as the source base",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			dirty, err := setBaseSiding(cmd.Context(), &app, args[0])
			if err != nil {
				return err
			}
			fmt.Printf("%s %q is now the source base at %s\n", tick(), app.BaseSiding, app.BaseCommit)
			if dirty {
				fmt.Printf("  warning: %q has uncommitted or untracked files; default `%s new` is blocked until it is clean\n", app.BaseSiding, bin())
			}
			return nil
		},
	}
}

func ensureBaseSelected(ctx context.Context, app *state.App) error {
	if app == nil || !state.NeedsBaseSelection(*app) {
		return nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("this existing project needs a source base; run `%s base set <siding>`", bin())
	}
	name, err := pickSiding(ctx, *app)
	if err != nil {
		return err
	}
	_, err = setBaseSiding(ctx, app, name)
	return err
}

func setBaseSiding(ctx context.Context, app *state.App, name string) (bool, error) {
	if app == nil {
		return false, fmt.Errorf("app is required")
	}
	var dirty bool
	err := siding.WithProjectOperation(ctx, app.ConfigDir, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		if err := siding.EnsureNoRemovalInProgress(current, "set source base"); err != nil {
			return err
		}
		sd, ok := current.Sidings[name]
		if !ok {
			return fmt.Errorf("no siding %q", name)
		}
		src, _, err := siding.Paths(current, name)
		if err != nil {
			return err
		}
		branch, err := currentWorktreeBranch(ctx, src)
		if err != nil {
			return fmt.Errorf("siding %q must be on its recorded branch %q: %w", name, sd.Branch, err)
		}
		if branch != sd.Branch {
			return fmt.Errorf("siding %q is on branch %q, expected %q", name, branch, sd.Branch)
		}
		status, err := gitText(ctx, src, "status", "--porcelain=v1", "--untracked-files=normal")
		if err != nil {
			return fmt.Errorf("inspect siding %q worktree: %w", name, err)
		}
		dirty = status != ""
		commit, err := gitText(ctx, src, "rev-parse", "--verify", "HEAD^{commit}")
		if err != nil {
			return fmt.Errorf("resolve siding %q committed HEAD: %w", name, err)
		}
		control := current.ControlRepoPath
		owner := state.WorktreeOwner(current, sd)
		if control == "" {
			return fmt.Errorf("managed Git control repository is not configured; run `%s app add`", bin())
		}
		if err := ensureControlRepository(ctx, &current, owner, commit); err != nil {
			return err
		}
		pinned, err := fsclone.PinBaseCommit(ctx, control, owner, commit)
		if err != nil {
			return err
		}
		state.EnsureV2(&current)
		current.BaseSiding = name
		current.BaseCommit = pinned
		if err := state.SaveApp(current); err != nil {
			return err
		}
		*app = current
		return nil
	})
	return dirty, err
}

func validateCleanBase(ctx context.Context, app *state.App) (string, error) {
	if app == nil {
		return "", fmt.Errorf("app is required")
	}
	if state.NeedsBaseSelection(*app) {
		return "", fmt.Errorf("this existing project needs a source base; run `%s base set <siding>`", bin())
	}
	if app.BaseSiding == "" {
		if app.BaseCommit == "" {
			return "", fmt.Errorf("no source seed is configured; run `%s app add`", bin())
		}
		if err := ensureControlRepository(ctx, app, app.RepoPath, app.BaseCommit); err != nil {
			return "", err
		}
		pinned, err := fsclone.PinBaseCommit(ctx, app.ControlRepoPath, app.ControlRepoPath, app.BaseCommit)
		if err != nil {
			return "", err
		}
		app.BaseCommit = pinned
		return pinned, nil
	}
	sd, ok := app.Sidings[app.BaseSiding]
	if !ok {
		return "", fmt.Errorf("source base %q is missing; run `%s base set <siding>`", app.BaseSiding, bin())
	}
	src, _, err := siding.Paths(*app, app.BaseSiding)
	if err != nil {
		return "", err
	}
	branch, err := currentWorktreeBranch(ctx, src)
	if err != nil || branch != sd.Branch {
		return "", fmt.Errorf("source base %q is not on its recorded branch %q", app.BaseSiding, sd.Branch)
	}
	status, err := gitText(ctx, src, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "", fmt.Errorf("source base %q has uncommitted or untracked files; commit them or use `--branch`/`--from` explicitly", app.BaseSiding)
	}
	commit, err := gitText(ctx, src, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	owner := state.WorktreeOwner(*app, sd)
	if err := ensureControlRepository(ctx, app, owner, commit); err != nil {
		return "", err
	}
	pinned, err := fsclone.PinBaseCommit(ctx, app.ControlRepoPath, owner, commit)
	if err != nil {
		return "", err
	}
	app.BaseCommit = pinned
	return pinned, nil
}

func currentWorktreeBranch(ctx context.Context, src string) (string, error) {
	branch, detached, err := currentWorktreeBranchState(ctx, src)
	if err != nil {
		return "", err
	}
	if detached {
		return "", fmt.Errorf("HEAD is detached")
	}
	return branch, nil
}

func currentWorktreeBranchState(ctx context.Context, src string) (branch string, detached bool, err error) {
	result, err := proc.Run(ctx, "git", "-C", src, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		if result.ExitCode == 1 {
			return "", true, nil
		}
		return "", false, err
	}
	return strings.TrimPrefix(strings.TrimSpace(result.Stdout), "refs/heads/"), false, nil
}

func ensureControlRepository(ctx context.Context, app *state.App, source, seed string) error {
	if app.ControlRepoPath == "" {
		return fmt.Errorf("managed Git control repository path is not configured")
	}
	if seed == "" {
		seed = "HEAD"
	}
	commit, err := fsclone.EnsureControlRepo(ctx, app.ControlRepoPath, source, app.RepoOrigin, seed)
	if err != nil {
		return err
	}
	if app.BaseCommit == "" {
		app.BaseCommit = commit
	}
	return nil
}

func gitText(ctx context.Context, repo string, args ...string) (string, error) {
	full := append([]string{"-C", repo}, args...)
	result, err := proc.Run(ctx, "git", full...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}

func sortedSidingNames(app state.App, exclude map[string]bool) []string {
	names := make([]string, 0, len(app.Sidings))
	for name := range app.Sidings {
		if !exclude[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
