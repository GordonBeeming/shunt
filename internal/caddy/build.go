package caddy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
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
	caddyModule          = "github.com/caddyserver/caddy/v2"
	caddyVersionSum      = "h1:XKxkMTgNSizEvKG6QHue6cAsFOteU2qA61w2tKkCWi0="
	l4Module             = "github.com/mholt/caddy-l4"
	l4Version            = "v0.1.2"
	l4VersionSum         = "h1:23rhxVar8F5Sl7sYKDgEReI1yT//+e8J7EtMwO2yJpU="
	xcaddyVersion        = "v0.4.6"
	xcaddyVersionSum     = "h1:/kbArNJZFPewjwlijr83WdssSuhSZ9XT2cDSWmonkjc="
	buildOutputToken     = "<temporary-output>"
	publicGoProxy        = "https://proxy.golang.org,direct"
	publicGoSumDB        = "sum.golang.org"
	buildGOOS            = "darwin"
	buildGOARCH          = "arm64"
	buildGOARM64         = "v8.0"
)

const buildManifestVersion = 3

var deterministicGoEnvironment = []string{
	"GOENV=off",
	"GO111MODULE=on",
	"GOAUTH=off",
	"GODEBUG=",
	"GOFLAGS=",
	"GOINSECURE=",
	"GOVCS=public:git|hg,private:off",
	"GOWORK=off",
	"GOEXPERIMENT=",
	"GOTOOLCHAIN=local",
	"GOTMPDIR=",
	"GOCACHEPROG=",
	"CGO_ENABLED=0",
	"GOOS=" + buildGOOS,
	"GOARCH=" + buildGOARCH,
	"GOARM64=" + buildGOARM64,
	"GOHOSTOS=",
	"GOHOSTARCH=",
	"GO386=",
	"GOAMD64=",
	"GOARM=",
	"GOMIPS=",
	"GOMIPS64=",
	"GOPPC64=",
	"GORISCV64=",
	"GOWASM=",
	"GOFIPS140=off",
	"GO_EXTLINK_ENABLED=",
	"GO_LDSO=",
	"GOTOOLDIR=",
	"GOROOT=",
	"CC=",
	"CXX=",
	"GCCGO=",
	"AR=",
	"FC=",
	"PKG_CONFIG=",
	"CGO_CFLAGS=",
	"CGO_CPPFLAGS=",
	"CGO_CXXFLAGS=",
	"CGO_FFLAGS=",
	"CGO_LDFLAGS=",
	"GOPROXY=" + publicGoProxy,
	"GOSUMDB=" + publicGoSumDB,
	"GOPRIVATE=",
	"GONOPROXY=",
	"GONOSUMDB=",
	"CADDY_VERSION=",
}

var deterministicBuildSettings = []string{
	"-buildmode=exe",
	"-compiler=gc",
	"-tags=nobadger,nomysql,nopgx",
	"-trimpath=true",
	"CGO_ENABLED=0",
	"GOARCH=" + buildGOARCH,
	"GOOS=" + buildGOOS,
	"GOARM64=" + buildGOARM64,
}

type goToolchainIdentity struct {
	Version string
}

type buildManifest struct {
	Version       int      `json:"version"`
	CaddyVersion  string   `json:"caddyVersion"`
	CaddySum      string   `json:"caddySum"`
	Module        string   `json:"module"`
	ModuleVersion string   `json:"moduleVersion"`
	ModuleSum     string   `json:"moduleSum"`
	XCaddyVersion string   `json:"xcaddyVersion"`
	XCaddySum     string   `json:"xcaddySum"`
	GoToolchain   string   `json:"goToolchain"`
	GoEnvironment []string `json:"goEnvironment"`
	BuildSettings []string `json:"buildSettings"`
	BuildRecipe   []string `json:"buildRecipe"`
	BinarySHA256  string   `json:"binarySHA256"`
}

type buildOperations struct {
	findGo        func() (string, error)
	goVersion     func(context.Context, string) (string, error)
	findXCaddy    func() (string, error)
	xcaddyVersion func(context.Context, string) (string, error)
	build         func(context.Context, string, []string, ...string) error
	verifyBinary  func(string, goToolchainIdentity) error
}

var systemBuildOperations = buildOperations{
	findGo: func() (string, error) {
		path, err := exec.LookPath("go")
		if err != nil {
			return "", err
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(path)
	},
	goVersion: func(ctx context.Context, goPath string) (string, error) {
		cmd := exec.CommandContext(ctx, goPath, "env", "GOVERSION", "GOOS", "GOARCH")
		cmd.Env = controlledBuildEnvironment(os.Environ(), goPath)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("run %s env: %w: %s", goPath, err, strings.TrimSpace(string(output)))
		}
		return string(output), nil
	},
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

func expectedBuildManifest(toolchain goToolchainIdentity) buildManifest {
	args := xcaddyBuildArgs(buildOutputToken)
	return buildManifest{
		Version:       buildManifestVersion,
		CaddyVersion:  caddyVersion,
		CaddySum:      caddyVersionSum,
		Module:        l4Module,
		ModuleVersion: l4Version,
		ModuleSum:     l4VersionSum,
		XCaddyVersion: xcaddyVersion,
		XCaddySum:     xcaddyVersionSum,
		GoToolchain:   toolchain.Version,
		GoEnvironment: append([]string(nil), deterministicGoEnvironment...),
		BuildSettings: append([]string(nil), deterministicBuildSettings...),
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
	if err := validateBuildPlatform(runtime.GOOS, runtime.GOARCH); err != nil {
		return "", err
	}
	binPath, err := config.CaddyBinaryPath()
	if err != nil {
		return "", err
	}
	return build(ctx, force, binPath, systemBuildOperations)
}

func validateBuildPlatform(goos, goarch string) error {
	if goos != buildGOOS || goarch != buildGOARCH {
		return fmt.Errorf("Caddy builds are supported only on %s/%s; this host is %s/%s", buildGOOS, buildGOARCH, goos, goarch)
	}
	return nil
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
	goPath, toolchain, err := resolveGoToolchain(ctx, operations)
	if err != nil {
		return "", err
	}
	manifestPath := buildManifestPath(binPath)
	expected := expectedBuildManifest(toolchain)
	if !force {
		if buildCacheMatches(binPath, manifestPath, expected) {
			return binPath, nil
		}
	}

	if operations.findXCaddy == nil {
		return "", errors.New("xcaddy resolver is required")
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
		return "", fmt.Errorf("xcaddy version mismatch: got %q, want %s %s; install it with "+
			"`go install github.com/caddyserver/xcaddy/cmd/xcaddy@%s`",
			strings.TrimSpace(versionOutput), xcaddyVersion, xcaddyVersionSum, xcaddyVersion)
	}
	tempPath, err := newBuildOutputPath(filepath.Dir(binPath))
	if err != nil {
		return "", fmt.Errorf("reserve temporary caddy output: %w", err)
	}
	defer os.Remove(tempPath)
	if operations.build == nil {
		return "", errors.New("xcaddy build callback is required")
	}
	if err := operations.build(ctx, xcaddyPath, controlledBuildEnvironment(os.Environ(), goPath), xcaddyBuildArgs(tempPath)...); err != nil {
		return "", fmt.Errorf("xcaddy build: %w", err)
	}
	if err := validateBuiltExecutable(tempPath); err != nil {
		return "", fmt.Errorf("validate xcaddy output: %w", err)
	}
	if operations.verifyBinary == nil {
		return "", errors.New("Caddy build-info verifier is required")
	}
	if err := operations.verifyBinary(tempPath, toolchain); err != nil {
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

func controlledBuildEnvironment(environment []string, goPath string) []string {
	controlled := stripControlledBuildEnvironment(environment)
	controlled = append(controlled, deterministicGoEnvironment...)
	return append(controlled, "XCADDY_WHICH_GO="+goPath)
}

func stripControlledBuildEnvironment(environment []string) []string {
	controlled := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if found && (strings.HasPrefix(name, "XCADDY_") || controlledGoVariable(name)) {
			continue
		}
		controlled = append(controlled, entry)
	}
	return controlled
}

func controlledGoVariable(name string) bool {
	for _, setting := range deterministicGoEnvironment {
		settingName, _, _ := strings.Cut(setting, "=")
		if name == settingName {
			return true
		}
	}
	return false
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

func verifyBuiltCaddy(path string, toolchain goToolchainIdentity) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read Go build info: %w", err)
	}
	return verifyCaddyBuildInfo(info, toolchain)
}

func verifyCaddyBuildInfo(info *debug.BuildInfo, toolchain goToolchainIdentity) error {
	if info == nil {
		return errors.New("Go build info is missing")
	}
	if info.GoVersion != toolchain.Version {
		return fmt.Errorf("Go toolchain is %q, want exactly %s", info.GoVersion, toolchain.Version)
	}
	if err := verifyCaddyBuildSettings(info.Settings); err != nil {
		return err
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
			if foundCaddy {
				return fmt.Errorf("Caddy module %s occurs more than once", caddyModule)
			}
			if module.Version != caddyVersion {
				return fmt.Errorf("Caddy module version is %q, want exactly %s", module.Version, caddyVersion)
			}
			if module.Sum != caddyVersionSum {
				return fmt.Errorf("Caddy module sum is %q, want exactly %s", module.Sum, caddyVersionSum)
			}
			foundCaddy = true
		case l4Module:
			if foundL4 {
				return fmt.Errorf("caddy-l4 module %s occurs more than once", l4Module)
			}
			if module.Version != l4Version {
				return fmt.Errorf("caddy-l4 module version is %q, want exactly %s", module.Version, l4Version)
			}
			if module.Sum != l4VersionSum {
				return fmt.Errorf("caddy-l4 module sum is %q, want exactly %s", module.Sum, l4VersionSum)
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

func resolveGoToolchain(ctx context.Context, operations buildOperations) (string, goToolchainIdentity, error) {
	if operations.findGo == nil {
		return "", goToolchainIdentity{}, errors.New("Go executable resolver is required")
	}
	goPath, err := operations.findGo()
	if err != nil {
		return "", goToolchainIdentity{}, fmt.Errorf("Go executable not found on PATH: %w", err)
	}
	if goPath == "" || !filepath.IsAbs(goPath) {
		return "", goToolchainIdentity{}, fmt.Errorf("resolved Go executable path %q is not absolute", goPath)
	}
	if operations.goVersion == nil {
		return "", goToolchainIdentity{}, errors.New("Go version reader is required")
	}
	output, err := operations.goVersion(ctx, goPath)
	if err != nil {
		return "", goToolchainIdentity{}, fmt.Errorf("read Go toolchain identity: %w", err)
	}
	identity, err := parseGoToolchainIdentity(output)
	if err != nil {
		return "", goToolchainIdentity{}, err
	}
	return goPath, identity, nil
}

func parseGoToolchainIdentity(output string) (goToolchainIdentity, error) {
	fields := strings.Fields(output)
	if len(fields) != 3 || !strings.HasPrefix(fields[0], "go1.") || fields[1] != buildGOOS || fields[2] != buildGOARCH {
		return goToolchainIdentity{}, fmt.Errorf("unsupported Go toolchain identity %q; want `go1.x %s %s`", strings.TrimSpace(output), buildGOOS, buildGOARCH)
	}
	return goToolchainIdentity{Version: fields[0]}, nil
}

func verifyCaddyBuildSettings(settings []debug.BuildSetting) error {
	actual := make(map[string]string, len(settings))
	for _, setting := range settings {
		if _, exists := actual[setting.Key]; exists {
			return fmt.Errorf("Go build setting %s occurs more than once", setting.Key)
		}
		actual[setting.Key] = setting.Value
	}
	for _, expected := range deterministicBuildSettings {
		key, value, _ := strings.Cut(expected, "=")
		if actual[key] != value {
			return fmt.Errorf("Go build setting %s is %q, want exactly %q", key, actual[key], value)
		}
	}
	for _, forbidden := range []string{"-race", "-asan", "-msan", "-cover", "-covermode", "-gcflags", "-asmflags", "-toolexec", "-pgo", "GOEXPERIMENT", "GOFIPS140"} {
		if value, exists := actual[forbidden]; exists {
			return fmt.Errorf("unexpected Go build setting %s=%q", forbidden, value)
		}
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
	return strings.TrimSpace(output) == xcaddyVersion+" "+xcaddyVersionSum
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
