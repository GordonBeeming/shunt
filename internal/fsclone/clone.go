// Package fsclone makes a siding's host-resident working copy: a git worktree of
// the repo (off main) plus APFS clones (cp -c) of any baseline data volumes.
package fsclone

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/state"
)

// EnsureRemovalRecoveryRefs creates deterministic Shunt-owned refs at the exact
// witnessed OIDs in one update-ref transaction. Explicitly absent targets need
// no recovery ref.
func EnsureRemovalRecoveryRefs(ctx context.Context, repoPath, operationID string, targets []state.RemovalTarget) ([]string, error) {
	lines := []string{"start"}
	refs := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.ExpectedOID == "" {
			continue
		}
		digest := sha256.Sum256([]byte(target.Ref))
		ref := fmt.Sprintf("refs/shunt/recovery/%s/%x", safeOperationID(operationID), digest[:8])
		refs = append(refs, ref)
		lines = append(lines, "update "+ref+" "+target.ExpectedOID)
	}
	if len(refs) == 0 {
		return refs, nil
	}
	lines = append(lines, "prepare", "commit")
	tmp, err := os.CreateTemp("", "shunt-update-ref-*.stdin")
	if err != nil {
		return nil, err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := proc.RunStdin(ctx, path, "git", "-C", repoPath, "update-ref", "--stdin"); err != nil {
		return nil, fmt.Errorf("create removal recovery refs: %w", err)
	}
	sort.Strings(refs)
	return refs, nil
}

func RemoveRecoveryRefs(ctx context.Context, repoPath string, refs []string) error {
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "refs/shunt/recovery/") {
			return fmt.Errorf("refuse to remove non-recovery ref %q", ref)
		}
		if _, err := proc.Run(ctx, "git", "-C", repoPath, "update-ref", "-d", ref); err != nil {
			return err
		}
	}
	return nil
}

func ValidateRecoveryRefs(ctx context.Context, repoPath string, refs []string) error {
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "refs/shunt/recovery/") {
			return fmt.Errorf("invalid recovery ref %q", ref)
		}
		if _, err := proc.Run(ctx, "git", "-C", repoPath, "show-ref", "--verify", "--quiet", ref); err != nil {
			return fmt.Errorf("recovery ref %q is missing: %w", ref, err)
		}
	}
	return nil
}

func ValidateRemovalTargets(ctx context.Context, repoPath string, targets []state.RemovalTarget) error {
	for _, target := range targets {
		present, presentErr := proc.Run(ctx, "git", "-C", repoPath, "show-ref", "--verify", "--quiet", target.Ref)
		if target.ExpectedOID == "" {
			if presentErr == nil {
				return fmt.Errorf("removal target %q appeared after confirmation", target.Ref)
			}
			if present.ExitCode != 1 {
				return fmt.Errorf("inspect expected-absent removal target %q: %w", target.Ref, presentErr)
			}
			continue
		}
		if presentErr != nil {
			return fmt.Errorf("removal target %q disappeared after confirmation: %w", target.Ref, presentErr)
		}
		result, err := proc.Run(ctx, "git", "-C", repoPath, "rev-parse", "--verify", target.Ref+"^{commit}")
		if err != nil || strings.TrimSpace(result.Stdout) != target.ExpectedOID {
			return fmt.Errorf("removal target %q moved after confirmation", target.Ref)
		}
	}
	return nil
}

func safeOperationID(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "operation"
	}
	return b.String()
}

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
	// Clear any existing worktree at this exact path first — a previous siding of
	// this name, or a half-finished `new` that failed after creating the worktree,
	// leaves it checked out on newBranch, and `-B` can't force-update a branch
	// still in use by a live worktree.
	if err := clearRegisteredWorktree(ctx, repoPath, dest); err != nil {
		return err
	}
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
	if err := clearRegisteredWorktree(ctx, repoPath, dest); err != nil {
		return err
	}
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
	if _, err := proc.Run(ctx, "git", "-C", dest, "branch", "--set-upstream-to=origin/"+branch, branch); err != nil {
		return fmt.Errorf("set upstream for branch %q: %w", branch, err)
	}
	return nil
}

// RemoveWorktree tears down a siding's worktree at dest and deletes its branch,
// leaving the main repo clean (no dangling worktree registration).
func RemoveWorktree(ctx context.Context, repoPath, dest, branch string) error {
	if err := removeExactRegisteredWorktree(ctx, repoPath, dest); err != nil {
		return err
	}
	if branch != "" {
		result, err := proc.Run(ctx, "git", "-C", repoPath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
		if err != nil && result.ExitCode != 1 {
			return fmt.Errorf("inspect branch %q in owner %q: %w", branch, repoPath, err)
		}
		if err == nil {
			if _, err := proc.Run(ctx, "git", "-C", repoPath, "branch", "-D", branch); err != nil {
				return fmt.Errorf("delete branch %q in owner %q: %w", branch, repoPath, err)
			}
		}
	}
	return nil
}

// RemoveLocalBranchRef retires one exact local branch ref after its preservation
// evidence has been validated by the caller. Missing refs are idempotent.
func RemoveLocalBranchRef(ctx context.Context, repoPath, ref string) error {
	const prefix = "refs/heads/"
	if !strings.HasPrefix(ref, prefix) || strings.TrimPrefix(ref, prefix) == "" {
		return fmt.Errorf("refuse to remove non-local-branch ref %q", ref)
	}
	branch := strings.TrimPrefix(ref, prefix)
	result, err := proc.Run(ctx, "git", "-C", repoPath, "show-ref", "--verify", "--quiet", ref)
	if err != nil && result.ExitCode != 1 {
		return fmt.Errorf("inspect branch %q: %w", branch, err)
	}
	if err != nil {
		return nil
	}
	if _, err := proc.Run(ctx, "git", "-C", repoPath, "branch", "-D", branch); err != nil {
		return fmt.Errorf("delete branch %q: %w", branch, err)
	}
	return nil
}

// WorktreeQuarantine identifies a worktree that has been atomically moved out
// of its live path while its Git registration is still intact.
type WorktreeQuarantine struct {
	OwnerPath    string
	OriginalPath string
	RecoveryPath string
	Branch       string
	RetainBranch bool
}

// WorktreeQuarantineFor returns the deterministic recovery location for an
// operation without changing the filesystem.
func WorktreeQuarantineFor(repoPath, dest, branch, operationID string) WorktreeQuarantine {
	var safeID strings.Builder
	for _, r := range operationID {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_", r) {
			safeID.WriteRune(r)
		} else {
			safeID.WriteByte('-')
		}
	}
	if safeID.Len() == 0 {
		safeID.WriteString("operation")
	}
	return WorktreeQuarantine{
		OwnerPath: repoPath, OriginalPath: dest,
		RecoveryPath: filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+".shunt-removing-"+safeID.String()),
		Branch:       branch,
	}
}

// QuarantineWorktree atomically moves an exactly registered worktree to its
// recovery path. Git registration and branch retirement happen separately,
// after the caller has validated the quarantined bytes.
func QuarantineWorktree(ctx context.Context, repoPath, dest, branch, operationID string) (WorktreeQuarantine, error) {
	quarantine := WorktreeQuarantineFor(repoPath, dest, branch, operationID)
	if err := ctx.Err(); err != nil {
		return quarantine, err
	}
	registered, err := worktreeRegistered(ctx, repoPath, dest)
	if err != nil {
		return quarantine, err
	}
	if !registered {
		return quarantine, fmt.Errorf("refuse to quarantine unregistered worktree path %q", dest)
	}
	if _, err := os.Lstat(quarantine.RecoveryPath); err == nil {
		return quarantine, fmt.Errorf("recovery path %q already exists", quarantine.RecoveryPath)
	} else if !os.IsNotExist(err) {
		return quarantine, fmt.Errorf("inspect recovery path %q: %w", quarantine.RecoveryPath, err)
	}
	if err := os.Rename(dest, quarantine.RecoveryPath); err != nil {
		return quarantine, fmt.Errorf("quarantine worktree %q at recovery path %q: %w", dest, quarantine.RecoveryPath, err)
	}
	return quarantine, nil
}

// RestoreQuarantinedWorktree puts quarantined bytes back at their original
// path. It never overwrites a path recreated after quarantine.
func RestoreQuarantinedWorktree(quarantine WorktreeQuarantine) error {
	if _, err := os.Lstat(quarantine.OriginalPath); err == nil {
		return fmt.Errorf("original path %q was recreated; recovery remains at %q", quarantine.OriginalPath, quarantine.RecoveryPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect original path %q; recovery remains at %q: %w", quarantine.OriginalPath, quarantine.RecoveryPath, err)
	}
	if err := os.Rename(quarantine.RecoveryPath, quarantine.OriginalPath); err != nil {
		return fmt.Errorf("restore worktree %q from recovery path %q: %w", quarantine.OriginalPath, quarantine.RecoveryPath, err)
	}
	if _, err := proc.Run(context.Background(), "git", "-C", quarantine.OwnerPath, "worktree", "repair", quarantine.OriginalPath); err != nil {
		return fmt.Errorf("worktree bytes were restored to %q, but its exact Git registration could not be repaired: %w", quarantine.OriginalPath, err)
	}
	registered, err := worktreeRegistered(context.Background(), quarantine.OwnerPath, quarantine.OriginalPath)
	if err != nil {
		return fmt.Errorf("verify restored worktree registration at %q: %w", quarantine.OriginalPath, err)
	}
	if !registered {
		return fmt.Errorf("worktree bytes were restored to %q, but it is not exactly registered there", quarantine.OriginalPath)
	}
	return nil
}

// RetireQuarantinedWorktree repairs the exact registration to the recovery
// path, keeps that Git metadata usable while deleting the validated bytes, then
// retires only that registration and branch after deletion succeeds.
func RetireQuarantinedWorktree(ctx context.Context, quarantine WorktreeQuarantine) error {
	return retireQuarantinedWorktree(ctx, quarantine, os.RemoveAll)
}

func retireQuarantinedWorktree(ctx context.Context, quarantine WorktreeQuarantine, remove func(string) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(quarantine.OriginalPath); err == nil {
		return fmt.Errorf("original path %q was recreated; recovery remains at %q", quarantine.OriginalPath, quarantine.RecoveryPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect original path %q; recovery remains at %q: %w", quarantine.OriginalPath, quarantine.RecoveryPath, err)
	}
	if _, err := os.Lstat(quarantine.RecoveryPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("recovery path %q is missing", quarantine.RecoveryPath)
		}
		return fmt.Errorf("inspect recovery path %q: %w", quarantine.RecoveryPath, err)
	}
	adminPath, metadataRoot, err := repairQuarantinedWorktreeRegistration(ctx, quarantine)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("retire quarantined worktree; recovery remains at %q: %w", quarantine.RecoveryPath, err)
	}
	if err := remove(quarantine.RecoveryPath); err != nil {
		return fmt.Errorf("remove validated quarantine; recovery remains at %q with its Git registration intact: %w", quarantine.RecoveryPath, err)
	}
	if err := os.RemoveAll(adminPath); err != nil {
		return fmt.Errorf("remove exact worktree registration %q after deleting its bytes: %w", adminPath, err)
	}
	if _, err := os.Lstat(adminPath); !os.IsNotExist(err) {
		if err == nil {
			return fmt.Errorf("exact worktree registration %q remains after removal", adminPath)
		}
		return fmt.Errorf("verify exact worktree registration removal %q: %w", adminPath, err)
	}
	if err := syncDirectory(metadataRoot); err != nil {
		return fmt.Errorf("durably retire exact worktree registration %q: %w", adminPath, err)
	}
	if quarantine.Branch != "" && !quarantine.RetainBranch {
		result, err := proc.Run(ctx, "git", "-C", quarantine.OwnerPath, "show-ref", "--verify", "--quiet", "refs/heads/"+quarantine.Branch)
		if err != nil && result.ExitCode != 1 {
			return fmt.Errorf("inspect branch %q after deleting its worktree: %w", quarantine.Branch, err)
		}
		if err == nil {
			if _, err := proc.Run(ctx, "git", "-C", quarantine.OwnerPath, "branch", "-D", quarantine.Branch); err != nil {
				return fmt.Errorf("delete branch %q after deleting its worktree: %w", quarantine.Branch, err)
			}
		}
	}
	return nil
}

// RetireRemovalTargetRefs verifies every durable recovery archive and deletes
// all existing target refs at their exact witnessed OIDs in one transaction.
func RetireRemovalTargetRefs(ctx context.Context, repoPath string, targets []state.RemovalTarget, recoveryRefs []string) error {
	expected := map[string]int{}
	for _, target := range targets {
		if target.ExpectedOID != "" {
			expected[target.ExpectedOID]++
		}
	}
	lines := []string{"start"}
	seen := map[string]int{}
	for _, ref := range recoveryRefs {
		if !strings.HasPrefix(ref, "refs/shunt/recovery/") {
			return fmt.Errorf("invalid recovery ref %q", ref)
		}
		result, err := proc.Run(ctx, "git", "-C", repoPath, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return fmt.Errorf("resolve recovery ref %q: %w", ref, err)
		}
		oid := strings.TrimSpace(result.Stdout)
		seen[oid]++
		lines = append(lines, "verify "+ref+" "+oid)
	}
	if !maps.Equal(expected, seen) {
		return fmt.Errorf("recovery refs do not retain the exact target OIDs")
	}
	for _, target := range targets {
		if target.ExpectedOID == "" {
			lines = append(lines, "verify "+target.Ref+" 0000000000000000000000000000000000000000")
			continue
		}
		lines = append(lines, "delete "+target.Ref+" "+target.ExpectedOID)
	}
	lines = append(lines, "prepare", "commit")
	tmp, err := os.CreateTemp("", "shunt-retire-refs-*.stdin")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := proc.RunStdin(ctx, path, "git", "-C", repoPath, "update-ref", "--stdin"); err != nil {
		return fmt.Errorf("retire removal target refs: %w", err)
	}
	return nil
}

// repairQuarantinedWorktreeRegistration updates only the named linked
// worktree's reverse pointer after its atomic filesystem rename. It returns the
// exact administrative entry to retire later, after validating that entry is a
// direct child of the owner's worktree metadata directory.
func repairQuarantinedWorktreeRegistration(ctx context.Context, quarantine WorktreeQuarantine) (adminPath, metadataRoot string, err error) {
	if _, err := proc.Run(ctx, "git", "-C", quarantine.OwnerPath, "worktree", "repair", quarantine.RecoveryPath); err != nil {
		return "", "", fmt.Errorf("repair quarantined worktree registration at %q; recovery remains there: %w", quarantine.RecoveryPath, err)
	}
	registered, err := worktreeRegistered(ctx, quarantine.OwnerPath, quarantine.RecoveryPath)
	if err != nil {
		return "", "", fmt.Errorf("verify repaired worktree registration at %q: %w", quarantine.RecoveryPath, err)
	}
	if !registered {
		return "", "", fmt.Errorf("repaired worktree %q is not exactly registered", quarantine.RecoveryPath)
	}
	originalRegistered, err := worktreeRegistered(ctx, quarantine.OwnerPath, quarantine.OriginalPath)
	if err != nil {
		return "", "", fmt.Errorf("verify original worktree registration %q after repair: %w", quarantine.OriginalPath, err)
	}
	if originalRegistered {
		return "", "", fmt.Errorf("original worktree %q remains registered after repair; recovery remains at %q", quarantine.OriginalPath, quarantine.RecoveryPath)
	}
	if quarantine.Branch != "" {
		result, err := proc.Run(ctx, "git", "-C", quarantine.RecoveryPath, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil {
			return "", "", fmt.Errorf("inspect repaired worktree branch at %q: %w", quarantine.RecoveryPath, err)
		}
		if branch := strings.TrimSpace(result.Stdout); branch != quarantine.Branch {
			return "", "", fmt.Errorf("repaired worktree %q is on branch %q, expected %q", quarantine.RecoveryPath, branch, quarantine.Branch)
		}
	}
	adminPath, metadataRoot, err = validatedLinkedWorktreeAdminPath(ctx, quarantine.OwnerPath, quarantine.RecoveryPath)
	if err != nil {
		return "", "", fmt.Errorf("validate repaired worktree registration at %q: %w", quarantine.RecoveryPath, err)
	}
	return adminPath, metadataRoot, nil
}

func validatedLinkedWorktreeAdminPath(ctx context.Context, ownerPath, worktreePath string) (string, string, error) {
	dotGitPath := filepath.Join(worktreePath, ".git")
	dotGitInfo, err := os.Lstat(dotGitPath)
	if err != nil {
		return "", "", fmt.Errorf("inspect linked-worktree pointer: %w", err)
	}
	if !dotGitInfo.Mode().IsRegular() || dotGitInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("linked-worktree pointer %q is not a regular file", dotGitPath)
	}
	dotGit, err := os.ReadFile(dotGitPath)
	if err != nil {
		return "", "", fmt.Errorf("read linked-worktree pointer: %w", err)
	}
	const prefix = "gitdir: "
	pointer := strings.TrimSpace(string(dotGit))
	if !strings.HasPrefix(pointer, prefix) || strings.TrimSpace(strings.TrimPrefix(pointer, prefix)) == "" {
		return "", "", fmt.Errorf("linked-worktree pointer %q is malformed", dotGitPath)
	}
	adminPath := strings.TrimSpace(strings.TrimPrefix(pointer, prefix))
	if !filepath.IsAbs(adminPath) {
		adminPath = filepath.Join(worktreePath, adminPath)
	}
	adminPath, err = filepath.Abs(adminPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve linked-worktree administration path absolutely: %w", err)
	}
	adminInfo, err := os.Lstat(adminPath)
	if err != nil {
		return "", "", fmt.Errorf("inspect linked-worktree administration path: %w", err)
	}
	if !adminInfo.IsDir() || adminInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("linked-worktree administration path %q is not a real directory", adminPath)
	}
	adminPath, err = filepath.EvalSymlinks(adminPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve linked-worktree administration path: %w", err)
	}

	result, err := proc.Run(ctx, "git", "-C", ownerPath, "rev-parse", "--git-path", "worktrees")
	if err != nil {
		return "", "", fmt.Errorf("resolve owner worktree metadata directory: %w", err)
	}
	metadataRoot := strings.TrimSpace(result.Stdout)
	if !filepath.IsAbs(metadataRoot) {
		metadataRoot = filepath.Join(ownerPath, metadataRoot)
	}
	metadataRoot, err = filepath.EvalSymlinks(metadataRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve owner worktree metadata directory: %w", err)
	}
	relative, err := filepath.Rel(metadataRoot, adminPath)
	if err != nil || relative == "." || filepath.IsAbs(relative) || filepath.Dir(relative) != "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("linked-worktree administration path %q is not an exact child of %q", adminPath, metadataRoot)
	}

	reversePath := filepath.Join(adminPath, "gitdir")
	reverseInfo, err := os.Lstat(reversePath)
	if err != nil {
		return "", "", fmt.Errorf("inspect linked-worktree reverse pointer: %w", err)
	}
	if !reverseInfo.Mode().IsRegular() || reverseInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("linked-worktree reverse pointer %q is not a regular file", reversePath)
	}
	reverse, err := os.ReadFile(reversePath)
	if err != nil {
		return "", "", fmt.Errorf("read linked-worktree reverse pointer: %w", err)
	}
	reverseTarget := strings.TrimSpace(string(reverse))
	if !filepath.IsAbs(reverseTarget) {
		reverseTarget = filepath.Join(adminPath, reverseTarget)
	}
	reverseTarget, err = filepath.EvalSymlinks(reverseTarget)
	if err != nil {
		return "", "", fmt.Errorf("resolve linked-worktree reverse pointer: %w", err)
	}
	wantDotGit, err := filepath.EvalSymlinks(dotGitPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve linked-worktree pointer path: %w", err)
	}
	if filepath.Clean(reverseTarget) != filepath.Clean(wantDotGit) {
		return "", "", fmt.Errorf("linked-worktree reverse pointer targets %q, expected %q", reverseTarget, wantDotGit)
	}
	return adminPath, metadataRoot, nil
}

func clearRegisteredWorktree(ctx context.Context, repoPath, dest string) error {
	return removeExactRegisteredWorktree(ctx, repoPath, dest)
}

// removeExactRegisteredWorktree retires only dest's Git registration. Git's
// path-targeted remove also handles an exactly registered worktree whose bytes
// are already absent, so repository-wide pruning is neither needed nor safe:
// it could retire an unrelated siding's recoverable registration.
func removeExactRegisteredWorktree(ctx context.Context, repoPath, dest string) error {
	registered, err := worktreeRegistered(ctx, repoPath, dest)
	if err != nil {
		return err
	}
	if registered {
		if _, err := proc.Run(ctx, "git", "-C", repoPath, "worktree", "remove", "--force", dest); err != nil {
			return fmt.Errorf("remove exact worktree %q from owner %q: %w", dest, repoPath, err)
		}
	} else if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("refuse to remove unregistered worktree path %q from repository %q", dest, repoPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect worktree destination %q: %w", dest, err)
	}
	stillRegistered, err := worktreeRegistered(ctx, repoPath, dest)
	if err != nil {
		return fmt.Errorf("verify exact worktree registration removal for %q: %w", dest, err)
	}
	if stillRegistered {
		return fmt.Errorf("exact worktree registration for %q remains after removal", dest)
	}
	return nil
}

func worktreeRegistered(ctx context.Context, repoPath, dest string) (bool, error) {
	result, err := proc.Run(ctx, "git", "-C", repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("list worktrees for owner %q: %w", repoPath, err)
	}
	want, err := canonicalWorktreePath(dest)
	if err != nil {
		return false, fmt.Errorf("resolve worktree destination %q: %w", dest, err)
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		got, err := canonicalWorktreePath(strings.TrimPrefix(line, "worktree "))
		if err == nil && filepath.Clean(got) == filepath.Clean(want) {
			return true, nil
		}
	}
	return false, nil
}

func canonicalWorktreePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	ancestor := abs
	var suffix []string
	for {
		if resolved, resolveErr := filepath.EvalSymlinks(ancestor); resolveErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		} else if !os.IsNotExist(resolveErr) {
			return "", resolveErr
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return abs, nil
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = parent
	}
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
