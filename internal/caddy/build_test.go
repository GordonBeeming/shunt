package caddy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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
		BuildRecipe:   append([]string{"xcaddy"}, want...),
		BinarySHA256:  "",
	}
	if got := expectedBuildManifest(bin); !reflect.DeepEqual(got, wantManifest) {
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
		build: func(context.Context, string, ...string) error {
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
	if !buildCacheMatches(binPath, buildManifestPath(binPath), expectedBuildManifest(binPath)) {
		t.Fatal("changed binary was not replaced with a digest-bound cache entry")
	}
}

func TestBuildRebuildsWhenCachedBinaryIsNotExecutable(t *testing.T) {
	binPath := filepath.Join(t.TempDir(), "caddy", "shunt")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeMatchingBuildManifest(t, binPath); err != nil {
		t.Fatal(err)
	}
	calls := &buildCallCounts{}

	if _, err := build(context.Background(), false, binPath, successfulBuildOperations(t, binPath, calls)); err != nil {
		t.Fatalf("rebuild non-executable binary: %v", err)
	}
	if calls.build != 1 {
		t.Fatalf("build calls = %d, want 1", calls.build)
	}
}

func TestBuildRebuildsWhenManifestIsMissing(t *testing.T) {
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
	if calls.find != 1 || calls.version != 1 || calls.build != 1 {
		t.Fatalf("operation calls = %+v, want one of each", calls)
	}
	if !buildCacheMatches(binPath, buildManifestPath(binPath), expectedBuildManifest(binPath)) {
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
	if !buildCacheMatches(binPath, buildManifestPath(binPath), expectedBuildManifest(binPath)) {
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
		build: func(context.Context, string, ...string) error {
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

func TestBuildFailureInvalidatesMatchingManifest(t *testing.T) {
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
	operations := buildOperations{
		findXCaddy: func() (string, error) { return "/test/xcaddy", nil },
		xcaddyVersion: func(context.Context, string) (string, error) {
			return "v0.4.6\n", nil
		},
		build: func(context.Context, string, ...string) error {
			return errors.New("compile failed")
		},
	}

	_, err := build(context.Background(), true, binPath, operations)
	if err == nil || !strings.Contains(err.Error(), "xcaddy build: compile failed") {
		t.Fatalf("build error = %v, want compile failure", err)
	}
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("manifest remains after failed rebuild: %v", statErr)
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

type buildCallCounts struct {
	find    int
	version int
	build   int
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
		build: func(_ context.Context, path string, args ...string) error {
			calls.build++
			if path != "/test/xcaddy" {
				t.Fatalf("build path = %q, want /test/xcaddy", path)
			}
			if !reflect.DeepEqual(args, xcaddyBuildArgs(binPath)) {
				t.Fatalf("build args = %q, want %q", args, xcaddyBuildArgs(binPath))
			}
			return os.WriteFile(binPath, []byte("fresh"), 0o755)
		},
	}
}

func manifestForBinary(t *testing.T, binPath string) buildManifest {
	t.Helper()
	manifest := expectedBuildManifest(binPath)
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
