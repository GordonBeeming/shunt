package imagecache

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	dockerarchive "github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/gordonbeeming/shunt/internal/proc"
)

// LocalBuildSource is a framework-neutral image build declaration. Paths are
// host paths resolved by the lifecycle layer; imagecache never imports CLI or
// application-contract packages.
type LocalBuildSource struct {
	Ref        string
	ContextDir string
	Dockerfile string
	Platform   string
	BuildArgs  map[string]string
}

type localBuild struct {
	source      LocalBuildSource
	ref         name.Tag
	platform    v1.Platform
	fingerprint string
}

func parseLocalBuilds(sources []LocalBuildSource) ([]localBuild, error) {
	seen := make(map[string]bool, len(sources))
	result := make([]localBuild, 0, len(sources))
	for _, source := range sources {
		parsed, err := name.ParseReference(source.Ref)
		if err != nil {
			return nil, fmt.Errorf("invalid local image reference: %w", redact(err))
		}
		ref, ok := parsed.(name.Tag)
		if !ok {
			return nil, fmt.Errorf("local image reference %q must use a tag", source.Ref)
		}
		if seen[ref.Name()] {
			return nil, fmt.Errorf("duplicate local image reference %q", source.Ref)
		}
		seen[ref.Name()] = true

		if strings.TrimSpace(source.ContextDir) == "" {
			return nil, fmt.Errorf("local image %q has no build context", source.Ref)
		}
		source.ContextDir, err = filepath.Abs(filepath.Clean(source.ContextDir))
		if err != nil {
			return nil, fmt.Errorf("resolve build context for %s: %w", source.Ref, err)
		}
		info, err := os.Stat(source.ContextDir)
		if err != nil {
			return nil, fmt.Errorf("stat build context for %s: %w", source.Ref, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("build context for %s is not a directory", source.Ref)
		}

		if strings.TrimSpace(source.Dockerfile) == "" {
			source.Dockerfile = filepath.Join(source.ContextDir, "Dockerfile")
		} else if !filepath.IsAbs(source.Dockerfile) {
			source.Dockerfile = filepath.Join(source.ContextDir, source.Dockerfile)
		}
		source.Dockerfile = filepath.Clean(source.Dockerfile)
		if info, err := os.Stat(source.Dockerfile); err != nil {
			return nil, fmt.Errorf("stat Dockerfile for %s: %w", source.Ref, err)
		} else if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("Dockerfile for %s is not a regular file", source.Ref)
		}

		if source.Platform == "" {
			source.Platform = "linux/" + runtime.GOARCH
		}
		platform, err := v1.ParsePlatform(source.Platform)
		if err != nil {
			return nil, fmt.Errorf("invalid platform for %s: %w", source.Ref, err)
		}
		source.Platform = platform.String()
		fingerprint, err := localBuildFingerprint(source)
		if err != nil {
			return nil, fmt.Errorf("fingerprint local image %s: %w", source.Ref, err)
		}
		result = append(result, localBuild{source: source, ref: ref, platform: *platform, fingerprint: fingerprint})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ref.Name() < result[j].ref.Name() })
	return result, nil
}

func localBuildFingerprint(source LocalBuildSource) (string, error) {
	type buildArgument struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	declaration := struct {
		ContextDir string          `json:"contextDir"`
		Dockerfile string          `json:"dockerfile"`
		Platform   string          `json:"platform"`
		BuildArgs  []buildArgument `json:"buildArgs"`
	}{
		ContextDir: source.ContextDir,
		Dockerfile: source.Dockerfile,
		Platform:   source.Platform,
		BuildArgs:  make([]buildArgument, 0, len(source.BuildArgs)),
	}
	keys := make([]string, 0, len(source.BuildArgs))
	for key := range source.BuildArgs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		declaration.BuildArgs = append(declaration.BuildArgs, buildArgument{Name: key, Value: source.BuildArgs[key]})
	}
	data, err := json.Marshal(declaration)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", digest), nil
}

var runContainer = func(ctx context.Context, args ...string) error {
	return proc.RunPassthrough(ctx, "container", args...)
}

var localStageSequence atomic.Uint64

type stagedLocalImage struct {
	build   localBuild
	image   v1.Image
	cleanup func()
}

func (stage stagedLocalImage) close() {
	if stage.cleanup != nil {
		stage.cleanup()
	}
}

// stageLocalBuild runs the expensive host build and export before the cache
// publication lock. The caller imports the resulting immutable image during
// its short compare-and-swap section.
func stageLocalBuild(ctx context.Context, cachePath string, build localBuild) (stagedLocalImage, error) {
	stageRef, err := name.NewTag("shunt-stage/" + strings.TrimPrefix(build.fingerprint, "sha256:")[:16] + "-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(localStageSequence.Add(1), 10) + ":cache")
	if err != nil {
		return stagedLocalImage{}, fmt.Errorf("create local stage tag: %w", err)
	}
	args := []string{"build", "--platform", build.source.Platform}
	keys := make([]string, 0, len(build.source.BuildArgs))
	for key := range build.source.BuildArgs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "=\x00\r\n") {
			return stagedLocalImage{}, fmt.Errorf("invalid empty or delimited build argument name")
		}
		if strings.ContainsRune(build.source.BuildArgs[key], 0) {
			return stagedLocalImage{}, fmt.Errorf("build argument %q contains a NUL byte", key)
		}
		args = append(args, "--build-arg", key+"="+build.source.BuildArgs[key])
	}
	args = append(args, "-t", stageRef.Name(), "-f", build.source.Dockerfile, build.source.ContextDir)
	if err := runContainer(ctx, args...); err != nil {
		return stagedLocalImage{}, fmt.Errorf("container build: %w", err)
	}
	stageRefOwned := true
	defer func() {
		if stageRefOwned {
			deleteLocalStageRef(stageRef.Name())
		}
	}()

	tmp, err := newCaptureTemp(cachePath)
	if err != nil {
		return stagedLocalImage{}, err
	}
	archivePath := tmp.Name()
	archiveOwned := true
	defer func() {
		if archiveOwned {
			_ = os.Remove(archivePath)
		}
	}()
	if err := tmp.Close(); err != nil {
		return stagedLocalImage{}, err
	}
	if err := os.Remove(archivePath); err != nil {
		return stagedLocalImage{}, fmt.Errorf("prepare local image archive: %w", err)
	}
	if err := runContainer(ctx, "image", "save", "--platform", build.source.Platform, "-o", archivePath, stageRef.Name()); err != nil {
		return stagedLocalImage{}, fmt.Errorf("container image save: %w", err)
	}
	if err := os.Chmod(archivePath, 0o600); err != nil {
		return stagedLocalImage{}, fmt.Errorf("chmod local image archive: %w", err)
	}
	img, err := dockerarchive.ImageFromPath(archivePath, &stageRef)
	cleanupImage := func() {}
	if err != nil {
		img, cleanupImage, err = imageFromOCIArchive(archivePath, stageRef, build.platform)
		if err != nil {
			return stagedLocalImage{}, fmt.Errorf("read saved local image: %w", err)
		}
	}
	stageRefOwned = false
	archiveOwned = false
	return stagedLocalImage{build: build, image: img, cleanup: func() {
		cleanupImage()
		_ = os.Remove(archivePath)
		deleteLocalStageRef(stageRef.Name())
	}}, nil
}

func deleteLocalStageRef(ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = runContainer(ctx, "image", "delete", ref)
}

func buildAndStoreLocalImage(ctx context.Context, cachePath string, build localBuild) (ImageRecord, error) {
	buildStage, err := stageLocalBuild(ctx, cachePath, build)
	if err != nil {
		return ImageRecord{}, err
	}
	defer buildStage.close()
	stage, err := stageImage(cachePath, build.ref, fetchedImage{image: buildStage.image, platform: build.platform}, sourceLocal, build.fingerprint)
	if err != nil {
		return ImageRecord{}, fmt.Errorf("import saved local image: %w", err)
	}
	defer stage.close()
	if err := adoptStagedImage(cachePath, &stage); err != nil {
		return ImageRecord{}, fmt.Errorf("adopt saved local image: %w", err)
	}
	return stage.record, nil
}

func imageFromOCIArchive(archivePath string, ref name.Tag, platform v1.Platform) (v1.Image, func(), error) {
	directory, err := os.MkdirTemp(filepath.Dir(archivePath), ".shunt-local-oci-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create OCI extraction directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	if err := extractOCIArchive(archivePath, directory); err != nil {
		cleanup()
		return nil, nil, err
	}
	index, err := layout.ImageIndexFromPath(directory)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("open OCI layout: %w", err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("read OCI index: %w", err)
	}
	descriptor, err := descriptorForRef(manifest.Manifests, ref)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	img, err := imageFromDescriptor(index, descriptor, platform)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return img, cleanup, nil
}

func descriptorForRef(descriptors []v1.Descriptor, ref name.Tag) (v1.Descriptor, error) {
	for _, descriptor := range descriptors {
		for _, key := range []string{"org.opencontainers.image.ref.name", "io.containerd.image.name"} {
			candidate := descriptor.Annotations[key]
			parsed, err := name.ParseReference(candidate)
			if err == nil && parsed.Name() == ref.Name() {
				return descriptor, nil
			}
		}
	}
	if len(descriptors) == 1 {
		return descriptors[0], nil
	}
	return v1.Descriptor{}, fmt.Errorf("OCI archive has no unique descriptor for %s", ref.Name())
}

func imageFromDescriptor(index v1.ImageIndex, descriptor v1.Descriptor, platform v1.Platform) (v1.Image, error) {
	switch descriptor.MediaType {
	case types.OCIManifestSchema1, types.DockerManifestSchema2:
		return index.Image(descriptor.Digest)
	case types.OCIImageIndex, types.DockerManifestList:
		nested, err := index.ImageIndex(descriptor.Digest)
		if err != nil {
			return nil, err
		}
		manifest, err := nested.IndexManifest()
		if err != nil {
			return nil, err
		}
		for _, child := range manifest.Manifests {
			if child.Platform != nil && child.Platform.Satisfies(platform) {
				return imageFromDescriptor(nested, child, platform)
			}
		}
		if len(manifest.Manifests) == 1 {
			return imageFromDescriptor(nested, manifest.Manifests[0], platform)
		}
		return nil, fmt.Errorf("OCI index has no image for platform %s", platform.String())
	default:
		return nil, fmt.Errorf("unsupported OCI descriptor media type %q", descriptor.MediaType)
	}
}

func extractOCIArchive(archivePath, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open OCI archive: %w", err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read OCI archive: %w", err)
		}
		clean := filepath.Clean(header.Name)
		if clean == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("OCI archive contains unsafe path %q", header.Name)
		}
		target := filepath.Join(destination, clean)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("create OCI directory: %w", err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 {
				return fmt.Errorf("OCI archive entry %q has negative size", header.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return fmt.Errorf("create OCI parent directory: %w", err)
			}
			output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return fmt.Errorf("create OCI entry %q: %w", header.Name, err)
			}
			written, copyErr := io.CopyN(output, reader, header.Size)
			closeErr := output.Close()
			if copyErr != nil {
				return fmt.Errorf("extract OCI entry %q after %d bytes: %w", header.Name, written, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close OCI entry %q: %w", header.Name, closeErr)
			}
		default:
			return fmt.Errorf("OCI archive contains unsupported entry type %d for %q", header.Typeflag, header.Name)
		}
	}
}
