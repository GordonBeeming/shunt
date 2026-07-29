// Package databaseline manages the promotable, host-backed data set shared by
// a project's sidings.
package databaseline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gordonbeeming/shunt/internal/fsclone"
	"golang.org/x/sys/unix"
)

const metadataName = ".shunt-baseline.json"

// Metadata records the coherent volume set that was promoted.
type Metadata struct {
	SourceSiding string    `json:"sourceSiding"`
	Timestamp    time.Time `json:"timestamp"`
	Volumes      []string  `json:"volumes"`
}

// Result reports whether a mutating operation has committed even when its
// follow-up cleanup could not finish.
type Result struct {
	Committed     bool
	RecoveryPaths []string
	Restore       RestoreResult
}

// CommittedCleanupError means the requested current baseline is installed, but
// cleanup after that commit failed and should be repaired before the next run.
type CommittedCleanupError struct {
	Operation     string
	RecoveryPaths []string
	Err           error
}

func (e *CommittedCleanupError) Error() string {
	return fmt.Sprintf("%s committed, but cleanup failed; recover from %v: %v", e.Operation, e.RecoveryPaths, e.Err)
}

func (e *CommittedCleanupError) Unwrap() error { return e.Err }

// RestoreError reports a failed lifecycle restore separately from whether the
// baseline promotion itself committed.
type RestoreError struct {
	Committed    bool
	Restore      RestoreResult
	OperationErr error
	Err          error
}

func (e *RestoreError) Error() string {
	if e.OperationErr != nil {
		return fmt.Sprintf("data lifecycle failed: %v; restore failed: %v", e.OperationErr, e.Err)
	}
	return fmt.Sprintf("data baseline committed=%t, but restore failed: %v", e.Committed, e.Err)
}

func (e *RestoreError) Unwrap() error { return e.Err }

// RestoreResult lets runner adapters report the state restored after a failed
// quiesce, export, or snapshot sequence.
type RestoreResult struct {
	Restored bool
	Details  []string
}

// Lifecycle is implemented by runner adapters. The baseline core only manages
// host paths; adapters own process quiescence and guest-specific data transfer.
type Lifecycle interface {
	Quiesce(context.Context) error
	StopVolumeConsumers(context.Context) error
	Sync(context.Context) error
	ExportLegacyGuestVolume(context.Context, string, string) error
	SnapshotHostVolume(context.Context, string, string) error
	Restore(context.Context) (RestoreResult, error)
}

// Manager operates on a project's configured data-volume set.
type Manager struct {
	ConfigDir string
	Volumes   []string

	clock  func() time.Time
	clone  func(context.Context, string, string) error
	rename func(string, string) error
	remove func(string) error
	swap   func(string, string) error
}

// New creates a manager for one project's configuration root.
func New(configDir string, volumes []string) (*Manager, error) {
	if configDir == "" {
		return nil, errors.New("data baseline config directory is required")
	}
	abs, err := filepath.Abs(configDir)
	if err != nil {
		return nil, fmt.Errorf("resolve config directory: %w", err)
	}
	if err := validateNames(volumes); err != nil {
		return nil, err
	}
	return &Manager{
		ConfigDir: filepath.Clean(abs), Volumes: append([]string(nil), volumes...), clock: time.Now,
		clone: fsclone.CloneVolume, rename: os.Rename, remove: os.RemoveAll, swap: fscloneSwap,
	}, nil
}

// Promote atomically installs a clone of every configured volume from source.
func (m *Manager) Promote(ctx context.Context, sourceSiding, sourceRoot string) (Result, error) {
	if err := m.requireVolumes(); err != nil {
		return Result{}, err
	}
	if sourceSiding == "" {
		return Result{}, errors.New("source siding is required")
	}
	if err := m.ensureWithinConfig(sourceRoot); err != nil {
		return Result{}, fmt.Errorf("source volume root: %w", err)
	}
	return m.withLock(func() (Result, error) {
		return m.promoteLocked(ctx, sourceSiding, func(stage string) error { return m.cloneSet(ctx, sourceRoot, stage) })
	})
}

// PromoteWithLifecycle holds the project lock across quiescing, capture,
// promotion, and restore so another data operation cannot observe an in-flight
// application state. Runner adapters may no-op the capture method that does not
// apply to their volume type.
func (m *Manager) PromoteWithLifecycle(ctx context.Context, sourceSiding string, lifecycle Lifecycle) (Result, error) {
	if err := m.requireVolumes(); err != nil {
		return Result{}, err
	}
	if sourceSiding == "" {
		return Result{}, errors.New("source siding is required")
	}
	if lifecycle == nil {
		return Result{}, errors.New("data lifecycle is required")
	}
	return m.withLock(func() (result Result, err error) {
		if err := lifecycle.Quiesce(ctx); err != nil {
			return m.restoreAfterLifecycle(ctx, lifecycle, result, err)
		}
		if err := lifecycle.StopVolumeConsumers(ctx); err != nil {
			return m.restoreAfterLifecycle(ctx, lifecycle, result, err)
		}
		if err := lifecycle.Sync(ctx); err != nil {
			return m.restoreAfterLifecycle(ctx, lifecycle, result, err)
		}
		result, err = m.promoteLocked(ctx, sourceSiding, func(stage string) error {
			for _, volume := range m.Volumes {
				destination := filepath.Join(stage, volume)
				if err := lifecycle.ExportLegacyGuestVolume(ctx, volume, destination); err != nil {
					return fmt.Errorf("export legacy volume %q: %w", volume, err)
				}
				if err := lifecycle.SnapshotHostVolume(ctx, volume, destination); err != nil {
					return fmt.Errorf("snapshot host volume %q: %w", volume, err)
				}
			}
			return nil
		})
		return m.restoreAfterLifecycle(ctx, lifecycle, result, err)
	})
}

// Rollback atomically makes the immediately preceding baseline current.
func (m *Manager) Rollback() (Result, error) {
	if err := m.requireVolumes(); err != nil {
		return Result{}, err
	}
	return m.withLock(func() (Result, error) {
		current, previous := m.currentRoot(), m.previousRoot()
		if err := validateRoot(current, m.Volumes, true); err != nil {
			return Result{}, fmt.Errorf("current baseline: %w", err)
		}
		if err := validateRoot(previous, m.Volumes, true); err != nil {
			return Result{}, fmt.Errorf("previous baseline: %w", err)
		}
		if err := m.swap(current, previous); err != nil {
			return Result{}, fmt.Errorf("swap baseline with previous: %w", err)
		}
		return Result{Committed: true}, nil
	})
}

// ResetVolumeRoot replaces a siding's entire volume root with the current
// baseline. If no baseline exists, it installs the complete empty volume set.
func (m *Manager) ResetVolumeRoot(ctx context.Context, destinationRoot string) (Result, error) {
	if err := m.requireVolumes(); err != nil {
		return Result{}, err
	}
	if err := m.ensureWithinConfig(destinationRoot); err != nil {
		return Result{}, fmt.Errorf("destination volume root: %w", err)
	}
	return m.withLock(func() (Result, error) {
		if err := fsclone.CloneVolumeSet(ctx, m.currentRoot(), destinationRoot, m.Volumes); err != nil {
			return Result{}, fmt.Errorf("reset volume root: %w", err)
		}
		return Result{Committed: true}, nil
	})
}

func (m *Manager) promoteLocked(ctx context.Context, sourceSiding string, capture func(string) error) (Result, error) {
	stage, err := os.MkdirTemp(m.ConfigDir, ".shunt-baseline-stage-")
	if err != nil {
		return Result{}, fmt.Errorf("create baseline stage: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = m.remove(stage)
		}
	}()
	if err := capture(stage); err != nil {
		return Result{}, err
	}
	metadata := Metadata{SourceSiding: sourceSiding, Timestamp: m.clock().UTC(), Volumes: append([]string(nil), m.Volumes...)}
	if err := writeMetadata(stage, metadata); err != nil {
		return Result{}, err
	}
	if err := validateRoot(stage, m.Volumes, true); err != nil {
		return Result{}, err
	}
	result, err := m.install(stage, "promote")
	committed = result.Committed
	return result, err
}

func (m *Manager) restoreAfterLifecycle(ctx context.Context, lifecycle Lifecycle, result Result, operationErr error) (Result, error) {
	restore, restoreErr := lifecycle.Restore(ctx)
	result.Restore = restore
	if restoreErr != nil {
		return result, &RestoreError{Committed: result.Committed, Restore: restore, OperationErr: operationErr, Err: restoreErr}
	}
	return result, operationErr
}

func (m *Manager) install(stage, operation string) (Result, error) {
	current, previous := m.currentRoot(), m.previousRoot()
	if _, err := os.Stat(current); os.IsNotExist(err) {
		if err := m.rename(stage, current); err != nil {
			return Result{}, fmt.Errorf("install first baseline: %w", err)
		}
		if err := m.remove(previous); err != nil {
			paths := existingPaths(previous)
			return Result{Committed: true, RecoveryPaths: paths}, &CommittedCleanupError{Operation: operation, RecoveryPaths: paths, Err: fmt.Errorf("remove stale previous baseline: %w", err)}
		}
		return Result{Committed: true}, nil
	} else if err != nil {
		return Result{}, fmt.Errorf("stat current baseline: %w", err)
	}

	if err := m.swap(stage, current); err != nil {
		return Result{}, fmt.Errorf("swap staged baseline into place: %w", err)
	}
	retired, err := os.MkdirTemp(m.ConfigDir, ".shunt-baseline-retired-")
	if err != nil {
		paths := existingPaths(stage, previous)
		return Result{Committed: true, RecoveryPaths: paths}, &CommittedCleanupError{Operation: operation, RecoveryPaths: paths, Err: fmt.Errorf("create retired baseline path: %w", err)}
	}
	if err := m.remove(retired); err != nil {
		paths := existingPaths(stage, previous, retired)
		return Result{Committed: true, RecoveryPaths: paths}, &CommittedCleanupError{Operation: operation, RecoveryPaths: paths, Err: fmt.Errorf("prepare retired baseline path: %w", err)}
	}
	if _, err := os.Stat(previous); err == nil {
		if err := m.rename(previous, retired); err != nil {
			paths := existingPaths(stage, previous)
			return Result{Committed: true, RecoveryPaths: paths}, &CommittedCleanupError{Operation: operation, RecoveryPaths: paths, Err: fmt.Errorf("retire older previous baseline: %w", err)}
		}
	} else if !os.IsNotExist(err) {
		paths := existingPaths(stage)
		return Result{Committed: true, RecoveryPaths: paths}, &CommittedCleanupError{Operation: operation, RecoveryPaths: paths, Err: fmt.Errorf("stat older previous baseline: %w", err)}
	}
	if err := m.rename(stage, previous); err != nil {
		paths := existingPaths(stage, retired)
		return Result{Committed: true, RecoveryPaths: paths}, &CommittedCleanupError{Operation: operation, RecoveryPaths: paths, Err: fmt.Errorf("install replaced baseline as previous: %w", err)}
	}
	if err := m.remove(retired); err != nil {
		paths := existingPaths(previous, retired)
		return Result{Committed: true, RecoveryPaths: paths}, &CommittedCleanupError{Operation: operation, RecoveryPaths: paths, Err: fmt.Errorf("remove retired baseline: %w", err)}
	}
	return Result{Committed: true}, nil
}

func existingPaths(paths ...string) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			result = append(result, path)
		}
	}
	return result
}

func (m *Manager) cloneSet(ctx context.Context, sourceRoot, destinationRoot string) error {
	for _, volume := range m.Volumes {
		source, destination := filepath.Join(sourceRoot, volume), filepath.Join(destinationRoot, volume)
		if _, err := os.Stat(source); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("stat source volume %q: %w", volume, err)
			}
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return fmt.Errorf("create empty source volume %q: %w", volume, err)
			}
			continue
		}
		if err := m.clone(ctx, source, destination); err != nil {
			return fmt.Errorf("clone source volume %q: %w", volume, err)
		}
	}
	return nil
}

func (m *Manager) withLock(run func() (Result, error)) (Result, error) {
	if err := os.MkdirAll(m.ConfigDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create config directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(m.ConfigDir, ".shunt-data.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Result{}, fmt.Errorf("open data baseline lock: %w", err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return Result{}, fmt.Errorf("lock data baseline: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	return run()
}

func (m *Manager) currentRoot() string  { return filepath.Join(m.ConfigDir, "baseline") }
func (m *Manager) previousRoot() string { return filepath.Join(m.ConfigDir, "baseline.previous") }

func (m *Manager) requireVolumes() error {
	if len(m.Volumes) == 0 {
		return errors.New("no data volumes are declared for this app")
	}
	return nil
}

func (m *Manager) ensureWithinConfig(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	configRoot, err := resolveExistingAncestor(m.ConfigDir)
	if err != nil {
		return fmt.Errorf("resolve project config root: %w", err)
	}
	pathRoot, err := resolveExistingAncestor(abs)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	rel, err := filepath.Rel(configRoot, pathRoot)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return fmt.Errorf("%q escapes project config root %q", path, configRoot)
	}
	return nil
}

func resolveExistingAncestor(path string) (string, error) {
	for candidate := filepath.Clean(path); ; candidate = filepath.Dir(candidate) {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", err
		}
	}
}

func validateNames(volumes []string) error {
	seen := make(map[string]struct{}, len(volumes))
	for _, volume := range volumes {
		if volume == "" || volume == "." || volume == ".." || filepath.Base(volume) != volume {
			return fmt.Errorf("unsafe data volume name %q", volume)
		}
		if _, exists := seen[volume]; exists {
			return fmt.Errorf("duplicate data volume name %q", volume)
		}
		seen[volume] = struct{}{}
	}
	return nil
}

func writeMetadata(root string, metadata Metadata) error {
	sort.Strings(metadata.Volumes)
	contents, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal baseline metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, metadataName), append(contents, '\n'), 0o644); err != nil {
		return fmt.Errorf("write baseline metadata: %w", err)
	}
	return nil
}

func validateRoot(root string, volumes []string, metadataRequired bool) error {
	for _, volume := range volumes {
		info, err := os.Stat(filepath.Join(root, volume))
		if err != nil {
			return fmt.Errorf("volume %q: %w", volume, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("volume %q is not a directory", volume)
		}
	}
	if metadataRequired {
		contents, err := os.ReadFile(filepath.Join(root, metadataName))
		if err != nil {
			return fmt.Errorf("read metadata: %w", err)
		}
		var metadata Metadata
		if err := json.Unmarshal(contents, &metadata); err != nil {
			return fmt.Errorf("parse metadata: %w", err)
		}
		if !sameNames(metadata.Volumes, volumes) {
			return fmt.Errorf("metadata volumes %q do not match configured volumes %q", metadata.Volumes, volumes)
		}
	}
	return nil
}

func sameNames(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy, rightCopy := append([]string(nil), left...), append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	for i := range leftCopy {
		if leftCopy[i] != rightCopy[i] {
			return false
		}
	}
	return true
}
