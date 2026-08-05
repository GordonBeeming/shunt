// Package imagecache maintains the daemon-free image cache shared by shunt
// sidings. The cache is a content-addressed directory; Docker-compatible
// archives are immutable derived exports, never the authoritative store.
package imagecache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	archive "github.com/google/go-containerregistry/pkg/v1/tarball"
	"golang.org/x/sys/unix"
)

// Change describes what a cache operation did for a configured reference.
type Change struct {
	Ref            string
	PreviousDigest string
	Digest         string
	Action         string // added, updated, unchanged
	Platform       string
	Fallback       bool
}

// ProgressEvent is a credential-safe, ordered status update for long cache
// work. It deliberately contains only configured image references and the
// selected platform, never registry URLs with credentials or bearer realms.
type ProgressEvent struct {
	Step     string
	Ref      string
	Platform string
	Fallback bool
}

// CommittedCleanupError means a new cache generation is current, but the
// automatic collection that followed it failed. Callers must not report the
// publication itself as failed or discard the returned changes.
type CommittedCleanupError struct {
	Err error
}

func (e *CommittedCleanupError) Error() string {
	return fmt.Sprintf("image cache updated, but automatic collection failed: %v", e.Err)
}

func (e *CommittedCleanupError) Unwrap() error { return e.Err }

type cachedImage struct {
	ref      name.Tag
	logical  string
	digest   string
	platform string
	fallback bool
	img      v1.Image
}

// Assure makes the current generation contain precisely refs. Existing
// references are reused without registry traffic; only missing refs are
// fetched.
func Assure(ctx context.Context, path string, refs []string) ([]Change, error) {
	return AssureSources(ctx, path, refs, nil)
}

// Refresh resolves every configured tag, downloads only changed images, and
// publishes a new generation only when the selected content changed.
func Refresh(ctx context.Context, path string, refs []string) ([]Change, error) {
	return RefreshSources(ctx, path, refs, nil)
}

// AssureSources makes the current generation contain precisely registryRefs
// and local. Existing usable local images are reused; missing or corrupt local
// images are built with Apple container without relying on a siding or a
// Docker-compatible host daemon.
func AssureSources(ctx context.Context, path string, registryRefs []string, local []LocalBuildSource) ([]Change, error) {
	return syncCache(ctx, path, registryRefs, local, false, nil)
}

// RefreshSources resolves every registry tag and rebuilds every local source.
func RefreshSources(ctx context.Context, path string, registryRefs []string, local []LocalBuildSource) ([]Change, error) {
	return syncCache(ctx, path, registryRefs, local, true, nil)
}

// RefreshSourcesProgress is RefreshSources with ordered, safe progress events.
func RefreshSourcesProgress(ctx context.Context, path string, registryRefs []string, local []LocalBuildSource, progress func(ProgressEvent)) ([]Change, error) {
	return syncCache(ctx, path, registryRefs, local, true, progress)
}

func syncCache(ctx context.Context, path string, refs []string, local []LocalBuildSource, refresh bool, progress func(ProgressEvent)) ([]Change, error) {
	wanted, err := parseRefs(refs)
	if err != nil {
		return nil, err
	}
	builds, err := parseLocalBuilds(local)
	if err != nil {
		return nil, err
	}
	if len(wanted) == 0 && len(builds) == 0 {
		return nil, fmt.Errorf("no images configured")
	}
	for _, build := range builds {
		for _, want := range wanted {
			if build.ref.Name() == want.ref.Name() {
				return nil, fmt.Errorf("image reference %q is configured as both registry and local", build.source.Ref)
			}
		}
	}
	var result []Change
	err = withCacheUpdateLock(ctx, path, func() error {
		result, err = syncCacheOnce(ctx, path, wanted, builds, refresh, progress)
		return err
	})
	return result, err
}

func syncCacheOnce(ctx context.Context, path string, wanted []wantedRef, builds []localBuild, refresh bool, progress func(ProgressEvent)) ([]Change, error) {
	snapshot, err := readCacheSnapshot(ctx, path)
	if err != nil {
		return nil, err
	}

	// Registry resolution is deliberately staged before the publication lock.
	// The lock protects only generation comparison and atomic publication, so a
	// slow registry cannot block guests that can still read the current cache.
	staged, err := stageRegistryFetches(ctx, path, wanted, snapshot.images, refresh, progress)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, stage := range staged {
			stage.close()
		}
	}()
	stagedBuilds, err := stageLocalBuilds(ctx, path, builds, snapshot.images, refresh, progress)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, stage := range stagedBuilds {
			stage.close()
		}
	}()

	var changes []Change
	published := false
	err = withCacheSweepLock(ctx, path, unix.LOCK_SH, func() error {
		return withStoreLock(ctx, path, true, func() error {
			current, err := readCurrentUnlocked(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if errors.Is(err, os.ErrNotExist) {
				current = Manifest{Version: storeVersion}
			}
			if current.Generation != snapshot.generation {
				return fmt.Errorf("image cache changed from generation %q to %q while images were staged; retry the cache operation", snapshot.generation, current.Generation)
			}

			existing := make(map[string]ImageRecord, len(current.Images))
			for _, image := range current.Images {
				existing[image.Ref] = image
			}

			changes = make([]Change, 0, len(wanted)+len(builds))
			next := make([]ImageRecord, 0, len(wanted)+len(builds))
			for _, want := range wanted {
				old, found := existing[want.ref.Name()]
				compatible := found && registrySourceCompatible(old)
				usable := compatible && recordUsable(path, old) == nil
				if usable && !refresh {
					next = append(next, old)
					changes = append(changes, changeFromRecord(want.text, old, "unchanged", ""))
					continue
				}

				stage, ok := staged[want.ref.Name()]
				if !ok {
					return fmt.Errorf("cache changed while %s was staged; retry the cache operation", want.text)
				}
				record := stage.record
				if usable && old.SourceKind == sourceRegistry && old.Digest == record.Digest {
					next = append(next, old)
					changes = append(changes, changeFromRecord(want.text, old, "unchanged", old.Digest))
					continue
				}

				record, change, err := adoptPreparedImage(path, want.text, "cache "+want.text, old, found, &stage, progress)
				if err != nil {
					return err
				}
				next = append(next, record)
				changes = append(changes, change)
			}

			for _, build := range builds {
				old, found := existing[build.ref.Name()]
				if found && localSourceCompatible(old, build.fingerprint) && !refresh && recordUsable(path, old) == nil {
					next = append(next, old)
					changes = append(changes, changeFromRecord(build.source.Ref, old, "unchanged", ""))
					continue
				}

				stage, ok := stagedBuilds[build.ref.Name()]
				if !ok {
					return fmt.Errorf("cache changed while local image %s was staged; retry the cache operation", build.source.Ref)
				}
				record, change, err := adoptPreparedImage(path, build.source.Ref, "build and cache local image "+build.source.Ref, old, found, &stage, progress)
				if err != nil {
					return err
				}
				next = append(next, record)
				changes = append(changes, change)
			}

			sortImageRecords(next)
			sort.Slice(changes, func(i, j int) bool { return changes[i].Ref < changes[j].Ref })
			candidate := Manifest{Version: storeVersion, Images: next}
			if sameGenerationContent(current, candidate) {
				return nil
			}
			if err := publishGeneration(path, candidate); err != nil {
				return err
			}
			published = true
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if published {
		_, err = Collect(ctx, path, GCOptions{Progress: func(line string) {
			if progress != nil {
				progress(ProgressEvent{Step: line})
			}
		}})
		if err != nil {
			return changes, &CommittedCleanupError{Err: err}
		}
	}
	return changes, nil
}

func adoptPreparedImage(path, displayRef, operation string, old ImageRecord, found bool, stage *stagedImageRecord, progress func(ProgressEvent)) (ImageRecord, Change, error) {
	if err := adoptStagedImage(path, stage); err != nil {
		return ImageRecord{}, Change{}, fmt.Errorf("%s: %w", operation, redact(err))
	}
	record := stage.record
	if progress != nil {
		progress(ProgressEvent{Step: "stored", Ref: displayRef, Platform: record.Platform, Fallback: record.Fallback})
	}
	action := "added"
	previous := ""
	if found {
		previous = old.Digest
		if sameImageSource(old, record) && old.Digest == record.Digest && old.ConfigDigest == record.ConfigDigest {
			action = "unchanged"
		} else {
			action = "updated"
		}
	}
	return record, changeFromRecord(displayRef, record, action, previous), nil
}

type cacheSnapshot struct {
	generation string
	images     map[string]ImageRecord
}

func readCacheSnapshot(ctx context.Context, path string) (cacheSnapshot, error) {
	snapshot := cacheSnapshot{images: map[string]ImageRecord{}}
	err := withStoreLock(ctx, path, false, func() error {
		manifest, err := readCurrentUnlocked(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		snapshot.generation = manifest.Generation
		for _, record := range manifest.Images {
			snapshot.images[record.Ref] = record
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return cacheSnapshot{}, err
	}
	return snapshot, nil
}

func stageLocalBuilds(ctx context.Context, path string, builds []localBuild, existing map[string]ImageRecord, refresh bool, progress func(ProgressEvent)) (map[string]stagedImageRecord, error) {
	staged := make(map[string]stagedImageRecord)
	for _, build := range builds {
		old, found := existing[build.ref.Name()]
		if !refresh && found && localSourceCompatible(old, build.fingerprint) && recordUsable(path, old) == nil {
			continue
		}
		if progress != nil {
			progress(ProgressEvent{Step: "building", Ref: build.source.Ref})
		}
		buildStage, stageErr := stageLocalBuild(ctx, path, build)
		if stageErr != nil {
			for _, completed := range staged {
				completed.close()
			}
			return nil, fmt.Errorf("build local image %s: %w", build.source.Ref, redact(stageErr))
		}
		objectStage, objectErr := stageImage(path, build.ref, fetchedImage{image: buildStage.image, platform: build.platform}, sourceLocal, build.fingerprint)
		buildStage.close()
		if objectErr != nil {
			for _, completed := range staged {
				completed.close()
			}
			return nil, fmt.Errorf("stage local image %s: %w", build.source.Ref, redact(objectErr))
		}
		staged[build.ref.Name()] = objectStage
	}
	return staged, nil
}

const registryFetchParallelism = 4

type stagedRegistryImage struct {
	fetchedImage
	cleanup func()
}

func (stage stagedRegistryImage) close() {
	if stage.cleanup != nil {
		stage.cleanup()
	}
}

func stageRegistryFetches(ctx context.Context, path string, wanted []wantedRef, existing map[string]ImageRecord, refresh bool, progress func(ProgressEvent)) (map[string]stagedImageRecord, error) {
	pending := make([]wantedRef, 0, len(wanted))
	for _, want := range wanted {
		record, found := existing[want.ref.Name()]
		if !refresh && found && registrySourceCompatible(record) && recordUsable(path, record) == nil {
			continue
		}
		pending = append(pending, want)
	}
	result := make(map[string]stagedImageRecord, len(pending))
	if len(pending) == 0 {
		return result, nil
	}

	jobs := make(chan wantedRef)
	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	workers := registryFetchParallelism
	if workers > len(pending) {
		workers = len(pending)
	}
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for want := range jobs {
				mu.Lock()
				cancelled := firstErr != nil
				mu.Unlock()
				if cancelled {
					continue
				}
				registryCtx, registryCancel := context.WithTimeout(fetchCtx, registryOperationTimeout)
				fetched, fetchErr := fetchImage(registryCtx, want.ref)
				if fetchErr != nil {
					registryCancel()
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("fetch %s: %w", want.text, redact(fetchErr))
						cancel()
					}
					mu.Unlock()
					continue
				}
				materialized, stageErr := materializeRegistryImage(path, want.ref, fetched)
				registryCancel()
				if stageErr != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("stage %s: %w", want.text, redact(stageErr))
						cancel()
					}
					mu.Unlock()
					continue
				}
				stage, stageErr := stageImage(path, want.ref, materialized.fetchedImage, sourceRegistry, "")
				materialized.close()
				if stageErr != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("prepare %s: %w", want.text, redact(stageErr))
						cancel()
					}
					mu.Unlock()
					continue
				}
				mu.Lock()
				result[want.ref.Name()] = stage
				mu.Unlock()
			}
		}()
	}
	for _, want := range pending {
		if err := ctx.Err(); err != nil {
			break
		}
		jobs <- want
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		for _, stage := range result {
			stage.close()
		}
		return nil, err
	}
	if firstErr != nil {
		for _, stage := range result {
			stage.close()
		}
		return nil, firstErr
	}
	// Workers finish in arbitrary order. Emit only after they all succeed so
	// command output stays in the declared, deterministic reference order.
	if progress != nil {
		for _, want := range pending {
			record := result[want.ref.Name()].record
			progress(ProgressEvent{Step: "downloaded", Ref: want.text, Platform: record.Platform, Fallback: record.Fallback})
		}
	}
	return result, nil
}

// materializeRegistryImage forces every image byte through a private archive
// before the publication lock. remote.Image is lazy, so merely resolving it is
// insufficient: archive.Write is the deliberate host-side download boundary.
func materializeRegistryImage(cachePath string, ref name.Tag, fetched fetchedImage) (stagedRegistryImage, error) {
	tmp, err := newCaptureTemp(cachePath)
	if err != nil {
		return stagedRegistryImage{}, err
	}
	tmpPath := tmp.Name()
	if err := archive.Write(ref, fetched.image, tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return stagedRegistryImage{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return stagedRegistryImage{}, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return stagedRegistryImage{}, err
	}
	image, err := archive.ImageFromPath(tmpPath, &ref)
	if err != nil {
		_ = os.Remove(tmpPath)
		return stagedRegistryImage{}, err
	}
	fetched.image = image
	return stagedRegistryImage{fetchedImage: fetched, cleanup: func() { _ = os.Remove(tmpPath) }}, nil
}

func registrySourceCompatible(record ImageRecord) bool {
	return (record.SourceKind == sourceRegistry || record.SourceKind == sourceCapture) && record.SourceFingerprint == ""
}

func localSourceCompatible(record ImageRecord, fingerprint string) bool {
	return record.SourceKind == sourceLocal && record.SourceFingerprint == fingerprint
}

func sameImageSource(left, right ImageRecord) bool {
	return left.SourceKind == right.SourceKind && left.SourceFingerprint == right.SourceFingerprint
}

func changeFromRecord(ref string, record ImageRecord, action, previous string) Change {
	return Change{
		Ref:            ref,
		PreviousDigest: previous,
		Digest:         record.Digest,
		Action:         action,
		Platform:       record.Platform,
		Fallback:       record.Fallback,
	}
}

type wantedRef struct {
	text string
	ref  name.Tag
}

func parseRefs(refs []string) ([]wantedRef, error) {
	seen := map[string]bool{}
	wanted := make([]wantedRef, 0, len(refs))
	for _, text := range refs {
		parsed, err := name.ParseReference(text)
		if err != nil {
			return nil, fmt.Errorf("invalid image reference: %w", redact(err))
		}
		tag, ok := parsed.(name.Tag)
		if !ok {
			return nil, fmt.Errorf("image reference %q must use a tag", text)
		}
		if seen[tag.Name()] {
			continue
		}
		seen[tag.Name()] = true
		wanted = append(wanted, wantedRef{text: text, ref: tag})
	}
	sort.Slice(wanted, func(i, j int) bool { return wanted[i].ref.Name() < wanted[j].ref.Name() })
	return wanted, nil
}

type fetchedImage struct {
	image    v1.Image
	platform v1.Platform
	fallback bool
}

func fetch(ctx context.Context, ref name.Reference) (fetchedImage, error) {
	return fetchForPlatform(ctx, ref, runtime.GOARCH, pullImage)
}

type pull func(context.Context, name.Reference, v1.Platform) (v1.Image, error)

func fetchForPlatform(ctx context.Context, ref name.Reference, arch string, get pull) (fetchedImage, error) {
	platform := v1.Platform{OS: "linux", Architecture: arch}
	img, err := get(ctx, ref, platform)
	if err == nil {
		return fetchedImage{image: img, platform: platform}, nil
	}
	if arch != "arm64" || !missingPlatform(err) {
		return fetchedImage{}, err
	}
	fallback := v1.Platform{OS: "linux", Architecture: "amd64"}
	img, err = get(ctx, ref, fallback)
	if err != nil {
		return fetchedImage{}, err
	}
	return fetchedImage{image: img, platform: fallback, fallback: true}, nil
}

// fetchImage is replaceable by focused tests; production always uses fetch.
var fetchImage = fetch

func missingPlatform(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no child with platform")
}

func platformString(platform v1.Platform) string {
	result := platform.OS + "/" + platform.Architecture
	if platform.Variant != "" {
		result += "/" + platform.Variant
	}
	return result
}
