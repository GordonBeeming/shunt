// Package hostdocker contains the remaining optional host-Docker data-volume
// import helpers. Image caching is daemon-free; see internal/imagecache.
package hostdocker

import (
	"context"
	"fmt"
	"github.com/gordonbeeming/shunt/internal/proc"
)

// HasVolume reports whether the host Docker has a named volume.
func HasVolume(ctx context.Context, vol string) bool {
	_, err := proc.Run(ctx, "docker", "volume", "inspect", vol)
	return err == nil
}

// ExtractVolumeToDir copies a host Docker named volume's contents into a host
// directory (an APFS baseline shunt later cp -c clones per siding). A throwaway
// alpine container does the copy because named-volume data isn't reachable from
// the host filesystem directly; `cp -a` preserves numeric ownership (e.g. mssql's
// uid) and timestamps so the data lands intact.
func ExtractVolumeToDir(ctx context.Context, vol, dir string) error {
	_, err := proc.Run(ctx, "docker", "run", "--rm",
		"-v", vol+":/from:ro", "-v", dir+":/to",
		"alpine", "cp", "-a", "/from/.", "/to")
	if err != nil {
		return fmt.Errorf("extract volume %s: %w", vol, err)
	}
	return nil
}
