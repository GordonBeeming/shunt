package imagecache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	archive "github.com/google/go-containerregistry/pkg/v1/tarball"
)

// Atomic imports a Docker archive produced through f into a new cache
// generation. The current generation is untouched if production, validation,
// or import fails.
func Atomic(path string, produce func(*os.File) error) error {
	tmp, err := newCaptureTemp(path)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := produce(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return importArchive(context.Background(), path, tmpPath)
}

// Capture atomically imports a Docker-compatible archive produced by a guest.
func Capture(path string, produce func(tempPath string) error) error {
	return CaptureContext(context.Background(), path, produce)
}

// CaptureContext is Capture with cancellable lock acquisition and import.
func CaptureContext(ctx context.Context, path string, produce func(tempPath string) error) error {
	tmp, err := newCaptureTemp(path)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		return err
	}
	defer os.Remove(tmpPath)
	if err := produce(tmpPath); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("chmod captured image archive: %w", err)
	}
	return importArchive(ctx, path, tmpPath)
}

func newCaptureTemp(path string) (*os.File, error) {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(parent, ".shunt-images-capture-*.tar")
	if err != nil {
		return nil, fmt.Errorf("create image capture temp: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, err
	}
	return tmp, nil
}

func importArchive(ctx context.Context, root, archivePath string) error {
	tags, err := archiveTags(archivePath)
	if err != nil {
		return fmt.Errorf("validate captured image archive: %w", redact(err))
	}
	return withStoreLock(ctx, root, true, func() error {
		current, err := readCurrentUnlocked(root)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if errors.Is(err, os.ErrNotExist) {
			current = Manifest{Version: storeVersion}
		}

		next := make([]ImageRecord, 0, len(tags))
		for _, tag := range tags {
			if err := ctx.Err(); err != nil {
				return err
			}
			img, err := archive.ImageFromPath(archivePath, &tag)
			if err != nil {
				return fmt.Errorf("read captured image %s: %w", tag.Name(), redact(err))
			}
			platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
			if config, err := img.ConfigFile(); err == nil {
				if config.OS != "" {
					platform.OS = config.OS
				}
				if config.Architecture != "" {
					platform.Architecture = config.Architecture
				}
				platform.Variant = config.Variant
			}
			record, err := storeImage(root, tag, fetchedImage{image: img, platform: platform})
			if err != nil {
				return fmt.Errorf("import captured image %s: %w", tag.Name(), redact(err))
			}
			next = append(next, record)
		}
		sortImageRecords(next)
		candidate := Manifest{Version: storeVersion, Images: next}
		if sameGenerationContent(current, candidate) {
			return nil
		}
		return publishGeneration(root, candidate)
	})
}

func archiveTags(path string) ([]name.Tag, error) {
	manifest, err := archive.LoadManifest(func() (io.ReadCloser, error) { return os.Open(path) })
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var tags []name.Tag
	for _, entry := range manifest {
		if len(entry.RepoTags) == 0 {
			return nil, fmt.Errorf("captured archive contains an untagged image")
		}
		for _, raw := range entry.RepoTags {
			tag, err := name.NewTag(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid captured image tag: %w", err)
			}
			if seen[tag.Name()] {
				continue
			}
			seen[tag.Name()] = true
			tags = append(tags, tag)
		}
	}
	if len(tags) == 0 {
		return nil, fmt.Errorf("captured archive contains no tagged images")
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].Name() < tags[j].Name() })
	return tags, nil
}
