package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var (
	walkLogicalTree      = filepath.WalkDir
	walkStorageTree      = filepath.WalkDir
	onClassifiedAncestor = func() {}
)

type generatedCollector struct {
	sources    map[string]struct{}
	candidates map[string][]*discoveredCandidate
	byPath     map[string]*discoveredCandidate
}

func newGeneratedCollector(reports []SidingReport) *generatedCollector {
	collector := &generatedCollector{
		sources:    make(map[string]struct{}, len(reports)),
		candidates: make(map[string][]*discoveredCandidate, len(reports)),
		byPath:     make(map[string]*discoveredCandidate),
	}
	for _, report := range reports {
		collector.sources[filepath.Clean(report.Source.Measurement.Path)] = struct{}{}
	}
	return collector
}

func (collector *generatedCollector) observeDir(root, path string, entry fs.DirEntry) error {
	if collector == nil || path == root || entry.Type()&os.ModeSymlink != 0 {
		return nil
	}
	if _, ok := GeneratedDirNames[entry.Name()]; !ok {
		return nil
	}
	for ancestor := filepath.Dir(path); ancestor != root; ancestor = filepath.Dir(ancestor) {
		if _, ok := collector.sources[ancestor]; !ok {
			continue
		}
		rel, err := safeRelative(ancestor, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		identity, err := identifyDirectory(info)
		if err != nil {
			return err
		}
		candidate := &discoveredCandidate{TrimCandidate: TrimCandidate{Path: path, RelativePath: filepath.ToSlash(rel), identity: identity}}
		collector.candidates[ancestor] = append(collector.candidates[ancestor], candidate)
		collector.byPath[path] = candidate
		return nil
	}
	return nil
}

func (collector *generatedCollector) observeFile(root, path string, size int64) {
	if collector == nil {
		return
	}
	for ancestor := filepath.Dir(path); ancestor != root; ancestor = filepath.Dir(ancestor) {
		if candidate := collector.byPath[ancestor]; candidate != nil {
			candidate.LogicalBytes += size
		}
	}
}

type FilesystemView struct {
	Path           string `json:"path"`
	Measurement    string `json:"measurement"` // physical
	Observation    string `json:"observation"` // observed | unavailable
	Detail         string `json:"detail,omitempty"`
	TotalBytes     int64  `json:"totalBytes"`
	UsedBytes      int64  `json:"usedBytes"`
	AvailableBytes int64  `json:"availableBytes"`
}

// Filesystem reports filesystem capacity. Unlike directory scans, these values
// describe physical allocation and free space.
func Filesystem(path string) (FilesystemView, error) {
	probe, err := nearestExisting(path)
	if err != nil {
		return FilesystemView{}, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(probe, &stat); err != nil {
		return FilesystemView{}, fmt.Errorf("stat filesystem for %s: %w", probe, err)
	}
	blockSize := int64(stat.Bsize)
	total := saturatingMultiply(int64(stat.Blocks), blockSize)
	available := saturatingMultiply(int64(stat.Bavail), blockSize)
	free := saturatingMultiply(int64(stat.Bfree), blockSize)
	return FilesystemView{
		Path:           probe,
		Measurement:    "physical",
		Observation:    "observed",
		TotalBytes:     total,
		UsedBytes:      total - free,
		AvailableBytes: available,
	}, nil
}

// LogicalSize totals apparent file sizes without following symlinks. On APFS
// clones this intentionally includes shared extents and therefore is never a
// reclaimable-space claim.
func LogicalSize(ctx context.Context, path string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return info.Size(), nil
	}
	var total int64
	err = walkLogicalTree(path, func(_ string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// measureClassified walks a storage root once and attributes each apparent
// file size to every declared category that contains it. Categories may
// intentionally overlap in the report, but they never trigger extra walks.

func measureClassified(ctx context.Context, root string, generated *generatedCollector, measurements ...*Measurement) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	byRoot := make(map[string][]*Measurement, len(measurements))
	for _, measurement := range measurements {
		if measurement == nil {
			continue
		}
		measurement.LogicalBytes = 0
		info, err := os.Lstat(measurement.Path)
		switch {
		case err == nil:
			measurement.Observation = "observed"
		case errors.Is(err, os.ErrNotExist):
			measurement.Observation = "missing"
		default:
			measurement.Observation = "error"
			measurement.Detail = err.Error()
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			measurement.LogicalBytes = info.Size()
		} else if err == nil {
			measurement.Path = filepath.Clean(measurement.Path)
			byRoot[measurement.Path] = append(byRoot[measurement.Path], measurement)
		}
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	return walkStorageTree(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Type()&os.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			return generated.observeDir(root, path, entry)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		for ancestor := filepath.Dir(path); ; ancestor = filepath.Dir(ancestor) {
			onClassifiedAncestor()
			for _, measurement := range byRoot[ancestor] {
				measurement.LogicalBytes += info.Size()
			}
			if ancestor == root {
				break
			}
		}
		generated.observeFile(root, path, info.Size())
		return nil
	})
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func nearestExisting(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("filesystem path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for probe := filepath.Clean(abs); ; probe = filepath.Dir(probe) {
		if _, err := os.Lstat(probe); err == nil {
			return probe, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", fmt.Errorf("no existing ancestor for %s", path)
		}
	}
}

func saturatingMultiply(left, right int64) int64 {
	if left <= 0 || right <= 0 {
		return 0
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	if left > maxInt64/right {
		return maxInt64
	}
	return left * right
}
