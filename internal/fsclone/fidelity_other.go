//go:build !darwin

package fsclone

import (
	"context"
	"fmt"

	"github.com/gordonbeeming/shunt/internal/proc"
)

func cloneVolumeTree(ctx context.Context, src, dest string) error {
	// GNU archive mode preserves links and supported metadata; reflink=auto keeps
	// copy-on-write semantics when the filesystem exposes them.
	if _, err := proc.Run(ctx, "cp", "-a", "--reflink=auto", src, dest); err != nil {
		return fmt.Errorf("cp -a --reflink=auto: %w", err)
	}
	return nil
}
