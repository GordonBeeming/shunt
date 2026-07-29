package siding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gordonbeeming/shunt/internal/state"
	"golang.org/x/sys/unix"
)

const operationLockName = ".shunt-operation.lock"

// Lock order is project, siding, then state. Siding lifecycle work holds a
// shared project lock, so different sidings can run together while app config,
// routing, and data-baseline changes take the project lock exclusively. The
// state lock is only held while reloading, merging, and writing state.json.

// WithProjectOperation serializes routing, app configuration, and data-baseline
// changes for one project.
func WithProjectOperation(ctx context.Context, configDir string, operation func() error) error {
	return withProjectLock(ctx, configDir, unix.LOCK_EX, operation)
}

// WithSidingOperation runs long guest or worktree work under a shared project
// lock and one exclusive siding lock.
func WithSidingOperation(ctx context.Context, configDir, name string, operation func() error) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	return withProjectLock(ctx, configDir, unix.LOCK_SH, func() error {
		return withSidingLock(ctx, configDir, name, operation)
	})
}

// WithProjectSidingOperation is for a project-wide transaction that also acts
// on one guest, such as data promotion. It keeps the global lock order in one
// place instead of letting callers acquire a project lock after a siding lock.
func WithProjectSidingOperation(ctx context.Context, configDir, name string, operation func() error) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	return withProjectLock(ctx, configDir, unix.LOCK_EX, func() error {
		return withSidingLock(ctx, configDir, name, operation)
	})
}

func withProjectLock(ctx context.Context, configDir string, mode int, operation func() error) error {
	if operation == nil {
		return errors.New("project operation is required")
	}
	configDir = filepath.Clean(configDir)
	if !filepath.IsAbs(configDir) {
		return fmt.Errorf("project config directory must be absolute: %q", configDir)
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("create project config directory: %w", err)
	}
	return withFileLock(ctx, filepath.Join(configDir, operationLockName), mode, "project lifecycle", operation)
}

func withSidingLock(ctx context.Context, configDir, name string, operation func() error) error {
	if operation == nil {
		return errors.New("siding operation is required")
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	path := filepath.Join(filepath.Clean(configDir), ".shunt-siding-"+name+".lock")
	return withFileLock(ctx, path, unix.LOCK_EX, "siding "+name, operation)
}

func withFileLock(ctx context.Context, path string, mode int, description string, operation func() error) error {
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open %s lock: %w", description, err)
	}
	defer lock.Close()

	for {
		err = unix.Flock(int(lock.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("lock %s: %w", description, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	return operation()
}

// MergeSidingState changes one siding in the latest state snapshot.
func MergeSidingState(ctx context.Context, configDir string, sd state.Siding, clearLive bool) (state.App, error) {
	if err := ValidateName(sd.Name); err != nil {
		return state.App{}, err
	}
	return state.UpdateApp(ctx, configDir, func(app *state.App) error {
		app.Sidings[sd.Name] = sd
		if clearLive && app.LiveSiding == sd.Name {
			app.LiveSiding = ""
		}
		return nil
	})
}

// RemoveSidingState removes one siding from the latest state snapshot.
func RemoveSidingState(ctx context.Context, configDir, name string) (state.App, error) {
	if err := ValidateName(name); err != nil {
		return state.App{}, err
	}
	return state.UpdateApp(ctx, configDir, func(app *state.App) error {
		delete(app.Sidings, name)
		if app.LiveSiding == name {
			app.LiveSiding = ""
		}
		return nil
	})
}
