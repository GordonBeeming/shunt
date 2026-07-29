package databaseline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/fsclone"
)

func TestPromotePublishesImmutableGenerationAndMetadata(t *testing.T) {
	m, source := testManager(t, "db", "cache")
	writeVolume(t, source, "db", "one")
	writeVolume(t, source, "cache", "two")
	now := time.Date(2026, 7, 29, 1, 2, 3, 0, time.UTC)
	m.clock = func() time.Time { return now }

	result, err := m.Promote(context.Background(), "alpha", source)
	if err != nil || !result.Committed {
		t.Fatalf("Promote() = %#v, %v", result, err)
	}
	state := readState(t, m)
	if state.Version != stateVersion || state.Current == "" || state.Previous != "" {
		t.Fatalf("state = %#v", state)
	}
	current := m.generationRoot(state.Current)
	assertVolume(t, current, "db", "one")
	contents, err := os.ReadFile(filepath.Join(current, metadataName))
	if err != nil {
		t.Fatal(err)
	}
	var metadata Metadata
	if err := json.Unmarshal(contents, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.SourceSiding != "alpha" || !metadata.Timestamp.Equal(now) || !sameNames(metadata.Volumes, m.Volumes) {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestPromoteKeepsOnlyCurrentAndPreviousGenerations(t *testing.T) {
	m, source := testManager(t, "db")
	for _, value := range []string{"one", "two", "three"} {
		writeVolume(t, source, "db", value)
		if _, err := m.Promote(context.Background(), value, source); err != nil {
			t.Fatal(err)
		}
	}
	state := readState(t, m)
	assertVolume(t, m.generationRoot(state.Current), "db", "three")
	assertVolume(t, m.generationRoot(state.Previous), "db", "two")
	want := []string{state.Current, state.Previous}
	sort.Strings(want)
	if got := generationEntries(t, m); !reflect.DeepEqual(got, want) {
		t.Fatalf("generations = %v, want current and previous", got)
	}
}

func TestRollbackPublishesPreviousAsCurrent(t *testing.T) {
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
	state := readState(t, m)
	assertVolume(t, m.generationRoot(state.Current), "db", "old")
	assertVolume(t, m.generationRoot(state.Previous), "db", "new")
}

func TestSeedInstallsOnlyWhenCanonicalStateIsAbsent(t *testing.T) {
	m, _ := testManager(t, "db", "cache")
	result, err := m.Seed(context.Background(), "explicit-seed", testCapture(map[string]string{"db": "one", "cache": "two"}))
	if err != nil || !result.Committed {
		t.Fatalf("Seed() = %#v, %v", result, err)
	}
	state := readState(t, m)
	assertVolume(t, m.generationRoot(state.Current), "db", "one")

	called := false
	result, err = m.Seed(context.Background(), "explicit-seed", func(context.Context, string, string) error {
		called = true
		return nil
	})
	if err != nil || result.Committed || called {
		t.Fatalf("second Seed() = %#v, %v; capture called=%t", result, err, called)
	}
}

func TestSeedRejectsInvalidExistingStateWithoutMutation(t *testing.T) {
	m, _ := testManager(t, "db")
	if err := os.WriteFile(m.statePath(), []byte("{not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotBytes(t, m.ConfigDir)
	called := false
	result, err := m.Seed(context.Background(), "seed", func(context.Context, string, string) error {
		called = true
		return nil
	})
	if err == nil || result.Committed || called {
		t.Fatalf("Seed() = %#v, %v; capture called=%t", result, err, called)
	}
	after := snapshotBytes(t, m.ConfigDir)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("invalid state mutated:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestSeedRejectsIncompleteExistingStateWithoutMutation(t *testing.T) {
	m, _ := testManager(t, "db")
	orphan := filepath.Join(m.generationsRoot(), generationPrefix+"orphan")
	writeVolume(t, orphan, "db", "do-not-touch")
	before := snapshotBytes(t, m.ConfigDir)

	result, err := m.InitializeEmpty(context.Background())
	if err == nil || result.Committed {
		t.Fatalf("InitializeEmpty() = %#v, %v", result, err)
	}
	if after := snapshotBytes(t, m.ConfigDir); !reflect.DeepEqual(after, before) {
		t.Fatalf("incomplete state mutated:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestRejectsUnsupportedLegacyLayoutWithoutMutation(t *testing.T) {
	m, _ := testManager(t, "db")
	writeVolume(t, filepath.Join(m.ConfigDir, "baseline"), "db", "legacy")
	before := snapshotBytes(t, m.ConfigDir)

	result, err := m.InitializeEmpty(context.Background())
	if err == nil || result.Committed || !strings.Contains(err.Error(), "unsupported pre-generation baseline layout") {
		t.Fatalf("InitializeEmpty() = %#v, %v", result, err)
	}
	if after := snapshotBytes(t, m.ConfigDir); !reflect.DeepEqual(after, before) {
		t.Fatalf("legacy layout mutated:\nbefore=%v\nafter=%v", before, after)
	}
}

func TestInitializeEmptyCreatesEveryDeclaredVolume(t *testing.T) {
	m, _ := testManager(t, "db", "cache")
	result, err := m.InitializeEmpty(context.Background())
	if err != nil || !result.Committed {
		t.Fatalf("InitializeEmpty() = %#v, %v", result, err)
	}
	state := readState(t, m)
	for _, volume := range m.Volumes {
		entries, err := os.ReadDir(filepath.Join(m.generationRoot(state.Current), volume))
		if err != nil || len(entries) != 0 {
			t.Fatalf("empty generation volume %q = %v, %v", volume, entries, err)
		}
	}
}

func TestResetRequiresExplicitInitialization(t *testing.T) {
	m, _ := testManager(t, "db")
	destination := filepath.Join(m.ConfigDir, "alpha", "vol")
	result, err := m.ResetVolumeRoot(context.Background(), destination)
	if err == nil || result.Committed {
		t.Fatalf("ResetVolumeRoot() = %#v, %v", result, err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("destination exists after rejected reset: %v", err)
	}
}

func TestResetClonesInitializedEmptyGeneration(t *testing.T) {
	m, _ := testManager(t, "db", "cache")
	m.cloneVolumeSet = cloneVolumeSetForTest
	if _, err := m.InitializeEmpty(context.Background()); err != nil {
		t.Fatal(err)
	}
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

func TestPromoteCaptureFailurePreservesCurrent(t *testing.T) {
	m, source := testManager(t, "db", "cache")
	writeVolume(t, source, "db", "stable")
	writeVolume(t, source, "cache", "stable")
	if _, err := m.Promote(context.Background(), "stable", source); err != nil {
		t.Fatal(err)
	}
	stable := readState(t, m)
	writeVolume(t, source, "db", "candidate")
	m.clone = func(_ context.Context, src, dest string) error {
		if filepath.Base(src) == "cache" {
			return errors.New("copy failed")
		}
		return copyVolume(src, dest)
	}

	result, err := m.Promote(context.Background(), "candidate", source)
	if err == nil || result.Committed {
		t.Fatalf("Promote() = %#v, %v", result, err)
	}
	if got := readState(t, m); got != stable {
		t.Fatalf("state changed = %#v, want %#v", got, stable)
	}
	assertVolume(t, m.generationRoot(stable.Current), "db", "stable")
}

func TestPromotionCleanupFailureReportsCommittedRecoveryPath(t *testing.T) {
	m, source := testManager(t, "db")
	for _, value := range []string{"one", "two"} {
		writeVolume(t, source, "db", value)
		if _, err := m.Promote(context.Background(), value, source); err != nil {
			t.Fatal(err)
		}
	}
	oldPrevious := m.generationRoot(readState(t, m).Previous)
	originalRemove := m.remove
	m.remove = func(path string) error {
		if path == oldPrevious {
			return errors.New("cleanup denied")
		}
		return originalRemove(path)
	}
	writeVolume(t, source, "db", "three")

	result, err := m.Promote(context.Background(), "three", source)
	if !result.Committed {
		t.Fatalf("result = %#v, want committed", result)
	}
	var cleanup *CommittedCleanupError
	if !errors.As(err, &cleanup) || !reflect.DeepEqual(cleanup.RecoveryPaths, []string{oldPrevious}) {
		t.Fatalf("Promote() error = %v, want recovery path %s", err, oldPrevious)
	}
	state := readState(t, m)
	assertVolume(t, m.generationRoot(state.Current), "db", "three")
	assertVolume(t, m.generationRoot(state.Previous), "db", "two")
	if _, statErr := os.Stat(oldPrevious); statErr != nil {
		t.Fatalf("recovery generation missing: %v", statErr)
	}
}

func TestResetCleanupFailureReportsCommittedDestination(t *testing.T) {
	m, _ := testManager(t, "db")
	if _, err := m.InitializeEmpty(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovery := filepath.Join(m.ConfigDir, "alpha", ".volumes-stage-recovery")
	if err := os.MkdirAll(recovery, 0o755); err != nil {
		t.Fatal(err)
	}
	m.cloneVolumeSet = func(context.Context, string, string, []string) (fsclone.VolumeSetResult, error) {
		result := fsclone.VolumeSetResult{Committed: true, RecoveryPaths: []string{recovery}}
		return result, &fsclone.VolumeSetCleanupError{Committed: true, RecoveryPaths: result.RecoveryPaths, Err: errors.New("cleanup denied")}
	}
	result, err := m.ResetVolumeRoot(context.Background(), filepath.Join(m.ConfigDir, "alpha", "vol"))
	var cleanup *CommittedCleanupError
	if !result.Committed || !errors.As(err, &cleanup) || !reflect.DeepEqual(result.RecoveryPaths, []string{recovery}) {
		t.Fatalf("ResetVolumeRoot() = %#v, %v", result, err)
	}
}

func TestCleanupReportsDeterministicRecoveryPaths(t *testing.T) {
	m, _ := testManager(t, "db")
	if _, err := m.InitializeEmpty(context.Background()); err != nil {
		t.Fatal(err)
	}
	orphan := m.generationRoot(generationPrefix + "retired")
	writeVolume(t, orphan, "db", "retired")
	originalRemove := m.remove
	m.remove = func(path string) error {
		if path == orphan {
			return errors.New("cleanup denied")
		}
		return originalRemove(path)
	}

	result, err := m.Cleanup(context.Background())
	var cleanup *CommittedCleanupError
	if !result.Committed || !errors.As(err, &cleanup) {
		t.Fatalf("Cleanup() = %#v, %v", result, err)
	}
	if !reflect.DeepEqual(result.RecoveryPaths, []string{orphan}) {
		t.Fatalf("recovery paths = %v, want %s", result.RecoveryPaths, orphan)
	}
}

func TestLockSerializesPromotions(t *testing.T) {
	m, source := testManager(t, "db")
	writeVolume(t, source, "db", "one")
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	m.clone = func(_ context.Context, src, dest string) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
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

func TestLockWaitRespectsCancellation(t *testing.T) {
	m, source := testManager(t, "db")
	writeVolume(t, source, "db", "one")
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	m.clone = func(_ context.Context, src, dest string) error {
		once.Do(func() {
			close(entered)
			<-release
		})
		return copyVolume(src, dest)
	}
	first := make(chan error, 1)
	go func() { _, err := m.Promote(context.Background(), "one", source); first <- err }()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result, err := m.Promote(ctx, "two", source)
	if !errors.Is(err, context.DeadlineExceeded) || result.Committed {
		t.Fatalf("canceled Promote() = %#v, %v", result, err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestCancellationAtCommitStillReportsCommitted(t *testing.T) {
	m, source := testManager(t, "db")
	writeVolume(t, source, "db", "one")
	ctx, cancel := context.WithCancel(context.Background())
	m.failpoint = func(point string) {
		if point == "manifest-published" {
			cancel()
		}
	}
	result, err := m.Promote(ctx, "one", source)
	if err != nil || !result.Committed {
		t.Fatalf("Promote() = %#v, %v", result, err)
	}
}

func TestLifecycleRestoreRunsOutsideMutationLock(t *testing.T) {
	m, source := testManager(t, "db")
	writeVolume(t, source, "db", "second")
	restoreEntered := make(chan struct{})
	restoreRelease := make(chan struct{})
	lifecycle := &blockingRestoreLifecycle{restoreEntered: restoreEntered, restoreRelease: restoreRelease}
	first := make(chan error, 1)
	go func() {
		_, err := m.PromoteWithLifecycle(context.Background(), "first", lifecycle)
		first <- err
	}()
	<-restoreEntered

	second := make(chan error, 1)
	go func() {
		_, err := m.Promote(context.Background(), "second", source)
		second <- err
	}()
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("promotion blocked behind lifecycle restore")
	}
	close(restoreRelease)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestRetiredGenerationCleanupRunsOutsideMutationLock(t *testing.T) {
	m, source := testManager(t, "db")
	for _, value := range []string{"one", "two"} {
		writeVolume(t, source, "db", value)
		if _, err := m.Promote(context.Background(), value, source); err != nil {
			t.Fatal(err)
		}
	}
	retired := m.generationRoot(readState(t, m).Previous)
	cleanupEntered := make(chan struct{})
	cleanupRelease := make(chan struct{})
	originalRemove := m.remove
	var once sync.Once
	m.remove = func(path string) error {
		if path == retired {
			once.Do(func() {
				close(cleanupEntered)
				<-cleanupRelease
			})
		}
		return originalRemove(path)
	}
	writeVolume(t, source, "db", "three")
	promotion := make(chan error, 1)
	go func() {
		_, err := m.Promote(context.Background(), "three", source)
		promotion <- err
	}()
	<-cleanupEntered

	rollback := make(chan Result, 1)
	rollbackErr := make(chan error, 1)
	go func() {
		result, err := m.RollbackContext(context.Background())
		rollback <- result
		rollbackErr <- err
	}()
	select {
	case result := <-rollback:
		if !result.Committed {
			t.Fatalf("RollbackContext() result = %#v", result)
		}
		// The still-running cleanup may be reported as recoverable, but it must
		// not keep rollback from reaching its manifest commit.
		<-rollbackErr
	case <-time.After(time.Second):
		t.Fatal("rollback blocked behind retired-generation cleanup")
	}
	close(cleanupRelease)
	if err := <-promotion; err != nil {
		t.Fatal(err)
	}
}

func TestResetLeasePreservesGenerationAcrossConcurrentPromotions(t *testing.T) {
	m, source := testManager(t, "db")
	writeVolume(t, source, "db", "one")
	if _, err := m.Promote(context.Background(), "one", source); err != nil {
		t.Fatal(err)
	}
	leasedGeneration := m.generationRoot(readState(t, m).Current)
	cloneEntered := make(chan struct{})
	cloneRelease := make(chan struct{})
	m.cloneVolumeSet = func(ctx context.Context, sourceRoot, destinationRoot string, volumes []string) (fsclone.VolumeSetResult, error) {
		close(cloneEntered)
		<-cloneRelease
		return cloneVolumeSetForTest(ctx, sourceRoot, destinationRoot, volumes)
	}
	reset := make(chan error, 1)
	go func() {
		_, err := m.ResetVolumeRoot(context.Background(), filepath.Join(m.ConfigDir, "beta", "vol"))
		reset <- err
	}()
	<-cloneEntered

	for _, value := range []string{"two", "three"} {
		writeVolume(t, source, "db", value)
		result, err := m.Promote(context.Background(), value, source)
		if value == "two" && err != nil {
			t.Fatal(err)
		}
		if value == "three" {
			var cleanup *CommittedCleanupError
			if !result.Committed || !errors.As(err, &cleanup) {
				t.Fatalf("third Promote() = %#v, %v", result, err)
			}
			if !reflect.DeepEqual(result.RecoveryPaths, []string{leasedGeneration}) {
				t.Fatalf("recovery paths = %v, want leased generation", result.RecoveryPaths)
			}
		}
	}
	if _, err := os.Stat(leasedGeneration); err != nil {
		t.Fatalf("leased generation was removed: %v", err)
	}
	close(cloneRelease)
	if err := <-reset; err != nil {
		t.Fatal(err)
	}
	if _, err := m.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leasedGeneration); !os.IsNotExist(err) {
		t.Fatalf("retired generation remains after lease release: %v", err)
	}
}

func TestPromotionCrashRecoveryPreservesRollbackPredecessor(t *testing.T) {
	if os.Getenv("SHUNT_BASELINE_CRASH_HELPER") != "" {
		t.Skip("parent-only test")
	}
	for _, point := range []string{"generation-installed", "manifest-staged", "manifest-published"} {
		t.Run(point, func(t *testing.T) {
			m, source := testManager(t, "db")
			for _, value := range []string{"one", "two"} {
				writeVolume(t, source, "db", value)
				if _, err := m.Promote(context.Background(), value, source); err != nil {
					t.Fatal(err)
				}
			}
			writeVolume(t, source, "db", "three")
			command := exec.Command(os.Args[0], "-test.run=^TestBaselineCrashHelper$")
			command.Env = append(os.Environ(),
				"SHUNT_BASELINE_CRASH_HELPER=1",
				"SHUNT_BASELINE_CONFIG="+m.ConfigDir,
				"SHUNT_BASELINE_SOURCE="+source,
				"SHUNT_BASELINE_FAILPOINT="+point,
			)
			err := command.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 73 {
				t.Fatalf("crash helper = %v, want exit 73", err)
			}

			reopened, err := New(m.ConfigDir, []string{"db"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := reopened.Rollback()
			if err != nil || !result.Committed {
				t.Fatalf("Rollback() after %s = %#v, %v", point, result, err)
			}
			state := readState(t, reopened)
			want := "one"
			if point == "manifest-published" {
				want = "two"
			}
			assertVolume(t, reopened.generationRoot(state.Current), "db", want)
			if got := generationEntries(t, reopened); len(got) != 2 {
				t.Fatalf("recovered generations after %s = %v", point, got)
			}
		})
	}
}

func TestBaselineCrashHelper(t *testing.T) {
	if os.Getenv("SHUNT_BASELINE_CRASH_HELPER") == "" {
		return
	}
	m, err := New(os.Getenv("SHUNT_BASELINE_CONFIG"), []string{"db"})
	if err != nil {
		t.Fatal(err)
	}
	m.clone = func(_ context.Context, src, dest string) error { return copyVolume(src, dest) }
	point := os.Getenv("SHUNT_BASELINE_FAILPOINT")
	m.failpoint = func(current string) {
		if current == point {
			os.Exit(73)
		}
	}
	if _, err := m.Promote(context.Background(), "crash-candidate", os.Getenv("SHUNT_BASELINE_SOURCE")); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("failpoint %q did not terminate process", point)
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
	for _, value := range []string{"old", "new"} {
		writeVolume(t, source, "db", value)
		writeVolume(t, source, "cache", value)
		if _, err := m.Promote(context.Background(), value, source); err != nil {
			t.Fatal(err)
		}
	}
	state := readState(t, m)
	badMetadata := Metadata{SourceSiding: "bad", Timestamp: time.Now(), Volumes: []string{"db"}}
	if err := writeMetadata(m.generationRoot(state.Previous), badMetadata); err != nil {
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

func testManager(t *testing.T, volumes ...string) (*Manager, string) {
	t.Helper()
	config := t.TempDir()
	m, err := New(config, volumes)
	if err != nil {
		t.Fatal(err)
	}
	m.clone = func(_ context.Context, src, dest string) error { return copyVolume(src, dest) }
	m.cloneVolumeSet = cloneVolumeSetForTest
	source := filepath.Join(config, "alpha", "vol")
	return m, source
}

func cloneVolumeSetForTest(_ context.Context, sourceRoot, destinationRoot string, volumes []string) (fsclone.VolumeSetResult, error) {
	stage, err := os.MkdirTemp(filepath.Dir(destinationRoot), ".test-volume-stage-")
	if err != nil {
		if os.IsNotExist(err) {
			if mkdirErr := os.MkdirAll(filepath.Dir(destinationRoot), 0o755); mkdirErr != nil {
				return fsclone.VolumeSetResult{}, mkdirErr
			}
			stage, err = os.MkdirTemp(filepath.Dir(destinationRoot), ".test-volume-stage-")
		}
		if err != nil {
			return fsclone.VolumeSetResult{}, err
		}
	}
	defer os.RemoveAll(stage)
	for _, volume := range volumes {
		source := filepath.Join(sourceRoot, volume)
		destination := filepath.Join(stage, volume)
		if err := copyDirectoryForTest(source, destination); err != nil {
			return fsclone.VolumeSetResult{}, err
		}
	}
	if err := os.RemoveAll(destinationRoot); err != nil {
		return fsclone.VolumeSetResult{}, err
	}
	if err := os.Rename(stage, destinationRoot); err != nil {
		return fsclone.VolumeSetResult{}, err
	}
	return fsclone.VolumeSetResult{Committed: true}, nil
}

func copyDirectoryForTest(source, destination string) error {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err := copyDirectoryForTest(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
				return err
			}
			continue
		}
		contents, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), contents, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func readState(t *testing.T, m *Manager) stateManifest {
	t.Helper()
	contents, err := os.ReadFile(m.statePath())
	if err != nil {
		t.Fatal(err)
	}
	var state stateManifest
	if err := json.Unmarshal(contents, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func generationEntries(t *testing.T, m *Manager) []string {
	t.Helper()
	entries, err := os.ReadDir(m.generationsRoot())
	if err != nil {
		t.Fatal(err)
	}
	var result []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), generationPrefix) {
			result = append(result, entry.Name())
		}
	}
	return result
}

func snapshotBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	result := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			result[rel] = "<dir>"
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			result[rel] = "<symlink>" + target
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		result[rel] = string(contents)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
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

func testCapture(values map[string]string) VolumeCapture {
	return func(_ context.Context, volume, destination string) error {
		return writeCapturedVolume(destination, values[volume])
	}
}

func writeCapturedVolume(destination, value string) error {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(destination, "value"), []byte(value), 0o644)
}

type blockingRestoreLifecycle struct {
	restoreEntered chan struct{}
	restoreRelease chan struct{}
}

func (*blockingRestoreLifecycle) Quiesce(context.Context) error             { return nil }
func (*blockingRestoreLifecycle) StopVolumeConsumers(context.Context) error { return nil }
func (*blockingRestoreLifecycle) Sync(context.Context) error                { return nil }
func (*blockingRestoreLifecycle) SnapshotHostVolume(_ context.Context, volume, destination string) error {
	return writeCapturedVolume(destination, volume)
}
func (l *blockingRestoreLifecycle) Restore(context.Context) (RestoreResult, error) {
	close(l.restoreEntered)
	<-l.restoreRelease
	return RestoreResult{Restored: true}, nil
}
