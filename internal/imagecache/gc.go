package imagecache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// DefaultMaxBytes is the cache budget used by automatic and explicit collection.
const DefaultMaxBytes int64 = 100 * 1024 * 1024 * 1024

const cacheMaxBytesEnv = "SHUNT_CACHE_MAX_BYTES"

// GCOptions controls an explicit cache collection run.
type GCOptions struct {
	DryRun   bool
	MaxBytes int64
	Progress func(string)
}

// GCResult describes one mark-and-sweep collection run.
type GCResult struct {
	ReclaimedBytes int64
	Removed        []string
	ProtectedBytes int64
	Warning        string
}

// ConfiguredMaxBytes returns the automatic collection budget. Invalid values
// fail closed to the documented default rather than allowing an accidental
// zero/negative environment value to disable retention safeguards.
func ConfiguredMaxBytes() int64 {
	value := strings.TrimSpace(os.Getenv(cacheMaxBytesEnv))
	if value == "" {
		return DefaultMaxBytes
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return DefaultMaxBytes
	}
	return parsed
}

// generationLease keeps an immutable generation available while a guest loads
// its exports. GC takes the same lock exclusively before removing a generation.
type generationLease struct{ file *os.File }

func (l *generationLease) close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return errors.Join(unix.Flock(int(l.file.Fd()), unix.LOCK_UN), l.file.Close())
}

func generationLeasePath(root, generation string) (string, error) {
	if !validDigest(generation) {
		return "", fmt.Errorf("invalid image generation %q", generation)
	}
	return filepath.Join(root, "leases", strings.TrimPrefix(generation, "sha256:")+".lock"), nil
}

func acquireGenerationLease(root, generation string, mode int) (*generationLease, error) {
	path, err := generationLeasePath(root, generation)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), mode); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &generationLease{file: file}, nil
}

// Collect removes cache objects unreachable from the current/previous
// generations. A generation leased by an active reader is always protected,
// even when it is no longer current. Protected data is never evicted merely to
// satisfy the budget.
func Collect(ctx context.Context, root string, options GCOptions) (GCResult, error) {
	if options.MaxBytes <= 0 {
		options.MaxBytes = ConfiguredMaxBytes()
	}
	var result GCResult
	err := withCacheSweepLock(ctx, root, unix.LOCK_EX, func() error {
		protected := map[string]Manifest{}
		liveExports := map[string]bool{}
		liveBlobs := map[string]bool{}
		var stale []string
		var leases []*generationLease
		defer func() {
			for _, lease := range leases {
				_ = lease.close()
			}
		}()

		// Snapshot manifests and claim stale generations under the short store
		// lock. The sweep lock prevents publishers from adopting new objects,
		// while held exclusive generation leases protect readers of old ones.
		if err := withStoreLock(ctx, root, false, func() error {
			current, err := readCurrentUnlocked(root)
			if err != nil {
				return err
			}
			protected[current.Generation] = current
			if previous, err := readPreviousUnlocked(root); err == nil {
				protected[previous.Generation] = previous
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			entries, err := os.ReadDir(filepath.Join(root, "generations", "sha256"))
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					return err
				}
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
					continue
				}
				generation := "sha256:" + strings.TrimSuffix(entry.Name(), ".json")
				if _, known := protected[generation]; known || !validDigest(generation) {
					continue
				}
				lease, leaseErr := acquireGenerationLease(root, generation, unix.LOCK_EX|unix.LOCK_NB)
				if leaseErr != nil {
					if errors.Is(leaseErr, unix.EWOULDBLOCK) || errors.Is(leaseErr, unix.EAGAIN) {
						manifest, readErr := readGenerationUnlocked(root, generation)
						if readErr != nil {
							return readErr
						}
						protected[generation] = manifest
						continue
					}
					return leaseErr
				}
				leases = append(leases, lease)
				stale = append(stale, generation)
			}
			return nil
		}); err != nil {
			return err
		}

		for _, manifest := range protected {
			for _, image := range manifest.Images {
				liveExports[image.Export] = true
				for _, blob := range image.Blobs {
					liveBlobs[blob.Digest] = true
				}
			}
		}
		sort.Strings(stale)
		for _, generation := range stale {
			if err := collectPath(ctx, root, generationPath(root, generation), options, &result); err != nil {
				return err
			}
			leasePath, err := generationLeasePath(root, generation)
			if err != nil {
				return err
			}
			if err := collectPath(ctx, root, leasePath, options, &result); err != nil {
				return err
			}
		}
		if err := sweepObjects(ctx, root, filepath.Join(root, "exports"), func(path string) bool {
			rel, err := filepath.Rel(root, path)
			return err == nil && liveExports[rel]
		}, options, &result); err != nil {
			return err
		}
		if err := sweepObjects(ctx, root, filepath.Join(root, "oci", "blobs", "sha256"), func(path string) bool {
			return liveBlobs["sha256:"+filepath.Base(path)]
		}, options, &result); err != nil {
			return err
		}

		result.ProtectedBytes = protectedBytes(root, protected)
		if result.ProtectedBytes > options.MaxBytes {
			result.Warning = fmt.Sprintf("protected cache data is %d bytes, above the %d-byte budget; nothing protected was removed", result.ProtectedBytes, options.MaxBytes)
			if options.Progress != nil {
				options.Progress("cache GC warning: " + result.Warning)
			}
		}
		return nil
	})
	sort.Strings(result.Removed)
	return result, err
}

func collectPath(ctx context.Context, root, path string, options GCOptions, result *GCResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if options.Progress != nil {
		options.Progress("cache GC removes " + strings.TrimPrefix(path, root+string(filepath.Separator)))
	}
	result.ReclaimedBytes += info.Size()
	result.Removed = append(result.Removed, path)
	if options.DryRun {
		return nil
	}
	return os.Remove(path)
}

func sweepObjects(ctx context.Context, root, base string, keep func(string) bool, options GCOptions, result *GCResult) error {
	return filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil || entry.IsDir() || keep(path) {
			return walkErr
		}
		return collectPath(ctx, root, path, options, result)
	})
}

func readGenerationUnlocked(root, generation string) (Manifest, error) {
	data, err := readLimitedFile(generationPath(root, generation), maxGenerationBytes)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := decodeStrict(data, &manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.Generation != generation || manifest.Version != storeVersion {
		return Manifest{}, fmt.Errorf("invalid image generation %s", generation)
	}
	return manifest, nil
}

func protectedBytes(root string, manifests map[string]Manifest) int64 {
	var total int64
	seenExports := map[string]bool{}
	seenBlobs := map[string]bool{}
	for _, manifest := range manifests {
		for _, image := range manifest.Images {
			if path, err := cacheRelativePath(root, image.Export); err == nil {
				if info, err := os.Stat(path); err == nil && !seenExports[path] {
					total += info.Size()
					seenExports[path] = true
				}
			}
			for _, blob := range image.Blobs {
				if path, err := blobPath(root, blob.Digest); err == nil {
					if info, err := os.Stat(path); err == nil && !seenBlobs[path] {
						total += info.Size()
						seenBlobs[path] = true
					}
				}
			}
		}
	}
	return total
}
