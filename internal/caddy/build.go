package caddy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

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
	caddyModule          = "github.com/caddyserver/caddy/v2"
	buildOutputToken     = "<temporary-output>"
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
	build         func(context.Context, string, []string, ...string) error
	verifyBinary  func(string) error
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
	build: func(ctx context.Context, xcaddyPath string, environment []string, args ...string) error {
		cmd := exec.CommandContext(ctx, xcaddyPath, args...)
		cmd.Env = environment
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("run %s: %w", xcaddyPath, err)
		}
		return nil
	},
	verifyBinary: verifyBuiltCaddy,
}

type localBuildLock struct {
	semaphore chan struct{}
}

type heldBuildLock struct {
	file  *os.File
	local *localBuildLock
}

var localBuildLocks sync.Map

func xcaddyBuildArgs(binPath string) []string {
	return []string{"build", caddyVersion, "--output", binPath, "--with", l4Module + "@" + l4Version}
}

func expectedBuildManifest() buildManifest {
	args := xcaddyBuildArgs(buildOutputToken)
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

func buildLockPath(binPath string) string {
	return binPath + ".lock"
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

func build(ctx context.Context, force bool, binPath string, operations buildOperations) (result string, err error) {
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		return "", fmt.Errorf("create caddy dir: %w", err)
	}
	lock, err := acquireBuildLock(ctx, buildLockPath(binPath))
	if err != nil {
		return "", fmt.Errorf("acquire caddy build lock: %w", err)
	}
	defer func() {
		if releaseErr := lock.release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release caddy build lock: %w", releaseErr))
		}
	}()
	return buildLocked(ctx, force, binPath, operations)
}

func buildLocked(ctx context.Context, force bool, binPath string, operations buildOperations) (string, error) {
	manifestPath := buildManifestPath(binPath)
	expected := expectedBuildManifest()
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
	tempPath, err := newBuildOutputPath(filepath.Dir(binPath))
	if err != nil {
		return "", fmt.Errorf("reserve temporary caddy output: %w", err)
	}
	defer os.Remove(tempPath)
	if operations.build == nil {
		return "", errors.New("xcaddy build callback is required")
	}
	if err := operations.build(ctx, xcaddyPath, controlledBuildEnvironment(os.Environ()), xcaddyBuildArgs(tempPath)...); err != nil {
		return "", fmt.Errorf("xcaddy build: %w", err)
	}
	if err := validateBuiltExecutable(tempPath); err != nil {
		return "", fmt.Errorf("validate xcaddy output: %w", err)
	}
	if operations.verifyBinary == nil {
		return "", errors.New("Caddy build-info verifier is required")
	}
	if err := operations.verifyBinary(tempPath); err != nil {
		return "", fmt.Errorf("verify xcaddy output: %w", err)
	}
	digest, err := fileSHA256(tempPath)
	if err != nil {
		return "", fmt.Errorf("digest caddy binary: %w", err)
	}
	expected.BinarySHA256 = digest
	if err := os.Rename(tempPath, binPath); err != nil {
		return "", fmt.Errorf("install caddy binary: %w", err)
	}
	if err := writeBuildManifestAtomic(manifestPath, expected); err != nil {
		return "", fmt.Errorf("write caddy build manifest: %w", err)
	}
	return binPath, nil
}

func controlledBuildEnvironment(environment []string) []string {
	controlled := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if found && strings.HasPrefix(name, "XCADDY_") {
			continue
		}
		controlled = append(controlled, entry)
	}
	return controlled
}

func newBuildOutputPath(directory string) (string, error) {
	temp, err := os.CreateTemp(directory, ".caddy-build-*")
	if err != nil {
		return "", err
	}
	path := temp.Name()
	if err := temp.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func validateBuiltExecutable(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect newly built binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("newly built binary is not a regular file: mode=%s", info.Mode())
	}
	if info.Mode().Perm()&0o100 == 0 {
		return fmt.Errorf("newly built binary is not owner-executable: mode=%#o", info.Mode().Perm())
	}
	return nil
}

func verifyBuiltCaddy(path string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read Go build info: %w", err)
	}
	return verifyCaddyBuildInfo(info)
}

func verifyCaddyBuildInfo(info *debug.BuildInfo) error {
	if info == nil {
		return errors.New("Go build info is missing")
	}
	modules := make([]*debug.Module, 0, len(info.Deps)+1)
	modules = append(modules, &info.Main)
	modules = append(modules, info.Deps...)
	foundCaddy := false
	foundL4 := false
	for _, module := range modules {
		if module == nil {
			continue
		}
		if module.Replace != nil {
			return fmt.Errorf("Go module %s uses a replacement", module.Path)
		}
		switch module.Path {
		case caddyModule:
			if foundCaddy || module.Version != caddyVersion {
				return fmt.Errorf("Caddy module version is %q, want exactly %s", module.Version, caddyVersion)
			}
			foundCaddy = true
		case l4Module:
			if foundL4 || module.Version != l4Version {
				return fmt.Errorf("caddy-l4 module version is %q, want exactly %s", module.Version, l4Version)
			}
			foundL4 = true
		}
	}
	if !foundCaddy {
		return fmt.Errorf("Caddy module %s@%s is missing", caddyModule, caddyVersion)
	}
	if !foundL4 {
		return fmt.Errorf("caddy-l4 module %s@%s is missing", l4Module, l4Version)
	}
	return nil
}

func buildCacheMatches(binPath, manifestPath string, expected buildManifest) bool {
	info, err := os.Stat(binPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
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

func acquireBuildLock(ctx context.Context, path string) (*heldBuildLock, error) {
	value, _ := localBuildLocks.LoadOrStore(filepath.Clean(path), &localBuildLock{semaphore: make(chan struct{}, 1)})
	local := value.(*localBuildLock)
	select {
	case local.semaphore <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		<-local.semaphore
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		<-local.semaphore
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &heldBuildLock{file: file, local: local}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			<-local.semaphore
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			file.Close()
			<-local.semaphore
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (lock *heldBuildLock) release() error {
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	<-lock.local.semaphore
	return errors.Join(unlockErr, closeErr)
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
