// Package databaseline manages the promotable, host-backed data set shared by
// a project's sidings.
package databaseline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/fsclone"
	"golang.org/x/sys/unix"
)

const (
	metadataName        = ".shunt-baseline.json"
	stateName           = ".shunt-baseline-state.json"
	stateVersion        = 1
	generationsName     = ".shunt-baseline-generations"
	generationLocks     = ".shunt-baseline-generation-locks"
	generationPrefix    = "generation-"
	stagePrefix         = ".stage-"
	stateTempPrefix     = ".shunt-baseline-state.tmp-"
	emptyBaselineSource = "empty"
)

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

// CommittedCleanupError means the requested state is installed, but cleanup
// after that commit failed. RecoveryPaths are safe to remove after confirming
// they are not named by the state manifest.
type CommittedCleanupError struct {
	Operation     string
	RecoveryPaths []string
	Err           error
}

func (e *CommittedCleanupError) Error() string {
	return fmt.Sprintf("%s committed, but cleanup failed; recover from %v: %v", e.Operation, e.RecoveryPaths, e.Err)
}

func (e *CommittedCleanupError) Unwrap() error { return e.Err }

// CleanupError reports uncommitted leftovers from an interrupted or failed
// operation. The canonical manifest is unchanged.
type CleanupError struct {
	Operation     string
	RecoveryPaths []string
	Err           error
}

func (e *CleanupError) Error() string {
	return fmt.Sprintf("%s did not commit and cleanup failed; recover from %v: %v", e.Operation, e.RecoveryPaths, e.Err)
}

func (e *CleanupError) Unwrap() error { return e.Err }

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
// quiesce, capture, or snapshot sequence.
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
	SnapshotHostVolume(context.Context, string, string) error
	Restore(context.Context) (RestoreResult, error)
}

// VolumeCapture writes one configured volume into destination. A missing source
// volume is represented by an empty destination directory.
type VolumeCapture func(context.Context, string, string) error

type stateManifest struct {
	Version  int    `json:"version"`
	Current  string `json:"current"`
	Previous string `json:"previous,omitempty"`
}

type generationLease struct {
	file *os.File
}

func (l *generationLease) close() {
	if l == nil || l.file == nil {
		return
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	_ = l.file.Close()
}

// Manager operates on a project's configured data-volume set.
type Manager struct {
	ConfigDir string
	Volumes   []string

	clock          func() time.Time
	clone          func(context.Context, string, string) error
	cloneVolumeSet func(context.Context, string, string, []string) (fsclone.VolumeSetResult, error)
	rename         func(string, string) error
	remove         func(string) error
	write          func(string, Metadata) error
	failpoint      func(string)
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
		ConfigDir:      filepath.Clean(abs),
		Volumes:        append([]string(nil), volumes...),
		clock:          time.Now,
		clone:          fsclone.CloneVolume,
		cloneVolumeSet: fsclone.CloneVolumeSetResult,
		rename:         os.Rename,
		remove:         os.RemoveAll,
		write:          writeMetadata,
		failpoint:      func(string) {},
	}, nil
}

// Promote atomically publishes a clone of every configured volume from source.
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

	var cleanup []string
	result, operationErr := m.withLock(ctx, func() (Result, error) {
		state, recovery, err := m.loadState()
		cleanup = append(cleanup, recovery...)
		if err != nil {
			return Result{}, err
		}
		result, leftovers, err := m.publishGeneration(ctx, state, sourceSiding, func(stage string) error {
			return m.cloneSet(ctx, sourceRoot, stage)
		})
		cleanup = append(cleanup, leftovers...)
		return result, err
	})
	return m.finishOperation("promote", result, operationErr, cleanup)
}

// Seed installs the first complete baseline only when canonical state is truly
// absent. Invalid, incomplete, unreadable, and legacy layouts fail closed.
func (m *Manager) Seed(ctx context.Context, source string, capture VolumeCapture) (Result, error) {
	if err := m.requireVolumes(); err != nil {
		return Result{}, err
	}
	if source == "" {
		return Result{}, errors.New("baseline seed source is required")
	}
	if capture == nil {
		return Result{}, errors.New("baseline seed capture is required")
	}

	var cleanup []string
	result, operationErr := m.withLock(ctx, func() (Result, error) {
		state, recovery, err := m.loadState()
		cleanup = append(cleanup, recovery...)
		if err != nil {
			return Result{}, err
		}
		if state != nil {
			return Result{}, nil
		}
		result, leftovers, err := m.publishGeneration(ctx, nil, source, func(stage string) error {
			for _, volume := range m.Volumes {
				if err := capture(ctx, volume, filepath.Join(stage, volume)); err != nil {
					return fmt.Errorf("capture seed volume %q: %w", volume, err)
				}
			}
			return nil
		})
		cleanup = append(cleanup, leftovers...)
		return result, err
	})
	return m.finishOperation("seed", result, operationErr, cleanup)
}

// InitializeEmpty publishes a first canonical generation containing every
// declared volume as an empty directory. It is a no-op when valid state exists.
func (m *Manager) InitializeEmpty(ctx context.Context) (Result, error) {
	return m.Seed(ctx, emptyBaselineSource, func(ctx context.Context, _ string, destination string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Mkdir(destination, 0o755); err != nil {
			return fmt.Errorf("create empty volume: %w", err)
		}
		return nil
	})
}

// PromoteWithLifecycle keeps quiescence, capture, and manifest publication
// serialized. Restore and retired-generation cleanup run after releasing the
// canonical mutation lock.
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

	var cleanup []string
	lifecycleStarted := false
	result, operationErr := m.withLock(ctx, func() (Result, error) {
		state, recovery, err := m.loadState()
		cleanup = append(cleanup, recovery...)
		if err != nil {
			return Result{}, err
		}
		lifecycleStarted = true
		if err := lifecycle.Quiesce(ctx); err != nil {
			return Result{}, err
		}
		if err := lifecycle.StopVolumeConsumers(ctx); err != nil {
			return Result{}, err
		}
		if err := lifecycle.Sync(ctx); err != nil {
			return Result{}, err
		}
		result, leftovers, err := m.publishGeneration(ctx, state, sourceSiding, func(stage string) error {
			for _, volume := range m.Volumes {
				if err := lifecycle.SnapshotHostVolume(ctx, volume, filepath.Join(stage, volume)); err != nil {
					return fmt.Errorf("snapshot host volume %q: %w", volume, err)
				}
			}
			return nil
		})
		cleanup = append(cleanup, leftovers...)
		return result, err
	})

	if lifecycleStarted {
		// Once quiescence begins, caller cancellation must not prevent the restore
		// attempt or obscure whether publication reached its atomic commit point.
		restore, restoreErr := lifecycle.Restore(context.WithoutCancel(ctx))
		result.Restore = restore
		result, operationErr = m.finishOperation("promote", result, operationErr, cleanup)
		if restoreErr != nil {
			return result, &RestoreError{Committed: result.Committed, Restore: restore, OperationErr: operationErr, Err: restoreErr}
		}
		return result, operationErr
	}
	return m.finishOperation("promote", result, operationErr, cleanup)
}

// Rollback atomically makes the immediately preceding generation current.
func (m *Manager) Rollback() (Result, error) {
	return m.RollbackContext(context.Background())
}

// RollbackContext is Rollback with cancellation-aware lock acquisition.
func (m *Manager) RollbackContext(ctx context.Context) (Result, error) {
	if err := m.requireVolumes(); err != nil {
		return Result{}, err
	}
	var cleanup []string
	result, operationErr := m.withLock(ctx, func() (Result, error) {
		state, recovery, err := m.loadState()
		cleanup = append(cleanup, recovery...)
		if err != nil {
			return Result{}, err
		}
		if state == nil {
			return Result{}, errors.New("data baseline is not initialized")
		}
		if state.Previous == "" {
			return Result{}, errors.New("no previous data baseline generation is available")
		}
		next := stateManifest{Version: stateVersion, Current: state.Previous, Previous: state.Current}
		leftovers, err := m.publishState(next)
		cleanup = append(cleanup, leftovers...)
		if err != nil {
			return Result{}, fmt.Errorf("publish rollback state: %w", err)
		}
		return Result{Committed: true}, nil
	})
	return m.finishOperation("rollback", result, operationErr, cleanup)
}

// Cleanup validates canonical state and removes transaction artifacts and
// generations no longer referenced as current or previous.
func (m *Manager) Cleanup(ctx context.Context) (Result, error) {
	if err := m.requireVolumes(); err != nil {
		return Result{}, err
	}
	var cleanup []string
	result, operationErr := m.withLock(ctx, func() (Result, error) {
		_, recovery, err := m.loadState()
		cleanup = append(cleanup, recovery...)
		if err != nil {
			return Result{}, err
		}
		return Result{Committed: true}, nil
	})
	return m.finishOperation("cleanup", result, operationErr, cleanup)
}

// ResetVolumeRoot replaces a siding's entire volume root with a leased clone
// of the current immutable generation. Empty canonical state must be created
// explicitly with InitializeEmpty first.
func (m *Manager) ResetVolumeRoot(ctx context.Context, destinationRoot string) (Result, error) {
	if err := m.requireVolumes(); err != nil {
		return Result{}, err
	}
	if err := m.ensureWithinConfig(destinationRoot); err != nil {
		return Result{}, fmt.Errorf("destination volume root: %w", err)
	}

	var cleanup []string
	var sourceRoot string
	var lease *generationLease
	_, lockErr := m.withLock(ctx, func() (Result, error) {
		state, recovery, err := m.loadState()
		cleanup = append(cleanup, recovery...)
		if err != nil {
			return Result{}, err
		}
		if state == nil {
			return Result{}, errors.New("data baseline is not initialized; initialize the declared empty volumes first")
		}
		lease, err = m.acquireGenerationLease(ctx, state.Current, unix.LOCK_SH)
		if err != nil {
			return Result{}, fmt.Errorf("lease current baseline generation: %w", err)
		}
		sourceRoot = m.generationRoot(state.Current)
		return Result{}, nil
	})
	if lockErr != nil {
		if lease != nil {
			lease.close()
		}
		return m.finishOperation("reset", Result{}, lockErr, cleanup)
	}

	cloneResult, cloneErr := m.cloneVolumeSet(ctx, sourceRoot, destinationRoot, m.Volumes)
	lease.close()
	result := Result{Committed: cloneResult.Committed, RecoveryPaths: append([]string(nil), cloneResult.RecoveryPaths...)}
	return m.finishOperation("reset", result, cloneErr, cleanup)
}

func (m *Manager) publishGeneration(ctx context.Context, state *stateManifest, source string, capture func(string) error) (Result, []string, error) {
	if err := os.MkdirAll(m.generationsRoot(), 0o700); err != nil {
		return Result{}, nil, fmt.Errorf("create baseline generations directory: %w", err)
	}
	stage, err := os.MkdirTemp(m.generationsRoot(), stagePrefix)
	if err != nil {
		return Result{}, nil, fmt.Errorf("create baseline generation stage: %w", err)
	}
	leftovers := []string{stage}
	if err := capture(stage); err != nil {
		return Result{}, leftovers, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, leftovers, err
	}
	metadata := Metadata{SourceSiding: source, Timestamp: m.clock().UTC(), Volumes: append([]string(nil), m.Volumes...)}
	if err := m.write(stage, metadata); err != nil {
		return Result{}, leftovers, err
	}
	if err := validateRoot(stage, m.Volumes); err != nil {
		return Result{}, leftovers, fmt.Errorf("validate staged baseline generation: %w", err)
	}

	generationID := m.newGenerationID(filepath.Base(stage))
	generationPath := m.generationRoot(generationID)
	if err := m.rename(stage, generationPath); err != nil {
		return Result{}, leftovers, fmt.Errorf("install immutable baseline generation: %w", err)
	}
	leftovers = []string{generationPath}
	m.failpoint("generation-installed")

	next := stateManifest{Version: stateVersion, Current: generationID}
	if state != nil {
		next.Previous = state.Current
	}
	stateLeftovers, err := m.publishState(next)
	leftovers = append(leftovers, stateLeftovers...)
	if err != nil {
		return Result{}, leftovers, fmt.Errorf("publish baseline state: %w", err)
	}

	// The generation is now referenced by the atomically published manifest.
	leftovers = stateLeftovers
	if state != nil && state.Previous != "" {
		leftovers = append(leftovers, m.generationRoot(state.Previous))
	}
	return Result{Committed: true}, leftovers, nil
}

func (m *Manager) publishState(state stateManifest) ([]string, error) {
	contents, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal baseline state: %w", err)
	}
	temp, err := os.CreateTemp(m.ConfigDir, stateTempPrefix)
	if err != nil {
		return nil, fmt.Errorf("create baseline state stage: %w", err)
	}
	tempPath := temp.Name()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return []string{tempPath}, fmt.Errorf("secure baseline state stage: %w", err)
	}
	if _, err := temp.Write(append(contents, '\n')); err != nil {
		_ = temp.Close()
		return []string{tempPath}, fmt.Errorf("write baseline state stage: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return []string{tempPath}, fmt.Errorf("sync baseline state stage: %w", err)
	}
	if err := temp.Close(); err != nil {
		return []string{tempPath}, fmt.Errorf("close baseline state stage: %w", err)
	}
	m.failpoint("manifest-staged")
	if err := m.rename(tempPath, m.statePath()); err != nil {
		return []string{tempPath}, err
	}
	m.failpoint("manifest-published")
	return nil, nil
}

func (m *Manager) finishOperation(operation string, result Result, operationErr error, cleanup []string) (Result, error) {
	failedPaths, cleanupErr := m.cleanupPaths(cleanup)
	result.RecoveryPaths = mergePaths(result.RecoveryPaths, failedPaths)
	if cleanupErr == nil && len(result.RecoveryPaths) == 0 {
		return result, operationErr
	}
	combined := errors.Join(operationErr, cleanupErr)
	if combined == nil {
		combined = errors.New("cleanup left recoverable paths")
	}
	if result.Committed {
		return result, &CommittedCleanupError{Operation: operation, RecoveryPaths: result.RecoveryPaths, Err: combined}
	}
	if len(result.RecoveryPaths) > 0 {
		return result, &CleanupError{Operation: operation, RecoveryPaths: result.RecoveryPaths, Err: combined}
	}
	return result, combined
}

func (m *Manager) cleanupPaths(paths []string) ([]string, error) {
	paths = mergePaths(paths)
	var failed []string
	var errs []error
	for _, path := range paths {
		if !m.isCleanupPath(path) {
			failed = append(failed, path)
			errs = append(errs, fmt.Errorf("refuse cleanup outside baseline transaction paths: %s", path))
			continue
		}
		if err := m.cleanupPath(path); err != nil {
			if pathExists(path) {
				failed = append(failed, path)
			}
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	return mergePaths(failed), errors.Join(errs...)
}

func (m *Manager) cleanupPath(path string) error {
	if filepath.Dir(path) == m.generationsRoot() && strings.HasPrefix(filepath.Base(path), generationPrefix) {
		lease, err := m.acquireGenerationLease(context.Background(), filepath.Base(path), unix.LOCK_EX|unix.LOCK_NB)
		if err != nil {
			return err
		}
		defer lease.close()
	}
	return m.remove(path)
}

func (m *Manager) isCleanupPath(path string) bool {
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	if filepath.Dir(clean) == m.generationsRoot() {
		return strings.HasPrefix(base, generationPrefix) || strings.HasPrefix(base, stagePrefix)
	}
	return filepath.Dir(clean) == m.ConfigDir && strings.HasPrefix(base, stateTempPrefix)
}

func (m *Manager) loadState() (*stateManifest, []string, error) {
	if err := m.rejectLegacyLayout(); err != nil {
		return nil, nil, err
	}

	temps, err := m.stateTemps()
	if err != nil {
		return nil, nil, err
	}
	stateInfo, err := os.Lstat(m.statePath())
	if os.IsNotExist(err) {
		absent, absentErr := m.verifyAbsentState(temps)
		if absentErr != nil {
			return nil, nil, absentErr
		}
		if absent {
			return nil, nil, nil
		}
		return nil, nil, errors.New("data baseline state is incomplete; remove the interrupted generation artifacts before initializing")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("inspect baseline state: %w", err)
	}
	if !stateInfo.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("baseline state %q is not a regular file", m.statePath())
	}
	contents, err := os.ReadFile(m.statePath())
	if err != nil {
		return nil, nil, fmt.Errorf("read baseline state: %w", err)
	}
	var state stateManifest
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, nil, fmt.Errorf("parse baseline state: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, nil, fmt.Errorf("parse baseline state: %w", err)
	}
	if state.Version != stateVersion {
		return nil, nil, fmt.Errorf("unsupported baseline state version %d; expected %d and no migration is available", state.Version, stateVersion)
	}
	if err := validateGenerationID(state.Current); err != nil {
		return nil, nil, fmt.Errorf("invalid current generation: %w", err)
	}
	if state.Previous != "" {
		if err := validateGenerationID(state.Previous); err != nil {
			return nil, nil, fmt.Errorf("invalid previous generation: %w", err)
		}
		if state.Previous == state.Current {
			return nil, nil, errors.New("baseline current and previous generations must differ")
		}
	}

	generationInfo, err := os.Lstat(m.generationsRoot())
	if err != nil {
		return nil, nil, fmt.Errorf("inspect baseline generations: %w", err)
	}
	if !generationInfo.IsDir() || generationInfo.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("baseline generations path %q is not a directory", m.generationsRoot())
	}
	if err := validateRoot(m.generationRoot(state.Current), m.Volumes); err != nil {
		return nil, nil, fmt.Errorf("current baseline generation %q: %w", state.Current, err)
	}
	if state.Previous != "" {
		if err := validateRoot(m.generationRoot(state.Previous), m.Volumes); err != nil {
			return nil, nil, fmt.Errorf("previous baseline generation %q: %w", state.Previous, err)
		}
	}

	entries, err := os.ReadDir(m.generationsRoot())
	if err != nil {
		return nil, nil, fmt.Errorf("read baseline generations: %w", err)
	}
	referenced := map[string]struct{}{state.Current: {}}
	if state.Previous != "" {
		referenced[state.Previous] = struct{}{}
	}
	recovery := append([]string(nil), temps...)
	for _, entry := range entries {
		name := entry.Name()
		if _, ok := referenced[name]; ok {
			continue
		}
		if strings.HasPrefix(name, generationPrefix) || strings.HasPrefix(name, stagePrefix) {
			recovery = append(recovery, filepath.Join(m.generationsRoot(), name))
			continue
		}
		return nil, nil, fmt.Errorf("unsupported entry %q in baseline generations directory", name)
	}
	return &state, mergePaths(recovery), nil
}

func (m *Manager) verifyAbsentState(temps []string) (bool, error) {
	if len(temps) > 0 {
		return false, nil
	}
	info, err := os.Lstat(m.generationsRoot())
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect baseline generations: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("baseline generations path %q is not a directory", m.generationsRoot())
	}
	entries, err := os.ReadDir(m.generationsRoot())
	if err != nil {
		return false, fmt.Errorf("read baseline generations: %w", err)
	}
	return len(entries) == 0, nil
}

func (m *Manager) rejectLegacyLayout() error {
	var found []string
	for _, name := range []string{"baseline", "baseline.previous"} {
		path := filepath.Join(m.ConfigDir, name)
		if _, err := os.Lstat(path); err == nil {
			found = append(found, path)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect legacy baseline path %q: %w", path, err)
		}
	}
	if len(found) > 0 {
		return fmt.Errorf("unsupported pre-generation baseline layout at %v; no migration is available", found)
	}
	return nil
}

func (m *Manager) stateTemps() ([]string, error) {
	entries, err := os.ReadDir(m.ConfigDir)
	if err != nil {
		return nil, fmt.Errorf("read config directory for baseline recovery: %w", err)
	}
	var result []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), stateTempPrefix) {
			result = append(result, filepath.Join(m.ConfigDir, entry.Name()))
		}
	}
	return mergePaths(result), nil
}

func (m *Manager) acquireGenerationLease(ctx context.Context, generation string, operation int) (*generationLease, error) {
	if err := validateGenerationID(generation); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.generationLocksRoot(), 0o700); err != nil {
		return nil, fmt.Errorf("create baseline generation locks: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(m.generationLocksRoot(), generation+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open baseline generation lock: %w", err)
	}
	if err := flockContext(ctx, lock, operation); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &generationLease{file: lock}, nil
}

func (m *Manager) cloneSet(ctx context.Context, sourceRoot, destinationRoot string) error {
	for _, volume := range m.Volumes {
		if err := ctx.Err(); err != nil {
			return err
		}
		source, destination := filepath.Join(sourceRoot, volume), filepath.Join(destinationRoot, volume)
		info, err := os.Lstat(source)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("source volume %q is missing", volume)
			}
			return fmt.Errorf("stat source volume %q: %w", volume, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("source volume %q is not a directory", volume)
		}
		if err := m.clone(ctx, source, destination); err != nil {
			return fmt.Errorf("clone source volume %q: %w", volume, err)
		}
	}
	return nil
}

func (m *Manager) withLock(ctx context.Context, run func() (Result, error)) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("data baseline context is required")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(m.ConfigDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create config directory: %w", err)
	}
	// Lock the configuration directory itself so validation of invalid state does
	// not create or rewrite canonical files merely to discover the error.
	lock, err := os.Open(m.ConfigDir)
	if err != nil {
		return Result{}, fmt.Errorf("open data baseline lock: %w", err)
	}
	defer lock.Close()
	if err := flockContext(ctx, lock, unix.LOCK_EX); err != nil {
		return Result{}, fmt.Errorf("lock data baseline: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	return run()
}

func flockContext(ctx context.Context, file *os.File, operation int) error {
	if operation&unix.LOCK_NB != 0 {
		if err := unix.Flock(int(file.Fd()), operation); err != nil {
			return err
		}
		return nil
	}
	for delay := 5 * time.Millisecond; ; {
		if err := unix.Flock(int(file.Fd()), operation|unix.LOCK_NB); err == nil {
			return nil
		} else if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
		if delay < 50*time.Millisecond {
			delay *= 2
		}
	}
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
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
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

func (m *Manager) statePath() string           { return filepath.Join(m.ConfigDir, stateName) }
func (m *Manager) generationsRoot() string     { return filepath.Join(m.ConfigDir, generationsName) }
func (m *Manager) generationLocksRoot() string { return filepath.Join(m.ConfigDir, generationLocks) }
func (m *Manager) generationRoot(id string) string {
	return filepath.Join(m.generationsRoot(), id)
}

func (m *Manager) newGenerationID(stageBase string) string {
	suffix := strings.TrimPrefix(stageBase, stagePrefix)
	return generationPrefix + m.clock().UTC().Format("20060102T150405.000000000Z") + "-" + suffix
}

func (m *Manager) requireVolumes() error {
	if len(m.Volumes) == 0 {
		return errors.New("no data volumes are declared for this app")
	}
	return nil
}

func validateGenerationID(id string) error {
	if id == "" || filepath.Base(id) != id || !strings.HasPrefix(id, generationPrefix) || len(id) == len(generationPrefix) {
		return fmt.Errorf("unsafe generation identifier %q", id)
	}
	return nil
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

func validateRoot(root string, volumes []string) error {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("generation root is not a directory")
	}
	wantEntries := map[string]struct{}{metadataName: {}}
	for _, volume := range volumes {
		wantEntries[volume] = struct{}{}
		info, err := os.Lstat(filepath.Join(root, volume))
		if err != nil {
			return fmt.Errorf("volume %q: %w", volume, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("volume %q is not a directory", volume)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read generation root: %w", err)
	}
	for _, entry := range entries {
		if _, ok := wantEntries[entry.Name()]; !ok {
			return fmt.Errorf("unexpected generation entry %q", entry.Name())
		}
	}
	if len(entries) != len(wantEntries) {
		return errors.New("generation root is incomplete")
	}

	metadataPath := filepath.Join(root, metadataName)
	metadataInfo, err := os.Lstat(metadataPath)
	if err != nil {
		return fmt.Errorf("inspect metadata: %w", err)
	}
	if !metadataInfo.Mode().IsRegular() {
		return errors.New("metadata is not a regular file")
	}
	contents, err := os.ReadFile(metadataPath)
	if err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}
	var metadata Metadata
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return fmt.Errorf("parse metadata: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return fmt.Errorf("parse metadata: %w", err)
	}
	if metadata.SourceSiding == "" || metadata.Timestamp.IsZero() {
		return errors.New("metadata source and timestamp are required")
	}
	if !sameNames(metadata.Volumes, volumes) {
		return fmt.Errorf("metadata volumes %q do not match configured volumes %q", metadata.Volumes, volumes)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("unexpected trailing JSON value")
	}
	return err
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

func mergePaths(groups ...[]string) []string {
	seen := make(map[string]struct{})
	for _, group := range groups {
		for _, path := range group {
			if path != "" {
				seen[filepath.Clean(path)] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(seen))
	for path := range seen {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
