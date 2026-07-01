// Package image builds shunt's base guest image (glibc + .NET SDK + Docker +
// socat) from assets embedded in the binary, so `shunt init` is self-contained.
package image

import (
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/proc"
)

//go:embed assets/Containerfile assets/shunt-entrypoint.sh
var assets embed.FS

// assetFiles are written verbatim into the temp build context.
var assetFiles = []string{"Containerfile", "shunt-entrypoint.sh"}

// EnsureBuilt builds the channel's base image if it's missing (or force=true).
func EnsureBuilt(ctx context.Context, force bool) error {
	tag := config.BaseImageTag()
	if !force && Exists(ctx, tag) {
		return nil
	}
	dir, err := os.MkdirTemp("", "shunt-base-*")
	if err != nil {
		return fmt.Errorf("create build context: %w", err)
	}
	defer os.RemoveAll(dir)

	for _, name := range assetFiles {
		data, err := assets.ReadFile("assets/" + name)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return fmt.Errorf("write %s to build context: %w", name, err)
		}
	}

	if err := proc.RunPassthrough(ctx, "container", "build",
		"-t", tag, "-f", filepath.Join(dir, "Containerfile"), dir); err != nil {
		return fmt.Errorf("build base image %s: %w", tag, err)
	}
	return nil
}

// Exists reports whether an image with the given tag is present.
func Exists(ctx context.Context, tag string) bool {
	res, err := proc.Run(ctx, "container", "image", "ls")
	if err != nil {
		return false
	}
	name := strings.SplitN(tag, ":", 2)[0]
	return strings.Contains(res.Stdout, name)
}
