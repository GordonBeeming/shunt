package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

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
			"origin/HEAD) into the siding's shunt/<name> branch — a plain 2-parent merge that picks\n" +
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
			names, err := syncTargets(ctx, app, loc, args, all)
			if err != nil {
				return err
			}

			var conflicted, failed []string
			for _, name := range names {
				fmt.Printf("• syncing %q…\n", name)
				res := syncSiding(ctx, app, name, from, rebase)
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
		if name, err = pickSiding(ctx, app, false); err != nil {
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
	base    string   // default branch synced from
	changed bool     // the target had commits we didn't — the sync integrated something
	ahead   bool     // the branch had local commits (a rebase rewrites them => force-push)
	files   []string // conflicted files (syncConflict)
	src     string
	err     error
}

// syncSiding fetches origin and merges (or rebases with --rebase) the default
// branch into the siding worktree's branch. Merge is the default because it's
// non-destructive — a rebase rewrites the siding's (often already-pushed) commits.
// Conflicts are reported, not auto-resolved, so the caller can hand off to
// `shunt git` in the worktree.
func syncSiding(ctx context.Context, app state.App, name, fromOverride string, rebase bool) syncOutcome {
	src, _ := siding.Paths(app, name)
	base := defaultBranch(ctx, src, fromOverride)
	target := "origin/" + base

	if err := proc.RunPassthrough(ctx, "git", "-C", src, "fetch", "origin"); err != nil {
		return syncOutcome{kind: syncError, src: src, base: base, err: fmt.Errorf("fetch origin: %w", err)}
	}
	// Measured before the operation: whether the target moved ahead of us (did the
	// sync bring anything in) and whether we carry local commits (a rebase rewrites
	// those, so a prior push then needs --force-with-lease).
	out := syncOutcome{src: src, base: base,
		changed: commitsBehind(ctx, src, target) > 0,
		ahead:   commitsAhead(ctx, src, target) > 0,
	}

	args := []string{"-C", src, "merge", "--no-edit", target}
	if rebase {
		args = []string{"-C", src, "rebase", target}
	}
	if err := proc.RunPassthrough(ctx, "git", args...); err != nil {
		// Unmerged paths => real conflicts to resolve; anything else (e.g. a dirty
		// worktree) is a plain failure — git's streamed output already explained it.
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

// defaultBranch resolves the repo's default branch from the worktree (origin/HEAD),
// honouring an explicit override; falls back to "main".
func defaultBranch(ctx context.Context, src, override string) string {
	if override != "" {
		return override
	}
	if res, err := proc.RunInDir(ctx, src, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(strings.TrimSpace(res.Stdout), "origin/")
	}
	return "main"
}

// commitsAhead counts commits on HEAD not yet on target (local work).
func commitsAhead(ctx context.Context, src, target string) int { return revListCount(ctx, src, target+"..HEAD") }

// commitsBehind counts commits on target not yet on HEAD (what a sync pulls in).
func commitsBehind(ctx context.Context, src, target string) int { return revListCount(ctx, src, "HEAD.."+target) }

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
