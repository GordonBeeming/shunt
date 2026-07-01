// Package fsclone makes a siding's host-resident working copy: a git worktree of
// the repo (off main) plus APFS clones (cp -c) of any baseline data volumes.
package fsclone

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gordonbeeming/shunt/internal/proc"
)

// AddWorktree creates a git worktree of repoPath at dest, on a fresh branch
// (newBranch) based on baseBranch. baseBranch defaults to HEAD — the repo's
// CURRENT state — which for a GitButler repo is the workspace commit (all applied
// virtual branches merged), i.e. the code you're actually working on. Basing off
// a long-lived branch like `main` would run stale code, since GitButler only
// advances main on integrate/push. The new branch is a frozen snapshot, so
// GitButler's later workspace rewrites don't affect it; it inherits the repo's
// signing config, and GitButler ignores extra worktrees on their own branches.
func AddWorktree(ctx context.Context, repoPath, dest, newBranch, baseBranch string) error {
	if baseBranch == "" {
		baseBranch = "HEAD"
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
