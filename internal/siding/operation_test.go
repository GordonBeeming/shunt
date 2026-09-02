package siding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/state"
	"golang.org/x/sys/unix"
)

func TestWithProjectOperationSerializesAndHonorsCancellation(t *testing.T) {
	configDir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithProjectOperation(context.Background(), configDir, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	called := false
	err := WithProjectOperation(ctx, configDir, func() error {
		called = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WithProjectOperation() error = %v; want deadline exceeded", err)
	}
	if !strings.Contains(err.Error(), "project lifecycle") {
		t.Fatalf("WithProjectOperation() error = %v; want lock description", err)
	}
	if called {
		t.Fatal("contending operation ran without acquiring the project lock")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first operation failed: %v", err)
	}
}

func TestWithProjectOperationSecuresExistingLockFile(t *testing.T) {
	configDir := t.TempDir()
	lockPath := filepath.Join(configDir, operationLockName)
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WithProjectOperation(context.Background(), configDir, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock permissions = %o, want 600", got)
	}
}

func TestDifferentSidingOperationsRunTogether(t *testing.T) {
	configDir := t.TempDir()
	alphaEntered := make(chan struct{})
	releaseAlpha := make(chan struct{})
	alphaDone := make(chan error, 1)
	go func() {
		alphaDone <- WithSidingOperation(context.Background(), configDir, "alpha", func() error {
			close(alphaEntered)
			<-releaseAlpha
			return nil
		})
	}()
	<-alphaEntered

	betaEntered := make(chan struct{})
	betaDone := make(chan error, 1)
	go func() {
		betaDone <- WithSidingOperation(context.Background(), configDir, "beta", func() error {
			close(betaEntered)
			return nil
		})
	}()
	select {
	case <-betaEntered:
	case <-time.After(time.Second):
		t.Fatal("different siding was blocked by long lifecycle work")
	}
	close(releaseAlpha)
	if err := <-alphaDone; err != nil {
		t.Fatal(err)
	}
	if err := <-betaDone; err != nil {
		t.Fatal(err)
	}
}

func TestSameSidingOperationSerializes(t *testing.T) {
	configDir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithSidingOperation(context.Background(), configDir, "alpha", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	called := false
	err := WithSidingOperation(ctx, configDir, "alpha", func() error {
		called = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("contending same-siding operation = called %v, error %v", called, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestProjectExclusiveOperationsBlockLifecycleAndEachOther(t *testing.T) {
	configDir := t.TempDir()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithProjectOperation(context.Background(), configDir, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	for _, test := range []struct {
		name string
		run  func(context.Context, func() error) error
	}{
		{name: "start", run: func(ctx context.Context, fn func() error) error {
			return WithSidingOperation(ctx, configDir, "alpha", fn)
		}},
		{name: "stop", run: func(ctx context.Context, fn func() error) error {
			return WithSidingOperation(ctx, configDir, "alpha", fn)
		}},
		{name: "switch", run: func(ctx context.Context, fn func() error) error {
			return WithProjectOperation(ctx, configDir, fn)
		}},
		{name: "promotion", run: func(ctx context.Context, fn func() error) error {
			return WithProjectSidingOperation(ctx, configDir, "alpha", fn)
		}},
		{name: "app registration", run: func(ctx context.Context, fn func() error) error {
			return WithProjectOperation(ctx, configDir, fn)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			called := false
			err := test.run(ctx, func() error { called = true; return nil })
			if !errors.Is(err, context.DeadlineExceeded) || called {
				t.Fatalf("contender = called %v, error %v", called, err)
			}
		})
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLockHolderRecordRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	writeLockHolderRecord(lock)

	record, ok := readLockHolderRecord(path)
	if !ok {
		t.Fatal("readLockHolderRecord() = false, want a readable record right after writing it")
	}
	if record.PID != os.Getpid() {
		t.Fatalf("record.PID = %d, want %d", record.PID, os.Getpid())
	}
	// The record names the holder without echoing what it was given: every
	// contending caller reads this file, and `run` forwards arbitrary arguments
	// into the guest.
	if want := holderCommand(os.Args); record.Command != want {
		t.Fatalf("record.Command = %q, want %q", record.Command, want)
	}
	if strings.Contains(record.Command, "-") && len(os.Args) > 1 {
		t.Errorf("record.Command = %q, want no flags or their values", record.Command)
	}
	if record.AcquiredAt.IsZero() || time.Since(record.AcquiredAt) > time.Minute {
		t.Fatalf("record.AcquiredAt = %v, want roughly now", record.AcquiredAt)
	}
}

func TestFileLockWaitStaysSilentBelowGracePeriod(t *testing.T) {
	originalGrace, originalWriter := lockWaitGracePeriod, lockWaitReportWriter
	defer func() { lockWaitGracePeriod, lockWaitReportWriter = originalGrace, originalWriter }()
	lockWaitGracePeriod = time.Hour // far past this test's short contention window
	var out bytes.Buffer
	lockWaitReportWriter = &out

	path := filepath.Join(t.TempDir(), "siding.lock")
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withFileLock(context.Background(), path, unix.LOCK_EX, "siding beta", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := withFileLock(ctx, path, unix.LOCK_EX, "siding beta", func() error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if out.Len() != 0 {
		t.Fatalf("wait report fired below the grace period: %q", out.String())
	}
}

func TestFileLockWaitReportsTheHolderAfterTheGracePeriod(t *testing.T) {
	originalGrace, originalInterval, originalWriter := lockWaitGracePeriod, lockWaitReportInterval, lockWaitReportWriter
	defer func() {
		lockWaitGracePeriod, lockWaitReportInterval, lockWaitReportWriter = originalGrace, originalInterval, originalWriter
	}()
	lockWaitGracePeriod = 30 * time.Millisecond
	lockWaitReportInterval = 30 * time.Millisecond
	var out bytes.Buffer
	lockWaitReportWriter = &out

	path := filepath.Join(t.TempDir(), "siding.lock")
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withFileLock(context.Background(), path, unix.LOCK_EX, `siding "alpha"`, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := withFileLock(ctx, path, unix.LOCK_EX, `siding "alpha"`, func() error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	text := out.String()
	if !strings.Contains(text, `waiting for the siding "alpha" lock`) {
		t.Fatalf("wait report = %q, want it to name the lock", text)
	}
	if !strings.Contains(text, "pid "+strconv.Itoa(os.Getpid())) {
		t.Fatalf("wait report = %q, want the holder's pid", text)
	}
}

func TestReportLockWaitDegradesGracefullyOnAnUntrustworthyRecord(t *testing.T) {
	originalWriter := lockWaitReportWriter
	defer func() { lockWaitReportWriter = originalWriter }()

	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{name: "missing record", setup: func(*testing.T, string) {}},
		{name: "unparseable record", setup: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "record names a dead pid", setup: func(t *testing.T, path string) {
			// Larger than any real pid_max, so this can never collide with a live process.
			data, err := json.Marshal(lockHolderRecord{PID: 2_000_000_000, Command: "ghost", AcquiredAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "record.lock")
			tt.setup(t, path)
			var out bytes.Buffer
			lockWaitReportWriter = &out

			reportLockWait(path, "siding gamma", 3*time.Second)

			text := out.String()
			if !strings.Contains(text, "held by another process") {
				t.Fatalf("degraded report = %q, want the unnamed-holder message", text)
			}
			if strings.Contains(text, "ghost") || strings.Contains(text, "pid ") {
				t.Fatalf("degraded report named an untrustworthy holder: %q", text)
			}
		})
	}
}

func TestWriteLockHolderRecordDoesNotFailOnAnUnwritableHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readonly.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ro, err := os.Open(path) // read-only handle: Truncate/WriteAt on it must fail
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	writeLockHolderRecord(ro) // must not panic and must not surface an error

	if _, ok := readLockHolderRecord(path); ok {
		t.Fatal("a failed write should not leave a readable record behind")
	}
}

func TestExclusiveLockClearsItsHolderRecordOnRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "siding.lock")
	if err := withFileLock(context.Background(), path, unix.LOCK_EX, "siding alpha", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := readLockHolderRecord(path); ok {
		t.Fatal("holder record survived past the exclusive lock's release")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("lock file = %q, want truncated to empty after release", data)
	}
}

// This is the exact defect shape reported: WithSidingOperation acquires the
// project lock SHARED (so different sidings run together) and writes no
// record for it. Without clearing on release, a shared waiter that reads the
// file mid-wait would find a departed exclusive holder's leftover record and
// could name a reused pid as though it still held the lock.
func TestSharedLockWaiterNeverSeesADepartedExclusiveHoldersRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "project.lock")
	if err := withFileLock(context.Background(), path, unix.LOCK_EX, "project lifecycle", func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	originalWriter := lockWaitReportWriter
	defer func() { lockWaitReportWriter = originalWriter }()
	var out bytes.Buffer
	lockWaitReportWriter = &out

	reportLockWait(path, "project lifecycle", 3*time.Second)

	if strings.Contains(out.String(), "held by pid") {
		t.Fatalf("report named the departed exclusive holder: %q", out.String())
	}
	if !strings.Contains(out.String(), "held by another process") {
		t.Fatalf("report = %q, want the unnamed-holder message", out.String())
	}
}

func TestConcurrentSidingStateMergesPreserveEveryWriter(t *testing.T) {
	configDir := t.TempDir()
	app := state.App{ConfigDir: configDir, Sidings: map[string]state.Siding{
		"alpha": {Name: "alpha"},
		"beta":  {Name: "beta"},
	}}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, sd := range []state.Siding{{Name: "alpha", LastIP: "10.0.0.1"}, {Name: "beta", LastIP: "10.0.0.2"}} {
		sd := sd
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := MergeSidingState(context.Background(), configDir, sd, false)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sidings["alpha"].LastIP != "10.0.0.1" || got.Sidings["beta"].LastIP != "10.0.0.2" {
		t.Fatalf("concurrent state merge lost a writer: %#v", got.Sidings)
	}
}

func TestMergeSidingStatePreservesFreshFields(t *testing.T) {
	configDir := t.TempDir()
	app := state.App{ConfigDir: configDir, Memory: "4g", Sidings: map[string]state.Siding{
		"one": {Name: "one", LastIP: "old"},
		"two": {Name: "two", LastIP: "keep"},
	}}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	app.Memory = "8g"
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}

	merged, err := MergeSidingState(context.Background(), configDir, state.Siding{Name: "one", LastIP: "new"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Memory != "8g" || merged.Sidings["two"].LastIP != "keep" || merged.Sidings["one"].LastIP != "new" {
		t.Fatalf("merged state lost a concurrent field: %#v", merged)
	}
}
