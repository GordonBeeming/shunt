// Package fsclone makes a siding's host-resident working copy: a git worktree of
// the repo (off main) plus APFS clones (cp -c) of any baseline data volumes.
package fsclone

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gordonbeeming/shunt/internal/proc"
)

// AddWorktree creates a git worktree of repoPath at dest, on a fresh branch
// (newBranch) based on baseBranch. An explicit baseBranch is always honoured.
// An implicit base normally uses HEAD, except when HEAD is GitButler's volatile
// workspace branch: refs created at that commit can be advanced by later
// GitButler workspace rewrites, so the siding starts from origin's default
// branch instead.
func AddWorktree(ctx context.Context, repoPath, dest, newBranch, baseBranch string) error {
	if baseBranch == "" {
		var err error
		baseBranch, err = implicitWorktreeBase(ctx, repoPath)
		if err != nil {
			return err
		}
	}
	// Clear any existing worktree at this path first — a previous siding of this
	// name, or a half-finished `new` that failed after creating the worktree,
	// leaves it checked out on newBranch, and `-B` can't force-update a branch
	// still in use by a live worktree. Remove + prune frees the branch so the add
	// below succeeds on retry.
	_, _ = proc.Run(ctx, "git", "-C", repoPath, "worktree", "remove", "--force", dest)
	_, _ = proc.Run(ctx, "git", "-C", repoPath, "worktree", "prune")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create siding dir: %w", err)
	}
	// --force + -B so re-creating a siding of the same name resets cleanly.
	if _, err := proc.Run(ctx, "git", "-C", repoPath,
		"worktree", "add", "--force", "-B", newBranch, dest, baseBranch); err != nil {
		return fmt.Errorf("git worktree add (base %q): %w", baseBranch, err)
	}
	return nil
}

func implicitWorktreeBase(ctx context.Context, repoPath string) (string, error) {
	head, err := proc.Run(ctx, "git", "-C", repoPath, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		// Exit 1 means HEAD is detached, where the existing HEAD behaviour is still
		// correct. Other failures indicate a repository problem worth reporting.
		if head.ExitCode == 1 {
			return "HEAD", nil
		}
		return "", fmt.Errorf("inspect repository HEAD: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(head.Stdout), "refs/heads/gitbutler/") {
		return "HEAD", nil
	}

	remoteHead, err := proc.Run(ctx, "git", "-C", repoPath,
		"symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err == nil {
		base := strings.TrimSpace(remoteHead.Stdout)
		if !strings.HasPrefix(base, "origin/") || base == "origin/" {
			return "", fmt.Errorf("origin/HEAD resolved to unexpected ref %q", base)
		}
		if err := verifyCommit(ctx, repoPath, base); err != nil {
			return "", fmt.Errorf("resolve GitButler siding base %q: %w", base, err)
		}
		return base, nil
	}
	if remoteHead.ExitCode != 1 {
		return "", fmt.Errorf("resolve origin/HEAD for GitButler siding: %w", err)
	}

	// Clones occasionally lack origin/HEAD. origin/main is a safe conventional
	// fallback only when it actually exists; never fall back to volatile HEAD.
	const fallback = "origin/main"
	if err := verifyCommit(ctx, repoPath, fallback); err != nil {
		return "", fmt.Errorf("GitButler workspace HEAD requires origin/HEAD (or %s); configure the remote default or pass --branch: %w", fallback, err)
	}
	return fallback, nil
}

func verifyCommit(ctx context.Context, repoPath, ref string) error {
	if _, err := proc.Run(ctx, "git", "-C", repoPath,
		"rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		return err
	}
	return nil
}

// AddWorktreeTracking creates a worktree at dest checked out on an EXISTING
// branch (fetched from origin), tracking origin/<branch>. Unlike AddWorktree —
// which forks a fresh siding branch off a start point — this stays on the branch
// itself, so commits continue it and `git push` goes back to the same branch.
// Used by `new --from <branch>` to pick up an existing remote branch in a siding.
func AddWorktreeTracking(ctx context.Context, repoPath, dest, branch string) error {
	_, _ = proc.Run(ctx, "git", "-C", repoPath, "worktree", "remove", "--force", dest)
	_, _ = proc.Run(ctx, "git", "-C", repoPath, "worktree", "prune")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create siding dir: %w", err)
	}
	if _, err := proc.Run(ctx, "git", "-C", repoPath, "fetch", "origin", branch); err != nil {
		return fmt.Errorf("fetch origin/%s (does the remote branch exist?): %w", branch, err)
	}
	// If a local branch of this name already exists, check it out as-is — never
	// `-B`, which would hard-reset it to origin/<branch> and silently discard the
	// user's unpushed commits. Only create it (tracking origin) when it's absent.
	localExists := false
	if _, err := proc.Run(ctx, "git", "-C", repoPath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		localExists = true
	}
	addArgs := []string{"-C", repoPath, "worktree", "add", "--force", "-b", branch, dest, "origin/" + branch}
	if localExists {
		addArgs = []string{"-C", repoPath, "worktree", "add", "--force", dest, branch}
	}
	if _, err := proc.Run(ctx, "git", addArgs...); err != nil {
		return fmt.Errorf("git worktree add (branch %q): %w", branch, err)
	}
	_, _ = proc.Run(ctx, "git", "-C", dest, "branch", "--set-upstream-to=origin/"+branch, branch)
	return nil
}

// RemoveWorktree tears down a siding's worktree at dest and deletes its branch,
// leaving the main repo clean (no dangling worktree registration).
func RemoveWorktree(ctx context.Context, repoPath, dest, branch string) error {
	_, _ = proc.Run(ctx, "git", "-C", repoPath, "worktree", "remove", "--force", dest)
	_, _ = proc.Run(ctx, "git", "-C", repoPath, "worktree", "prune")
	if branch != "" {
		_, _ = proc.Run(ctx, "git", "-C", repoPath, "branch", "-D", branch)
	}
	return nil
}

// CloneVolume APFS-clones a baseline data dir to dest (cp -c, copy-on-write —
// near-instant and space-efficient until written). No-op if src doesn't exist.
func CloneVolume(ctx context.Context, src, dest string) error {
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat baseline %s: %w", src, err)
	}
	// `cp -c -R src dest` copies src *inside* dest when dest already exists
	// (dest/<basename>/… instead of overwriting dest/…), so a re-clone of the same
	// siding would nest. Clear a stale dest first — it's this siding's own COW
	// clone, cheap to recreate.
	if _, err := os.Stat(dest); err == nil {
		if err := os.RemoveAll(dest); err != nil {
			return fmt.Errorf("clear stale volume clone %s: %w", dest, err)
		}
	}
	if _, err := proc.Run(ctx, "cp", "-c", "-R", src, dest); err != nil {
		return fmt.Errorf("cp -c %s -> %s: %w", src, dest, err)
	}
	return nil
}
