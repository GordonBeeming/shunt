package databaseline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPromoteFirstInstallWritesMetadata(t *testing.T) {
	m, source := testManager(t, "db", "cache")
	writeVolume(t, source, "db", "one")
	writeVolume(t, source, "cache", "two")
	m.clock = func() time.Time { return time.Date(2026, 7, 29, 1, 2, 3, 0, time.UTC) }

	result, err := m.Promote(context.Background(), "alpha", source)
	if err != nil || !result.Committed {
		t.Fatalf("Promote() = %#v, %v", result, err)
	}
	assertVolume(t, m.currentRoot(), "db", "one")
	contents, err := os.ReadFile(filepath.Join(m.currentRoot(), metadataName))
	if err != nil {
		t.Fatal(err)
	}
	var metadata Metadata
	if err := json.Unmarshal(contents, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.SourceSiding != "alpha" || !metadata.Timestamp.Equal(m.clock()) || len(metadata.Volumes) != 2 {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestPromoteReplacementKeepsOnlyCurrentAndPrevious(t *testing.T) {
	m, source := testManager(t, "db")
	writeVolume(t, source, "db", "one")
	if _, err := m.Promote(context.Background(), "one", source); err != nil {
		t.Fatal(err)
	}
	writeVolume(t, source, "db", "two")
	if _, err := m.Promote(context.Background(), "two", source); err != nil {
		t.Fatal(err)
	}
	writeVolume(t, source, "db", "three")
	if _, err := m.Promote(context.Background(), "three", source); err != nil {
		t.Fatal(err)
	}
	assertVolume(t, m.currentRoot(), "db", "three")
	assertVolume(t, m.previousRoot(), "db", "two")
}

func TestPromoteRotationFailureKeepsRecoveryPaths(t *testing.T) {
	m, source := testManager(t, "db")
	writeVolume(t, source, "db", "old")
	if _, err := m.Promote(context.Background(), "old", source); err != nil {
		t.Fatal(err)
	}
	writeVolume(t, source, "db", "new")
	originalRename := m.rename
	m.rename = func(from, to string) error {
		if to == m.previousRoot() {
			return errors.New("rename previous failed")
		}
		return originalRename(from, to)
	}
	result, err := m.Promote(context.Background(), "new", source)
	if !result.Committed {
		t.Fatalf("result = %#v, want committed", result)
	}
	var cleanup *CommittedCleanupError
	if !errors.As(err, &cleanup) {
		t.Fatalf("Promote() error = %v, want CommittedCleanupError", err)
	}
	assertVolume(t, m.currentRoot(), "db", "new")
	if len(cleanup.RecoveryPaths) == 0 {
		t.Fatal("cleanup error has no recovery path")
	}
	for _, path := range cleanup.RecoveryPaths {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("recovery path %q missing: %v", path, statErr)
		}
	}
}

func TestRollbackSwapsCurrentAndPrevious(t *testing.T) {
	m, source := testManager(t, "db")
	writeVolume(t, source, "db", "old")
	if _, err := m.Promote(context.Background(), "old", source); err != nil {
		t.Fatal(err)
	}
	writeVolume(t, source, "db", "new")
	if _, err := m.Promote(context.Background(), "new", source); err != nil {
		t.Fatal(err)
	}
	result, err := m.Rollback()
	if err != nil || !result.Committed {
		t.Fatalf("Rollback() = %#v, %v", result, err)
	}
	assertVolume(t, m.currentRoot(), "db", "old")
	assertVolume(t, m.previousRoot(), "db", "new")
}

func TestPromoteSecondCopyFailurePreservesCurrent(t *testing.T) {
	m, source := testManager(t, "db", "cache")
	writeVolume(t, source, "db", "stable")
	writeVolume(t, source, "cache", "stable")
	if _, err := m.Promote(context.Background(), "stable", source); err != nil {
		t.Fatal(err)
	}
	writeVolume(t, source, "db", "candidate")
	m.clone = func(_ context.Context, src, dest string) error {
		if filepath.Base(src) == "cache" {
			return errors.New("copy failed")
		}
		return copyVolume(src, dest)
	}
	if _, err := m.Promote(context.Background(), "candidate", source); err == nil {
		t.Fatal("Promote() error = nil")
	}
	assertVolume(t, m.currentRoot(), "db", "stable")
	assertVolume(t, m.currentRoot(), "cache", "stable")
}

func TestResetVolumeRootCreatesAtomicEmptySetWithoutBaseline(t *testing.T) {
	m, _ := testManager(t, "db", "cache")
	destination := filepath.Join(m.ConfigDir, "alpha", "vol")
	writeVolume(t, destination, "db", "old")
	result, err := m.ResetVolumeRoot(context.Background(), destination)
	if err != nil || !result.Committed {
		t.Fatalf("ResetVolumeRoot() = %#v, %v", result, err)
	}
	for _, volume := range m.Volumes {
		entries, err := os.ReadDir(filepath.Join(destination, volume))
		if err != nil || len(entries) != 0 {
			t.Fatalf("empty reset %q = %v, %v", volume, entries, err)
		}
	}
}

func TestNewRejectsUnsafeNames(t *testing.T) {
	for _, volumes := range [][]string{{"../db"}, {"db/cache"}, {"."}, {"db", "db"}} {
		if _, err := New(t.TempDir(), volumes); err == nil {
			t.Fatalf("New(%q) error = nil", volumes)
		}
	}
}

func TestPromoteRejectsPathOutsideConfig(t *testing.T) {
	m, _ := testManager(t, "db")
	if _, err := m.Promote(context.Background(), "alpha", t.TempDir()); err == nil {
		t.Fatal("Promote() error = nil")
	}
}

func TestPromoteRejectsSymlinkEscapingConfig(t *testing.T) {
	m, _ := testManager(t, "db")
	outside := t.TempDir()
	link := filepath.Join(m.ConfigDir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Promote(context.Background(), "alpha", filepath.Join(link, "vol")); err == nil {
		t.Fatal("Promote() error = nil")
	}
}

func TestRollbackRejectsMetadataWithDifferentVolumeSet(t *testing.T) {
	m, source := testManager(t, "db", "cache")
	writeVolume(t, source, "db", "old")
	writeVolume(t, source, "cache", "old")
	if _, err := m.Promote(context.Background(), "old", source); err != nil {
		t.Fatal(err)
	}
	writeVolume(t, source, "db", "new")
	writeVolume(t, source, "cache", "new")
	if _, err := m.Promote(context.Background(), "new", source); err != nil {
		t.Fatal(err)
	}
	badMetadata := Metadata{SourceSiding: "bad", Timestamp: time.Now(), Volumes: []string{"db"}}
	if err := writeMetadata(m.previousRoot(), badMetadata); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rollback(); err == nil {
		t.Fatal("Rollback() error = nil")
	}
}

func TestPromoteAndRollbackRejectZeroVolumes(t *testing.T) {
	m, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Promote(context.Background(), "alpha", m.ConfigDir); err == nil {
		t.Fatal("Promote() error = nil")
	}
	if _, err := m.Rollback(); err == nil {
		t.Fatal("Rollback() error = nil")
	}
}

func TestPromoteWithLifecycleOrdersCallsAndRestores(t *testing.T) {
	m, _ := testManager(t, "db", "cache")
	lifecycle := &recordingLifecycle{}
	result, err := m.PromoteWithLifecycle(context.Background(), "alpha", lifecycle)
	if err != nil || !result.Committed || !result.Restore.Restored {
		t.Fatalf("PromoteWithLifecycle() = %#v, %v", result, err)
	}
	want := []string{"quiesce", "stop", "sync", "export:db", "snapshot:db", "export:cache", "snapshot:cache", "restore"}
	if got := strings.Join(lifecycle.calls, ","); got != strings.Join(want, ",") {
		t.Fatalf("lifecycle calls = %s, want %s", got, strings.Join(want, ","))
	}
}

func TestPromoteWithLifecycleReportsCommittedRestoreFailure(t *testing.T) {
	m, _ := testManager(t, "db")
	lifecycle := &recordingLifecycle{restoreErr: errors.New("restart failed")}
	result, err := m.PromoteWithLifecycle(context.Background(), "alpha", lifecycle)
	if !result.Committed {
		t.Fatalf("result = %#v, want committed", result)
	}
	var restore *RestoreError
	if !errors.As(err, &restore) || !restore.Committed {
		t.Fatalf("error = %v, want committed RestoreError", err)
	}
}

func TestLockSerializesPromotions(t *testing.T) {
	m, source := testManager(t, "db")
	writeVolume(t, source, "db", "one")
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	m.clone = func(srcCtx context.Context, src, dest string) error {
		once.Do(func() { close(entered); <-release })
		return copyVolume(src, dest)
	}
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { _, err := m.Promote(context.Background(), "one", source); first <- err }()
	<-entered
	go func() { _, err := m.Promote(context.Background(), "two", source); second <- err }()
	select {
	case err := <-second:
		t.Fatalf("second promotion escaped lock early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}

func testManager(t *testing.T, volumes ...string) (*Manager, string) {
	t.Helper()
	config := t.TempDir()
	m, err := New(config, volumes)
	if err != nil {
		t.Fatal(err)
	}
	m.clone = func(_ context.Context, src, dest string) error { return copyVolume(src, dest) }
	source := filepath.Join(config, "alpha", "vol")
	return m, source
}

func writeVolume(t *testing.T, root, volume, value string) {
	t.Helper()
	dir := filepath.Join(root, volume)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "value"), []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertVolume(t *testing.T, root, volume, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, volume, "value"))
	if err != nil || string(got) != want {
		t.Fatalf("%s/%s = %q, %v; want %q", root, volume, got, err, want)
	}
}

func copyVolume(src, dest string) error {
	contents, err := os.ReadFile(filepath.Join(src, "value"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dest, "value"), contents, 0o644)
}

type recordingLifecycle struct {
	calls      []string
	restoreErr error
}

func (l *recordingLifecycle) Quiesce(context.Context) error {
	l.calls = append(l.calls, "quiesce")
	return nil
}

func (l *recordingLifecycle) StopVolumeConsumers(context.Context) error {
	l.calls = append(l.calls, "stop")
	return nil
}

func (l *recordingLifecycle) Sync(context.Context) error {
	l.calls = append(l.calls, "sync")
	return nil
}

func (l *recordingLifecycle) ExportLegacyGuestVolume(_ context.Context, volume, _ string) error {
	l.calls = append(l.calls, "export:"+volume)
	return nil
}

func (l *recordingLifecycle) SnapshotHostVolume(_ context.Context, volume, destination string) error {
	l.calls = append(l.calls, "snapshot:"+volume)
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destination, "value"), []byte(volume), 0o644)
}

func (l *recordingLifecycle) Restore(context.Context) (RestoreResult, error) {
	l.calls = append(l.calls, "restore")
	return RestoreResult{Restored: l.restoreErr == nil}, l.restoreErr
}
