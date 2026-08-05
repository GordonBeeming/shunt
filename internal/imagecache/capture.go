package imagecache

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	archive "github.com/google/go-containerregistry/pkg/v1/tarball"
	"golang.org/x/sys/unix"
)

const (
	maxCaptureManifestBytes = 16 * 1024 * 1024
	maxCaptureMembers       = 100_000
	maxCaptureDescriptors   = 10_000
	maxCaptureTags          = 100_000
	maxCaptureLayers        = 10_000
	maxCaptureNameBytes     = 4 * 1024
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
	indexed, err := indexDockerArchive(ctx, root, archivePath)
	if err != nil {
		return fmt.Errorf("validate captured image archive: %w", redact(err))
	}
	defer indexed.cleanup()
	staged, err := stageCapturedImages(ctx, root, indexed)
	if err != nil {
		return err
	}
	defer func() {
		for index := range staged {
			staged[index].close()
		}
	}()
	published := false
	if err := withCacheSweepLock(ctx, root, unix.LOCK_SH, func() error {
		return withStoreLock(ctx, root, true, func() error {
			current, err := readCurrentUnlocked(root)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if errors.Is(err, os.ErrNotExist) {
				current = Manifest{Version: storeVersion}
			}

			next := make([]ImageRecord, 0, len(staged))
			for index := range staged {
				stage := &staged[index]
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := adoptStagedImage(root, stage); err != nil {
					return fmt.Errorf("import captured image %s: %w", stage.record.Ref, redact(err))
				}
				next = append(next, stage.record)
			}
			sortImageRecords(next)
			candidate := Manifest{Version: storeVersion, Images: next}
			if sameGenerationContent(current, candidate) {
				return nil
			}
			if err := publishGeneration(root, candidate); err != nil {
				return err
			}
			published = true
			return nil
		})
	}); err != nil {
		return err
	}
	if published {
		if _, err := Collect(ctx, root, GCOptions{}); err != nil {
			return &CommittedCleanupError{Err: err}
		}
	}
	return nil
}

func stageCapturedImages(ctx context.Context, root string, indexed indexedDockerArchive) ([]stagedImageRecord, error) {
	staged := make([]stagedImageRecord, 0, len(indexed.tags))
	for _, tag := range indexed.tags {
		if err := ctx.Err(); err != nil {
			for index := range staged {
				staged[index].close()
			}
			return nil, err
		}
		img, err := archive.ImageFromPath(indexed.images[tag.Name()], &tag)
		if err != nil {
			for index := range staged {
				staged[index].close()
			}
			return nil, fmt.Errorf("read captured image %s: %w", tag.Name(), redact(err))
		}
		platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
		if config, configErr := img.ConfigFile(); configErr == nil {
			if config.OS != "" {
				platform.OS = config.OS
			}
			if config.Architecture != "" {
				platform.Architecture = config.Architecture
			}
			platform.Variant = config.Variant
		}
		stage, err := stageImage(root, tag, fetchedImage{image: img, platform: platform}, sourceCapture, "")
		if err != nil {
			for index := range staged {
				staged[index].close()
			}
			return nil, fmt.Errorf("stage captured image %s: %w", tag.Name(), redact(err))
		}
		staged = append(staged, stage)
	}
	return staged, nil
}

type indexedDockerArchive struct {
	tags    []name.Tag
	images  map[string]string
	cleanup func()
}

// indexDockerArchive scans a captured multi-image Docker archive once, stages
// its regular members, then creates a small single-image archive per tag. The
// import path never restarts a linear scan of the original multi-gigabyte tar.
func indexDockerArchive(ctx context.Context, root, source string) (indexedDockerArchive, error) {
	maxBytes := ConfiguredMaxBytes()
	info, err := os.Stat(source)
	if err != nil {
		return indexedDockerArchive{}, err
	}
	if info.Size() > maxBytes {
		return indexedDockerArchive{}, fmt.Errorf("captured archive is %d bytes, above the %d-byte cache limit", info.Size(), maxBytes)
	}
	stage, err := os.MkdirTemp(filepath.Dir(root), ".shunt-images-index-*")
	if err != nil {
		return indexedDockerArchive{}, err
	}
	cleanup := func() { _ = os.RemoveAll(stage) }
	keepStage := false
	defer func() {
		if !keepStage {
			cleanup()
		}
	}()
	knownMembers, err := extractCaptureMembers(ctx, stage, source, maxBytes)
	if err != nil {
		return indexedDockerArchive{}, err
	}
	manifestPath := filepath.Join(stage, "members", "manifest.json")
	manifestInfo, err := os.Stat(manifestPath)
	if err != nil {
		return indexedDockerArchive{}, err
	}
	if manifestInfo.Size() > maxCaptureManifestBytes {
		return indexedDockerArchive{}, fmt.Errorf("captured image manifest exceeds %d bytes", maxCaptureManifestBytes)
	}
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return indexedDockerArchive{}, err
	}
	var manifest archive.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return indexedDockerArchive{}, err
	}
	if len(manifest) > maxCaptureDescriptors {
		return indexedDockerArchive{}, fmt.Errorf("captured archive contains more than %d image descriptors", maxCaptureDescriptors)
	}
	result := indexedDockerArchive{images: map[string]string{}, cleanup: cleanup}
	seen := map[string]bool{}
	tagCount := 0
	var expandedBytes int64
	for descriptorIndex, descriptor := range manifest {
		if err := ctx.Err(); err != nil {
			return indexedDockerArchive{}, err
		}
		if len(descriptor.RepoTags) == 0 {
			return indexedDockerArchive{}, fmt.Errorf("captured archive contains an untagged image")
		}
		if len(descriptor.Layers) > maxCaptureLayers {
			return indexedDockerArchive{}, fmt.Errorf("captured image descriptor contains more than %d layers", maxCaptureLayers)
		}
		for _, member := range append([]string{descriptor.Config}, descriptor.Layers...) {
			clean := filepath.Clean(member)
			if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
				return indexedDockerArchive{}, fmt.Errorf("captured image contains unsafe member reference %q", member)
			}
			memberPath, ok := knownMembers[clean]
			if !ok {
				return indexedDockerArchive{}, fmt.Errorf("captured image references missing member %q", member)
			}
			memberInfo, err := os.Stat(memberPath)
			if err != nil {
				return indexedDockerArchive{}, err
			}
			if memberInfo.Size() > maxBytes-expandedBytes {
				return indexedDockerArchive{}, fmt.Errorf("indexed captured images exceed the %d-byte cache limit", maxBytes)
			}
			expandedBytes += memberInfo.Size()
		}
		archivePath := filepath.Join(stage, "images", fmt.Sprintf("%d.tar", descriptorIndex))
		if err := writeIndexedImageArchive(ctx, archivePath, knownMembers, descriptor); err != nil {
			return indexedDockerArchive{}, err
		}
		for _, raw := range descriptor.RepoTags {
			tagCount++
			if tagCount > maxCaptureTags {
				return indexedDockerArchive{}, fmt.Errorf("captured archive contains more than %d image tags", maxCaptureTags)
			}
			if len(raw) > maxCaptureNameBytes {
				return indexedDockerArchive{}, fmt.Errorf("captured image tag exceeds %d bytes", maxCaptureNameBytes)
			}
			tag, err := name.NewTag(raw)
			if err != nil {
				return indexedDockerArchive{}, fmt.Errorf("invalid captured image tag: %w", err)
			}
			if seen[tag.Name()] {
				continue
			}
			seen[tag.Name()] = true
			result.tags = append(result.tags, tag)
			result.images[tag.Name()] = archivePath
		}
	}
	if len(result.tags) == 0 {
		return indexedDockerArchive{}, fmt.Errorf("captured archive contains no tagged images")
	}
	sort.Slice(result.tags, func(i, j int) bool { return result.tags[i].Name() < result.tags[j].Name() })
	keepStage = true
	return result, nil
}

func extractCaptureMembers(ctx context.Context, stage, source string, maxBytes int64) (_ map[string]string, retErr error) {
	file, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	reader := tar.NewReader(file)
	knownMembers := map[string]string{}
	var totalBytes int64
	for memberCount := 1; ; memberCount++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return knownMembers, nil
		}
		if err != nil {
			return nil, err
		}
		if memberCount > maxCaptureMembers {
			return nil, fmt.Errorf("captured archive contains more than %d members", maxCaptureMembers)
		}
		if len(header.Name) > maxCaptureNameBytes {
			return nil, fmt.Errorf("captured archive member name exceeds %d bytes", maxCaptureNameBytes)
		}
		clean := filepath.Clean(header.Name)
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("captured archive contains unsafe path %q", header.Name)
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("captured archive contains unsupported entry %q", header.Name)
		}
		if header.Size < 0 || header.Size > maxBytes-totalBytes {
			return nil, fmt.Errorf("captured archive expands beyond the %d-byte cache limit", maxBytes)
		}
		totalBytes += header.Size
		destination := filepath.Join(stage, "members", clean)
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return nil, err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		written, copyErr := copyContext(ctx, output, io.LimitReader(reader, header.Size))
		closeErr := output.Close()
		if copyErr == nil && written != header.Size {
			copyErr = io.ErrUnexpectedEOF
		}
		if copyErr != nil || closeErr != nil {
			return nil, errors.Join(copyErr, closeErr)
		}
		knownMembers[clean] = destination
	}
}

func writeIndexedImageArchive(ctx context.Context, path string, members map[string]string, descriptor archive.Descriptor) (retErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, file.Close()) }()
	writer := tar.NewWriter(file)
	defer func() { retErr = errors.Join(retErr, writer.Close()) }()
	manifest, err := json.Marshal([]archive.Descriptor{descriptor})
	if err != nil {
		return err
	}
	if err := writeIndexedBytes(writer, "manifest.json", manifest); err != nil {
		return err
	}
	for _, member := range append([]string{descriptor.Config}, descriptor.Layers...) {
		if err := writeIndexedMember(ctx, writer, member, members); err != nil {
			return err
		}
	}
	return nil
}

func writeIndexedBytes(writer *tar.Writer, member string, data []byte) error {
	if err := writer.WriteHeader(&tar.Header{Name: member, Mode: 0o600, Size: int64(len(data))}); err != nil {
		return err
	}
	_, err := writer.Write(data)
	return err
}

func writeIndexedMember(ctx context.Context, writer *tar.Writer, member string, members map[string]string) error {
	clean, err := safeArchiveMember(member)
	if err != nil {
		return err
	}
	path, ok := members[clean]
	if !ok {
		return fmt.Errorf("captured archive references missing member %q", member)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if err := writer.WriteHeader(&tar.Header{Name: clean, Mode: 0o600, Size: info.Size()}); err != nil {
		return err
	}
	_, err = copyContext(ctx, writer, file)
	return err
}

func safeArchiveMember(member string) (string, error) {
	clean := filepath.Clean(member)
	if member == "" || filepath.IsAbs(member) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean != member {
		return "", fmt.Errorf("captured archive contains unsafe member reference %q", member)
	}
	return clean, nil
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			written, writeErr := destination.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != read {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
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
