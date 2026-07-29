package imagecache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	archive "github.com/google/go-containerregistry/pkg/v1/tarball"
)

func TestAssureUsesCurrentGenerationWithoutRegistryAndPlansChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	one := mustRef(t, "example.test/one:latest")
	img := mustImage(t, 1)
	installFetcher(t, map[string]v1.Image{one.Name(): img})

	changes, err := Assure(context.Background(), path, []string{one.Name()})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Action != "added" || changes[0].Platform != "linux/"+runtime.GOARCH {
		t.Fatalf("changes = %#v", changes)
	}
	manifest, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !validDigest(manifest.Generation) || len(manifest.Images) != 1 {
		t.Fatalf("manifest = %#v", manifest)
	}

	old := fetchImage
	t.Cleanup(func() { fetchImage = old })
	fetchImage = func(context.Context, name.Reference) (fetchedImage, error) {
		t.Fatal("exact hit contacted registry")
		return fetchedImage{}, nil
	}
	changes, err = Assure(context.Background(), path, []string{one.Name()})
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Action != "unchanged" {
		t.Fatalf("changes = %#v", changes)
	}

	full, err := Plan(context.Background(), path, GuestState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Images) != 1 || full.Images[0].Ref != one.Name() || full.Images[0].Path == "" {
		t.Fatalf("full plan = %#v", full)
	}
	noOp, err := Plan(context.Background(), path, GuestState{Marker: full.Marker, ImageIDs: full.Marker.Images})
	if err != nil {
		t.Fatal(err)
	}
	if len(noOp.Images) != 0 || noOp.Generation != manifest.Generation {
		t.Fatalf("no-op plan = %#v", noOp)
	}
	file, err := OpenExport(context.Background(), path, one.Name(), manifest.Images[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
}

func TestRefreshNoOpDoesNotRewriteGenerationBlobsOrExports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/one:latest")
	img := mustImage(t, 2)
	installFetcher(t, map[string]v1.Image{ref.Name(): img})
	if _, err := Assure(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}
	manifest, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	indexBefore := statSnapshot(t, filepath.Join(path, "index.json"))
	generationBefore := statSnapshot(t, generationPath(path, manifest.Generation))
	blobPath, err := blobPath(path, manifest.Images[0].Blobs[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	blobBefore := statSnapshot(t, blobPath)
	exportPath, _ := cacheRelativePath(path, manifest.Images[0].Export)
	exportBefore := statSnapshot(t, exportPath)

	changes, err := Refresh(context.Background(), path, []string{ref.Name()})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Action != "unchanged" {
		t.Fatalf("changes = %#v", changes)
	}
	assertSnapshotUnchanged(t, filepath.Join(path, "index.json"), indexBefore)
	assertSnapshotUnchanged(t, generationPath(path, manifest.Generation), generationBefore)
	assertSnapshotUnchanged(t, blobPath, blobBefore)
	assertSnapshotUnchanged(t, exportPath, exportBefore)
}

func TestAddingImageReusesExistingContentAndRefreshPrunesRemovedRefs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	one := mustRef(t, "example.test/one:latest")
	two := mustRef(t, "example.test/two:latest")
	images := map[string]v1.Image{one.Name(): mustImage(t, 3), two.Name(): mustImage(t, 4)}
	calls := installFetcher(t, images)
	if _, err := Assure(context.Background(), path, []string{one.Name()}); err != nil {
		t.Fatal(err)
	}
	first, _ := Inspect(context.Background(), path)
	oldExport, _ := cacheRelativePath(path, first.Images[0].Export)
	exportBefore := statSnapshot(t, oldExport)
	oldBlob, _ := blobPath(path, first.Images[0].Blobs[0].Digest)
	blobBefore := statSnapshot(t, oldBlob)

	changes, err := Assure(context.Background(), path, []string{one.Name(), two.Name()})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || changes[0].Action != "unchanged" || changes[1].Action != "added" {
		t.Fatalf("calls=%d changes=%#v", calls.Load(), changes)
	}
	assertSnapshotUnchanged(t, oldExport, exportBefore)
	assertSnapshotUnchanged(t, oldBlob, blobBefore)

	if _, err := Refresh(context.Background(), path, []string{two.Name()}); err != nil {
		t.Fatal(err)
	}
	current, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Images) != 1 || current.Images[0].Ref != two.Name() {
		t.Fatalf("removed ref was not pruned: %#v", current.Images)
	}
}

func TestChangedTagPublishesNewGenerationAndPlanLoadsOnlyChangedRef(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	one := mustRef(t, "example.test/one:latest")
	two := mustRef(t, "example.test/two:latest")
	first := mustImage(t, 5)
	second := mustImage(t, 6)
	updated := mustImage(t, 7)
	images := map[string]v1.Image{one.Name(): first, two.Name(): second}
	installFetcher(t, images)
	if _, err := Assure(context.Background(), path, []string{one.Name(), two.Name()}); err != nil {
		t.Fatal(err)
	}
	before, _ := Inspect(context.Background(), path)
	marker := markerFor(before)
	images[one.Name()] = updated

	changes, err := Refresh(context.Background(), path, []string{one.Name(), two.Name()})
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Action != "updated" || changes[1].Action != "unchanged" {
		t.Fatalf("changes = %#v", changes)
	}
	after, _ := Inspect(context.Background(), path)
	if after.Generation == before.Generation {
		t.Fatal("changed tag did not publish a generation")
	}
	plan, err := Plan(context.Background(), path, GuestState{Marker: marker, ImageIDs: marker.Images})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Images) != 1 || plan.Images[0].Ref != one.Name() {
		t.Fatalf("incremental plan = %#v", plan)
	}
}

func TestPlanReloadsCurrentMarkerWhenGuestImageIdentityIsWrong(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/identity:latest")
	installFetcher(t, map[string]v1.Image{ref.Name(): mustImage(t, 70)})
	if _, err := Assure(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}
	manifest, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	marker := markerFor(manifest)
	wrong := mustImage(t, 71)
	wrongID, err := wrong.ConfigName()
	if err != nil {
		t.Fatal(err)
	}

	plan, err := Plan(context.Background(), path, GuestState{
		Marker:   marker,
		ImageIDs: map[string]string{ref.Name(): wrongID.String()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Images) != 1 || plan.Images[0].Ref != ref.Name() || plan.Images[0].ImageID != manifest.Images[0].ConfigDigest {
		t.Fatalf("wrong identity plan = %#v", plan)
	}

	matching, err := Plan(context.Background(), path, GuestState{
		Marker:   GuestMarker{},
		ImageIDs: map[string]string{ref.Name(): manifest.Images[0].ConfigDigest},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(matching.Images) != 0 {
		t.Fatalf("matching identity unnecessarily loaded: %#v", matching.Images)
	}
}

func TestLocalBuildAssureBuildsMissingAndUnusableButRefreshAlwaysBuilds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	contextDir := t.TempDir()
	dockerfile := filepath.Join(contextDir, "Containerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := mustRef(t, "example.test/local:latest")
	img := mustImage(t, 72)
	source := LocalBuildSource{
		Ref:        ref.Name(),
		ContextDir: contextDir,
		Dockerfile: "Containerfile",
		Platform:   "linux/arm64",
		BuildArgs:  map[string]string{"ZED": "last", "ALPHA": "first"},
	}

	var calls [][]string
	installContainerRunner(t, func(_ context.Context, args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		if len(args) >= 3 && args[0] == "image" && args[1] == "save" {
			output := argumentValue(t, args, "-o")
			file, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			defer file.Close()
			return archive.Write(ref, img, file)
		}
		return nil
	})
	changes, err := AssureSources(context.Background(), path, nil, []LocalBuildSource{source})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Action != "added" || len(calls) != 2 {
		t.Fatalf("first local assure calls=%#v changes=%#v", calls, changes)
	}
	wantBuild := []string{"build", "--platform", "linux/arm64", "--build-arg", "ALPHA=first", "--build-arg", "ZED=last", "-t", ref.Name(), "-f", dockerfile, contextDir}
	if strings.Join(calls[0], "\x00") != strings.Join(wantBuild, "\x00") {
		t.Fatalf("build args = %#v, want %#v", calls[0], wantBuild)
	}
	firstManifest, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if firstManifest.Images[0].SourceKind != sourceLocal || !validDigest(firstManifest.Images[0].SourceFingerprint) {
		t.Fatalf("local source provenance = %#v", firstManifest.Images[0])
	}

	calls = nil
	changes, err = AssureSources(context.Background(), path, nil, []LocalBuildSource{source})
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Action != "unchanged" || len(calls) != 0 {
		t.Fatalf("usable local assure rebuilt: calls=%#v changes=%#v", calls, changes)
	}

	source.BuildArgs["ALPHA"] = "changed"
	calls = nil
	changes, err = AssureSources(context.Background(), path, nil, []LocalBuildSource{source})
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Action != "updated" || len(calls) != 2 {
		t.Fatalf("changed local declaration was reused: calls=%#v changes=%#v", calls, changes)
	}
	changedManifest, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if changedManifest.Images[0].SourceFingerprint == firstManifest.Images[0].SourceFingerprint {
		t.Fatal("changed local declaration retained its source fingerprint")
	}

	manifest, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	exportPath, err := cacheRelativePath(path, manifest.Images[0].Export)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exportPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls = nil
	changes, err = AssureSources(context.Background(), path, nil, []LocalBuildSource{source})
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Action != "unchanged" || len(calls) != 2 {
		t.Fatalf("unusable local assure did not rebuild: calls=%#v changes=%#v", calls, changes)
	}

	calls = nil
	changes, err = RefreshSources(context.Background(), path, nil, []LocalBuildSource{source})
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Action != "unchanged" || len(calls) != 2 {
		t.Fatalf("local refresh did not rebuild: calls=%#v changes=%#v", calls, changes)
	}
}

func TestSourceProvenanceCompatibilityMatrix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/provenance:latest")
	capturedImage := mustImage(t, 73)
	if err := Capture(path, func(temp string) error {
		file, err := os.OpenFile(temp, os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		return archive.Write(ref, capturedImage, file)
	}); err != nil {
		t.Fatal(err)
	}
	manifest, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Images[0].SourceKind != sourceCapture {
		t.Fatalf("captured source kind = %q", manifest.Images[0].SourceKind)
	}

	oldFetch := fetchImage
	t.Cleanup(func() { fetchImage = oldFetch })
	fetchImage = func(context.Context, name.Reference) (fetchedImage, error) {
		t.Fatal("registry Assure fetched a compatible captured image")
		return fetchedImage{}, nil
	}
	changes, err := Assure(context.Background(), path, []string{ref.Name()})
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Action != "unchanged" {
		t.Fatalf("captured registry Assure changes = %#v", changes)
	}

	var fetchCalls atomic.Int32
	fetchImage = func(_ context.Context, _ name.Reference) (fetchedImage, error) {
		fetchCalls.Add(1)
		return fetchedImage{image: capturedImage, platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH}}, nil
	}
	changes, err = Refresh(context.Background(), path, []string{ref.Name()})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err = Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if fetchCalls.Load() != 1 || changes[0].Action != "updated" || manifest.Images[0].SourceKind != sourceRegistry {
		t.Fatalf("captured registry Refresh calls=%d changes=%#v source=%q", fetchCalls.Load(), changes, manifest.Images[0].SourceKind)
	}

	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	localImage := mustImage(t, 74)
	var containerCalls atomic.Int32
	installContainerRunner(t, func(_ context.Context, args ...string) error {
		containerCalls.Add(1)
		if len(args) >= 2 && args[0] == "image" && args[1] == "save" {
			file, err := os.OpenFile(argumentValue(t, args, "-o"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			defer file.Close()
			return archive.Write(ref, localImage, file)
		}
		return nil
	})
	changes, err = AssureSources(context.Background(), path, nil, []LocalBuildSource{{Ref: ref.Name(), ContextDir: contextDir}})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err = Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if containerCalls.Load() != 2 || changes[0].Action != "updated" || manifest.Images[0].SourceKind != sourceLocal {
		t.Fatalf("registry to local calls=%d changes=%#v source=%q", containerCalls.Load(), changes, manifest.Images[0].SourceKind)
	}

	fetchCalls.Store(0)
	changes, err = Assure(context.Background(), path, []string{ref.Name()})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err = Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if fetchCalls.Load() != 1 || changes[0].Action != "updated" || manifest.Images[0].SourceKind != sourceRegistry {
		t.Fatalf("local to registry calls=%d changes=%#v source=%q", fetchCalls.Load(), changes, manifest.Images[0].SourceKind)
	}
}

func TestStoreVersionOneIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"version":1,"generation":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	if err := os.WriteFile(filepath.Join(path, "index.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), path); err == nil || !strings.Contains(err.Error(), "unsupported image cache index") {
		t.Fatalf("legacy store error = %v", err)
	}
}

func TestCaptureImportsAtomicallyAndInvalidCapturePreservesGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/one:latest")
	img := mustImage(t, 8)
	if err := Capture(path, func(temp string) error {
		file, err := os.OpenFile(temp, os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		return archive.Write(ref, img, file)
	}); err != nil {
		t.Fatal(err)
	}
	before, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Capture(path, func(temp string) error { return os.WriteFile(temp, []byte("not a tar"), 0o600) }); err == nil {
		t.Fatal("invalid capture succeeded")
	}
	after, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation {
		t.Fatalf("invalid capture replaced generation: %s -> %s", before.Generation, after.Generation)
	}
	if err := Validate(path); err != nil {
		t.Fatal(err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".shunt-images-capture-*.tar")); len(leftovers) != 0 {
		t.Fatalf("capture temps remain: %v", leftovers)
	}
}

func TestFetchForPlatformReportsArm64Fallback(t *testing.T) {
	ref := mustRef(t, "example.test/one:latest")
	img := mustImage(t, 9)
	var platforms []v1.Platform
	result, err := fetchForPlatform(context.Background(), ref, "arm64", func(_ context.Context, _ name.Reference, platform v1.Platform) (v1.Image, error) {
		platforms = append(platforms, platform)
		if len(platforms) == 1 {
			return nil, errors.New("no child with platform linux/arm64")
		}
		return img, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.fallback || result.platform.Architecture != "amd64" || len(platforms) != 2 {
		t.Fatalf("result=%#v platforms=%#v", result, platforms)
	}

	_, err = fetchForPlatform(context.Background(), ref, "arm64", func(context.Context, name.Reference, v1.Platform) (v1.Image, error) {
		return nil, errors.New("unauthorized")
	})
	if err == nil || strings.Contains(err.Error(), "amd64") {
		t.Fatalf("auth failure unexpectedly fell back: %v", err)
	}
}

func TestDigestReferenceIsRejected(t *testing.T) {
	img := mustImage(t, 10)
	digest, _ := img.Digest()
	_, err := Assure(context.Background(), filepath.Join(t.TempDir(), "images"), []string{"example.test/one@" + digest.String()})
	if err == nil || !strings.Contains(err.Error(), "must use a tag") {
		t.Fatalf("digest ref error = %v", err)
	}
}

func TestStaleExternalCredentialHelpersDoNotAffectAnonymousPull(t *testing.T) {
	server := httptest.NewServer(registry.New())
	defer server.Close()
	ref, err := name.NewTag(strings.TrimPrefix(server.URL, "http://")+"/public/image:latest", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	img := mustImage(t, 11)
	if err := remote.Write(ref, img, remote.WithTransport(http.DefaultTransport)); err != nil {
		t.Fatal(err)
	}
	dockerConfig := t.TempDir()
	if err := os.WriteFile(filepath.Join(dockerConfig, "config.json"), []byte(`{"credsStore":"desktop","credHelpers":{"`+ref.Context().RegistryStr()+`":"orbstack"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dockerConfig)
	t.Setenv("DOCKER_AUTH_CONFIG", "")
	pulled, err := pullImage(context.Background(), ref, v1.Platform{})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := img.Digest()
	got, err := pulled.Digest()
	if err != nil || got != want {
		t.Fatalf("anonymous pull digest=%s err=%v, want %s", got, err, want)
	}
}

func TestInlineDockerAuthConfigIsExplicitAndHelpersAreIgnored(t *testing.T) {
	t.Setenv("DOCKER_AUTH_CONFIG", `{"auths":{"registry.example":{"username":"user","password":"hunter2"}},"credsStore":"desktop","credHelpers":{"registry.example":"orbstack"}}`)
	keychain, err := inlineKeychainFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	registryRef := mustRef(t, "registry.example/team/image:latest")
	authenticator, err := keychain.Resolve(registryRef.Context())
	if err != nil {
		t.Fatal(err)
	}
	config, err := authenticator.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	if config.Username != "user" || config.Password != "hunter2" {
		t.Fatalf("inline config not resolved: %#v", config)
	}
}

func TestCrossDomainRealmCannotReceiveCredentials(t *testing.T) {
	var calls atomic.Int32
	base := roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	transport := &domainBoundTransport{base: base, registryHost: "registry.example"}
	request, _ := http.NewRequest(http.MethodGet, "https://auth.attacker.example/token", nil)
	request.Header.Set("Authorization", "Basic dXNlcjpodW50ZXIy")
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("cross-domain credential request succeeded")
	}
	if calls.Load() != 0 {
		t.Fatal("cross-domain realm received a request")
	}

	allowed := &domainBoundTransport{base: base, registryHost: "index.docker.io"}
	request, _ = http.NewRequest(http.MethodGet, "https://auth.docker.io/token", nil)
	request.Header.Set("Authorization", "Basic dXNlcjpodW50ZXIy")
	if _, err := allowed.RoundTrip(request); err != nil {
		t.Fatalf("built-in Docker auth pair rejected: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("allowed auth host calls = %d", calls.Load())
	}
}

func TestCrossDomainRealmCannotReceiveFormCredentials(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "identity token", body: "grant_type=refresh_token&identity_token=identity-secret"},
		{name: "refresh token", body: "grant_type=refresh_token&refresh_token=refresh-secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			base := roundTripperFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			})
			transport := &domainBoundTransport{base: base, registryHost: "registry.example"}
			request, err := http.NewRequest(http.MethodPost, "https://auth.attacker.example/token", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			_, err = transport.RoundTrip(request)
			if err == nil {
				t.Fatal("cross-domain form credential request succeeded")
			}
			if calls.Load() != 0 {
				t.Fatal("cross-domain realm received a credential form")
			}
			for _, secret := range []string{"identity-secret", "refresh-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("credential leaked in error: %v", err)
				}
			}
		})
	}
}

func TestAllowedRealmReceivesRestoredFormBody(t *testing.T) {
	want := "grant_type=refresh_token&refresh_token=refresh-secret"
	base := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != want {
			t.Fatalf("body = %q, want %q", body, want)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})
	transport := &domainBoundTransport{base: base, registryHost: "registry.example"}
	request, err := http.NewRequest(http.MethodPost, "https://registry.example/token", strings.NewReader(want))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	if _, err := transport.RoundTrip(request); err != nil {
		t.Fatal(err)
	}
}

func TestPermissionRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/one:latest")
	installFetcher(t, map[string]v1.Image{ref.Name(): mustImage(t, 12)})
	if _, err := Assure(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := Inspect(context.Background(), path)
	blob, _ := blobPath(path, manifest.Images[0].Blobs[0].Digest)
	export, _ := cacheRelativePath(path, manifest.Images[0].Export)
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{{path, 0o755}, {filepath.Join(path, "index.json"), 0o644}, {blob, 0o644}, {export, 0o644}, {path + ".lock", 0o644}} {
		if err := os.Chmod(item.path, item.mode); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Inspect(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	assertMode(t, path, 0o700)
	assertMode(t, filepath.Join(path, "index.json"), 0o600)
	assertMode(t, blob, 0o600)
	assertMode(t, export, 0o600)
	assertMode(t, path+".lock", 0o600)
}

func TestRegistryWaitHonorsCancellation(t *testing.T) {
	ref := mustRef(t, "example.test/one:latest")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := fetchForPlatform(ctx, ref, "amd64", func(ctx context.Context, _ name.Reference, _ v1.Platform) (v1.Image, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("registry cancellation err=%v elapsed=%s", err, time.Since(started))
	}
}

func TestLockCancellationAndStableCrossProcessGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/one:latest")
	installFetcher(t, map[string]v1.Image{ref.Name(): mustImage(t, 13)})
	if _, err := Assure(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}
	before, _ := Inspect(context.Background(), path)
	ready := filepath.Join(t.TempDir(), "ready")
	release := filepath.Join(t.TempDir(), "release")
	cmd := exec.Command(os.Args[0], "-test.run=^TestImagecacheSubprocessWriter$")
	cmd.Env = append(os.Environ(),
		"SHUNT_IMAGECACHE_TEST_HELPER=writer",
		"SHUNT_IMAGECACHE_TEST_PATH="+path,
		"SHUNT_IMAGECACHE_TEST_READY="+ready,
		"SHUNT_IMAGECACHE_TEST_RELEASE="+release,
	)
	output := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := waitForPath(ready, 5*time.Second); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("writer did not acquire lock: %v\n%s", err, output.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	_, err := Inspect(ctx, path)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		_ = cmd.Process.Kill()
		t.Fatalf("lock wait error = %v", err)
	}

	result := make(chan Manifest, 1)
	errResult := make(chan error, 1)
	go func() {
		manifest, err := Inspect(context.Background(), path)
		if err != nil {
			errResult <- err
			return
		}
		result <- manifest
	}()
	select {
	case manifest := <-result:
		_ = cmd.Process.Kill()
		t.Fatalf("reader observed generation before writer committed: %s", manifest.Generation)
	case err := <-errResult:
		_ = cmd.Process.Kill()
		t.Fatal(err)
	case <-time.After(40 * time.Millisecond):
	}
	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	var after Manifest
	select {
	case after = <-result:
	case err := <-errResult:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not resume after writer commit")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("writer failed: %v\n%s", err, output.String())
	}
	if after.Generation == before.Generation || after.Images[0].Platform != "linux/test" {
		t.Fatalf("reader saw unstable generation: before=%s after=%#v", before.Generation, after)
	}
}

func TestImagecacheSubprocessWriter(t *testing.T) {
	if os.Getenv("SHUNT_IMAGECACHE_TEST_HELPER") != "writer" {
		return
	}
	path := os.Getenv("SHUNT_IMAGECACHE_TEST_PATH")
	lock, err := acquireStoreLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.close()
	manifest, err := readCurrentUnlocked(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Images[0].Platform = "linux/test"
	if err := os.WriteFile(os.Getenv("SHUNT_IMAGECACHE_TEST_READY"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForPath(os.Getenv("SHUNT_IMAGECACHE_TEST_RELEASE"), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := publishGeneration(path, manifest); err != nil {
		t.Fatal(err)
	}
}

func TestRedactDoesNotExposeCredentialValues(t *testing.T) {
	err := redact(errors.New(`unauthorized password=hunter2 token:abc123 Authorization: Bearer ey.secret https://user:pass@example.test/v2 {"identitytoken":"identity-secret","auth":"dXNlcjpwYXNz"}`))
	got := err.Error()
	for _, secret := range []string{"hunter2", "abc123", "ey.secret", "pass@", "identity-secret", "dXNlcjpwYXNz"} {
		if strings.Contains(got, secret) {
			t.Fatalf("credential %q leaked: %s", secret, got)
		}
	}
}

func TestValidateDetectsBlobCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/one:latest")
	installFetcher(t, map[string]v1.Image{ref.Name(): mustImage(t, 14)})
	if _, err := Assure(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := Inspect(context.Background(), path)
	blob, _ := blobPath(path, manifest.Images[0].Blobs[0].Digest)
	if err := os.WriteFile(blob, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Validate(path); err == nil {
		t.Fatal("corrupt blob validated")
	}
}

type fetchCount struct{ atomic.Int32 }

func installFetcher(t *testing.T, images map[string]v1.Image) *fetchCount {
	t.Helper()
	old := fetchImage
	t.Cleanup(func() { fetchImage = old })
	calls := &fetchCount{}
	fetchImage = func(_ context.Context, ref name.Reference) (fetchedImage, error) {
		calls.Add(1)
		img, ok := images[ref.Name()]
		if !ok {
			return fetchedImage{}, fmt.Errorf("no fixture for %s", ref.Name())
		}
		return fetchedImage{image: img, platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH}}, nil
	}
	return calls
}

func installContainerRunner(t *testing.T, runner func(context.Context, ...string) error) {
	t.Helper()
	old := runContainer
	t.Cleanup(func() { runContainer = old })
	runContainer = runner
}

func argumentValue(t *testing.T, args []string, name string) string {
	t.Helper()
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	t.Fatalf("argument %q missing from %#v", name, args)
	return ""
}

type fileSnapshot struct {
	data    string
	modTime time.Time
	size    int64
}

func statSnapshot(t *testing.T, path string) fileSnapshot {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fileSnapshot{data: string(data), modTime: info.ModTime(), size: info.Size()}
}

func assertSnapshotUnchanged(t *testing.T, path string, before fileSnapshot) {
	t.Helper()
	after := statSnapshot(t, path)
	if after != before {
		t.Fatalf("%s was rewritten", path)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}

func waitForPath(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func mustRef(t *testing.T, value string) name.Tag {
	t.Helper()
	ref, err := name.NewTag(value)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func mustImage(t *testing.T, seed int64) v1.Image {
	t.Helper()
	img, err := random.Image(1024+seed, 1)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

var _ authn.Keychain = inlineKeychain{}
