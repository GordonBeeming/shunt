package imagecache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	archive "github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"golang.org/x/sys/unix"
)

const (
	storeVersion       = 3
	maxIndexBytes      = 1 << 20
	maxGenerationBytes = 16 << 20
	sourceRegistry     = "registry"
	sourceLocal        = "local"
	sourceCapture      = "capture"
)

// Manifest is an immutable description of one cache generation.
type Manifest struct {
	Version    int           `json:"version"`
	Generation string        `json:"generation"`
	Images     []ImageRecord `json:"images"`
}

// ImageRecord describes one configured reference and its immutable export.
type ImageRecord struct {
	Ref               string       `json:"ref"`
	Digest            string       `json:"digest"`
	ConfigDigest      string       `json:"configDigest"`
	MediaType         string       `json:"mediaType"`
	ManifestSize      int64        `json:"manifestSize"`
	Platform          string       `json:"platform"`
	Fallback          bool         `json:"fallback,omitempty"`
	SourceKind        string       `json:"sourceKind"`
	SourceFingerprint string       `json:"sourceFingerprint,omitempty"`
	Export            string       `json:"export"`
	ExportChecksum    string       `json:"exportChecksum"`
	Blobs             []BlobRecord `json:"blobs"`
}

// BlobRecord identifies one compressed OCI blob reused by cached images.
type BlobRecord struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// GuestMarker records the image identifiers Docker actually assigned after a
// verified cache load. Docker's containerd image store may derive an identifier
// that differs from the source config digest, so future plans compare the live
// identifier with this observation and the per-ref digest with the host cache.
type GuestMarker struct {
	Generation string            `json:"generation"`
	Images     map[string]string `json:"images"`
	Digests    map[string]string `json:"digests"`
}

// GuestState combines the last marker with image identities observed from one
// batch Docker inspect. Marker data alone is never trusted to skip a load.
type GuestState struct {
	Marker   GuestMarker
	ImageIDs map[string]string
}

// PlannedImage is a Docker-load-compatible export needed by a guest.
type PlannedImage struct {
	Ref      string
	Digest   string
	ImageID  string
	Checksum string
	Path     string
	Platform string
	Fallback bool
}

// LoadPlan contains only missing or changed refs plus the marker to commit
// after the caller verifies every declared image in the guest.
type LoadPlan struct {
	Generation string
	Images     []PlannedImage
	Marker     GuestMarker
	lease      *generationLease
}

// Release relinquishes the stable-generation lease acquired by Plan. It is
// safe to call on test-created plans and more than once.
func (plan *LoadPlan) Release() error {
	if plan == nil || plan.lease == nil {
		return nil
	}
	err := plan.lease.close()
	plan.lease = nil
	return err
}

type currentIndex struct {
	Version    int    `json:"version"`
	Generation string `json:"generation"`
}

// Inspect returns one stable current generation.
func Inspect(ctx context.Context, path string) (Manifest, error) {
	var manifest Manifest
	err := withStoreLock(ctx, path, false, func() error {
		var err error
		manifest, err = readCurrentUnlocked(path)
		return err
	})
	return manifest, err
}

// Plan compares the current generation and the IDs observed after its last
// verified load with the IDs currently in the guest. It returns immutable
// Docker archives for missing, substituted, or generation-stale refs.
func Plan(ctx context.Context, path string, guest GuestState) (LoadPlan, error) {
	var plan LoadPlan
	err := withStoreLock(ctx, path, false, func() error {
		manifest, err := readCurrentUnlocked(path)
		if err != nil {
			return err
		}
		plan.Generation = manifest.Generation
		plan.Marker = markerFor(manifest)
		lease, err := acquireGenerationLease(path, manifest.Generation, unix.LOCK_SH)
		if err != nil {
			return fmt.Errorf("lease image generation: %w", err)
		}
		plan.lease = lease
		for _, image := range manifest.Images {
			observed := normalizeImageID(guest.ImageIDs[image.Ref])
			marked := normalizeImageID(guest.Marker.Images[image.Ref])
			if guest.Marker.Digests[image.Ref] == image.Digest && observed != "" && observed == marked {
				continue
			}
			exportPath, err := cacheRelativePath(path, image.Export)
			if err != nil {
				return err
			}
			plan.Images = append(plan.Images, PlannedImage{
				Ref:      image.Ref,
				Digest:   image.Digest,
				ImageID:  image.ConfigDigest,
				Checksum: image.ExportChecksum,
				Path:     exportPath,
				Platform: image.Platform,
				Fallback: image.Fallback,
			})
		}
		return nil
	})
	if err != nil {
		_ = plan.Release()
	}
	return plan, err
}

// OpenExport opens an immutable Docker archive only when ref and digest belong
// to the current generation. The returned file remains valid after the lock is
// released because cache exports are never modified in place.
func OpenExport(ctx context.Context, path, ref, digest string) (*os.File, error) {
	var file *os.File
	err := withStoreLock(ctx, path, false, func() error {
		manifest, err := readCurrentUnlocked(path)
		if err != nil {
			return err
		}
		for _, image := range manifest.Images {
			if image.Ref != ref || image.Digest != digest {
				continue
			}
			exportPath, err := cacheRelativePath(path, image.Export)
			if err != nil {
				return err
			}
			if err := verifyFile(exportPath, image.ExportChecksum, -1); err != nil {
				return fmt.Errorf("verify export for %s: %w", image.Ref, err)
			}
			file, err = os.Open(exportPath)
			return err
		}
		return fmt.Errorf("image %q at %q is not in generation %s", ref, digest, manifest.Generation)
	})
	return file, err
}

func markerFor(manifest Manifest) GuestMarker {
	marker := GuestMarker{
		Generation: manifest.Generation,
		Images:     make(map[string]string, len(manifest.Images)),
		Digests:    make(map[string]string, len(manifest.Images)),
	}
	for _, image := range manifest.Images {
		marker.Digests[image.Ref] = image.Digest
	}
	return marker
}

func sortImageRecords(images []ImageRecord) {
	for i := range images {
		sort.Slice(images[i].Blobs, func(a, b int) bool { return images[i].Blobs[a].Digest < images[i].Blobs[b].Digest })
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Ref < images[j].Ref })
}

func generationFor(manifest Manifest) (string, error) {
	manifest.Generation = ""
	sortImageRecords(manifest.Images)
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func sameGenerationContent(current, candidate Manifest) bool {
	generation, err := generationFor(candidate)
	return err == nil && generation == current.Generation
}

func publishGeneration(root string, manifest Manifest) error {
	manifest.Version = storeVersion
	sortImageRecords(manifest.Images)
	generation, err := generationFor(manifest)
	if err != nil {
		return fmt.Errorf("checksum image generation: %w", err)
	}
	manifest.Generation = generation
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode image generation: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Join(root, "generations", "sha256"), 0o700); err != nil {
		return err
	}
	if err := writeImmutable(generationPath(root, generation), data); err != nil {
		return fmt.Errorf("write image generation: %w", err)
	}
	if err := writeOCIIndex(root, manifest.Images); err != nil {
		return fmt.Errorf("write OCI index: %w", err)
	}
	if current, err := readCurrentUnlocked(root); err == nil && current.Generation != generation {
		previous, err := json.MarshalIndent(currentIndex{Version: storeVersion, Generation: current.Generation}, "", "  ")
		if err != nil {
			return err
		}
		if err := writeFileAtomic(filepath.Join(root, "previous.json"), append(previous, '\n')); err != nil {
			return fmt.Errorf("publish previous image generation: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	indexData, err := json.MarshalIndent(currentIndex{Version: storeVersion, Generation: generation}, "", "  ")
	if err != nil {
		return err
	}
	indexData = append(indexData, '\n')
	if err := writeFileAtomic(filepath.Join(root, "index.json"), indexData); err != nil {
		return fmt.Errorf("publish image generation: %w", err)
	}
	return nil
}

func readPreviousUnlocked(root string) (Manifest, error) {
	data, err := readLimitedFile(filepath.Join(root, "previous.json"), maxIndexBytes)
	if err != nil {
		return Manifest{}, err
	}
	var index currentIndex
	if err := decodeStrict(data, &index); err != nil {
		return Manifest{}, fmt.Errorf("decode previous image cache index: %w", err)
	}
	if index.Version != storeVersion || !validDigest(index.Generation) {
		return Manifest{}, fmt.Errorf("unsupported previous image cache index")
	}
	return readGenerationUnlocked(root, index.Generation)
}

func readCurrentUnlocked(root string) (Manifest, error) {
	indexData, err := readLimitedFile(filepath.Join(root, "index.json"), maxIndexBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("read image cache index: %w", err)
	}
	var index currentIndex
	if err := decodeStrict(indexData, &index); err != nil {
		return Manifest{}, fmt.Errorf("decode image cache index: %w", err)
	}
	if index.Version != storeVersion || !validDigest(index.Generation) {
		return Manifest{}, fmt.Errorf("unsupported image cache index")
	}
	data, err := readLimitedFile(generationPath(root, index.Generation), maxGenerationBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("read image generation %s: %w", index.Generation, err)
	}
	var manifest Manifest
	if err := decodeStrict(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode image generation: %w", err)
	}
	if manifest.Version != storeVersion || manifest.Generation != index.Generation {
		return Manifest{}, fmt.Errorf("image generation does not match index")
	}
	calculated, err := generationFor(manifest)
	if err != nil || calculated != manifest.Generation {
		return Manifest{}, fmt.Errorf("image generation checksum mismatch")
	}
	seenRefs := make(map[string]bool, len(manifest.Images))
	for _, image := range manifest.Images {
		ref, refErr := name.NewTag(image.Ref)
		if refErr != nil || ref.Name() != image.Ref || seenRefs[image.Ref] || !validDigest(image.Digest) || !validDigest(image.ConfigDigest) || !validDigest(image.ExportChecksum) || image.MediaType == "" || image.ManifestSize <= 0 || image.Platform == "" || !validSource(image.SourceKind, image.SourceFingerprint) {
			return Manifest{}, fmt.Errorf("invalid image record for %q", image.Ref)
		}
		seenRefs[image.Ref] = true
		if _, err := cacheRelativePath(root, image.Export); err != nil {
			return Manifest{}, err
		}
		for _, blob := range image.Blobs {
			if !validDigest(blob.Digest) || blob.Size < 0 {
				return Manifest{}, fmt.Errorf("invalid blob record for %q", image.Ref)
			}
		}
	}
	return manifest, nil
}

func generationPath(root, generation string) string {
	return filepath.Join(root, "generations", "sha256", strings.TrimPrefix(generation, "sha256:")+".json")
}

func storeImage(root string, ref name.Tag, fetched fetchedImage, sourceKind, sourceFingerprint string) (ImageRecord, error) {
	if !validSource(sourceKind, sourceFingerprint) {
		return ImageRecord{}, fmt.Errorf("invalid image source provenance")
	}
	img := fetched.image
	digest, err := img.Digest()
	if err != nil {
		return ImageRecord{}, err
	}
	manifest, err := img.Manifest()
	if err != nil {
		return ImageRecord{}, err
	}
	mediaType, err := img.MediaType()
	if err != nil {
		return ImageRecord{}, err
	}
	rawManifest, err := img.RawManifest()
	if err != nil {
		return ImageRecord{}, err
	}
	rawConfig, err := img.RawConfigFile()
	if err != nil {
		return ImageRecord{}, err
	}
	configDigest, err := img.ConfigName()
	if err != nil {
		return ImageRecord{}, err
	}

	blobs := make([]BlobRecord, 0, len(manifest.Layers)+2)
	blobs = append(blobs, BlobRecord{Digest: digest.String(), Size: int64(len(rawManifest))})
	blobs = append(blobs, BlobRecord{Digest: manifest.Config.Digest.String(), Size: manifest.Config.Size})
	for _, layer := range manifest.Layers {
		blobs = append(blobs, BlobRecord{Digest: layer.Digest.String(), Size: layer.Size})
	}
	if err := ensureOCILayout(root); err != nil {
		return ImageRecord{}, err
	}
	layoutPath := layout.Path(filepath.Join(root, "oci"))
	if err := writeImageBlobs(root, img, manifest, digest, rawManifest, rawConfig); err != nil {
		return ImageRecord{}, err
	}

	record := ImageRecord{
		Ref:               ref.Name(),
		Digest:            digest.String(),
		ConfigDigest:      configDigest.String(),
		MediaType:         string(mediaType),
		ManifestSize:      int64(len(rawManifest)),
		Platform:          platformString(fetched.platform),
		Fallback:          fetched.fallback,
		SourceKind:        sourceKind,
		SourceFingerprint: sourceFingerprint,
		Blobs:             blobs,
	}
	if err := writeOCIIndex(root, []ImageRecord{record}); err != nil {
		return ImageRecord{}, err
	}
	localImage, err := layoutPath.Image(digest)
	if err != nil {
		return ImageRecord{}, err
	}
	record.Export, record.ExportChecksum, err = ensureExport(root, ref, localImage, digest)
	if err != nil {
		return ImageRecord{}, err
	}
	return record, nil
}

// stagedImageRecord is a complete private cache object tree. Expensive image
// decoding, blob verification, and Docker-export generation happen here before
// the live store lock is acquired. Publication only hard-links these immutable
// objects into the live store and writes the small generation/index files.
type stagedImageRecord struct {
	root   string
	record ImageRecord
}

func stageImage(cacheRoot string, ref name.Tag, fetched fetchedImage, sourceKind, sourceFingerprint string) (stagedImageRecord, error) {
	if err := os.MkdirAll(filepath.Dir(cacheRoot), 0o700); err != nil {
		return stagedImageRecord{}, err
	}
	root, err := os.MkdirTemp(filepath.Dir(cacheRoot), ".shunt-image-object-*")
	if err != nil {
		return stagedImageRecord{}, err
	}
	record, err := storeImage(root, ref, fetched, sourceKind, sourceFingerprint)
	if err != nil {
		_ = os.RemoveAll(root)
		return stagedImageRecord{}, err
	}
	return stagedImageRecord{root: root, record: record}, nil
}

func (stage *stagedImageRecord) close() {
	if stage == nil || stage.root == "" {
		return
	}
	_ = os.RemoveAll(stage.root)
	stage.root = ""
}

func adoptStagedImage(root string, stage *stagedImageRecord) error {
	if stage == nil || stage.root == "" {
		return fmt.Errorf("staged image is unavailable")
	}
	if err := ensureOCILayout(root); err != nil {
		return err
	}
	for _, blob := range stage.record.Blobs {
		source, err := blobPath(stage.root, blob.Digest)
		if err != nil {
			return err
		}
		destination, err := blobPath(root, blob.Digest)
		if err != nil {
			return err
		}
		if err := linkImmutableObject(source, destination, blob.Size); err != nil {
			return fmt.Errorf("adopt image blob %s: %w", blob.Digest, err)
		}
	}
	source, err := cacheRelativePath(stage.root, stage.record.Export)
	if err != nil {
		return err
	}
	destination, err := cacheRelativePath(root, stage.record.Export)
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if existing, statErr := os.Lstat(destination); statErr == nil && (!existing.Mode().IsRegular() || existing.Size() != info.Size()) {
		recovery, recoveryErr := recoveryExportPath(root, stage.record.Export)
		if recoveryErr != nil {
			return recoveryErr
		}
		stage.record.Export = recovery
		destination, err = cacheRelativePath(root, recovery)
		if err != nil {
			return err
		}
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := linkImmutableObject(source, destination, info.Size()); err != nil {
		return fmt.Errorf("adopt image export %s: %w", stage.record.Ref, err)
	}
	return nil
}

func recoveryExportPath(root, relative string) (string, error) {
	destination, err := cacheRelativePath(root, relative)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(filepath.Dir(destination), strings.TrimSuffix(filepath.Base(destination), ".tar")+"-recovery-*.tar")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := errors.Join(file.Close(), os.Remove(path)); err != nil {
		return "", err
	}
	recovery, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if _, err := cacheRelativePath(root, recovery); err != nil {
		return "", err
	}
	return recovery, nil
}

func linkImmutableObject(source, destination string, expectedSize int64) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != expectedSize {
		return fmt.Errorf("staged object is not a regular %d-byte file", expectedSize)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	if err := os.Link(source, destination); err == nil {
		return os.Chmod(destination, 0o600)
	} else if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("hard-link staged object: %w", err)
	}
	existing, err := os.Lstat(destination)
	if err != nil {
		return err
	}
	if !existing.Mode().IsRegular() || existing.Size() != expectedSize {
		return fmt.Errorf("immutable destination already exists with unexpected type or size")
	}
	return os.Chmod(destination, 0o600)
}

func validSource(kind, fingerprint string) bool {
	switch kind {
	case sourceRegistry, sourceCapture:
		return fingerprint == ""
	case sourceLocal:
		return validDigest(fingerprint)
	default:
		return false
	}
}

func normalizeImageID(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if len(value) == sha256.Size*2 {
		value = "sha256:" + value
	}
	if !validDigest(value) {
		return ""
	}
	return value
}

func recordUsable(root string, record ImageRecord) error {
	if !validDigest(record.ConfigDigest) {
		return fmt.Errorf("image config identity is missing")
	}
	exportPath, err := cacheRelativePath(root, record.Export)
	if err != nil {
		return err
	}
	info, err := os.Stat(exportPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("image export is not a regular file")
	}
	for _, blob := range record.Blobs {
		path, pathErr := blobPath(root, blob.Digest)
		if pathErr != nil {
			return pathErr
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return statErr
		}
		if !info.Mode().IsRegular() || info.Size() != blob.Size {
			return fmt.Errorf("image blob %s is unavailable", blob.Digest)
		}
	}
	return nil
}

func ensureOCILayout(root string) error {
	ociRoot := filepath.Join(root, "oci")
	if err := os.MkdirAll(filepath.Join(ociRoot, "blobs", "sha256"), 0o700); err != nil {
		return err
	}
	if err := writeImmutable(filepath.Join(ociRoot, "oci-layout"), []byte("{\"imageLayoutVersion\":\"1.0.0\"}\n")); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(ociRoot, "index.json")); errors.Is(err, os.ErrNotExist) {
		return writeOCIIndex(root, nil)
	} else if err != nil {
		return err
	}
	return nil
}

func writeOCIIndex(root string, images []ImageRecord) error {
	index := v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex}
	seen := make(map[string]bool, len(images))
	for _, image := range images {
		if seen[image.Digest] {
			continue
		}
		seen[image.Digest] = true
		digest, err := v1.NewHash(image.Digest)
		if err != nil {
			return err
		}
		index.Manifests = append(index.Manifests, v1.Descriptor{
			MediaType: types.MediaType(image.MediaType),
			Size:      image.ManifestSize,
			Digest:    digest,
		})
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Join(root, "oci"), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(root, "oci", "index.json"), data)
}

func ensureExport(root string, ref name.Tag, img v1.Image, digest v1.Hash) (string, string, error) {
	refHash := sha256.Sum256([]byte(ref.Name()))
	directory := filepath.Join(root, "exports", "sha256", digest.Hex)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", "", err
	}
	tmp, err := os.CreateTemp(directory, ".export-*.tar")
	if err != nil {
		return "", "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", "", err
	}
	hash := sha256.New()
	if err := archive.Write(ref, img, io.MultiWriter(tmp, hash)); err != nil {
		_ = tmp.Close()
		return "", "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	checksum := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	relative := filepath.Join("exports", "sha256", digest.Hex,
		hex.EncodeToString(refHash[:])+"-"+strings.TrimPrefix(checksum, "sha256:")+".tar")
	path := filepath.Join(root, relative)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return "", "", fmt.Errorf("immutable image export is not a regular file")
		}
		return relative, checksum, os.Chmod(path, 0o600)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", "", err
	}
	return relative, checksum, nil
}

func validateExport(path string, ref name.Tag, expected v1.Image) error {
	manifest, err := archive.LoadManifest(func() (io.ReadCloser, error) { return os.Open(path) })
	if err != nil {
		return err
	}
	found := false
	for _, entry := range manifest {
		for _, rawTag := range entry.RepoTags {
			if sameTagName(rawTag, ref.Name()) {
				found = true
			}
		}
	}
	if !found {
		return fmt.Errorf("Docker export does not contain tag %s", ref.Name())
	}
	actual, err := archive.ImageFromPath(path, &ref)
	if err != nil {
		return err
	}
	return compareImageContent(expected, actual)
}

func sameTagName(raw, canonical string) bool {
	ref, err := name.NewTag(raw)
	return err == nil && ref.Name() == canonical
}

func compareImageContent(expected, actual v1.Image) error {
	wantConfig, err := expected.ConfigName()
	if err != nil {
		return err
	}
	gotConfig, err := actual.ConfigName()
	if err != nil {
		return err
	}
	if wantConfig != gotConfig {
		return fmt.Errorf("Docker export config digest mismatch")
	}
	wantLayers, err := expected.Layers()
	if err != nil {
		return err
	}
	gotLayers, err := actual.Layers()
	if err != nil {
		return err
	}
	if len(wantLayers) != len(gotLayers) {
		return fmt.Errorf("Docker export layer count mismatch")
	}
	for i := range wantLayers {
		wantDigest, err := wantLayers[i].Digest()
		if err != nil {
			return err
		}
		gotDigest, err := gotLayers[i].Digest()
		if err != nil {
			return err
		}
		if wantDigest != gotDigest {
			return fmt.Errorf("Docker export layer digest mismatch")
		}
	}
	return nil
}

func writeImageBlobs(root string, img v1.Image, manifest *v1.Manifest, digest v1.Hash, rawManifest, rawConfig []byte) error {
	if err := writeBlob(root, digest.String(), int64(len(rawManifest)), io.NopCloser(bytes.NewReader(rawManifest))); err != nil {
		return fmt.Errorf("write image manifest blob: %w", err)
	}
	if err := writeBlob(root, manifest.Config.Digest.String(), manifest.Config.Size, io.NopCloser(bytes.NewReader(rawConfig))); err != nil {
		return fmt.Errorf("write image config blob: %w", err)
	}
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	if len(layers) != len(manifest.Layers) {
		return fmt.Errorf("image layer count does not match manifest")
	}
	for i, layer := range layers {
		descriptor := manifest.Layers[i]
		reused, err := reuseBlob(root, descriptor.Digest.String(), descriptor.Size)
		if err != nil {
			return err
		}
		if reused {
			continue
		}
		reader, err := layer.Compressed()
		if err != nil {
			return err
		}
		if err := writeBlob(root, descriptor.Digest.String(), descriptor.Size, reader); err != nil {
			return fmt.Errorf("write image layer blob %s: %w", descriptor.Digest, err)
		}
	}
	return nil
}

func writeBlob(root, digest string, size int64, reader io.ReadCloser) error {
	defer reader.Close()
	reused, err := reuseBlob(root, digest, size)
	if err != nil {
		return err
	}
	if reused {
		return nil
	}
	path, err := blobPath(root, digest)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(directory, ".blob-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hash), reader)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if written != size {
		_ = tmp.Close()
		return fmt.Errorf("blob size is %d, want %d", written, size)
	}
	actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actual != digest {
		_ = tmp.Close()
		return fmt.Errorf("blob digest is %s, want %s", actual, digest)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func reuseBlob(root, digest string, size int64) (bool, error) {
	path, err := blobPath(root, digest)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := verifyFile(path, digest, size); err == nil {
		return true, os.Chmod(path, 0o600)
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return false, nil
}

func blobPath(root, digest string) (string, error) {
	if !validDigest(digest) {
		return "", fmt.Errorf("invalid blob digest %q", digest)
	}
	return filepath.Join(root, "oci", "blobs", "sha256", strings.TrimPrefix(digest, "sha256:")), nil
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func verifyFile(path, digest string, expectedSize int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if expectedSize >= 0 && info.Size() != expectedSize {
		return fmt.Errorf("size is %d, want %d", info.Size(), expectedSize)
	}
	actual, err := fileDigest(path)
	if err != nil {
		return err
	}
	if actual != digest {
		return fmt.Errorf("digest is %s, want %s", actual, digest)
	}
	return nil
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func writeImmutable(path string, data []byte) error {
	existing, err := os.ReadFile(path)
	if err == nil {
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("immutable cache object already exists with different content: %s", path)
		}
		return os.Chmod(path, 0o600)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeFileAtomic(path, data)
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".atomic-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
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
	return os.Rename(tmpPath, path)
}

func readLimitedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := io.LimitReader(file, limit+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON")
		}
		return err
	}
	return nil
}

func cacheRelativePath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", fmt.Errorf("invalid cache-relative path %q", relative)
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid cache-relative path %q", relative)
	}
	return filepath.Join(root, clean), nil
}

// Read returns the images in one stable generation. It exists for cache
// inspection and tests; guest loading should use Plan and immutable exports.
func Read(path string) (map[string]cachedImage, error) {
	result := make(map[string]cachedImage)
	err := withStoreLock(context.Background(), path, false, func() error {
		manifest, err := readCurrentUnlocked(path)
		if err != nil {
			return err
		}
		for _, record := range manifest.Images {
			tag, err := name.NewTag(record.Ref)
			if err != nil {
				return err
			}
			exportPath, err := cacheRelativePath(path, record.Export)
			if err != nil {
				return err
			}
			img, err := archive.ImageFromPath(exportPath, &tag)
			if err != nil {
				return fmt.Errorf("read image cache %s: %w", path, err)
			}
			result[record.Ref] = cachedImage{
				ref:      tag,
				logical:  record.Ref,
				digest:   record.Digest,
				platform: record.Platform,
				fallback: record.Fallback,
				img:      img,
			}
		}
		return nil
	})
	return result, err
}

// Validate verifies generation checksums, every content-addressed blob, and
// every derived Docker export.
func Validate(path string) error {
	return withStoreLock(context.Background(), path, false, func() error {
		manifest, err := readCurrentUnlocked(path)
		if err != nil {
			return err
		}
		if err := verifyManifestFiles(path, manifest); err != nil {
			return err
		}
		return nil
	})
}

func verifyManifestFiles(root string, manifest Manifest) error {
	seenBlobs := map[string]bool{}
	for _, record := range manifest.Images {
		for _, blob := range record.Blobs {
			if seenBlobs[blob.Digest] {
				continue
			}
			seenBlobs[blob.Digest] = true
			path, err := blobPath(root, blob.Digest)
			if err != nil {
				return err
			}
			if err := verifyFile(path, blob.Digest, blob.Size); err != nil {
				return fmt.Errorf("verify image cache blob %s: %w", blob.Digest, err)
			}
		}
		exportPath, err := cacheRelativePath(root, record.Export)
		if err != nil {
			return err
		}
		if err := verifyFile(exportPath, record.ExportChecksum, -1); err != nil {
			return fmt.Errorf("verify image export %s: %w", record.Ref, err)
		}
		exportManifest, err := archive.LoadManifest(func() (io.ReadCloser, error) { return os.Open(exportPath) })
		if err != nil {
			return fmt.Errorf("validate image export %s: %w", record.Ref, err)
		}
		foundTag := false
		for _, entry := range exportManifest {
			for _, tag := range entry.RepoTags {
				foundTag = foundTag || sameTagName(tag, record.Ref)
			}
		}
		if !foundTag {
			return fmt.Errorf("validate image export %s: configured tag is missing", record.Ref)
		}
	}
	return nil
}
