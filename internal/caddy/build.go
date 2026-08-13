package caddy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/proc"
)

// l4Module is the layer4 plugin that gives Caddy raw TCP proxying (for DB/TCP
// front-door routes alongside HTTP).
const (
	caddyVersion              = "v2.11.4"
	forwardAuthFixedInVersion = "v2.11.5"
	// forwardAuthSupported must remain false while the pinned Caddy version is
	// below the upstream fix. Shunt only generates reverse_proxy and layer4
	// proxy handlers; this policy blocks forward_auth from becoming supported
	// accidentally without reviewing a pin update.
	forwardAuthSupported = false
	l4Module             = "github.com/mholt/caddy-l4"
	l4Version            = "v0.1.2"
	xcaddyVersion        = "v0.4.6"
)

const buildManifestVersion = 1

type buildManifest struct {
	Version       int      `json:"version"`
	CaddyVersion  string   `json:"caddyVersion"`
	Module        string   `json:"module"`
	ModuleVersion string   `json:"moduleVersion"`
	XCaddyVersion string   `json:"xcaddyVersion"`
	BuildRecipe   []string `json:"buildRecipe"`
	BinarySHA256  string   `json:"binarySHA256"`
}

type buildOperations struct {
	findXCaddy    func() (string, error)
	xcaddyVersion func(context.Context, string) (string, error)
	build         func(context.Context, string, ...string) error
}

var systemBuildOperations = buildOperations{
	findXCaddy: func() (string, error) {
		return exec.LookPath("xcaddy")
	},
	xcaddyVersion: func(ctx context.Context, xcaddyPath string) (string, error) {
		result, err := proc.Run(ctx, xcaddyPath, "version")
		if err != nil {
			return "", err
		}
		return result.Stdout, nil
	},
	build: proc.RunPassthrough,
}

func xcaddyBuildArgs(binPath string) []string {
	return []string{"build", caddyVersion, "--output", binPath, "--with", l4Module + "@" + l4Version}
}

func expectedBuildManifest(binPath string) buildManifest {
	args := xcaddyBuildArgs(binPath)
	return buildManifest{
		Version:       buildManifestVersion,
		CaddyVersion:  caddyVersion,
		Module:        l4Module,
		ModuleVersion: l4Version,
		XCaddyVersion: xcaddyVersion,
		BuildRecipe:   append([]string{"xcaddy"}, args...),
	}
}

func buildManifestPath(binPath string) string {
	return binPath + ".manifest.json"
}

// Build produces this channel's Caddy binary (Caddy + caddy-l4) via xcaddy,
// writing it to the channel's global dir. It's a no-op if the binary and its
// build-identity manifest match the current recipe, unless force is set.
func Build(ctx context.Context, force bool) (string, error) {
	binPath, err := config.CaddyBinaryPath()
	if err != nil {
		return "", err
	}
	return build(ctx, force, binPath, systemBuildOperations)
}

func build(ctx context.Context, force bool, binPath string, operations buildOperations) (string, error) {
	manifestPath := buildManifestPath(binPath)
	expected := expectedBuildManifest(binPath)
	if !force {
		if buildCacheMatches(binPath, manifestPath, expected) {
			return binPath, nil
		}
	}

	xcaddyPath, err := operations.findXCaddy()
	if err != nil {
		return "", fmt.Errorf("xcaddy not found on PATH; install it with " +
			"`go install github.com/caddyserver/xcaddy/cmd/xcaddy@" + xcaddyVersion + "`")
	}
	versionOutput, err := operations.xcaddyVersion(ctx, xcaddyPath)
	if err != nil {
		return "", fmt.Errorf("check xcaddy version: %w", err)
	}
	if !matchesXCaddyVersion(versionOutput) {
		return "", fmt.Errorf("xcaddy version mismatch: got %q, want %s; install it with "+
			"`go install github.com/caddyserver/xcaddy/cmd/xcaddy@%s`",
			strings.TrimSpace(versionOutput), xcaddyVersion, xcaddyVersion)
	}
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		return "", fmt.Errorf("create caddy dir: %w", err)
	}
	// Invalidate a previously matching manifest before xcaddy can replace or
	// truncate its binary. A failed forced rebuild must never leave that binary
	// looking reusable on the next non-forced call.
	if err := os.Remove(manifestPath); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("invalidate caddy build manifest: %w", err)
	}
	if err := operations.build(ctx, xcaddyPath, xcaddyBuildArgs(binPath)...); err != nil {
		return "", fmt.Errorf("xcaddy build: %w", err)
	}
	digest, err := fileSHA256(binPath)
	if err != nil {
		return "", fmt.Errorf("digest caddy binary: %w", err)
	}
	expected.BinarySHA256 = digest
	if err := writeBuildManifestAtomic(manifestPath, expected); err != nil {
		return "", fmt.Errorf("write caddy build manifest: %w", err)
	}
	return binPath, nil
}

func buildCacheMatches(binPath, manifestPath string, expected buildManifest) bool {
	info, err := os.Stat(binPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return false
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var actual buildManifest
	if err := decoder.Decode(&actual); err != nil {
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return false
	}
	digestBytes, err := hex.DecodeString(actual.BinarySHA256)
	if err != nil || len(digestBytes) != sha256.Size {
		return false
	}
	actualDigest := actual.BinarySHA256
	actual.BinarySHA256 = ""
	if !reflect.DeepEqual(actual, expected) {
		return false
	}
	digest, err := fileSHA256(binPath)
	return err == nil && digest == actualDigest
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func matchesXCaddyVersion(output string) bool {
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) == 1 {
		return fields[0] == xcaddyVersion
	}
	if len(fields) != 2 || fields[0] != xcaddyVersion || !strings.HasPrefix(fields[1], "h1:") {
		return false
	}
	digest, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(fields[1], "h1:"))
	return err == nil && len(digest) == 32
}

func writeBuildManifestAtomic(path string, manifest buildManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".caddy-manifest-*.json")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
