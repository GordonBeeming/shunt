package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/resolve"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	var all, rebase bool
	var from string
	c := &cobra.Command{
		Use:   "sync [name]",
		Short: "Pull the latest default branch (main) into a siding's worktree — merge (--rebase to rebase)",
		Long: "Fetches origin and **merges** the repo's default branch (main, auto-detected from\n" +
			"origin/HEAD) into the siding's shunt/<name> branch — a non-rewriting merge (fast-\n" +
			"forwards when the siding has no local commits, else a merge commit) that picks\n" +
			"up the latest code without rewriting the siding's history.\n" +
			"`--rebase` rebases onto the default instead (rewrites history — force-push afterwards);\n" +
			"`--all` syncs every siding; `--from <branch>` syncs from a branch other than the default.\n\n" +
			"On conflicts it stops with the conflicted files — resolve them in the worktree with\n" +
			"`" + bin() + " git add <files>` then `" + bin() + " git commit` (merge) or `" + bin() + " git rebase --continue` (--rebase).\n" +
			"The siding is taken from the name arg, else cwd, else the live siding.",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, loc, err := loadCurrentApp()
			if err != nil {
				return err
			}
			if err := ensureNoRemovalInProgress(app, "sync"); err != nil {
				return err
			}
			names, err := syncTargets(ctx, app, loc, args, all)
			if err != nil {
				return err
			}

			// Sync sidings concurrently — git fetch/merge is network-bound, so a serial
			// --all stalls on each. Outcomes stay indexed so the summary prints in a
			// deterministic order regardless of which siding finishes first.
			if len(names) > 1 {
				fmt.Printf("• syncing %d sidings…\n", len(names))
			}
			outcomes := make([]syncOutcome, len(names))
			var wg sync.WaitGroup
			for i, name := range names {
				wg.Add(1)
				go func(i int, name string) {
					defer wg.Done()
					outcomes[i] = syncSiding(ctx, app.ConfigDir, name, from, rebase)
				}(i, name)
			}
			wg.Wait()

			var conflicted, failed []string
			for _, res := range outcomes {
				name := res.name
				switch res.kind {
				case syncOK:
					switch {
					case !res.changed:
						fmt.Printf("%s %q: already up to date with %s\n", tick(), name, res.base)
					case rebase && res.ahead:
						fmt.Printf("%s %q: rebased onto %s — if it's been pushed, re-push with `%s git push --force-with-lease`\n",
							tick(), name, res.base, bin())
					case rebase:
						fmt.Printf("%s %q: rebased onto %s\n", tick(), name, res.base)
					default:
						fmt.Printf("%s %q: merged %s\n", tick(), name, res.base)
					}
				case syncConflict:
					conflicted = append(conflicted, name)
					if rebase {
						fmt.Printf("✗ %q: conflicts in %d file(s) — resolve in %s, then `%s git add …` + `%s git rebase --continue` (or `%s git rebase --abort`):\n",
							name, len(res.files), res.src, bin(), bin(), bin())
					} else {
						fmt.Printf("✗ %q: conflicts in %d file(s) — resolve in %s, then `%s git add …` + `%s git commit` (or `%s git merge --abort`):\n",
							name, len(res.files), res.src, bin(), bin(), bin())
					}
					for _, f := range res.files {
						fmt.Printf("    %s\n", f)
					}
				case syncError:
					failed = append(failed, name)
					fmt.Printf("✗ %q: %v\n", name, res.err)
					if res.log != "" {
						fmt.Printf("    %s\n", strings.ReplaceAll(res.log, "\n", "\n    "))
					}
				}
			}

			if all && len(names) > 1 {
				fmt.Printf("\n%d synced, %d with conflicts, %d failed\n",
					len(names)-len(conflicted)-len(failed), len(conflicted), len(failed))
			}
			if len(conflicted)+len(failed) > 0 {
				return fmt.Errorf("%d siding(s) need attention", len(conflicted)+len(failed))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "sync every siding of the current app")
	c.Flags().BoolVar(&rebase, "rebase", false, "rebase onto origin/<default> instead of merging (rewrites history)")
	c.Flags().StringVar(&from, "from", "", "sync from this branch instead of the repo default (main)")
	return c
}

// syncTargets resolves which siding names to sync: every siding with --all, else
// the one named / cwd's siding / the live siding / the picker.
func syncTargets(ctx context.Context, app state.App, loc resolve.Location, args []string, all bool) ([]string, error) {
	if all {
		names := make([]string, 0, len(app.Sidings))
		for n := range app.Sidings {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) == 0 {
			return nil, fmt.Errorf("no sidings to sync — create one with `%s new <name>`", bin())
		}
		return names, nil
	}
	name := ""
	switch {
	case len(args) == 1:
		name = args[0]
	case loc.Siding != "":
		name = loc.Siding
	default:
		name = app.LiveSiding
	}
	if name == "" {
		var err error
		if name, err = pickSiding(ctx, app); err != nil {
			return nil, err
		}
	}
	if _, ok := app.Sidings[name]; !ok {
		return nil, fmt.Errorf("no siding %q in %q", name, app.Name)
	}
	return []string{name}, nil
}

type syncKind int

const (
	syncOK syncKind = iota
	syncConflict
	syncError
)

type syncOutcome struct {
	kind    syncKind
	name    string
	base    string   // default branch synced from
	changed bool     // the target had commits we didn't — the sync integrated something
	ahead   bool     // the branch had local commits (a rebase rewrites them => force-push)
	files   []string // conflicted files (syncConflict)
	log     string   // captured git output (fetch + merge/rebase) — shown on conflict/error
	src     string
	err     error
}

// syncSiding fetches origin and merges (or rebases with --rebase) the default
// branch into the siding worktree's branch. Merge is the default because it's
// non-destructive — a rebase rewrites the siding's (often already-pushed) commits.
// Conflicts are reported, not auto-resolved, so the caller can hand off to
// `shunt git` in the worktree.
func syncSiding(ctx context.Context, configDir, name, fromOverride string, rebase bool) syncOutcome {
	out := syncOutcome{name: name}
	err := withLatestSiding(ctx, configDir, name, "sync", func(app state.App, _ state.Siding) error {
		src, _, err := siding.Paths(app, name)
		if err != nil {
			return err
		}
		out = syncSidingLocked(ctx, app, name, src, fromOverride, rebase)
		return out.err
	})
	if err != nil {
		if out.kind == syncConflict {
			return out
		}
		out.kind, out.err = syncError, err
	}
	return out
}

func syncSidingLocked(ctx context.Context, app state.App, name, src, fromOverride string, rebase bool) syncOutcome {
	base := defaultBranch(ctx, src, fromOverride)
	target := "origin/" + base
	out := syncOutcome{name: name, src: src, base: base}

	// Capture (not stream) the git output so `--all` can run these concurrently
	// without interleaving each siding's output.
	res, err := proc.RunInDir(ctx, src, "git", "fetch", "origin")
	out.log = gitLog(res)
	if err != nil {
		out.kind, out.err = syncError, fmt.Errorf("fetch origin: %w", err)
		return out
	}
	// Measured before the operation: whether the target moved ahead of us (did the
	// sync bring anything in) and whether we carry local commits (a rebase rewrites
	// those, so a prior push then needs --force-with-lease).
	out.changed = commitsBehind(ctx, src, target) > 0
	out.ahead = commitsAhead(ctx, src, target) > 0

	gitArgs := []string{"merge", "--no-edit", target}
	if rebase {
		gitArgs = []string{"rebase", target}
	}
	res, err = proc.RunInDir(ctx, src, "git", gitArgs...)
	if l := gitLog(res); l != "" {
		out.log = strings.TrimSpace(out.log + "\n" + l)
	}
	if err != nil {
		// Unmerged paths => real conflicts to resolve; anything else (e.g. a dirty
		// worktree) is a plain failure (git's captured output explains it).
		if files := conflictedFiles(ctx, src); len(files) > 0 {
			out.kind, out.files = syncConflict, files
			return out
		}
		out.kind, out.err = syncError, err
		return out
	}
	out.kind = syncOK
	return out
}

// gitLog joins a git command's captured stdout+stderr, trimmed.
func gitLog(res proc.Result) string {
	return strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
}

// defaultBranch resolves the repo's default branch from the worktree (origin/HEAD),
// honouring an explicit override; falls back to "main".
func defaultBranch(ctx context.Context, src, override string) string {
	if override != "" {
		return override
	}
	if res, err := proc.RunInDir(ctx, src, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		// Guard the empty/unexpected case: a bare "origin/" or non-origin output would
		// trim to "" and build an invalid "origin/" target — fall back to main.
		if b := strings.TrimPrefix(strings.TrimSpace(res.Stdout), "origin/"); b != "" {
			return b
		}
	}
	return "main"
}

// commitsAhead counts commits on HEAD not yet on target (local work).
func commitsAhead(ctx context.Context, src, target string) int {
	return revListCount(ctx, src, target+"..HEAD")
}

// commitsBehind counts commits on target not yet on HEAD (what a sync pulls in).
func commitsBehind(ctx context.Context, src, target string) int {
	return revListCount(ctx, src, "HEAD.."+target)
}

func revListCount(ctx context.Context, src, rng string) int {
	res, err := proc.RunInDir(ctx, src, "git", "rev-list", "--count", rng)
	if err != nil {
		return 0
	}
	n := 0
	_, _ = fmt.Sscanf(strings.TrimSpace(res.Stdout), "%d", &n)
	return n
}

func conflictedFiles(ctx context.Context, src string) []string {
	res, err := proc.RunInDir(ctx, src, "git", "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
