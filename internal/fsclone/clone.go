// Package fsclone makes a siding's host-resident working copy: a git clone of
// the repo and APFS clones (cp -c) of any baseline data volumes.
package fsclone

import (
	"context"
	"fmt"
	"os"

	"github.com/gordonbeeming/shunt/internal/proc"
)

// CloneRepo makes a working copy of origin at dest. It git-clones (so the siding
// has full history to commit experiments against), then overlays origin's
// working tree so UNCOMMITTED and untracked changes are included — shunt runs the
// code you're currently working on, not just the last commit. Build artifacts and
// node_modules are excluded (rebuilt in the guest).
func CloneRepo(ctx context.Context, origin, dest, branch string) error {
	args := []string{"clone", origin, dest}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	if _, err := proc.Run(ctx, "git", args...); err != nil {
		return fmt.Errorf("git clone %s: %w", origin, err)
	}
	// Overlay the live working tree (uncommitted + untracked) onto the clone.
	if _, err := proc.Run(ctx, "rsync", "-a",
		"--exclude=.git/", "--exclude=bin/", "--exclude=obj/", "--exclude=node_modules/",
		ensureTrailingSlash(origin), ensureTrailingSlash(dest)); err != nil {
		return fmt.Errorf("overlay working tree from %s: %w", origin, err)
	}
	return nil
}

func ensureTrailingSlash(p string) string {
	if len(p) > 0 && p[len(p)-1] == '/' {
		return p
	}
	return p + "/"
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
