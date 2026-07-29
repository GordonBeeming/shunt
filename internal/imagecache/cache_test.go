package imagecache

import (
	stdtar "archive/tar"
	"context"
	"encoding/json"
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
	observed := "sha256:" + strings.Repeat("1", 64)
	loadedMarker := GuestMarker{Generation: full.Generation, Images: map[string]string{one.Name(): observed}, Digests: full.Marker.Digests}
	noOp, err := Plan(context.Background(), path, GuestState{Marker: loadedMarker, ImageIDs: map[string]string{one.Name(): observed}})
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
	marker.Images[one.Name()] = "sha256:" + strings.Repeat("3", 64)
	marker.Images[two.Name()] = "sha256:" + strings.Repeat("4", 64)
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

func TestCollectPreservesLeasedGenerationThenReclaimsIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/gc:latest")
	images := map[string]v1.Image{ref.Name(): mustImage(t, 75)}
	installFetcher(t, images)
	if _, err := Assure(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}
	first, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := Plan(context.Background(), path, GuestState{})
	if err != nil {
		t.Fatal(err)
	}
	images[ref.Name()] = mustImage(t, 76)
	if _, err := Refresh(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}
	images[ref.Name()] = mustImage(t, 77)
	if _, err := Refresh(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}
	images[ref.Name()] = mustImage(t, 78)
	if _, err := Refresh(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}

	result, err := Collect(context.Background(), path, GCOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(generationPath(path, first.Generation)); err != nil {
		t.Fatalf("leased generation was removed: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	result, err = Collect(context.Background(), path, GCOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReclaimedBytes == 0 && len(result.Removed) == 0 {
		t.Fatal("collection did not reclaim stale cache content")
	}
	if _, err := os.Stat(generationPath(path, first.Generation)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released stale generation still exists: %v", err)
	}
	leasePath, err := generationLeasePath(path, first.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released stale generation lease still exists: %v", err)
	}
}

func TestCollectSweepDoesNotBlockCurrentGenerationReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/nonblocking-gc:latest")
	images := map[string]v1.Image{ref.Name(): mustImage(t, 100)}
	installFetcher(t, images)
	if _, err := Assure(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}
	lease, err := Plan(context.Background(), path, GuestState{})
	if err != nil {
		t.Fatal(err)
	}
	for seed := int64(101); seed <= 102; seed++ {
		images[ref.Name()] = mustImage(t, seed)
		if _, err := Refresh(context.Background(), path, []string{ref.Name()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	collectDone := make(chan error, 1)
	var once atomic.Bool
	go func() {
		_, err := Collect(context.Background(), path, GCOptions{Progress: func(string) {
			if once.CompareAndSwap(false, true) {
				close(started)
				<-release
			}
		}})
		collectDone <- err
	}()
	<-started
	readDone := make(chan error, 1)
	go func() {
		_, err := Inspect(context.Background(), path)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Inspect blocked behind the cache sweep")
	}
	close(release)
	if err := <-collectDone; err != nil {
		t.Fatal(err)
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
	observed := "sha256:" + strings.Repeat("2", 64)
	marker := markerFor(manifest)
	marker.Images[ref.Name()] = observed
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
		Marker:   marker,
		ImageIDs: map[string]string{ref.Name(): observed},
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
			stageRef, err := name.NewTag(args[len(args)-1])
			if err != nil {
				return err
			}
			return archive.Write(stageRef, img, file)
		}
		return nil
	})
	changes, err := AssureSources(context.Background(), path, nil, []LocalBuildSource{source})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Action != "added" || len(calls) != 3 {
		t.Fatalf("first local assure calls=%#v changes=%#v", calls, changes)
	}
	wantBuild := []string{"build", "--platform", "linux/arm64", "--build-arg", "ALPHA=first", "--build-arg", "ZED=last", "-t", calls[0][8], "-f", dockerfile, contextDir}
	if !strings.Contains(calls[0][8], "shunt-stage/") || strings.Join(calls[0], "\x00") != strings.Join(wantBuild, "\x00") {
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
	if changes[0].Action != "updated" || len(calls) != 3 {
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
	if changes[0].Action != "unchanged" || len(calls) != 0 {
		t.Fatalf("assure unexpectedly rebuilt before the load boundary: calls=%#v changes=%#v", calls, changes)
	}
	plan, err := Plan(context.Background(), path, GuestState{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Release()
	if len(plan.Images) != 1 {
		t.Fatalf("planned images = %d, want 1", len(plan.Images))
	}
	actualChecksum, err := fileDigest(plan.Images[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	if actualChecksum == plan.Images[0].Checksum {
		t.Fatal("corrupt export retained its planned checksum")
	}

	calls = nil
	changes, err = RefreshSources(context.Background(), path, nil, []LocalBuildSource{source})
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Action != "unchanged" || len(calls) != 3 {
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
			stageRef, err := name.NewTag(args[len(args)-1])
			if err != nil {
				return err
			}
			return archive.Write(stageRef, localImage, file)
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
	if containerCalls.Load() != 3 || changes[0].Action != "updated" || manifest.Images[0].SourceKind != sourceLocal {
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

func TestOlderStoreVersionsAreRejected(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("version-%d", version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "images")
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			legacy := fmt.Sprintf(`{"version":%d,"generation":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, version)
			if err := os.WriteFile(filepath.Join(path, "index.json"), []byte(legacy), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Inspect(context.Background(), path); err == nil || !strings.Contains(err.Error(), "unsupported image cache index") {
				t.Fatalf("legacy store error = %v", err)
			}
		})
	}
}

func TestStagedImageAdoptionHardLinksImmutableObjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/hard-link:latest")
	stage, err := stageImage(path, ref, fetchedImage{
		image:    mustImage(t, 81),
		platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH},
	}, sourceRegistry, "")
	if err != nil {
		t.Fatal(err)
	}
	stagedExport, err := cacheRelativePath(stage.root, stage.record.Export)
	if err != nil {
		t.Fatal(err)
	}
	stagedInfo, err := os.Stat(stagedExport)
	if err != nil {
		t.Fatal(err)
	}
	if err := withStoreLock(context.Background(), path, true, func() error {
		return adoptStagedImage(path, &stage)
	}); err != nil {
		t.Fatal(err)
	}
	liveExport, err := cacheRelativePath(path, stage.record.Export)
	if err != nil {
		t.Fatal(err)
	}
	liveInfo, err := os.Stat(liveExport)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(stagedInfo, liveInfo) {
		t.Fatal("adoption copied the immutable export instead of hard-linking it")
	}
	stage.close()
	if _, err := os.Stat(liveExport); err != nil {
		t.Fatalf("live export did not survive private-stage cleanup: %v", err)
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

func TestCaptureAutomaticallyCollectsSupersededGenerations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/capture-gc:latest")
	var first string
	for seed := int64(93); seed <= 95; seed++ {
		img := mustImage(t, seed)
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
		manifest, err := Inspect(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		if first == "" {
			first = manifest.Generation
		}
	}
	if _, err := os.Stat(generationPath(path, first)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("superseded captured generation still exists: %v", err)
	}
}

func TestRefreshReturnsCommittedCleanupErrorWithPublishedChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/committed-cleanup:latest")
	images := map[string]v1.Image{ref.Name(): mustImage(t, 96)}
	installFetcher(t, images)
	if _, err := Assure(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}
	before, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	images[ref.Name()] = mustImage(t, 97)
	ctx, cancel := context.WithCancel(context.Background())
	changes, err := RefreshSourcesProgress(ctx, path, []string{ref.Name()}, nil, func(event ProgressEvent) {
		if event.Step == "stored" {
			cancel()
		}
	})
	var cleanupErr *CommittedCleanupError
	if !errors.As(err, &cleanupErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("RefreshSourcesProgress() error = %v", err)
	}
	if len(changes) != 1 || changes[0].Action != "updated" {
		t.Fatalf("published changes = %#v", changes)
	}
	after, inspectErr := Inspect(context.Background(), path)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if after.Generation == before.Generation {
		t.Fatal("generation was not published before cleanup warning")
	}
}

func TestCaptureRejectsHostileManifestReferenceWithoutReplacingGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/capture:latest")
	installFetcher(t, map[string]v1.Image{ref.Name(): mustImage(t, 80)})
	if _, err := Assure(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}
	before, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	err = Capture(path, func(temp string) error {
		file, err := os.OpenFile(temp, os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		writer := stdtar.NewWriter(file)
		defer writer.Close()
		manifest, err := json.Marshal([]archive.Descriptor{{Config: "../host-file", RepoTags: []string{"example.test/hostile:latest"}, Layers: []string{"layer.tar"}}})
		if err != nil {
			return err
		}
		if err := writer.WriteHeader(&stdtar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(manifest))}); err != nil {
			return err
		}
		_, err = writer.Write(manifest)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe member reference") {
		t.Fatalf("hostile capture error = %v", err)
	}
	after, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation {
		t.Fatalf("hostile capture changed generation: %s -> %s", before.Generation, after.Generation)
	}
}

func TestCaptureRejectsArchiveAboveConfiguredCacheLimit(t *testing.T) {
	t.Setenv(cacheMaxBytesEnv, "1024")
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/oversized:latest")
	err := Capture(path, func(temp string) error {
		file, err := os.OpenFile(temp, os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		return archive.Write(ref, mustImage(t, 98), file)
	})
	if err == nil || !strings.Contains(err.Error(), "above the 1024-byte cache limit") {
		t.Fatalf("Capture() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "index.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized capture published cache state: %v", err)
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

func TestRegistryFetchContextLivesThroughLazyMaterialization(t *testing.T) {
	server := httptest.NewServer(registry.New())
	defer server.Close()
	registryRef, err := name.NewTag(strings.TrimPrefix(server.URL, "http://")+"/public/lazy:latest", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	image := mustImage(t, 12)
	if err := remote.Write(registryRef, image, remote.WithTransport(http.DefaultTransport)); err != nil {
		t.Fatal(err)
	}

	oldFetch := fetchImage
	t.Cleanup(func() { fetchImage = oldFetch })
	fetchImage = func(ctx context.Context, _ name.Reference) (fetchedImage, error) {
		lazyImage, err := remote.Image(
			registryRef,
			remote.WithContext(ctx),
			remote.WithTransport(http.DefaultTransport),
		)
		return fetchedImage{image: lazyImage, platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH}}, err
	}

	path := filepath.Join(t.TempDir(), "images")
	changes, err := Assure(context.Background(), path, []string{"example.test/public/lazy:latest"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Action != "added" {
		t.Fatalf("Assure() changes = %#v", changes)
	}
	manifest, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Images) != 1 || manifest.Images[0].Digest == "" {
		t.Fatalf("materialized manifest = %#v", manifest)
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

func TestPermissionRepairIsExplicit(t *testing.T) {
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
	assertMode(t, filepath.Join(path, "index.json"), 0o644)
	if err := RepairPermissions(path); err != nil {
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

func TestRegistryAndLocalBuildStagingDoNotHoldPublicationLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/staged-registry:latest")
	img := mustImage(t, 79)
	oldFetch := fetchImage
	t.Cleanup(func() { fetchImage = oldFetch })
	fetchImage = func(_ context.Context, _ name.Reference) (fetchedImage, error) {
		assertPublicationLockAvailable(t, path)
		return fetchedImage{image: img, platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH}}, nil
	}
	if _, err := Assure(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}

	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	localRef := mustRef(t, "example.test/staged-local:latest")
	installContainerRunner(t, func(_ context.Context, args ...string) error {
		assertPublicationLockAvailable(t, path)
		if len(args) > 1 && args[0] == "image" && args[1] == "save" {
			output := argumentValue(t, args, "-o")
			file, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			defer file.Close()
			stageRef, err := name.NewTag(args[len(args)-1])
			if err != nil {
				return err
			}
			return archive.Write(stageRef, img, file)
		}
		return nil
	})
	if _, err := AssureSources(context.Background(), path, nil, []LocalBuildSource{{Ref: localRef.Name(), ContextDir: contextDir}}); err != nil {
		t.Fatal(err)
	}
}

func TestLocalBuildFailureCleansCompletedRegistryStages(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "images")
	registryRef := mustRef(t, "example.test/staged-registry:latest")
	localRef := mustRef(t, "example.test/staged-local:latest")
	installFetcher(t, map[string]v1.Image{registryRef.Name(): mustImage(t, 91)})
	contextDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	installContainerRunner(t, func(context.Context, ...string) error {
		return errors.New("injected local build failure")
	})

	_, err := AssureSources(context.Background(), path, []string{registryRef.Name()}, []LocalBuildSource{{Ref: localRef.Name(), ContextDir: contextDir}})
	if err == nil || !strings.Contains(err.Error(), "injected local build failure") {
		t.Fatalf("AssureSources() error = %v", err)
	}
	assertNoImageObjectStages(t, parent)
}

func TestRegistryCancellationCleansCompletedStages(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "images")
	fast := mustRef(t, "example.test/fast:latest")
	slow := mustRef(t, "example.test/slow:latest")
	img := mustImage(t, 92)
	oldFetch := fetchImage
	t.Cleanup(func() { fetchImage = oldFetch })
	fetchImage = func(ctx context.Context, ref name.Reference) (fetchedImage, error) {
		if ref.Name() == slow.Name() {
			<-ctx.Done()
			return fetchedImage{}, ctx.Err()
		}
		return fetchedImage{image: img, platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Assure(ctx, path, []string{fast.Name(), slow.Name()})
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		matches, err := filepath.Glob(filepath.Join(parent, ".shunt-image-object-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("registry stage was not created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Assure() error = %v", err)
	}
	assertNoImageObjectStages(t, parent)
}

func assertNoImageObjectStages(t *testing.T, parent string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(parent, ".shunt-image-object-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("leaked image stages: %v", matches)
	}
}

func TestRefreshCompareAndSwapDoesNotOverwriteConcurrentGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/concurrent-refresh:latest")
	img := mustImage(t, 82)
	installFetcher(t, map[string]v1.Image{ref.Name(): img})
	if _, err := Assure(context.Background(), path, []string{ref.Name()}); err != nil {
		t.Fatal(err)
	}

	oldFetch := fetchImage
	t.Cleanup(func() { fetchImage = oldFetch })
	fetchImage = func(_ context.Context, _ name.Reference) (fetchedImage, error) {
		if err := withStoreLock(context.Background(), path, false, func() error {
			manifest, err := readCurrentUnlocked(path)
			if err != nil {
				return err
			}
			manifest.Images[0].Platform = "linux/concurrent"
			return publishGeneration(path, manifest)
		}); err != nil {
			return fetchedImage{}, err
		}
		return fetchedImage{image: img, platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH}}, nil
	}
	if _, err := Refresh(context.Background(), path, []string{ref.Name()}); err == nil || !strings.Contains(err.Error(), "while images were staged") {
		t.Fatalf("concurrent refresh error = %v", err)
	}
	manifest, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Images[0].Platform != "linux/concurrent" {
		t.Fatalf("slow refresh overwrote concurrent generation: %#v", manifest.Images[0])
	}
}

func TestConcurrentColdAssureStagesSourcesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images")
	ref := mustRef(t, "example.test/single-stage:latest")
	img := mustImage(t, 99)
	oldFetch := fetchImage
	t.Cleanup(func() { fetchImage = oldFetch })
	var fetches atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	fetchImage = func(context.Context, name.Reference) (fetchedImage, error) {
		if fetches.Add(1) == 1 {
			close(started)
			<-release
		}
		return fetchedImage{image: img, platform: v1.Platform{OS: "linux", Architecture: runtime.GOARCH}}, nil
	}
	errorsByCall := make(chan error, 2)
	go func() {
		_, err := Assure(context.Background(), path, []string{ref.Name()})
		errorsByCall <- err
	}()
	<-started
	go func() {
		_, err := Assure(context.Background(), path, []string{ref.Name()})
		errorsByCall <- err
	}()
	close(release)
	for range 2 {
		if err := <-errorsByCall; err != nil {
			t.Fatal(err)
		}
	}
	if fetches.Load() != 1 {
		t.Fatalf("registry fetches = %d, want 1", fetches.Load())
	}
}

func assertPublicationLockAvailable(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	lock, err := acquireStoreLock(ctx, path)
	if err != nil {
		t.Fatalf("staging ran while image-cache publication lock was held: %v", err)
	}
	if err := lock.close(); err != nil {
		t.Fatal(err)
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
	err := redact(errors.New(`unauthorized password=hunter2 token:abc123 Authorization: Bearer ey.secret client_secret=form-secret client-secret=dash-secret clientsecret=plain-secret https://user:pass@example.test/v2 {"identitytoken":"identity-secret","auth":"dXNlcjpwYXNz","client_secret":"json-secret"}`))
	got := err.Error()
	for _, secret := range []string{"hunter2", "abc123", "ey.secret", "form-secret", "dash-secret", "plain-secret", "json-secret", "pass@", "identity-secret", "dXNlcjpwYXNz"} {
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

func BenchmarkIndexDockerArchiveMultiImage(b *testing.B) {
	parent := b.TempDir()
	source := filepath.Join(parent, "multi-image.tar")
	images := make(map[name.Tag]v1.Image, 8)
	for index := 0; index < 8; index++ {
		ref, err := name.NewTag(fmt.Sprintf("example.test/benchmark/image-%d:latest", index))
		if err != nil {
			b.Fatal(err)
		}
		img, err := random.Image(256*1024+int64(index), 3)
		if err != nil {
			b.Fatal(err)
		}
		images[ref] = img
	}
	if err := archive.MultiWriteToFile(source, images); err != nil {
		b.Fatal(err)
	}
	info, err := os.Stat(source)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(info.Size())
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		indexed, err := indexDockerArchive(context.Background(), filepath.Join(parent, "images"), source)
		if err != nil {
			b.Fatal(err)
		}
		indexed.cleanup()
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
