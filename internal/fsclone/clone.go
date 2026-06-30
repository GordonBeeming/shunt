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
	// Drop any stale registration from a previous siding at this path.
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
	if _, err := proc.Run(ctx, "cp", "-c", "-R", src, dest); err != nil {
		return fmt.Errorf("cp -c %s -> %s: %w", src, dest, err)
	}
	return nil
}
