package caddy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestXcaddyBuildArgsPinEveryModule(t *testing.T) {
	bin := "/tmp/shunt"
	want := []string{"build", "v2.11.4", "--output", bin, "--with", "github.com/mholt/caddy-l4@v0.1.2"}
	if got := xcaddyBuildArgs(bin); !reflect.DeepEqual(got, want) {
		t.Fatalf("xcaddy build args = %q, want %q", got, want)
	}
	wantManifest := buildManifest{
		Version:       1,
		CaddyVersion:  "v2.11.4",
		Module:        "github.com/mholt/caddy-l4",
		ModuleVersion: "v0.1.2",
		XCaddyVersion: "v0.4.6",
		BuildRecipe:   append([]string{"xcaddy"}, xcaddyBuildArgs(buildOutputToken)...),
		BinarySHA256:  "",
	}
	if got := expectedBuildManifest(); !reflect.DeepEqual(got, wantManifest) {
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

	operations := buildOperations{
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
	}

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
	if !buildCacheMatches(binPath, buildManifestPath(binPath), expectedBuildManifest()) {
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
	if calls.find != 1 || calls.version != 1 || calls.build != 1 || calls.verify != 1 {
		t.Fatalf("operation calls = %+v, want one of each", calls)
	}
	if !buildCacheMatches(binPath, buildManifestPath(binPath), expectedBuildManifest()) {
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
	if !buildCacheMatches(binPath, buildManifestPath(binPath), expectedBuildManifest()) {
		t.Fatal("mismatched manifest was not replaced with the current build identity")
	}
}

func TestBuildRejectsMismatchedXCaddyVersion(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	buildCalled := false
	operations := buildOperations{
		findXCaddy: func() (string, error) { return "/test/xcaddy", nil },
		xcaddyVersion: func(_ context.Context, path string) (string, error) {
			if path != "/test/xcaddy" {
				t.Fatalf("version path = %q, want /test/xcaddy", path)
			}
			return "v0.4.5\n", nil
		},
		build: func(context.Context, string, []string, ...string) error {
			buildCalled = true
			return nil
		},
	}

	_, err := build(context.Background(), false, binPath, operations)
	if err == nil || !strings.Contains(err.Error(), "xcaddy version mismatch") {
		t.Fatalf("build error = %v, want xcaddy version mismatch", err)
	}
	if buildCalled {
		t.Fatal("build ran with a mismatched xcaddy version")
	}
	if _, statErr := os.Stat(buildManifestPath(binPath)); !os.IsNotExist(statErr) {
		t.Fatalf("manifest exists after version mismatch: %v", statErr)
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
	operations := buildOperations{
		findXCaddy: func() (string, error) { return "/test/xcaddy", nil },
		xcaddyVersion: func(context.Context, string) (string, error) {
			return "v0.4.6\n", nil
		},
		build: func(context.Context, string, []string, ...string) error {
			return errors.New("compile failed")
		},
		verifyBinary: func(string) error { return nil },
	}

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
	if !buildCacheMatches(binPath, manifestPath, expectedBuildManifest()) {
		t.Fatal("failed forced build did not preserve the known-good cache")
	}
}

func TestMatchesXCaddyVersionAcceptsOnlyReleaseOutput(t *testing.T) {
	validSum := "h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	for _, output := range []string{
		"v0.4.6",
		"v0.4.6\n",
		"v0.4.6 " + validSum + "\n",
	} {
		if !matchesXCaddyVersion(output) {
			t.Errorf("matchesXCaddyVersion(%q) = false, want true", output)
		}
	}
	for _, output := range []string{
		"",
		"v0.4.5",
		"xcaddy v0.4.6",
		"v0.4.6-dirty",
		"v0.4.6 h1:not-a-real-sum",
		"v0.4.6 " + validSum + " => ./local",
	} {
		if matchesXCaddyVersion(output) {
			t.Errorf("matchesXCaddyVersion(%q) = true, want false", output)
		}
	}
}

func TestControlledBuildEnvironmentRemovesEveryXCaddyVariable(t *testing.T) {
	environment := []string{
		"PATH=/usr/bin:/bin",
		"XCADDY_SKIP_BUILD=1",
		"XCADDY_GO_BUILD_FLAGS=-race",
		"XCADDY_RACE_DETECTOR=1",
		"XCADDY_SETCAP=1",
		"NOT_XCADDY_SKIP_BUILD=keep",
		"xcaddy_lowercase=keep",
	}
	want := []string{
		"PATH=/usr/bin:/bin",
		"NOT_XCADDY_SKIP_BUILD=keep",
		"xcaddy_lowercase=keep",
	}
	if got := controlledBuildEnvironment(environment); !reflect.DeepEqual(got, want) {
		t.Fatalf("controlled environment = %q, want %q", got, want)
	}
}

func TestBuildRejectsSuccessWithoutNewOutput(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	verifyCalled := false
	operations := buildOperations{
		findXCaddy: func() (string, error) { return "/test/xcaddy", nil },
		xcaddyVersion: func(context.Context, string) (string, error) {
			return "v0.4.6\n", nil
		},
		build: func(context.Context, string, []string, ...string) error {
			return nil
		},
		verifyBinary: func(string) error {
			verifyCalled = true
			return nil
		},
	}

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
	operations := buildOperations{
		findXCaddy: func() (string, error) { return "/test/xcaddy", nil },
		xcaddyVersion: func(context.Context, string) (string, error) {
			return "v0.4.6\n", nil
		},
		build: func(_ context.Context, _ string, _ []string, args ...string) error {
			return os.WriteFile(outputPathFromBuildArgs(t, args), []byte("not-owner-executable"), 0o001)
		},
		verifyBinary: func(string) error {
			verifyCalled = true
			return nil
		},
	}

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
			Main: debug.Module{Path: "caddy", Version: "(devel)"},
			Deps: []*debug.Module{
				{Path: caddyModule, Version: caddyVersion},
				{Path: l4Module, Version: l4Version},
				{Path: "example.com/transitive", Version: "v1.0.0"},
			},
		}
	}
	if err := verifyCaddyBuildInfo(good()); err != nil {
		t.Fatalf("verify exact build info: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*debug.BuildInfo)
		match  string
	}{
		{name: "missing Caddy", mutate: func(info *debug.BuildInfo) { info.Deps = info.Deps[1:] }, match: "Caddy module"},
		{name: "wrong Caddy", mutate: func(info *debug.BuildInfo) { info.Deps[0].Version = "v2.10.2" }, match: "want exactly v2.11.4"},
		{name: "missing caddy-l4", mutate: func(info *debug.BuildInfo) { info.Deps = append(info.Deps[:1], info.Deps[2:]...) }, match: "caddy-l4 module"},
		{name: "wrong caddy-l4", mutate: func(info *debug.BuildInfo) { info.Deps[1].Version = "v0.1.1" }, match: "want exactly v0.1.2"},
		{name: "pinned replacement", mutate: func(info *debug.BuildInfo) { info.Deps[1].Replace = &debug.Module{Path: "../local"} }, match: "uses a replacement"},
		{name: "transitive replacement", mutate: func(info *debug.BuildInfo) { info.Deps[2].Replace = &debug.Module{Path: "../local"} }, match: "uses a replacement"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := good()
			test.mutate(info)
			err := verifyCaddyBuildInfo(info)
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
	firstOperations := buildOperations{
		findXCaddy:    func() (string, error) { return "/test/xcaddy", nil },
		xcaddyVersion: func(context.Context, string) (string, error) { return "v0.4.6\n", nil },
		build: func(context.Context, string, []string, ...string) error {
			close(firstBuildStarted)
			<-allowFirstFailure
			return errors.New("first build failed")
		},
		verifyBinary: func(string) error { return nil },
	}
	secondOperations := buildOperations{
		findXCaddy: func() (string, error) {
			close(secondEntered)
			return "/test/xcaddy", nil
		},
		xcaddyVersion: func(context.Context, string) (string, error) { return "v0.4.6\n", nil },
		build: func(_ context.Context, _ string, _ []string, args ...string) error {
			return os.WriteFile(args[3], []byte("second build"), 0o755)
		},
		verifyBinary: func(string) error { return nil },
	}

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
	if !buildCacheMatches(binPath, buildManifestPath(binPath), expectedBuildManifest()) {
		t.Fatal("successful second build did not publish a coherent cache")
	}
	contents, err := os.ReadFile(binPath)
	if err != nil || string(contents) != "second build" {
		t.Fatalf("final binary contents = %q, err=%v", contents, err)
	}
}

type buildCallCounts struct {
	find    int
	version int
	build   int
	verify  int
}

func successfulBuildOperations(t *testing.T, binPath string, calls *buildCallCounts) buildOperations {
	t.Helper()
	return buildOperations{
		findXCaddy: func() (string, error) {
			calls.find++
			return "/test/xcaddy", nil
		},
		xcaddyVersion: func(_ context.Context, path string) (string, error) {
			calls.version++
			if path != "/test/xcaddy" {
				t.Fatalf("version path = %q, want /test/xcaddy", path)
			}
			return "v0.4.6 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n", nil
		},
		build: func(_ context.Context, path string, environment []string, args ...string) error {
			calls.build++
			if path != "/test/xcaddy" {
				t.Fatalf("build path = %q, want /test/xcaddy", path)
			}
			assertNoXCaddyEnvironment(t, environment)
			output := outputPathFromBuildArgs(t, args)
			if filepath.Dir(output) != filepath.Dir(binPath) || output == binPath {
				t.Fatalf("temporary output = %q, want unique path beside %q", output, binPath)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("temporary output existed before xcaddy build: %v", err)
			}
			return os.WriteFile(output, []byte("fresh"), 0o755)
		},
		verifyBinary: func(path string) error {
			calls.verify++
			if filepath.Dir(path) != filepath.Dir(binPath) || path == binPath {
				t.Fatalf("verified path = %q, want temporary output beside %q", path, binPath)
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

func assertNoXCaddyEnvironment(t *testing.T, environment []string) {
	t.Helper()
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if found && strings.HasPrefix(name, "XCADDY_") {
			t.Fatalf("controlled build environment retained %q", entry)
		}
	}
}

func manifestForBinary(t *testing.T, binPath string) buildManifest {
	t.Helper()
	manifest := expectedBuildManifest()
	digest, err := fileSHA256(binPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.BinarySHA256 = digest
	return manifest
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
