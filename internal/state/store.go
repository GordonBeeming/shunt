package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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
	return withLock(path, func() error { return writeJSON(path, reg) })
}

// CanonicalProject case-folds a project name against the registry. The macOS
// filesystem is case-insensitive, so `cd hubX` and `cd HubX` land in the same
// repo but yield different `basename(cwd)` values; without this, a differently-
// cased cwd forks a phantom project that then collides on the real app's ports.
// When a registered project matches case-insensitively, it returns the REGISTERED
// name + its config dir so every command lines up with the existing registration.
// ok is false when there's no match (a genuinely new project keeps its own name).
func CanonicalProject(name string) (canonicalName, configDir string, ok bool) {
	reg, err := LoadRegistry()
	if err != nil {
		return "", "", false
	}
	return canonicalIn(reg.Projects, name)
}

// canonicalIn is the pure case-fold against a project map (exact match wins,
// then case-insensitive), split out so it's testable without touching disk.
func canonicalIn(projects map[string]string, name string) (canonicalName, configDir string, ok bool) {
	if dir, exact := projects[name]; exact {
		return name, dir, true
	}
	for n, dir := range projects {
		if strings.EqualFold(n, name) {
			return n, dir, true
		}
	}
	return "", "", false
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
	return withLock(path, func() error { return writeJSON(path, app) })
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
func withLock(path string, fn func() error) error {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("create dir for lock: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", lockPath, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
