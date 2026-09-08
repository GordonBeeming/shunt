// Package image builds shunt's base guest image (glibc + .NET SDK + Docker +
// socat) from assets embedded in the binary, so `shunt init` is self-contained.
package image

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/dockerdpolicy"
	"github.com/gordonbeeming/shunt/internal/hostreach"
	"github.com/gordonbeeming/shunt/internal/proc"
)

//go:embed assets/Containerfile assets/shunt-entrypoint.sh assets/docker-api-admission.go assets/docker-build-admission.go assets/docker-api-admission.mod assets/docker-api-admission.sum assets/hostreach/relay.go
var assets embed.FS

// assetFiles are written verbatim into the temp build context.
var assetFiles = []string{
	"Containerfile",
	"shunt-entrypoint.sh",
	"docker-api-admission.go",
	"docker-build-admission.go",
	"hostreach/relay.go",
	"docker-api-admission.mod",
	"docker-api-admission.sum",
}

const contentVersionMarker = "/usr/local/share/shunt-base-content-version"

// ContentVersion is derived from every embedded base-image asset. Any asset
// change therefore selects a new image tag and guest capability marker.
func ContentVersion() string {
	hash := sha256.New()
	names := append([]string(nil), assetFiles...)
	sort.Strings(names)
	for _, name := range names {
		data, err := assets.ReadFile("assets/" + name)
		if err != nil {
			panic(fmt.Sprintf("read embedded base-image asset %s: %v", name, err))
		}
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data)
		_, _ = hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

// Tag returns the channel-specific, content-versioned base-image tag.
func Tag() string {
	base := config.BaseImageTag()
	if colon := strings.LastIndex(base, ":"); colon > strings.LastIndex(base, "/") {
		base = base[:colon]
	}
	digest := strings.TrimPrefix(ContentVersion(), "sha256:")
	return base + ":content-" + digest[:16]
}

// GuestCapabilityCheck returns argv for a fixed sh predicate with all dynamic
// values passed as positional arguments. Success proves that the guest root
// filesystem contains this binary's exact base-image assets and the required
// offline daemon/admission commands.
func GuestCapabilityCheck() []string {
	return []string{
		"sh", "-c",
		// Every program the image is supposed to provide is named here. The version
		// marker alone is not enough: it proves the assets that built the image,
		// not that the build actually installed what they describe, which is how a
		// binary went missing from a rebuilt image with the marker still correct.
		`test -x "$1" && test -x "$2" && test -x "$3" && test -r "$4" && test "$(cat "$4")" = "$5"`,
		"shunt-base-capability",
		dockerdpolicy.EnsureCommand,
		"/usr/local/bin/shunt-docker-api-admission",
		hostreach.RelayProgram,
		contentVersionMarker,
		ContentVersion(),
	}
}

// EnsureBuilt builds the channel's base image if it's missing (or force=true).
func EnsureBuilt(ctx context.Context, force bool) error {
	tag := Tag()
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
		target := filepath.Join(dir, name)
		// An asset may sit in a subdirectory (a second program needs its own
		// package), so create the parent rather than assuming a flat context.
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create build context dir for %s: %w", name, err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return fmt.Errorf("write %s to build context: %w", name, err)
		}
	}

	if err := proc.RunPassthrough(ctx, "container", "build",
		"--build-arg", "SHUNT_BASE_CONTENT_VERSION="+ContentVersion(),
		"-t", tag, "-f", filepath.Join(dir, "Containerfile"), dir); err != nil {
		return fmt.Errorf("build base image %s: %w", tag, err)
	}
	return nil
}

// Exists reports whether an image with the given tag is present.
func Exists(ctx context.Context, tag string) bool {
	_, err := proc.Run(ctx, "container", "image", "inspect", tag)
	return err == nil
}
