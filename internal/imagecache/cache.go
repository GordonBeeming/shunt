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

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
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
	return syncCache(ctx, path, registryRefs, local, false)
}

// RefreshSources resolves every registry tag and rebuilds every local source.
func RefreshSources(ctx context.Context, path string, registryRefs []string, local []LocalBuildSource) ([]Change, error) {
	return syncCache(ctx, path, registryRefs, local, true)
}

func syncCache(ctx context.Context, path string, refs []string, local []LocalBuildSource, refresh bool) ([]Change, error) {
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

	var changes []Change
	err = withStoreLock(ctx, path, true, func() error {
		current, err := readCurrentUnlocked(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if errors.Is(err, os.ErrNotExist) {
			current = Manifest{Version: storeVersion}
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

			fetched, err := fetchImage(ctx, want.ref)
			if err != nil {
				return fmt.Errorf("fetch %s: %w", want.text, redact(err))
			}
			digest, err := fetched.image.Digest()
			if err != nil {
				return fmt.Errorf("digest %s: %w", want.text, redact(err))
			}
			if usable && old.SourceKind == sourceRegistry && old.Digest == digest.String() {
				next = append(next, old)
				changes = append(changes, changeFromRecord(want.text, old, "unchanged", old.Digest))
				continue
			}

			record, err := storeImage(path, want.ref, fetched, sourceRegistry, "")
			if err != nil {
				return fmt.Errorf("cache %s: %w", want.text, redact(err))
			}
			next = append(next, record)
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
			changes = append(changes, changeFromRecord(want.text, record, action, previous))
		}

		for _, build := range builds {
			old, found := existing[build.ref.Name()]
			if found && localSourceCompatible(old, build.fingerprint) && !refresh && recordUsable(path, old) == nil {
				next = append(next, old)
				changes = append(changes, changeFromRecord(build.source.Ref, old, "unchanged", ""))
				continue
			}

			record, err := buildAndStoreLocalImage(ctx, path, build)
			if err != nil {
				return fmt.Errorf("build and cache local image %s: %w", build.source.Ref, redact(err))
			}
			next = append(next, record)
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
			changes = append(changes, changeFromRecord(build.source.Ref, record, action, previous))
		}

		sortImageRecords(next)
		sort.Slice(changes, func(i, j int) bool { return changes[i].Ref < changes[j].Ref })
		candidate := Manifest{Version: storeVersion, Images: next}
		if sameGenerationContent(current, candidate) {
			return nil
		}
		return publishGeneration(path, candidate)
	})
	if err != nil {
		return nil, err
	}
	return changes, nil
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
	registryCtx, cancel := context.WithTimeout(ctx, registryOperationTimeout)
	defer cancel()
	return fetchForPlatform(registryCtx, ref, runtime.GOARCH, pullImage)
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
