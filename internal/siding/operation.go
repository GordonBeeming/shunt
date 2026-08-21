package siding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/state"
	"golang.org/x/sys/unix"
)

const operationLockName = ".shunt-operation.lock"

var (
	// lockWaitGracePeriod is how long a lock wait stays silent before the first
	// report — a normal fast handoff (the common case) never prints anything.
	lockWaitGracePeriod = 2 * time.Second
	// lockWaitReportInterval is how often a wait past the grace period repeats
	// its report, so a long wait keeps showing progress instead of going quiet.
	lockWaitReportInterval = 15 * time.Second
	// lockWaitReportWriter is the wait-report seam, so tests can capture it
	// without redirecting the real os.Stderr.
	lockWaitReportWriter = io.Writer(os.Stderr)
)

// EnsureNoRemovalInProgress prevents lifecycle work from racing a resumable
// removal after it has published any destructive checkpoint. Callers load state
// after acquiring their project and siding locks before invoking it.
func EnsureNoRemovalInProgress(app state.App, operation string) error {
	if app.Removal == nil {
		return nil
	}
	return fmt.Errorf("%s is blocked while siding %q removal is at stage %q", operation, app.Removal.Siding, app.Removal.Stage)
}

func isCommittedStatePublication(err error) bool {
	var committed *state.CommittedDurabilityError
	return errors.As(err, &committed)
}

// Lock order is project, siding, then state. Siding lifecycle work holds a
// shared project lock, so different sidings can run together while app config,
// routing, and data-baseline changes take the project lock exclusively. The
// state lock is only held while reloading, merging, and writing state-v2.json.

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
	if err := lock.Chmod(0o600); err != nil {
		return fmt.Errorf("secure %s lock: %w", description, err)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	waitStart := time.Now()
	var lastReport time.Time
	for {
		err = unix.Flock(int(lock.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("lock %s: %w", description, err)
		}
		// A guest command with no output for the whole time it holds this lock
		// (the exact shape of the hang this exists to explain) must not leave a
		// contending caller looking hung too — report the wait, not a deadline;
		// legitimate long holds (a many-minute `up`) must never be cut off.
		if waited := time.Since(waitStart); waited >= lockWaitGracePeriod && time.Since(lastReport) >= lockWaitReportInterval {
			reportLockWait(path, description, waited)
			lastReport = time.Now()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("lock %s: %w", description, ctx.Err())
		case <-ticker.C:
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if mode&unix.LOCK_EX != 0 {
		// Only an exclusive holder writes the record: a shared lock can have
		// several concurrent holders, and they'd only race each other's writes
		// for a record that couldn't identify "the" holder anyway.
		writeLockHolderRecord(lock)
		// Defers run last-in-first-out, so this clears the record before the
		// flock above is released — a stale record left past release would name
		// this process for a lock it no longer holds, and a reused pid would then
		// let a later, unrelated holder be named by an earlier one's leftovers.
		// A crash still leaves the record behind; that's what processAlive guards.
		defer clearLockHolderRecord(lock)
	}
	return operation()
}

// lockHolderRecord identifies the process holding an exclusive flock, so a
// caller stuck waiting on it can say more than "waiting". It is written only
// while that flock is held, so there is no race between the write and a
// concurrent reader — the reader is, by definition, still blocked on the
// same flock.
type lockHolderRecord struct {
	PID        int       `json:"pid"`
	Command    string    `json:"command"`
	AcquiredAt time.Time `json:"acquiredAt"`
}

// writeLockHolderRecord annotates lock (already flocked exclusively by this
// process) with who's holding it. A lock that can't be annotated must still
// work, so every failure here is swallowed rather than surfaced.
func writeLockHolderRecord(lock *os.File) {
	data, err := json.Marshal(lockHolderRecord{
		PID:        os.Getpid(),
		Command:    strings.Join(os.Args, " "),
		AcquiredAt: time.Now(),
	})
	if err != nil {
		return
	}
	if err := lock.Truncate(0); err != nil {
		return
	}
	_, _ = lock.WriteAt(data, 0)
}

// clearLockHolderRecord truncates lock's holder record on release, so a
// waiter that finds the file non-empty knows it names the *current* holder
// (or, if the process crashed instead of returning normally, an evidenced but
// unconfirmed one — never a departed holder whose pid has since been reused).
// Same swallow-all-failures rule as writeLockHolderRecord: this is cleanup,
// not part of the lock protocol, and must never fail the operation.
func clearLockHolderRecord(lock *os.File) {
	_ = lock.Truncate(0)
}

// readLockHolderRecord reads path's holder record with a fresh handle,
// independent of the caller's own (still-blocked) lock attempt. A missing,
// unparseable, or dead-pid record is reported as "no named holder" rather
// than risking a wrong name — the record is only trustworthy evidence, never
// proof, since nothing guarantees the writer cleaned up after a crash.
func readLockHolderRecord(path string) (lockHolderRecord, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return lockHolderRecord{}, false
	}
	var record lockHolderRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return lockHolderRecord{}, false
	}
	if !processAlive(record.PID) {
		return lockHolderRecord{}, false
	}
	return record, true
}

// processAlive uses the standard existence probe (signal 0): an error other
// than "operation not permitted" means the pid isn't a live process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

// reportLockWait prints one progress line to lockWaitReportWriter for a lock
// wait that has passed the grace period. It never blocks and never fails the
// caller — this is diagnostic output, not part of the lock protocol.
func reportLockWait(path, description string, waited time.Duration) {
	if record, ok := readLockHolderRecord(path); ok {
		fmt.Fprintf(lockWaitReportWriter, "• waiting for the %s lock, held by pid %d (%s) for %s…\n",
			description, record.PID, record.Command, waited.Round(time.Second))
		return
	}
	fmt.Fprintf(lockWaitReportWriter, "• waiting for the %s lock, held by another process, for %s…\n",
		description, waited.Round(time.Second))
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
