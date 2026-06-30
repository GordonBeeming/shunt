// Package hostdocker treats the host's Docker daemon as the canonical image
// cache: pull each dependency image once on the host, then save it for loading
// into sidings — so siding guests never pull from the network themselves.
package hostdocker

import (
	"context"
	"fmt"
	"strings"

	"github.com/gordonbeeming/shunt/internal/proc"
)

// Available reports whether a host Docker daemon is reachable.
func Available(ctx context.Context) bool {
	_, err := proc.Run(ctx, "docker", "info", "--format", "{{.Name}}")
	return err == nil
}

// Has reports whether the host already has an image (no network).
func Has(ctx context.Context, image string) bool {
	_, err := proc.Run(ctx, "docker", "image", "inspect", image)
	return err == nil
}

// Ensure makes sure every image is present on the host, pulling only the ones
// that are missing — the single shared network call, reused by every siding and
// every project. Returns the images it had to pull.
func Ensure(ctx context.Context, images []string) (pulled []string, err error) {
	for _, img := range images {
		if Has(ctx, img) {
			continue
		}
		if _, err := proc.Run(ctx, "docker", "pull", img); err != nil {
			return pulled, fmt.Errorf("pull %s on host: %w", img, err)
		}
		pulled = append(pulled, img)
	}
	return pulled, nil
}

// Missing returns the subset of images not present on the host.
func Missing(ctx context.Context, images []string) []string {
	var miss []string
	for _, img := range images {
		if !Has(ctx, img) {
			miss = append(miss, img)
		}
	}
	return miss
}

// Save writes the given host images to an OCI tar at outPath (no network). The
// host daemon writes the file directly, so no streaming is needed.
func Save(ctx context.Context, images []string, outPath string) error {
	if len(images) == 0 {
		return fmt.Errorf("no images to save")
	}
	args := append([]string{"save", "-o", outPath}, images...)
	if _, err := proc.Run(ctx, "docker", args...); err != nil {
		return fmt.Errorf("docker save [%s]: %w", strings.Join(images, ", "), err)
	}
	return nil
}

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
