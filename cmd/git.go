package cmd

import (
	"fmt"

	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/spf13/cobra"
)

// newGitCmd is a full git pass-through in a siding's worktree. A siding's source
// is a git worktree on the host; git doesn't resolve inside the guest (the
// worktree's gitdir lives outside the mount), so every git verb runs host git in
// the worktree — where it signs with your usual key and acts on the siding's
// shunt/<name> branch. Everything after `git` is forwarded verbatim, so beyond
// commit/push this covers the conflict-resolution set — status, diff, add,
// restore, rebase --continue|--abort, merge --continue|--abort — that `shunt sync`
// hands off to when a rebase hits conflicts.
func newGitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "git [git args...]",
		Short: "Run any git command in a siding's worktree on the host",
		Long: "A siding's source is a git worktree on the host; git doesn't resolve inside the guest.\n" +
			"`shunt git <args...>` forwards everything to host git in that worktree, so commits\n" +
			"sign with your usual key and act on the siding's shunt/<name> branch.\n\n" +
			"Beyond commit/push this is the full git surface for resolving a `shunt sync` conflict\n" +
			"in the worktree: status, diff, add, restore, rebase --continue|--abort, merge --abort.\n\n" +
			"The siding is taken from cwd (when inside one) or the app's live siding.",
		// Forward flags (e.g. -m, --amend, --continue) to git instead of parsing here.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Keep our own help for the bare/`--help` invocation; forward everything else.
			if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
				return cmd.Help()
			}
			src, err := resolveSidingWorktree()
			if err != nil {
				return err
			}
			full := append([]string{"-C", src}, args...)
			return proc.RunPassthrough(cmd.Context(), "git", full...)
		},
	}
	return c
}

// resolveSidingWorktree returns the host worktree path of the siding to act on:
// the one cwd is inside, else the app's live siding.
func resolveSidingWorktree() (string, error) {
	app, loc, err := loadCurrentApp()
	if err != nil {
		return "", err
	}
	name := loc.Siding
	if name == "" {
		name = app.LiveSiding
	}
	if name == "" {
		return "", fmt.Errorf("which siding? cd into one, or make one live (`%s ls`)", bin())
	}
	if _, ok := app.Sidings[name]; !ok {
		return "", fmt.Errorf("no siding %q in %q", name, app.Name)
	}
	src, _ := siding.Paths(app, name)
	return src, nil
}
