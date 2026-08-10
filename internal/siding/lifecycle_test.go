package siding

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestSpinCreatesOnlyWorktreeAndChoosesOneBase(t *testing.T) {
	app := newLifecycleGitApp(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, name := range []string{"one", "two"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			_, err := Spin(context.Background(), app, name, "HEAD", "")
			errs <- err
		}(name)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := state.LoadApp(app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseSiding != "one" && got.BaseSiding != "two" {
		t.Fatalf("base siding = %q", got.BaseSiding)
	}
	if got.BaseCommit == "" || len(got.Sidings) != 2 {
		t.Fatalf("state = %#v", got)
	}
	for name, sd := range got.Sidings {
		if sd.MaterializationPhase != state.PhaseWorktree || sd.WorktreeRepoPath != app.ControlRepoPath {
			t.Fatalf("%s = %#v", name, sd)
		}
		_, vol, _ := Paths(got, name)
		if _, err := os.Stat(vol); !os.IsNotExist(err) {
			t.Fatalf("%s created data: %v", name, err)
		}
		base, _ := SidingBase(got, name)
		if _, err := os.Stat(filepath.Join(base, "out")); !os.IsNotExist(err) {
			t.Fatalf("%s created out: %v", name, err)
		}
	}
}

func TestUpPersistsDataPhaseAndRetriesGuestMaterialization(t *testing.T) {
	dir := t.TempDir()
	app := state.App{Name: "app", ConfigDir: dir, Sidings: map[string]state.Siding{}}
	sd := state.Siding{Name: "one", Container: "guest", MaterializationPhase: state.PhaseWorktree, Bridges: map[string]int{}}
	app.Sidings[sd.Name] = sd
	src, _, _ := Paths(app, sd.Name)
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	originalEnsure, originalRemove, originalRun, originalExec := ensureBaseImage, removeGuest, runGuest, execGuest
	originalUpEnsure, originalUpRoutes, originalUpProbe := upEnsureGuestLive, upResolveFrontDoor, upProbeAppRunning
	originalUpPrepare, originalUpStop, originalUpStart := upPrepareGuest, upStopApp, upStartApp
	originalUpWait, originalUpClear, originalUpIP := upWaitReady, upClearAppLog, upGuestIP
	t.Cleanup(func() {
		ensureBaseImage = originalEnsure
		removeGuest = originalRemove
		runGuest = originalRun
		execGuest = originalExec
		upEnsureGuestLive = originalUpEnsure
		upResolveFrontDoor = originalUpRoutes
		upProbeAppRunning = originalUpProbe
		upPrepareGuest = originalUpPrepare
		upStopApp = originalUpStop
		upStartApp = originalUpStart
		upWaitReady = originalUpWait
		upClearAppLog = originalUpClear
		upGuestIP = originalUpIP
	})
	ensureBaseImage = func(context.Context, bool) error { return nil }
	removeGuest = func(context.Context, string) error { return nil }
	execGuest = func(context.Context, string, ...string) (string, error) { return "", nil }
	attempts := 0
	runGuest = func(context.Context, container.RunOpts) error {
		attempts++
		if attempts == 1 {
			return errors.New("injected create failure")
		}
		return nil
	}
	upEnsureGuestLive = func(context.Context, state.Siding) error { return nil }
	upResolveFrontDoor = func(state.App, state.Siding) ([]state.Route, error) { return nil, nil }
	upProbeAppRunning = func(context.Context, state.App, state.Siding) (bool, error) { return false, nil }
	upPrepareGuest = func(context.Context, state.App, state.Siding) error { return nil }
	upStopApp = func(context.Context, state.App, state.Siding) error { return nil }
	upClearAppLog = func(context.Context, string) {}
	upStartApp = func(context.Context, state.App, state.Siding) error { return nil }
	upWaitReady = func(context.Context, state.App, state.Siding, time.Duration) error { return nil }
	upGuestIP = func(context.Context, string) (string, error) { return "10.0.0.2", nil }
	if _, err := Up(context.Background(), app, sd, false, io.Discard); err == nil {
		t.Fatal("first up succeeded")
	}
	persisted, _ := state.LoadApp(dir)
	if persisted.Sidings["one"].MaterializationPhase != state.PhaseData {
		t.Fatalf("phase after failure = %s", persisted.Sidings["one"].MaterializationPhase)
	}
	got, err := Up(context.Background(), persisted, persisted.Sidings["one"], false, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaterializationPhase != state.PhaseGuest {
		t.Fatalf("retry phase = %s", got.MaterializationPhase)
	}
}

func TestUpReloadsFreshStatePersistsResultAndRestoresLiveRoute(t *testing.T) {
	dir := t.TempDir()
	app := state.App{
		Name:       "app",
		ConfigDir:  dir,
		LiveSiding: "one",
	}
	app.Sidings = map[string]state.Siding{"one": {Name: "one", Container: "guest", LastIP: "fresh"}}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	originalUp, originalRestore := upSiding, upRestoreLiveRoute
	t.Cleanup(func() {
		upSiding = originalUp
		upRestoreLiveRoute = originalRestore
	})
	upSiding = func(_ context.Context, current state.App, sd state.Siding, _ bool, _ io.Writer) (state.Siding, error) {
		if current.LiveSiding != "one" || sd.LastIP != "fresh" {
			t.Fatalf("up received stale state: app=%#v siding=%#v", current, sd)
		}
		sd.LastIP = "updated"
		return sd, nil
	}
	restored := false
	upRestoreLiveRoute = func(_ context.Context, configDir, name string) error {
		if configDir != dir || name != "one" {
			t.Fatalf("restore target = %q/%q", configDir, name)
		}
		restored = true
		return nil
	}
	stale := state.Siding{Name: "one", Container: "guest", LastIP: "stale"}
	if _, err := Up(context.Background(), app, stale, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !restored {
		t.Fatal("live route was not restored")
	}
	persisted, err := state.LoadApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Sidings["one"].LastIP != "updated" {
		t.Fatalf("persisted siding = %#v", persisted.Sidings["one"])
	}
}

func TestUpFencesRemovalPublishedWhileWaitingForProjectLock(t *testing.T) {
	dir := t.TempDir()
	app := state.App{Name: "app", ConfigDir: dir, Sidings: map[string]state.Siding{"one": {Name: "one", Container: "guest"}}}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	originalUp := upSiding
	t.Cleanup(func() { upSiding = originalUp })
	called := false
	upSiding = func(context.Context, state.App, state.Siding, bool, io.Writer) (state.Siding, error) {
		called = true
		return state.Siding{}, nil
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	lockedDone := make(chan error, 1)
	go func() {
		lockedDone <- WithProjectOperation(context.Background(), dir, func() error {
			if _, err := state.UpdateApp(context.Background(), dir, func(current *state.App) error {
				current.Removal = &state.RemovalOperation{ID: "remove-one", Siding: "one", Stage: state.RemovalStarted}
				return nil
			}); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	upDone := make(chan error, 1)
	go func() { _, err := Up(context.Background(), app, app.Sidings["one"], false, io.Discard); upDone <- err }()
	close(release)
	if err := <-lockedDone; err != nil {
		t.Fatal(err)
	}
	err := <-upDone
	if err == nil || !strings.Contains(err.Error(), "removal") {
		t.Fatalf("Up() error = %v, want removal fence", err)
	}
	if called {
		t.Fatal("up ran after removal was published")
	}
}

func TestRemovalFenceRejectsMutatingLifecycleAndDataOperations(t *testing.T) {
	dir := t.TempDir()
	app := state.App{
		Name:      "app",
		ConfigDir: dir,
		Volumes:   []string{"db"},
	}
	app.Sidings = map[string]state.Siding{"one": {Name: "one", Container: "guest", MaterializationPhase: state.PhaseGuest}}
	app.Removal = &state.RemovalOperation{ID: "remove-one", Siding: "one", Stage: state.RemovalStarted}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		run  func() error
	}{
		{name: "spin", run: func() error { _, err := Spin(context.Background(), app, "two", "HEAD", ""); return err }},
		{name: "park", run: func() error { _, err := Park(context.Background(), app, "one"); return err }},
		{name: "stop", run: func() error { _, err := Stop(context.Background(), app, "one"); return err }},
		{name: "up", run: func() error {
			_, err := Up(context.Background(), app, app.Sidings["one"], false, io.Discard)
			return err
		}},
		{name: "switch", run: func() error { copy := app; return Switch(context.Background(), &copy, "one") }},
		{name: "restart", run: func() error { return Restart(context.Background(), app, app.Sidings["one"]) }},
		{name: "recreate", run: func() error { _, err := Recreate(context.Background(), app, app.Sidings["one"], false); return err }},
		{name: "promote", run: func() error { _, err := PromoteData(context.Background(), app, "one", io.Discard); return err }},
		{name: "rollback", run: func() error { _, err := RollbackData(context.Background(), app); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if err == nil || !strings.Contains(err.Error(), "removal") {
				t.Fatalf("operation error = %v, want removal fence", err)
			}
		})
	}
}

func TestParkPreservesFilesAndMarksPhase(t *testing.T) {
	dir := t.TempDir()
	app := state.App{Name: "app", ConfigDir: dir, Sidings: map[string]state.Siding{"one": {Name: "one", Container: "guest", MaterializationPhase: state.PhaseGuest}}}
	base, _ := SidingBase(app, "one")
	if err := os.MkdirAll(filepath.Join(base, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "src", "keep"), []byte("yes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	original := parkRemoveGuest
	t.Cleanup(func() { parkRemoveGuest = original })
	parkRemoveGuest = func(context.Context, string) error { return nil }
	parked, err := Park(context.Background(), app, "one")
	if err != nil {
		t.Fatal(err)
	}
	if parked.MaterializationPhase != state.PhaseParked {
		t.Fatalf("phase=%s", parked.MaterializationPhase)
	}
	if _, err := os.Stat(filepath.Join(base, "src", "keep")); err != nil {
		t.Fatalf("park removed files: %v", err)
	}
}

func TestParkDataOnlyDoesNotRemoveMissingGuest(t *testing.T) {
	dir := t.TempDir()
	app := state.App{Name: "app", ConfigDir: dir, Sidings: map[string]state.Siding{"one": {Name: "one", Container: "guest", MaterializationPhase: state.PhaseData}}}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	original := parkRemoveGuest
	t.Cleanup(func() { parkRemoveGuest = original })
	parkRemoveGuest = func(context.Context, string) error {
		t.Fatal("data-only siding attempted to remove a guest")
		return nil
	}
	parked, err := Park(context.Background(), app, "one")
	if err != nil {
		t.Fatal(err)
	}
	if parked.MaterializationPhase != state.PhaseParked {
		t.Fatalf("phase=%s", parked.MaterializationPhase)
	}
}

func newLifecycleGitApp(t *testing.T) state.App {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	mustGit(t, root, "init", "-b", "main", repo)
	mustGit(t, repo, "config", "user.name", "Test")
	mustGit(t, repo, "config", "user.email", "test@example.test")
	mustGit(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "add", "README.md")
	mustGit(t, repo, "commit", "-m", "initial")
	configDir := filepath.Join(root, "config")
	control := filepath.Join(configDir, ".control.git")
	commit, err := fsclone.EnsureControlRepo(context.Background(), control, repo, "", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	app := state.App{Name: "app", RepoPath: repo, ControlRepoPath: control, BaseCommit: commit, ConfigDir: configDir, Sidings: map[string]state.Siding{}}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	return app
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
