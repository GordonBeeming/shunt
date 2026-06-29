// Package fsclone makes a siding's host-resident working copy: a git clone of
// the repo and APFS clones (cp -c) of any baseline data volumes.
package fsclone

import (
	"context"
	"fmt"
	"os"

	"github.com/gordonbeeming/shunt/internal/proc"
)

// CloneRepo clones origin into dest. For a local origin path this is fast
// (git hardlinks objects). branch, if set, is checked out after cloning.
func CloneRepo(ctx context.Context, origin, dest, branch string) error {
	args := []string{"clone", origin, dest}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	if _, err := proc.Run(ctx, "git", args...); err != nil {
		return fmt.Errorf("git clone %s: %w", origin, err)
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
