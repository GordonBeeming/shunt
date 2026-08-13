package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/resolve"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

const newSidingResourceServicePort = 18890

func newNewCmd() *cobra.Command {
	var branch, from string
	c := &cobra.Command{
		Use:   "new <name>",
		Short: "Create a small worktree-only siding",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			if err := siding.ValidateName(name); err != nil {
				return err
			}
			if from != "" && branch != "" {
				return fmt.Errorf("--from and --branch are mutually exclusive: --from continues an existing branch, --branch forks a new siding branch off a start point")
			}
			app, err := loadOrInitializeNewApp(ctx)
			if err != nil {
				return err
			}
			if branch == "" && from == "" {
				if err := ensureBaseSelected(ctx, &app); err != nil {
					return err
				}
			}

			if from != "" {
				fmt.Printf("• creating worktree from existing branch %q for %q…\n", from, name)
			} else {
				fmt.Printf("• creating worktree for %q…\n", name)
			}
			app, sd, err := createSiding(ctx, app.ConfigDir, name, branch, from)
			if err != nil {
				return err
			}
			src, _, err := siding.Paths(app, name)
			if err != nil {
				return err
			}
			fmt.Printf("%s siding %q ready — worktree only; no data or guest has been created.\n", tick(), name)
			fmt.Printf("  edit code here:  %s\n", src)
			fmt.Printf("  on branch:       %s\n", sd.Branch)
			if app.Runner == "" {
				fmt.Printf("  grow it when needed:  add .shunt.app.json, run "+bin()+" app add, then "+bin()+" up %s\n", name)
			} else {
				fmt.Printf("  grow it when needed:  "+bin()+" up %s   (creates data + guest, then starts the app)\n", name)
			}
			return nil
		},
	}
	c.Flags().StringVar(&branch, "branch", "", "fork a new siding branch off this explicit start point (branch or commit; default: the clean source base)")
	c.Flags().StringVar(&from, "from", "", "create the siding ON an existing remote branch (fetched + tracked), so commits push back to it")
	return c
}

// loadOrInitializeNewApp keeps `new` useful before an application has any
// runtime contract. Existing project state wins unchanged. Otherwise, the Git
// top-level gets only the durable source-control state needed for managed
// worktrees; app registration and the channel registry remain untouched.
func loadOrInitializeNewApp(ctx context.Context) (state.App, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return state.App{}, fmt.Errorf("get cwd: %w", err)
	}

	repoRoot, err := gitText(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return state.App{}, fmt.Errorf("`%s new` requires a Git repository: %w", bin(), err)
	}
	repoRoot = filepath.Clean(repoRoot)
	loc, existing, err := resolveGitRootProject(repoRoot)
	if err != nil {
		return state.App{}, err
	}
	if existing != nil {
		app := *existing
		if app.LiveSiding == state.HostTarget {
			app.LiveSiding = ""
		}
		return app, nil
	}
	configDir := loc.ConfigDir

	var app state.App
	err = siding.WithProjectOperation(ctx, configDir, func() error {
		current, loadErr := state.LoadApp(configDir)
		if loadErr == nil {
			app = current
			if app.LiveSiding == state.HostTarget {
				app.LiveSiding = ""
			}
			return nil
		}
		if !errors.Is(loadErr, state.ErrNotFound) {
			return loadErr
		}

		app = state.App{
			Version:         state.StateVersion,
			Name:            filepath.Base(repoRoot),
			RepoOrigin:      gitOrigin(ctx, repoRoot),
			RepoPath:        repoRoot,
			ControlRepoPath: filepath.Join(configDir, ".control.git"),
			ConfigDir:       configDir,
			Sidings:         map[string]state.Siding{},
		}
		if err := ensureControlRepository(ctx, &app, repoRoot, ""); err != nil {
			return fmt.Errorf("initialize managed Git control repository: %w", err)
		}
		if err := state.SaveApp(app); err != nil {
			return fmt.Errorf("save worktree-only project state: %w", err)
		}
		return nil
	})
	if err != nil {
		return state.App{}, err
	}
	return app, nil
}

// resolveGitRootProject recognizes a Git root as a siding only when durable
// state records that siding and its canonical source path is exactly this root.
// A coincidental .shunt[-channel]/<project>/<name>/... path remains its own repo.
func resolveGitRootProject(repoRoot string) (resolve.Location, *state.App, error) {
	repoRoot = filepath.Clean(repoRoot)
	loc, err := resolve.From(repoRoot)
	if err != nil {
		return resolve.Location{}, nil, err
	}
	if loc.Siding == "" {
		return loc, nil, nil
	}

	app, err := state.LoadApp(loc.ConfigDir)
	if err != nil {
		if !errors.Is(err, state.ErrNotFound) {
			return resolve.Location{}, nil, err
		}
		return gitRepositoryLocation(repoRoot)
	}
	if _, exists := app.Sidings[loc.Siding]; exists {
		src, _, pathErr := siding.Paths(app, loc.Siding)
		if pathErr != nil {
			return resolve.Location{}, nil, pathErr
		}
		if filepath.Clean(src) == repoRoot {
			return loc, &app, nil
		}
	}
	return gitRepositoryLocation(repoRoot)
}

func gitRepositoryLocation(repoRoot string) (resolve.Location, *state.App, error) {
	configDir, err := config.ProjectConfigDir(repoRoot)
	if err != nil {
		return resolve.Location{}, nil, err
	}
	return resolve.Location{
		Project:   filepath.Base(repoRoot),
		ConfigDir: configDir,
	}, nil, nil
}

func createSiding(ctx context.Context, configDir, name, branch, from string) (state.App, state.Siding, error) {
	return createSidingWithOps(ctx, configDir, name, branch, from, createSidingOps{saveApp: state.SaveApp})
}

type createSidingOps struct {
	saveApp func(state.App) error
}

func createSidingWithOps(ctx context.Context, configDir, name, branch, from string, ops createSidingOps) (state.App, state.Siding, error) {
	if ops.saveApp == nil {
		return state.App{}, state.Siding{}, errors.New("state save callback is required")
	}
	var app state.App
	var created state.Siding
	err := siding.WithProjectOperation(ctx, configDir, func() error {
		current, err := state.LoadApp(configDir)
		if err != nil {
			return err
		}
		app = current
		if err := siding.EnsureNoRemovalInProgress(app, "create siding"); err != nil {
			return err
		}
		if _, exists := app.Sidings[name]; exists {
			return fmt.Errorf("siding %q already exists", name)
		}

		source := app.RepoPath
		if base, ok := app.Sidings[app.BaseSiding]; ok {
			source = state.WorktreeOwner(app, base)
		}
		start := branch
		if branch == "" && from == "" {
			start, err = validateCleanBase(ctx, &app)
			if err != nil {
				return err
			}
		} else {
			seed := app.BaseCommit
			if branch != "" {
				seed = branch
			}
			if err := ensureControlRepository(ctx, &app, source, seed); err != nil {
				return err
			}
			if branch != "" {
				start, err = fsclone.ResolveStartPoint(ctx, app.ControlRepoPath, source, branch)
				if err != nil {
					return err
				}
			}
		}

		src, _, err := siding.Paths(app, name)
		if err != nil {
			return err
		}
		worktreeBranch := config.BranchPrefix() + name
		if from != "" {
			worktreeBranch = from
			if _, err := fsclone.AddWorktreeFromRemoteBranch(ctx, app.ControlRepoPath, src, from); err != nil {
				return err
			}
		} else if err := fsclone.AddWorktree(ctx, app.ControlRepoPath, src, worktreeBranch, start); err != nil {
			return err
		}

		created = state.Siding{
			Name:                 name,
			Branch:               worktreeBranch,
			WorktreeRepoPath:     app.ControlRepoPath,
			MaterializationPhase: state.PhaseWorktree,
			Container:            config.ContainerName(app.Name, name),
			CreatedAt:            time.Now().Format(time.RFC3339),
			RSPort:               newSidingResourceServicePort,
			Bridges:              map[string]int{},
		}
		if app.BaseSiding == "" && len(app.Sidings) == 0 {
			commit, err := gitText(ctx, src, "rev-parse", "--verify", "HEAD^{commit}")
			if err != nil {
				return errors.Join(err, cleanupCreatedSiding(ctx, app, created, src))
			}
			pinned, err := fsclone.PinBaseCommit(ctx, app.ControlRepoPath, app.ControlRepoPath, commit)
			if err != nil {
				return errors.Join(err, cleanupCreatedSiding(ctx, app, created, src))
			}
			app.BaseSiding = name
			app.BaseCommit = pinned
		}
		app.Sidings[name] = created
		if err := ops.saveApp(app); err != nil {
			var committed *state.CommittedDurabilityError
			if errors.As(err, &committed) {
				return err
			}
			return errors.Join(err, cleanupCreatedSiding(ctx, app, created, src))
		}
		return nil
	})
	return app, created, err
}

func cleanupCreatedSiding(ctx context.Context, app state.App, created state.Siding, sourcePath string) error {
	if created.Branch == "" {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	err := fsclone.RemoveWorktree(cleanupCtx, app.ControlRepoPath, sourcePath, created.Branch)
	if err != nil {
		return fmt.Errorf("clean up unpublished siding %q: %w", created.Name, err)
	}
	return nil
}
