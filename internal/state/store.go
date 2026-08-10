package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gordonbeeming/shunt/internal/config"
)

// ErrNotFound is returned when a registry or per-project state file is absent.
var ErrNotFound = errors.New("not found")

const (
	legacyStateFilename = "state.json"
	stateFilename       = "state-v2.json"
)

// CommittedDurabilityError means the state file is visible after its atomic
// rename, but the parent-directory flush failed. Callers must not compensate for
// the published state or blindly retry non-idempotent work.
type CommittedDurabilityError struct {
	Path string
	Err  error
}

func (e *CommittedDurabilityError) Error() string {
	return fmt.Sprintf("state %s is visible but durability is unconfirmed; do not retry: %v", e.Path, e.Err)
}

func (e *CommittedDurabilityError) Unwrap() error { return e.Err }

// LoadRegistry reads the global project index, returning an empty (but valid)
// registry if none exists yet.
func LoadRegistry() (Registry, error) {
	path, err := config.RegistryPath()
	if err != nil {
		return Registry{}, err
	}
	var reg Registry
	if err := readJSON(path, &reg); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Registry{Version: RegistryVersion, Projects: map[string]string{}}, nil
		}
		return Registry{}, err
	}
	if reg.Projects == nil {
		reg.Projects = map[string]string{}
	}
	return reg, nil
}

// SaveRegistry writes the global project index atomically under a file lock.
func SaveRegistry(reg Registry) error {
	path, err := config.RegistryPath()
	if err != nil {
		return err
	}
	reg.Version = RegistryVersion
	return withLock(context.Background(), path, func() error { return writeJSON(path, reg) })
}

// UpdateRegistry holds the registry lock across reload, mutation, and atomic
// publication so concurrent project registrations cannot overwrite each other.
func UpdateRegistry(ctx context.Context, mutate func(*Registry) error) (Registry, error) {
	if mutate == nil {
		return Registry{}, errors.New("UpdateRegistry: mutate callback is required")
	}
	path, err := config.RegistryPath()
	if err != nil {
		return Registry{}, err
	}
	var reg Registry
	err = withLock(ctx, path, func() error {
		if err := readJSON(path, &reg); err != nil {
			if !errors.Is(err, ErrNotFound) {
				return err
			}
			reg = Registry{Version: RegistryVersion, Projects: map[string]string{}}
		}
		if reg.Projects == nil {
			reg.Projects = map[string]string{}
		}
		if err := mutate(&reg); err != nil {
			return err
		}
		reg.Version = RegistryVersion
		return writeJSON(path, reg)
	})
	return reg, err
}

// statePath is the version-owned state file. Older binaries continue to use
// state.json, which this version ignores once state-v2.json exists.
func statePath(configDir string) string {
	return filepath.Join(configDir, stateFilename)
}

func legacyStatePath(configDir string) string {
	return filepath.Join(configDir, legacyStateFilename)
}

// LoadApp reads a project's runtime state. Returns ErrNotFound if the project
// hasn't been registered with `shunt app add` yet.
func LoadApp(configDir string) (App, error) {
	var app App
	path := statePath(configDir)
	if err := readJSON(path, &app); err != nil {
		if !errors.Is(err, ErrNotFound) {
			return App{}, err
		}
		path = legacyStatePath(configDir)
		if err := readJSON(path, &app); err != nil {
			return App{}, err
		}
		if err := requireMigratableStateVersion(path, app.Version); err != nil {
			return App{}, err
		}
	} else if err := requireSupportedStateVersion(path, app.Version); err != nil {
		return App{}, err
	}
	projectCompatibility(&app)
	return app, nil
}

// SaveApp writes a project's runtime state atomically under a file lock.
func SaveApp(app App) error {
	if app.ConfigDir == "" {
		return errors.New("SaveApp: app.ConfigDir is empty")
	}
	if err := os.MkdirAll(app.ConfigDir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	path := statePath(app.ConfigDir)
	return withLock(context.Background(), path, func() error {
		if err := requireMigratableStateVersion(path, app.Version); err != nil {
			return err
		}
		if err := rejectUnsupportedSave(path); err != nil {
			return err
		}
		EnsureV2(&app)
		return writeJSON(path, app)
	})
}

// UpdateApp holds the state-file lock across reload, mutation, and publication.
// The callback only changes in-memory state; guest and routing work stays outside
// this short lock.
func UpdateApp(ctx context.Context, configDir string, update func(*App) error) (App, error) {
	if configDir == "" {
		return App{}, errors.New("UpdateApp: configDir is empty")
	}
	if update == nil {
		return App{}, errors.New("UpdateApp: update callback is required")
	}
	path := statePath(configDir)
	var app App
	err := withLock(ctx, path, func() error {
		if err := readJSON(path, &app); err != nil {
			if !errors.Is(err, ErrNotFound) {
				return err
			}
			if err := readJSON(legacyStatePath(configDir), &app); err != nil {
				return err
			}
			if err := requireMigratableStateVersion(legacyStatePath(configDir), app.Version); err != nil {
				return err
			}
		} else if err := requireSupportedStateVersion(path, app.Version); err != nil {
			return err
		}
		projectCompatibility(&app)
		if err := update(&app); err != nil {
			return err
		}
		EnsureV2(&app)
		return writeJSON(path, app)
	})
	return app, err
}

func rejectUnsupportedSave(path string) error {
	var current struct {
		Version int `json:"version"`
	}
	if err := readJSON(path, &current); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	return requireSupportedStateVersion(path, current.Version)
}

func requireSupportedStateVersion(path string, version int) error {
	if version != StateVersion {
		return fmt.Errorf("unsupported state version %d in %s; this binary supports version %d", version, path, StateVersion)
	}
	return nil
}

func requireMigratableStateVersion(path string, version int) error {
	if version < 0 || version > StateVersion {
		return fmt.Errorf("unsupported state version %d in %s; this binary supports migration through version %d", version, path, StateVersion)
	}
	return nil
}

// --- low-level helpers ---

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// writeJSON writes v as indented JSON via a temp file + atomic rename so a
// crash mid-write can't corrupt the file.
func writeJSON(path string, v any) error {
	return writeJSONWithDirectorySync(path, v, syncDirectory)
}

func writeJSONWithDirectorySync(path string, v any, syncParent func(string) error) error {
	if syncParent == nil {
		return errors.New("state directory sync callback is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create dir for %s: %w", path, err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename into %s: %w", path, err)
	}
	if err := syncParent(filepath.Dir(path)); err != nil {
		return &CommittedDurabilityError{Path: path, Err: fmt.Errorf("sync parent directory: %w", err)}
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// withLock takes an exclusive flock on <path>.lock for the duration of fn, so
// concurrent shunt invocations don't interleave reads/writes of shared state.
func withLock(ctx context.Context, path string, fn func() error) error {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("create dir for lock: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("secure lock %s: %w", lockPath, err)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("lock %s: %w", lockPath, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("lock %s: %w", lockPath, ctx.Err())
		case <-ticker.C:
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
