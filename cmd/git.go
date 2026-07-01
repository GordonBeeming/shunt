package cmd

import (
	"fmt"

	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/spf13/cobra"
)

// newGitCmd groups git pass-through commands. A siding's source is a git
// worktree on the host; git doesn't resolve inside the guest (the worktree's
// gitdir lives outside the mount), so these run host git in the worktree — where
// it signs with your usual key and pushes the siding's shunt/<name> branch.
func newGitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "git",
		Short: "Run git (commit/push) in a siding's worktree on the host",
		Long: "A siding's source is a git worktree on the host; git doesn't resolve inside the guest.\n" +
			"These pass commit/push straight through to host git in that worktree, so commits\n" +
			"sign with your usual key and push the siding's shunt/<name> branch.\n\n" +
			"The siding is taken from cwd (when inside one) or the app's live siding.",
	}
	c.AddCommand(newGitPassthroughCmd("commit"))
	c.AddCommand(newGitPassthroughCmd("push"))
	return c
}

// newGitPassthroughCmd builds `shunt git <sub> [git args...]` — everything after
// the subcommand is forwarded verbatim to `git -C <worktree> <sub> ...`.
func newGitPassthroughCmd(sub string) *cobra.Command {
	return &cobra.Command{
		Use:   sub + " [git args...]",
		Short: "Run `git " + sub + "` in the siding's worktree",
		// Forward flags (e.g. -m, --amend) to git instead of parsing them here.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			src, err := resolveSidingWorktree()
			if err != nil {
				return err
			}
			full := append([]string{"-C", src, sub}, args...)
			return proc.RunPassthrough(cmd.Context(), "git", full...)
		},
	}
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
