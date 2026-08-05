// Package fsclone makes a siding's host-resident working copy: a git worktree of
// the repo (off main) plus APFS clones (cp -c) of any baseline data volumes.
package fsclone

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
		return fmt.Errorf("verify commit ref %q: %w", ref, err)
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

// CloneVolume creates a fidelity-checked copy-on-write clone of a baseline data
// directory. No-op if src doesn't exist.
func CloneVolume(ctx context.Context, src, dest string) error {
	info, err := os.Lstat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat baseline %s: %w", src, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("baseline %s is not a directory", src)
	}
	// `cp -c -R src dest` copies src *inside* dest when dest already exists
	// (dest/<basename>/… instead of overwriting dest/…), so a re-clone of the same
	// siding would nest. Clear a stale dest first — it's this siding's own COW
	// clone, cheap to recreate.
	if _, err := os.Lstat(dest); err == nil {
		if err := os.RemoveAll(dest); err != nil {
			return fmt.Errorf("clear stale volume clone %s: %w", dest, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect stale volume clone %s: %w", dest, err)
	}
	if err := cloneVolumeTree(ctx, src, dest); err != nil {
		return fmt.Errorf("clone volume %s -> %s: %w", src, dest, err)
	}
	return nil
}

// VolumeSetResult preserves the commit point when cleanup of a replaced root
// fails after an atomic installation.
type VolumeSetResult struct {
	Committed     bool
	RecoveryPaths []string
}

// VolumeSetCleanupError reports whether the destination committed and the
// exact paths left for deterministic cleanup.
type VolumeSetCleanupError struct {
	Committed     bool
	RecoveryPaths []string
	Err           error
}

func (e *VolumeSetCleanupError) Error() string {
	return fmt.Sprintf("volume root committed=%t, but cleanup failed; recover from %v: %v", e.Committed, e.RecoveryPaths, e.Err)
}

func (e *VolumeSetCleanupError) Unwrap() error { return e.Err }

type volumeSetOps struct {
	rename     func(string, string) error
	renameSwap func(string, string) error
	remove     func(string) error
}

// CloneVolumeSet creates a complete copy-on-write volume root before replacing
// destination, so callers never expose a mixture of old and reset volumes.
func CloneVolumeSet(ctx context.Context, sourceRoot, destinationRoot string, volumes []string) error {
	_, err := CloneVolumeSetResult(ctx, sourceRoot, destinationRoot, volumes)
	return err
}

// CloneVolumeSetResult is CloneVolumeSet with an explicit post-commit cleanup
// result for callers that must distinguish an unchanged destination from an
// installed destination whose retired root still needs removal.
func CloneVolumeSetResult(ctx context.Context, sourceRoot, destinationRoot string, volumes []string) (VolumeSetResult, error) {
	return cloneVolumeSet(ctx, sourceRoot, destinationRoot, volumes, volumeSetOps{
		rename: os.Rename, renameSwap: renameSwap, remove: os.RemoveAll,
	})
}

func cloneVolumeSet(ctx context.Context, sourceRoot, destinationRoot string, volumes []string, ops volumeSetOps) (VolumeSetResult, error) {
	if err := validateVolumeNames(volumes); err != nil {
		return VolumeSetResult{}, err
	}
	parent := filepath.Dir(destinationRoot)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return VolumeSetResult{}, fmt.Errorf("create volume root parent: %w", err)
	}
	stage, err := os.MkdirTemp(parent, ".volumes-stage-")
	if err != nil {
		return VolumeSetResult{}, fmt.Errorf("create volume stage: %w", err)
	}

	for _, volume := range volumes {
		if err := ctx.Err(); err != nil {
			return cleanupVolumeSetFailure(ops, stage, err)
		}
		source := filepath.Join(sourceRoot, volume)
		destination := filepath.Join(stage, volume)
		info, err := os.Lstat(source)
		if err != nil {
			if !os.IsNotExist(err) {
				return cleanupVolumeSetFailure(ops, stage, fmt.Errorf("stat source volume %q: %w", volume, err))
			}
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return cleanupVolumeSetFailure(ops, stage, fmt.Errorf("create empty volume %q: %w", volume, err))
			}
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return cleanupVolumeSetFailure(ops, stage, fmt.Errorf("source volume %q is not a directory", volume))
		}
		if err := CloneVolume(ctx, source, destination); err != nil {
			return cleanupVolumeSetFailure(ops, stage, fmt.Errorf("clone volume %q: %w", volume, err))
		}
	}

	if err := validateVolumeRoot(stage, volumes); err != nil {
		return cleanupVolumeSetFailure(ops, stage, err)
	}
	if _, err := os.Lstat(destinationRoot); os.IsNotExist(err) {
		if err := ops.rename(stage, destinationRoot); err != nil {
			return cleanupVolumeSetFailure(ops, stage, fmt.Errorf("install volume root: %w", err))
		}
		return VolumeSetResult{Committed: true}, nil
	} else if err != nil {
		return cleanupVolumeSetFailure(ops, stage, fmt.Errorf("stat destination volume root: %w", err))
	}

	if err := ops.renameSwap(stage, destinationRoot); err != nil {
		return cleanupVolumeSetFailure(ops, stage, fmt.Errorf("swap volume root: %w", err))
	}
	if err := ops.remove(stage); err != nil {
		paths := existingVolumePaths(stage)
		result := VolumeSetResult{Committed: true, RecoveryPaths: paths}
		return result, &VolumeSetCleanupError{Committed: true, RecoveryPaths: paths, Err: fmt.Errorf("remove replaced root: %w", err)}
	}
	return VolumeSetResult{Committed: true}, nil
}

func cleanupVolumeSetFailure(ops volumeSetOps, stage string, operationErr error) (VolumeSetResult, error) {
	if err := ops.remove(stage); err != nil {
		paths := existingVolumePaths(stage)
		result := VolumeSetResult{RecoveryPaths: paths}
		return result, &VolumeSetCleanupError{RecoveryPaths: paths, Err: errors.Join(operationErr, fmt.Errorf("remove uncommitted stage: %w", err))}
	}
	return VolumeSetResult{}, operationErr
}

func existingVolumePaths(paths ...string) []string {
	var result []string
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			result = append(result, filepath.Clean(path))
		}
	}
	sort.Strings(result)
	return result
}

func validateVolumeNames(volumes []string) error {
	seen := make(map[string]struct{}, len(volumes))
	for _, volume := range volumes {
		if volume == "" || volume == "." || volume == ".." || filepath.Base(volume) != volume {
			return fmt.Errorf("unsafe data volume name %q", volume)
		}
		if _, exists := seen[volume]; exists {
			return fmt.Errorf("duplicate data volume name %q", volume)
		}
		seen[volume] = struct{}{}
	}
	return nil
}

func validateVolumeRoot(root string, volumes []string) error {
	for _, volume := range volumes {
		info, err := os.Lstat(filepath.Join(root, volume))
		if err != nil {
			return fmt.Errorf("validate volume %q: %w", volume, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("validate volume %q: not a directory", volume)
		}
	}
	return nil
}

func renameSwap(from, to string) error {
	return renamexSwap(from, to)
}
