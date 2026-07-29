// Package imagecache maintains the daemon-free Docker image archive shared by
// shunt sidings.  It deliberately uses registry APIs rather than a host Docker
// daemon: Docker is required only inside the isolated guests that load it.
package imagecache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/legacy/tarball"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	archive "github.com/google/go-containerregistry/pkg/v1/tarball"
)

// Change describes what a cache operation did for a configured reference.
type Change struct {
	Ref            string
	PreviousDigest string
	Digest         string
	Action         string // added, updated, unchanged
}

type cachedImage struct {
	ref     name.Tag // Docker archives can retain tags, not repo@digest names.
	logical string   // normalized configured reference, including a digest pin.
	img     v1.Image
}

// Assure makes the cache contain precisely refs. Existing exact references are
// reused without contacting a registry; only missing references are fetched.
func Assure(ctx context.Context, path string, refs []string) ([]Change, error) {
	return sync(ctx, path, refs, false)
}

// Refresh resolves every configured reference, including mutable tags, and
// atomically replaces the archive with exactly the current configured set.
func Refresh(ctx context.Context, path string, refs []string) ([]Change, error) {
	return sync(ctx, path, refs, true)
}

func sync(ctx context.Context, path string, refs []string, refresh bool) ([]Change, error) {
	wanted, err := parseRefs(refs)
	if err != nil {
		return nil, err
	}
	existing, err := Read(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	changes := make([]Change, 0, len(wanted))
	images := make(map[string]cachedImage, len(wanted))
	rewrite := refresh || len(existing) != len(wanted)
	for _, want := range wanted {
		old, found := existing[want.ref.Name()]
		if found && !refresh {
			images[want.ref.Name()] = old
			changes = append(changes, Change{Ref: want.text, Digest: digest(old.img), Action: "unchanged"})
			continue
		}
		img, err := fetchImage(ctx, want.ref)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", want.text, redact(err))
		}
		newDigest := digest(img)
		change := Change{Ref: want.text, Digest: newDigest, Action: "added"}
		if found {
			change.PreviousDigest = digest(old.img)
			if change.PreviousDigest == newDigest {
				change.Action = "unchanged"
			} else {
				change.Action = "updated"
			}
		}
		if change.Action != "unchanged" {
			rewrite = true
		}
		if digestRef, ok := want.ref.(name.Digest); ok && digestRef.DigestStr() != newDigest {
			return nil, fmt.Errorf("fetch %s: registry returned a different digest", want.text)
		}
		archiveRef, err := archiveRef(want.ref)
		if err != nil {
			return nil, err
		}
		images[want.ref.Name()] = cachedImage{ref: archiveRef, logical: want.ref.Name(), img: img}
		changes = append(changes, change)
	}
	if !rewrite {
		return changes, nil
	}
	if err := write(path, images); err != nil {
		return nil, err
	}
	return changes, nil
}

type wantedRef struct {
	text string
	ref  name.Reference
}

func parseRefs(refs []string) ([]wantedRef, error) {
	if len(refs) == 0 {
		return nil, fmt.Errorf("no images configured")
	}
	seen := map[string]bool{}
	wanted := make([]wantedRef, 0, len(refs))
	for _, text := range refs {
		ref, err := name.ParseReference(text)
		if err != nil {
			return nil, fmt.Errorf("invalid image reference: %w", redact(err))
		}
		if seen[ref.Name()] {
			continue
		}
		seen[ref.Name()] = true
		wanted = append(wanted, wantedRef{text: text, ref: ref})
	}
	sort.Slice(wanted, func(i, j int) bool { return wanted[i].ref.Name() < wanted[j].ref.Name() })
	return wanted, nil
}

func fetch(ctx context.Context, ref name.Reference) (v1.Image, error) {
	return fetchForPlatform(ctx, ref, runtime.GOARCH, pullImage)
}

type pull func(context.Context, name.Reference, v1.Platform) (v1.Image, error)

func fetchForPlatform(ctx context.Context, ref name.Reference, arch string, get pull) (v1.Image, error) {
	platform := v1.Platform{OS: "linux", Architecture: arch}
	img, err := get(ctx, ref, platform)
	if err == nil || arch != "arm64" || !missingPlatform(err) {
		return img, err
	}
	return get(ctx, ref, v1.Platform{OS: "linux", Architecture: "amd64"})
}

func pullImage(ctx context.Context, ref name.Reference, platform v1.Platform) (v1.Image, error) {
	return remote.Image(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithPlatform(platform))
}

// fetchImage is replaceable by focused tests; production always uses fetch.
var fetchImage = fetch

func missingPlatform(err error) bool { return strings.Contains(err.Error(), "no child with platform") }

const digestTagPrefix = "shunt-digest-"

// archiveRef returns the archive-visible tag for ref. Docker's save/load format
// has only RepoTags, so a digest pin is represented by a deterministic private
// tag and recovered to repo@digest while validating the archive. The Docker
// archive format does not retain a registry manifest digest, so the tag records
// the pin that was verified while fetching rather than the digest reconstructed
// from its legacy docker-save representation.
func archiveRef(ref name.Reference) (name.Tag, error) {
	if tag, ok := ref.(name.Tag); ok {
		return tag, nil
	}
	digestRef := ref.(name.Digest)
	if !strings.HasPrefix(digestRef.DigestStr(), "sha256:") {
		return name.Tag{}, fmt.Errorf("image digest must use sha256")
	}
	return name.NewTag(ref.Context().Name() + ":" + digestTagPrefix + strings.TrimPrefix(digestRef.DigestStr(), "sha256:"))
}

// Read validates a Docker archive and returns its images keyed by normalized
// configured reference. Pinned refs are recovered from their private archive
// tag only when that tag's digest matches the loaded image exactly.
func Read(path string) (map[string]cachedImage, error) {
	manifest, err := archive.LoadManifest(func() (io.ReadCloser, error) { return os.Open(path) })
	if err != nil {
		return nil, fmt.Errorf("read image cache %s: %w", path, err)
	}
	result := make(map[string]cachedImage)
	for _, entry := range manifest {
		if len(entry.RepoTags) == 0 {
			return nil, fmt.Errorf("read image cache %s: untagged image", path)
		}
		for _, raw := range entry.RepoTags {
			tag, err := name.NewTag(raw)
			if err != nil {
				return nil, fmt.Errorf("read image cache %s: invalid tag: %w", path, redact(err))
			}
			img, err := archive.ImageFromPath(path, &tag)
			if err != nil {
				return nil, fmt.Errorf("read image cache %s: %w", path, err)
			}
			if _, err := img.Digest(); err != nil {
				return nil, fmt.Errorf("read image cache %s: %w", path, err)
			}
			logical := tag.Name()
			if strings.HasPrefix(tag.TagStr(), digestTagPrefix) {
				pinned := "sha256:" + strings.TrimPrefix(tag.TagStr(), digestTagPrefix)
				if _, err := v1.NewHash(pinned); err != nil {
					return nil, fmt.Errorf("read image cache %s: invalid digest tag", path)
				}
				logical = tag.Context().Digest(pinned).Name()
			}
			result[logical] = cachedImage{ref: tag, logical: logical, img: img}
		}
	}
	return result, nil
}

// Validate checks that path is a readable, tagged Docker archive. It is useful
// before streaming a cache into a guest Docker daemon.
func Validate(path string) error {
	_, err := Read(path)
	return err
}

func write(path string, images map[string]cachedImage) error {
	return Atomic(path, func(f *os.File) error {
		refs := make(map[name.Reference]v1.Image, len(images))
		for _, image := range images {
			refs[image.ref] = image.img
		}
		return tarball.MultiWrite(refs, f)
	})
}

// Atomic writes a validated Docker archive to a sibling temporary file, then
// renames it into place. The existing cache remains untouched on every error.
func Atomic(path string, produce func(*os.File) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".images-*.tar")
	if err != nil {
		return fmt.Errorf("create image cache temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := produce(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close image cache temp: %w", err)
	}
	if _, err := Read(tmpName); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod image cache: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace image cache: %w", err)
	}
	return nil
}

// Capture atomically publishes a Docker-compatible archive produced by a guest.
func Capture(path string, produce func(tempPath string) error) error {
	return Atomic(path, func(f *os.File) error {
		return produce(f.Name())
	})
}

func digest(img v1.Image) string {
	d, err := img.Digest()
	if err != nil {
		return ""
	}
	return d.String()
}

// redact keeps credentials embedded in registry URLs or auth errors out of UI.
func redact(err error) error {
	s := err.Error()
	if at := strings.Index(s, "@"); at >= 0 && strings.Contains(s[:at], ":") {
		s = s[:strings.LastIndex(s[:at], "/")+1] + "…@" + s[at+1:]
	}
	s = secretValue.ReplaceAllString(s, "$1$2…")
	return fmt.Errorf("%s", s)
}

var secretValue = regexp.MustCompile(`(?i)(password|token|authorization|secret)(\s*[=:]\s*)[^\s,;]+`)
