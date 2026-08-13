package caddy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/state"
)

var testGoToolchain = goToolchainIdentity{Version: "go1.26.6"}

func TestXcaddyBuildArgsPinEveryModule(t *testing.T) {
	bin := "/tmp/shunt"
	want := []string{"build", "v2.11.4", "--output", bin, "--with", "github.com/mholt/caddy-l4@v0.1.2"}
	if got := xcaddyBuildArgs(bin); !reflect.DeepEqual(got, want) {
		t.Fatalf("xcaddy build args = %q, want %q", got, want)
	}
	wantManifest := buildManifest{
		Version:       3,
		CaddyVersion:  "v2.11.4",
		CaddySum:      "h1:XKxkMTgNSizEvKG6QHue6cAsFOteU2qA61w2tKkCWi0=",
		Module:        "github.com/mholt/caddy-l4",
		ModuleVersion: "v0.1.2",
		ModuleSum:     "h1:23rhxVar8F5Sl7sYKDgEReI1yT//+e8J7EtMwO2yJpU=",
		XCaddyVersion: "v0.4.6",
		XCaddySum:     "h1:/kbArNJZFPewjwlijr83WdssSuhSZ9XT2cDSWmonkjc=",
		GoToolchain:   testGoToolchain.Version,
		GoEnvironment: append([]string(nil), deterministicGoEnvironment...),
		BuildSettings: append([]string(nil), deterministicBuildSettings...),
		BuildRecipe:   append([]string{"xcaddy"}, xcaddyBuildArgs(buildOutputToken)...),
		BinarySHA256:  "",
	}
	if got := expectedBuildManifest(testGoToolchain); !reflect.DeepEqual(got, wantManifest) {
		t.Fatalf("build manifest = %#v, want %#v", got, wantManifest)
	}
}

func TestBuildReusesBinaryWithMatchingManifest(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("cached"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMatchingBuildManifest(t, binPath); err != nil {
		t.Fatal(err)
	}

	operations := withTestGo(buildOperations{
		findXCaddy: func() (string, error) {
			t.Fatal("matching cache should not resolve xcaddy")
			return "", nil
		},
		xcaddyVersion: func(context.Context, string) (string, error) {
			t.Fatal("matching cache should not check xcaddy version")
			return "", nil
		},
		build: func(context.Context, string, []string, ...string) error {
			t.Fatal("matching cache should not rebuild")
			return nil
		},
	})

	got, err := build(context.Background(), false, binPath, operations)
	if err != nil {
		t.Fatalf("build from matching cache: %v", err)
	}
	if got != binPath {
		t.Fatalf("build path = %q, want %q", got, binPath)
	}
}

func TestBuildRebuildsWhenCachedBinaryDigestDoesNotMatch(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("cached"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMatchingBuildManifest(t, binPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("changed"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := &buildCallCounts{}

	if _, err := build(context.Background(), false, binPath, successfulBuildOperations(t, binPath, calls)); err != nil {
		t.Fatalf("rebuild changed binary: %v", err)
	}
	if calls.build != 1 {
		t.Fatalf("build calls = %d, want 1", calls.build)
	}
	if !buildCacheMatches(binPath, buildManifestPath(binPath), expectedBuildManifest(testGoToolchain)) {
		t.Fatal("changed binary was not replaced with a digest-bound cache entry")
	}
}

func TestBuildRebuildsWhenCachedBinaryIsNotOwnerExecutable(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("cached"), 0o401); err != nil {
		t.Fatal(err)
	}
	if err := writeMatchingBuildManifest(t, binPath); err != nil {
		t.Fatal(err)
	}
	calls := &buildCallCounts{}

	if _, err := build(context.Background(), false, binPath, successfulBuildOperations(t, binPath, calls)); err != nil {
		t.Fatalf("rebuild non-owner-executable binary: %v", err)
	}
	if calls.build != 1 {
		t.Fatalf("build calls = %d, want 1", calls.build)
	}
}

func TestBuildRebuildsWhenManifestIsMissing(t *testing.T) {
	t.Setenv("XCADDY_SKIP_BUILD", "1")
	t.Setenv("XCADDY_GO_BUILD_FLAGS", "-race")
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := &buildCallCounts{}

	got, err := build(context.Background(), false, binPath, successfulBuildOperations(t, binPath, calls))
	if err != nil {
		t.Fatalf("rebuild missing manifest: %v", err)
	}
	if got != binPath {
		t.Fatalf("build path = %q, want %q", got, binPath)
	}
	if calls.findGo != 1 || calls.goVer != 1 || calls.find != 1 || calls.version != 1 || calls.build != 1 || calls.verify != 1 {
		t.Fatalf("operation calls = %+v, want one of each", calls)
	}
	if !buildCacheMatches(binPath, buildManifestPath(binPath), expectedBuildManifest(testGoToolchain)) {
		t.Fatal("rebuilt binary and manifest do not match the current build identity")
	}
}

func TestBuildRebuildsWhenManifestDoesNotMatch(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := manifestForBinary(t, binPath)
	stale.BuildRecipe = append(stale.BuildRecipe, "--unexpected")
	if err := writeBuildManifestAtomic(buildManifestPath(binPath), stale); err != nil {
		t.Fatal(err)
	}
	calls := &buildCallCounts{}

	if _, err := build(context.Background(), false, binPath, successfulBuildOperations(t, binPath, calls)); err != nil {
		t.Fatalf("rebuild mismatched manifest: %v", err)
	}
	if calls.build != 1 {
		t.Fatalf("build calls = %d, want 1", calls.build)
	}
	if !buildCacheMatches(binPath, buildManifestPath(binPath), expectedBuildManifest(testGoToolchain)) {
		t.Fatal("mismatched manifest was not replaced with the current build identity")
	}
}

func TestBuildRebuildsWhenGoToolchainChanges(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("cached"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMatchingBuildManifest(t, binPath); err != nil {
		t.Fatal(err)
	}

	newToolchain := goToolchainIdentity{Version: "go1.26.7"}
	calls := &buildCallCounts{}
	operations := successfulBuildOperations(t, binPath, calls)
	operations.goVersion = func(_ context.Context, path string) (string, error) {
		if path != "/test/toolchains/go" {
			t.Fatalf("go version path = %q, want /test/toolchains/go", path)
		}
		return newToolchain.Version + "\ndarwin\narm64\n", nil
	}
	operations.verifyBinary = func(_ string, toolchain goToolchainIdentity) error {
		if toolchain != newToolchain {
			t.Fatalf("verified toolchain = %#v, want %#v", toolchain, newToolchain)
		}
		return nil
	}

	if _, err := build(context.Background(), false, binPath, operations); err != nil {
		t.Fatalf("rebuild after toolchain change: %v", err)
	}
	if calls.build != 1 {
		t.Fatalf("build calls = %d, want 1", calls.build)
	}
	if !buildCacheMatches(binPath, buildManifestPath(binPath), expectedBuildManifest(newToolchain)) {
		t.Fatal("rebuilt cache does not carry the new Go toolchain identity")
	}
}

func TestBuildRebuildsWhenManifestAttestationSumDoesNotMatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*buildManifest)
	}{
		{name: "Caddy", mutate: func(manifest *buildManifest) { manifest.CaddySum = "h1:wrong" }},
		{name: "caddy-l4", mutate: func(manifest *buildManifest) { manifest.ModuleSum = "h1:wrong" }},
		{name: "xcaddy", mutate: func(manifest *buildManifest) { manifest.XCaddySum = "h1:wrong" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
			if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(binPath, []byte("stale"), 0o755); err != nil {
				t.Fatal(err)
			}
			stale := manifestForBinary(t, binPath)
			test.mutate(&stale)
			if err := writeBuildManifestAtomic(buildManifestPath(binPath), stale); err != nil {
				t.Fatal(err)
			}
			calls := &buildCallCounts{}

			if _, err := build(context.Background(), false, binPath, successfulBuildOperations(t, binPath, calls)); err != nil {
				t.Fatalf("rebuild mismatched %s sum: %v", test.name, err)
			}
			if calls.build != 1 {
				t.Fatalf("build calls = %d, want 1", calls.build)
			}
			if !buildCacheMatches(binPath, buildManifestPath(binPath), expectedBuildManifest(testGoToolchain)) {
				t.Fatal("sum-mismatched manifest was not replaced with the current attested identity")
			}
		})
	}
}

func TestBuildRejectsMismatchedXCaddyAttestation(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
	}{
		{name: "wrong version", output: "v0.4.5 " + xcaddyVersionSum + "\n"},
		{name: "missing sum", output: xcaddyVersion + "\n"},
		{name: "wrong sum", output: xcaddyVersion + " h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
			buildCalled := false
			operations := withTestGo(buildOperations{
				findXCaddy: func() (string, error) { return "/test/xcaddy", nil },
				xcaddyVersion: func(_ context.Context, path string) (string, error) {
					if path != "/test/xcaddy" {
						t.Fatalf("version path = %q, want /test/xcaddy", path)
					}
					return test.output, nil
				},
				build: func(context.Context, string, []string, ...string) error {
					buildCalled = true
					return nil
				},
			})

			_, err := build(context.Background(), false, binPath, operations)
			if err == nil || !strings.Contains(err.Error(), "xcaddy version mismatch") {
				t.Fatalf("build error = %v, want xcaddy version mismatch", err)
			}
			if buildCalled {
				t.Fatal("build ran with mismatched xcaddy attestation")
			}
			if _, statErr := os.Stat(buildManifestPath(binPath)); !os.IsNotExist(statErr) {
				t.Fatalf("manifest exists after xcaddy attestation mismatch: %v", statErr)
			}
		})
	}
}

func TestBuildFailurePreservesMatchingBinaryAndManifest(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("cached"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := buildManifestPath(binPath)
	if err := writeMatchingBuildManifest(t, binPath); err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	operations := withTestGo(buildOperations{
		findXCaddy: func() (string, error) { return "/test/xcaddy", nil },
		xcaddyVersion: func(context.Context, string) (string, error) {
			return xcaddyVersion + " " + xcaddyVersionSum + "\n", nil
		},
		build: func(context.Context, string, []string, ...string) error {
			return errors.New("compile failed")
		},
		verifyBinary: func(string, goToolchainIdentity) error { return nil },
	})

	_, err = build(context.Background(), true, binPath, operations)
	if err == nil || !strings.Contains(err.Error(), "xcaddy build: compile failed") {
		t.Fatalf("build error = %v, want compile failure", err)
	}
	manifestAfter, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		t.Fatalf("read preserved manifest: %v", readErr)
	}
	if !reflect.DeepEqual(manifestAfter, manifestBefore) {
		t.Fatal("failed forced build changed the matching manifest")
	}
	contents, readErr := os.ReadFile(binPath)
	if readErr != nil || string(contents) != "cached" {
		t.Fatalf("failed forced build changed cached binary: contents=%q err=%v", contents, readErr)
	}
	if !buildCacheMatches(binPath, manifestPath, expectedBuildManifest(testGoToolchain)) {
		t.Fatal("failed forced build did not preserve the known-good cache")
	}
}

func TestMatchesXCaddyVersionAcceptsOnlyReleaseOutput(t *testing.T) {
	exact := xcaddyVersion + " " + xcaddyVersionSum
	for _, output := range []string{
		exact,
		exact + "\n",
		" \t" + exact + "\r\n",
	} {
		if !matchesXCaddyVersion(output) {
			t.Errorf("matchesXCaddyVersion(%q) = false, want true", output)
		}
	}
	for _, output := range []string{
		"",
		xcaddyVersion,
		xcaddyVersion + "\n",
		"v0.4.5",
		"xcaddy v0.4.6",
		"v0.4.6-dirty",
		"v0.4.6 h1:not-a-real-sum",
		"v0.4.6 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		exact + " => ./local",
	} {
		if matchesXCaddyVersion(output) {
			t.Errorf("matchesXCaddyVersion(%q) = true, want false", output)
		}
	}
}

func TestResolveGoToolchainRequiresAbsoluteValidatedIdentity(t *testing.T) {
	for _, test := range []struct {
		name       string
		path       string
		version    string
		want       goToolchainIdentity
		errorMatch string
	}{
		{name: "Go 1.25 minimum", path: "/toolchains/go", version: "go1.25.13\ndarwin\narm64\n", want: goToolchainIdentity{Version: "go1.25.13"}},
		{name: "Go 1.26 minimum", path: "/toolchains/go", version: "go1.26.6\ndarwin\narm64\n", want: testGoToolchain},
		{name: "relative path", path: "bin/go", version: "go1.26.6\ndarwin\narm64\n", errorMatch: "not absolute"},
		{name: "Go 1.25 too old", path: "/toolchains/go", version: "go1.25.12\ndarwin\narm64\n", errorMatch: "upgrade to Go 1.25.13+ or Go 1.26.6+"},
		{name: "Go 1.26 too old", path: "/toolchains/go", version: "go1.26.5\ndarwin\narm64\n", errorMatch: "upgrade to Go 1.25.13+ or Go 1.26.6+"},
		{name: "prerelease", path: "/toolchains/go", version: "go1.26.6rc1\ndarwin\narm64\n", errorMatch: "unsupported Go toolchain identity"},
		{name: "future minor", path: "/toolchains/go", version: "go1.27.0\ndarwin\narm64\n", errorMatch: "newer minor and major lines require review"},
		{name: "future major", path: "/toolchains/go", version: "go2.0.0\ndarwin\narm64\n", errorMatch: "newer minor and major lines require review"},
		{name: "noncanonical", path: "/toolchains/go", version: "go1.026.6\ndarwin\narm64\n", errorMatch: "unsupported Go toolchain identity"},
		{name: "wrong target", path: "/toolchains/go", version: "go1.26.6\nlinux\narm64\n", errorMatch: "unsupported Go toolchain identity"},
		{name: "development toolchain", path: "/toolchains/go", version: "devel go1.27-deadbeef\ndarwin\narm64\n", errorMatch: "unsupported Go toolchain identity"},
		{name: "trailing text", path: "/toolchains/go", version: "go1.26.6\ndarwin\narm64\nunexpected\n", errorMatch: "unsupported Go toolchain identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			versionPath := ""
			operations := buildOperations{
				findGo: func() (string, error) { return test.path, nil },
				goVersion: func(_ context.Context, path string) (string, error) {
					versionPath = path
					return test.version, nil
				},
			}
			path, identity, err := resolveGoToolchain(context.Background(), operations)
			if test.errorMatch != "" {
				if err == nil || !strings.Contains(err.Error(), test.errorMatch) {
					t.Fatalf("resolve error = %v, want %q", err, test.errorMatch)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if path != test.path || versionPath != test.path {
				t.Fatalf("resolved/version paths = %q/%q, want exactly %q", path, versionPath, test.path)
			}
			if identity != test.want {
				t.Fatalf("identity = %#v, want %#v", identity, test.want)
			}
		})
	}
}

func TestValidateBuildPlatformRejectsOutsideProductBoundary(t *testing.T) {
	if err := validateBuildPlatform("darwin", "arm64"); err != nil {
		t.Fatalf("validate supported platform: %v", err)
	}
	for _, platform := range [][2]string{{"linux", "arm64"}, {"darwin", "amd64"}} {
		err := validateBuildPlatform(platform[0], platform[1])
		if err == nil || !strings.Contains(err.Error(), "supported only on darwin/arm64") {
			t.Fatalf("validate %s/%s error = %v", platform[0], platform[1], err)
		}
	}
}

func TestControlledBuildEnvironmentEnforcesPublicModuleVerification(t *testing.T) {
	environment := []string{
		"PATH=/usr/bin:/bin",
		"XCADDY_SKIP_BUILD=1",
		"XCADDY_GO_BUILD_FLAGS=-race",
		"XCADDY_RACE_DETECTOR=1",
		"XCADDY_SETCAP=1",
		"GOPROXY=https://proxy.example.test",
		"GOSUMDB=off",
		"GOPRIVATE=github.com/caddyserver/*",
		"GONOPROXY=*",
		"GONOSUMDB=github.com/mholt/*",
		"GOENV=/tmp/hostile-goenv",
		"GO111MODULE=off",
		"GOAUTH=/tmp/credentials",
		"GODEBUG=http2client=0",
		"GOFLAGS=-race -gcflags=all=-N",
		"GOINSECURE=*",
		"GOVCS=*:all",
		"GOWORK=/tmp/hostile.work",
		"GOEXPERIMENT=arenas",
		"GOTOOLCHAIN=auto",
		"GOTMPDIR=/tmp/hostile",
		"GOCACHEPROG=/tmp/hostile-cache",
		"CGO_ENABLED=1",
		"GOOS=linux",
		"GOARCH=amd64",
		"GOARM64=v9.5",
		"GOHOSTOS=linux",
		"GOHOSTARCH=amd64",
		"GO_EXTLINK_ENABLED=1",
		"GO_LDSO=/tmp/hostile-ldso",
		"GOTOOLDIR=/tmp/hostile-tools",
		"GOROOT=/tmp/fake-go",
		"CC=/tmp/fake-cc",
		"CGO_CFLAGS=-DHOSTILE",
		"CADDY_VERSION=master",
		"NOT_XCADDY_SKIP_BUILD=keep",
		"xcaddy_lowercase=keep",
	}
	workspace := "/test/workspace"
	want := append([]string{
		"PATH=/usr/bin:/bin",
		"NOT_XCADDY_SKIP_BUILD=keep",
		"xcaddy_lowercase=keep",
	}, resolvedDeterministicEnvironment(workspace)...)
	want = append(want, "XCADDY_WHICH_GO=/test/toolchains/go")
	if got := controlledBuildEnvironment(environment, "/test/toolchains/go", workspace); !reflect.DeepEqual(got, want) {
		t.Fatalf("controlled environment = %q, want %q", got, want)
	}
}

func TestControlledBuildEnvironmentIgnoresPersistentGoEnvironment(t *testing.T) {
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go is not installed")
	}
	goEnvPath := filepath.Join(t.TempDir(), "go.env")
	if err := os.WriteFile(goEnvPath, []byte("GOFLAGS=-race\nGOWORK=/tmp/hostile.work\nGOEXPERIMENT=arenas\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(goPath, "env", "GOENV", "GOFLAGS", "GOWORK", "GOEXPERIMENT", "GOTOOLCHAIN", "CGO_ENABLED", "GOOS", "GOARCH", "GOARM64")
	command.Env = controlledBuildEnvironment(append(os.Environ(), "GOENV="+goEnvPath), goPath, t.TempDir())
	output, err := command.Output()
	if err != nil {
		t.Fatalf("read controlled go environment: %v", err)
	}
	want := "\n\noff\n\nlocal\n0\ndarwin\narm64\nv8.0\n"
	if string(output) != want {
		t.Fatalf("controlled go env output = %q, want %q", output, want)
	}
}

func TestBuildUsesFreshWorkspaceAndIgnoresInheritedCaches(t *testing.T) {
	poisoned := t.TempDir()
	poisonedPaths := map[string]string{
		"GOMODCACHE": filepath.Join(poisoned, "gomodcache"),
		"GOCACHE":    filepath.Join(poisoned, "gocache"),
		"GOPATH":     filepath.Join(poisoned, "gopath"),
		"GOTMPDIR":   filepath.Join(poisoned, "tmp"),
	}
	for name, path := range poisonedPaths {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "poisoned"), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv(name, path)
	}

	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	var workspaces []string
	for range 2 {
		calls := &buildCallCounts{}
		operations := successfulBuildOperations(t, binPath, calls)
		buildOperation := operations.build
		operations.build = func(ctx context.Context, path string, environment []string, args ...string) error {
			output := outputPathFromBuildArgs(t, args)
			workspace := filepath.Dir(output)
			workspaces = append(workspaces, workspace)
			actual := environmentMap(environment)
			for name, leaf := range map[string]string{
				"GOMODCACHE": "gomodcache",
				"GOCACHE":    "gocache",
				"GOPATH":     "gopath",
				"GOTMPDIR":   "tmp",
			} {
				want := filepath.Join(workspace, leaf)
				if actual[name] != want {
					t.Fatalf("%s = %q, want fresh workspace path %q", name, actual[name], want)
				}
				if _, err := os.Stat(filepath.Join(actual[name], "poisoned")); !os.IsNotExist(err) {
					t.Fatalf("%s reused poisoned cache contents: %v", name, err)
				}
			}
			return buildOperation(ctx, path, environment, args...)
		}
		if _, err := build(context.Background(), true, binPath, operations); err != nil {
			t.Fatalf("isolated build: %v", err)
		}
		if _, err := os.Stat(workspaces[len(workspaces)-1]); !os.IsNotExist(err) {
			t.Fatalf("build workspace remained after success: %v", err)
		}
	}
	if workspaces[0] == workspaces[1] {
		t.Fatalf("successive builds reused workspace %q", workspaces[0])
	}
	manifest, err := os.ReadFile(buildManifestPath(binPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, workspace := range workspaces {
		if strings.Contains(string(manifest), workspace) {
			t.Fatalf("manifest persisted random workspace %q: %s", workspace, manifest)
		}
	}
	var recorded buildManifest
	if err := json.Unmarshal(manifest, &recorded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recorded.GoEnvironment, deterministicGoEnvironment) {
		t.Fatalf("manifest environment = %q, want stable policy %q", recorded.GoEnvironment, deterministicGoEnvironment)
	}
}

func TestBuildCleansWorkspaceAfterFailure(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	var workspace string
	operations := withTestGo(buildOperations{
		findXCaddy: func() (string, error) { return "/test/xcaddy", nil },
		xcaddyVersion: func(context.Context, string) (string, error) {
			return xcaddyVersion + " " + xcaddyVersionSum + "\n", nil
		},
		build: func(_ context.Context, _ string, _ []string, args ...string) error {
			workspace = filepath.Dir(outputPathFromBuildArgs(t, args))
			moduleDirectory := filepath.Join(workspace, "gomodcache", "example.com", "module@v1.0.0")
			if err := os.MkdirAll(moduleDirectory, 0o755); err != nil {
				return err
			}
			moduleFile := filepath.Join(moduleDirectory, "module.go")
			if err := os.WriteFile(moduleFile, []byte("package module\n"), 0o444); err != nil {
				return err
			}
			if err := os.Chmod(moduleDirectory, 0o555); err != nil {
				return err
			}
			return errors.New("injected build failure")
		},
	})
	if _, err := build(context.Background(), false, binPath, operations); err == nil || !strings.Contains(err.Error(), "injected build failure") {
		t.Fatalf("build error = %v, want injected failure", err)
	}
	if workspace == "" {
		t.Fatal("build did not create a workspace")
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("build workspace remained after failure: %v", err)
	}
}

func TestBuildCleansWorkspaceAfterCancellation(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	workspaceReady := make(chan string, 1)
	operations := withTestGo(buildOperations{
		findXCaddy: func() (string, error) { return "/test/xcaddy", nil },
		xcaddyVersion: func(context.Context, string) (string, error) {
			return xcaddyVersion + " " + xcaddyVersionSum + "\n", nil
		},
		build: func(ctx context.Context, _ string, _ []string, args ...string) error {
			workspaceReady <- filepath.Dir(outputPathFromBuildArgs(t, args))
			<-ctx.Done()
			return ctx.Err()
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := build(ctx, false, binPath, operations)
		result <- err
	}()
	workspace := <-workspaceReady
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled build error = %v, want context cancellation", err)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("cancelled build workspace remained: %v", err)
	}
}

func TestSystemXCaddyBuildCancellationTerminatesProcessGroup(t *testing.T) {
	dir := t.TempDir()
	childPIDPath := filepath.Join(dir, "child.pid")
	xcaddyPath := filepath.Join(dir, "xcaddy")
	script := "#!/bin/sh\nsleep 600 &\necho $! > " + childPIDPath + "\nwait\n"
	if err := os.WriteFile(xcaddyPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "workspace", "caddy")
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- systemBuildOperations.build(ctx, xcaddyPath, os.Environ(), xcaddyBuildArgs(output)...)
	}()
	var childPID int
	deadline := time.Now().Add(5 * time.Second)
	for childPID == 0 && time.Now().Before(deadline) {
		data, err := os.ReadFile(childPIDPath)
		if err == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
		if childPID == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if childPID == 0 {
		cancel()
		t.Fatal("fake xcaddy child did not start")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled xcaddy error = %v, want context cancellation", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("xcaddy child PID %d survived cancellation", childPID)
}

func TestRealBuildUsesReviewedToolchainAndFreshCaches(t *testing.T) {
	if os.Getenv("SHUNT_CADDY_REAL_BUILD") != "1" {
		t.Skip("set SHUNT_CADDY_REAL_BUILD=1 to exercise xcaddy with fresh authenticated caches")
	}
	for _, command := range []string{"go", "xcaddy"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Fatalf("%s is required for the real Caddy build: %v", command, err)
		}
	}
	poisoned := t.TempDir()
	for _, name := range []string{"GOMODCACHE", "GOCACHE", "GOPATH", "GOTMPDIR"} {
		path := filepath.Join(poisoned, strings.ToLower(name)+"-not-a-directory")
		if err := os.WriteFile(path, []byte("must not be used"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(name, path)
	}
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	if _, err := build(context.Background(), true, binPath, systemBuildOperations); err != nil {
		t.Fatalf("real isolated Caddy build: %v", err)
	}
	output, err := exec.Command(binPath, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("run real Caddy output: %v: %s", err, output)
	}
	want := caddyVersion + " " + caddyVersionSum
	if strings.TrimSpace(string(output)) != want {
		t.Fatalf("real Caddy version = %q, want %q", strings.TrimSpace(string(output)), want)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(binPath), ".caddy-build-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("real build left isolated workspaces: %q", matches)
	}
}

func TestBuildRejectsSuccessWithoutNewOutput(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	verifyCalled := false
	operations := withTestGo(buildOperations{
		findXCaddy: func() (string, error) { return "/test/xcaddy", nil },
		xcaddyVersion: func(context.Context, string) (string, error) {
			return xcaddyVersion + " " + xcaddyVersionSum + "\n", nil
		},
		build: func(context.Context, string, []string, ...string) error {
			return nil
		},
		verifyBinary: func(string, goToolchainIdentity) error {
			verifyCalled = true
			return nil
		},
	})

	_, err := build(context.Background(), false, binPath, operations)
	if err == nil || !strings.Contains(err.Error(), "inspect newly built binary") {
		t.Fatalf("build error = %v, want missing new output", err)
	}
	if verifyCalled {
		t.Fatal("missing build output reached module verification")
	}
	if _, statErr := os.Stat(binPath); !os.IsNotExist(statErr) {
		t.Fatalf("live binary exists after no-output build: %v", statErr)
	}
}

func TestBuildRejectsOutputWithoutOwnerExecutePermission(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	verifyCalled := false
	operations := withTestGo(buildOperations{
		findXCaddy: func() (string, error) { return "/test/xcaddy", nil },
		xcaddyVersion: func(context.Context, string) (string, error) {
			return xcaddyVersion + " " + xcaddyVersionSum + "\n", nil
		},
		build: func(_ context.Context, _ string, _ []string, args ...string) error {
			return os.WriteFile(outputPathFromBuildArgs(t, args), []byte("not-owner-executable"), 0o001)
		},
		verifyBinary: func(string, goToolchainIdentity) error {
			verifyCalled = true
			return nil
		},
	})

	_, err := build(context.Background(), false, binPath, operations)
	if err == nil || !strings.Contains(err.Error(), "not owner-executable") {
		t.Fatalf("build error = %v, want owner-executable rejection", err)
	}
	if verifyCalled {
		t.Fatal("non-owner-executable output reached module verification")
	}
	if _, statErr := os.Stat(binPath); !os.IsNotExist(statErr) {
		t.Fatalf("live binary exists after rejected output: %v", statErr)
	}
}

func TestVerifyCaddyBuildInfoRequiresExactUnreplacedPins(t *testing.T) {
	good := func() *debug.BuildInfo {
		return &debug.BuildInfo{
			GoVersion: testGoToolchain.Version,
			Main:      debug.Module{Path: "caddy", Version: "(devel)"},
			Deps: []*debug.Module{
				{Path: caddyModule, Version: caddyVersion, Sum: caddyVersionSum},
				{Path: l4Module, Version: l4Version, Sum: l4VersionSum},
				{Path: "example.com/transitive", Version: "v1.0.0"},
			},
			Settings: buildSettingsForTest(),
		}
	}
	if err := verifyCaddyBuildInfo(good(), testGoToolchain); err != nil {
		t.Fatalf("verify exact build info: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*debug.BuildInfo)
		match  string
	}{
		{name: "missing Caddy", mutate: func(info *debug.BuildInfo) { info.Deps = info.Deps[1:] }, match: "Caddy module"},
		{name: "wrong Caddy", mutate: func(info *debug.BuildInfo) { info.Deps[0].Version = "v2.10.2" }, match: "want exactly v2.11.4"},
		{name: "empty Caddy sum", mutate: func(info *debug.BuildInfo) { info.Deps[0].Sum = "" }, match: "Caddy module sum"},
		{name: "wrong Caddy sum", mutate: func(info *debug.BuildInfo) { info.Deps[0].Sum = "h1:wrong" }, match: "want exactly " + caddyVersionSum},
		{name: "duplicate Caddy", mutate: func(info *debug.BuildInfo) {
			info.Deps = append(info.Deps, &debug.Module{Path: caddyModule, Version: caddyVersion, Sum: caddyVersionSum})
		}, match: "occurs more than once"},
		{name: "missing caddy-l4", mutate: func(info *debug.BuildInfo) { info.Deps = append(info.Deps[:1], info.Deps[2:]...) }, match: "caddy-l4 module"},
		{name: "wrong caddy-l4", mutate: func(info *debug.BuildInfo) { info.Deps[1].Version = "v0.1.1" }, match: "want exactly v0.1.2"},
		{name: "empty caddy-l4 sum", mutate: func(info *debug.BuildInfo) { info.Deps[1].Sum = "" }, match: "caddy-l4 module sum"},
		{name: "wrong caddy-l4 sum", mutate: func(info *debug.BuildInfo) { info.Deps[1].Sum = "h1:wrong" }, match: "want exactly " + l4VersionSum},
		{name: "duplicate caddy-l4", mutate: func(info *debug.BuildInfo) {
			info.Deps = append(info.Deps, &debug.Module{Path: l4Module, Version: l4Version, Sum: l4VersionSum})
		}, match: "occurs more than once"},
		{name: "Caddy replacement", mutate: func(info *debug.BuildInfo) { info.Deps[0].Replace = &debug.Module{Path: "../local-caddy"} }, match: "uses a replacement"},
		{name: "caddy-l4 replacement", mutate: func(info *debug.BuildInfo) { info.Deps[1].Replace = &debug.Module{Path: "../local-l4"} }, match: "uses a replacement"},
		{name: "transitive replacement", mutate: func(info *debug.BuildInfo) { info.Deps[2].Replace = &debug.Module{Path: "../local"} }, match: "uses a replacement"},
		{name: "wrong toolchain", mutate: func(info *debug.BuildInfo) { info.GoVersion = "go1.26.4" }, match: "Go toolchain"},
		{name: "race build", mutate: func(info *debug.BuildInfo) {
			info.Settings = append(info.Settings, debug.BuildSetting{Key: "-race", Value: "true"})
		}, match: "unexpected Go build setting -race"},
		{name: "wrong target", mutate: func(info *debug.BuildInfo) { setBuildSetting(info, "GOOS", "linux") }, match: "Go build setting GOOS"},
		{name: "CGO enabled", mutate: func(info *debug.BuildInfo) { setBuildSetting(info, "CGO_ENABLED", "1") }, match: "Go build setting CGO_ENABLED"},
		{name: "missing trimpath", mutate: func(info *debug.BuildInfo) { removeBuildSetting(info, "-trimpath") }, match: "Go build setting -trimpath"},
		{name: "unexpected compiler flags", mutate: func(info *debug.BuildInfo) {
			info.Settings = append(info.Settings, debug.BuildSetting{Key: "-gcflags", Value: "all=-N"})
		}, match: "unexpected Go build setting -gcflags"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := good()
			test.mutate(info)
			err := verifyCaddyBuildInfo(info, testGoToolchain)
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("verify error = %v, want %q", err, test.match)
			}
		})
	}
}

func TestBuildLockExcludesAnotherProcess(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "shunt.lock")
	lock, err := acquireBuildLock(context.Background(), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := lock.release(); err != nil {
			t.Errorf("release parent lock: %v", err)
		}
	}()

	command := exec.Command(os.Args[0], "-test.run=^TestBuildLockHelper$")
	command.Env = append(os.Environ(), "SHUNT_CADDY_LOCK_HELPER="+lockPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("lock helper: %v\n%s", err, output)
	}
}

func TestBuildLockHelper(t *testing.T) {
	lockPath := os.Getenv("SHUNT_CADDY_LOCK_HELPER")
	if lockPath == "" {
		t.Skip("helper process only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	lock, err := acquireBuildLock(ctx, lockPath)
	if lock != nil {
		lock.release()
		t.Fatal("helper unexpectedly acquired the parent's build lock")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("helper lock error = %v, want deadline exceeded", err)
	}
}

func TestConcurrentFailedAndSuccessfulBuildsCannotInterleave(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	firstBuildStarted := make(chan struct{})
	allowFirstFailure := make(chan struct{})
	secondEntered := make(chan struct{})
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	firstOperations := withTestGo(buildOperations{
		findXCaddy: func() (string, error) { return "/test/xcaddy", nil },
		xcaddyVersion: func(context.Context, string) (string, error) {
			return xcaddyVersion + " " + xcaddyVersionSum + "\n", nil
		},
		build: func(context.Context, string, []string, ...string) error {
			close(firstBuildStarted)
			<-allowFirstFailure
			return errors.New("first build failed")
		},
		verifyBinary: func(string, goToolchainIdentity) error { return nil },
	})
	secondOperations := withTestGo(buildOperations{
		findXCaddy: func() (string, error) {
			close(secondEntered)
			return "/test/xcaddy", nil
		},
		xcaddyVersion: func(context.Context, string) (string, error) {
			return xcaddyVersion + " " + xcaddyVersionSum + "\n", nil
		},
		build: func(_ context.Context, _ string, _ []string, args ...string) error {
			return os.WriteFile(args[3], []byte("second build"), 0o755)
		},
		verifyBinary: func(string, goToolchainIdentity) error { return nil },
	})

	go func() {
		_, err := build(context.Background(), false, binPath, firstOperations)
		firstDone <- err
	}()
	<-firstBuildStarted
	go func() {
		_, err := build(context.Background(), false, binPath, secondOperations)
		secondDone <- err
	}()
	select {
	case <-secondEntered:
		t.Fatal("second build crossed cache validation while the first held the lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(allowFirstFailure)
	if err := <-firstDone; err == nil || !strings.Contains(err.Error(), "first build failed") {
		t.Fatalf("first build error = %v, want injected failure", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second build: %v", err)
	}
	if !buildCacheMatches(binPath, buildManifestPath(binPath), expectedBuildManifest(testGoToolchain)) {
		t.Fatal("successful second build did not publish a coherent cache")
	}
	contents, err := os.ReadFile(binPath)
	if err != nil || string(contents) != "second build" {
		t.Fatalf("final binary contents = %q, err=%v", contents, err)
	}
}

type buildCallCounts struct {
	findGo  int
	goVer   int
	find    int
	version int
	build   int
	verify  int
}

func successfulBuildOperations(t *testing.T, binPath string, calls *buildCallCounts) buildOperations {
	t.Helper()
	return buildOperations{
		findGo: func() (string, error) {
			calls.findGo++
			return "/test/toolchains/go", nil
		},
		goVersion: func(_ context.Context, path string) (string, error) {
			calls.goVer++
			if path != "/test/toolchains/go" {
				t.Fatalf("go version path = %q, want /test/toolchains/go", path)
			}
			return testGoToolchain.Version + "\ndarwin\narm64\n", nil
		},
		findXCaddy: func() (string, error) {
			calls.find++
			return "/test/xcaddy", nil
		},
		xcaddyVersion: func(_ context.Context, path string) (string, error) {
			calls.version++
			if path != "/test/xcaddy" {
				t.Fatalf("version path = %q, want /test/xcaddy", path)
			}
			return xcaddyVersion + " " + xcaddyVersionSum + "\n", nil
		},
		build: func(_ context.Context, path string, environment []string, args ...string) error {
			calls.build++
			if path != "/test/xcaddy" {
				t.Fatalf("build path = %q, want /test/xcaddy", path)
			}
			output := outputPathFromBuildArgs(t, args)
			workspace := filepath.Dir(output)
			assertControlledBuildEnvironment(t, environment, workspace)
			if filepath.Dir(workspace) != filepath.Dir(binPath) || !strings.HasPrefix(filepath.Base(workspace), ".caddy-build-") || filepath.Base(output) != "caddy" {
				t.Fatalf("temporary output = %q, want caddy in an isolated workspace beside %q", output, binPath)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("temporary output existed before xcaddy build: %v", err)
			}
			return os.WriteFile(output, []byte("fresh"), 0o755)
		},
		verifyBinary: func(path string, toolchain goToolchainIdentity) error {
			calls.verify++
			if toolchain != testGoToolchain {
				t.Fatalf("verified toolchain = %#v, want %#v", toolchain, testGoToolchain)
			}
			if filepath.Dir(filepath.Dir(path)) != filepath.Dir(binPath) || filepath.Base(path) != "caddy" {
				t.Fatalf("verified path = %q, want workspace output beside %q", path, binPath)
			}
			return nil
		},
	}
}

func outputPathFromBuildArgs(t *testing.T, args []string) string {
	t.Helper()
	if len(args) != 6 {
		t.Fatalf("build args = %q, want six arguments", args)
	}
	output := args[3]
	normalized := append([]string(nil), args...)
	normalized[3] = buildOutputToken
	if want := xcaddyBuildArgs(buildOutputToken); !reflect.DeepEqual(normalized, want) {
		t.Fatalf("build args = %q, want recipe %q", args, want)
	}
	return output
}

func assertControlledBuildEnvironment(t *testing.T, environment []string, workspace string) {
	t.Helper()
	want := map[string]string{"XCADDY_WHICH_GO": "/test/toolchains/go"}
	for _, setting := range resolvedDeterministicEnvironment(workspace) {
		name, value, _ := strings.Cut(setting, "=")
		want[name] = value
	}
	seen := map[string]int{}
	for _, entry := range environment {
		name, value, found := strings.Cut(entry, "=")
		if found && strings.HasPrefix(name, "XCADDY_") && name != "XCADDY_WHICH_GO" {
			t.Fatalf("controlled build environment retained %q", entry)
		}
		if expected, exists := want[name]; exists {
			seen[name]++
			if value != expected {
				t.Errorf("controlled build environment has %s=%q, want %q", name, value, expected)
			}
		}
	}
	for name := range want {
		if seen[name] != 1 {
			t.Errorf("controlled build environment has %d %s entries, want exactly one", seen[name], name)
		}
	}
}

func resolvedDeterministicEnvironment(workspace string) []string {
	resolved := make([]string, 0, len(deterministicGoEnvironment))
	for _, setting := range deterministicGoEnvironment {
		resolved = append(resolved, strings.ReplaceAll(setting, buildWorkspaceToken, workspace))
	}
	return resolved
}

func environmentMap(environment []string) map[string]string {
	values := make(map[string]string, len(environment))
	for _, setting := range environment {
		name, value, found := strings.Cut(setting, "=")
		if found {
			values[name] = value
		}
	}
	return values
}

func manifestForBinary(t *testing.T, binPath string) buildManifest {
	t.Helper()
	manifest := expectedBuildManifest(testGoToolchain)
	digest, err := fileSHA256(binPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.BinarySHA256 = digest
	return manifest
}

func withTestGo(operations buildOperations) buildOperations {
	operations.findGo = func() (string, error) { return "/test/toolchains/go", nil }
	operations.goVersion = func(_ context.Context, path string) (string, error) {
		if path != "/test/toolchains/go" {
			return "", errors.New("unexpected Go executable path")
		}
		return testGoToolchain.Version + "\ndarwin\narm64\n", nil
	}
	return operations
}

func buildSettingsForTest() []debug.BuildSetting {
	settings := make([]debug.BuildSetting, 0, len(deterministicBuildSettings))
	for _, setting := range deterministicBuildSettings {
		key, value, _ := strings.Cut(setting, "=")
		settings = append(settings, debug.BuildSetting{Key: key, Value: value})
	}
	return settings
}

func setBuildSetting(info *debug.BuildInfo, key, value string) {
	for index := range info.Settings {
		if info.Settings[index].Key == key {
			info.Settings[index].Value = value
			return
		}
	}
}

func removeBuildSetting(info *debug.BuildInfo, key string) {
	for index := range info.Settings {
		if info.Settings[index].Key == key {
			info.Settings = append(info.Settings[:index], info.Settings[index+1:]...)
			return
		}
	}
}

func writeMatchingBuildManifest(t *testing.T, binPath string) error {
	t.Helper()
	return writeBuildManifestAtomic(buildManifestPath(binPath), manifestForBinary(t, binPath))
}

func TestPinnedCaddyDoesNotSupportForwardAuth(t *testing.T) {
	if caddyVersion != "v2.11.4" {
		t.Fatalf("Caddy version = %s, update this forward_auth boundary when changing the pin", caddyVersion)
	}
	if forwardAuthFixedInVersion != "v2.11.5" {
		t.Fatalf("forward_auth fix version = %s, want v2.11.5", forwardAuthFixedInVersion)
	}
	if forwardAuthSupported {
		t.Fatal("forward_auth must remain disabled for the pinned Caddy version")
	}
	_, generated, err := ServerForRoute("test", state.Route{
		Key:        "web",
		Kind:       state.KindHTTP,
		ListenPort: 5000,
		CaddyID:    "app_test_http_web",
	}, false)
	if err != nil {
		t.Fatalf("generate HTTP route: %v", err)
	}
	if strings.Contains(string(generated), "forward_auth") {
		t.Fatalf("generated HTTP route enables unsupported forward_auth: %s", generated)
	}
}
