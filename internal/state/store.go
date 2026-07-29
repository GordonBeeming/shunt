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

// statePath is <configDir>/state.json.
func statePath(configDir string) string {
	return filepath.Join(configDir, "state.json")
}

// LoadApp reads a project's runtime state. Returns ErrNotFound if the project
// hasn't been registered with `shunt app add` yet.
func LoadApp(configDir string) (App, error) {
	var app App
	if err := readJSON(statePath(configDir), &app); err != nil {
		return App{}, err
	}
	if app.Sidings == nil {
		app.Sidings = map[string]Siding{}
	}
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
	return withLock(context.Background(), path, func() error { return writeJSON(path, app) })
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
			return err
		}
		if app.Sidings == nil {
			app.Sidings = map[string]Siding{}
		}
		if err := update(&app); err != nil {
			return err
		}
		return writeJSON(path, app)
	})
	return app, err
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
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename into %s: %w", path, err)
	}
	return nil
}

// withLock takes an exclusive flock on <path>.lock for the duration of fn, so
// concurrent shunt invocations don't interleave reads/writes of shared state.
func withLock(ctx context.Context, path string, fn func() error) error {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("create dir for lock: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	defer f.Close()
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
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
