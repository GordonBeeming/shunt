package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestUpdateRegistryPreservesConcurrentProjectRegistrations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	var done sync.WaitGroup
	done.Add(2)
	for name, path := range map[string]string{"alpha": "/projects/alpha", "beta": "/projects/beta"} {
		name, path := name, path
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			if _, err := UpdateRegistry(context.Background(), func(reg *Registry) error {
				reg.Projects[name] = path
				return nil
			}); err != nil {
				t.Errorf("register %s: %v", name, err)
			}
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if reg.Projects["alpha"] != "/projects/alpha" || reg.Projects["beta"] != "/projects/beta" {
		t.Fatalf("concurrent registry = %#v", reg.Projects)
	}
}

func TestLoadAppProjectsLegacyStateWithoutPublishingMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"name":"app","repoPath":"/legacy/repo","configDir":"` + dir + `","sidings":{"one":{"name":"one","branch":"feature"}}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	app, err := LoadApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if app.Version != 0 {
		t.Fatalf("read projection changed persisted version marker: %d", app.Version)
	}
	if app.ControlRepoPath != filepath.Join(dir, ".control.git") || app.BaseSiding != "one" {
		t.Fatalf("legacy projection = %#v", app)
	}
	siding := app.Sidings["one"]
	if siding.WorktreeRepoPath != "/legacy/repo" || siding.MaterializationPhase != PhaseGuest {
		t.Fatalf("legacy siding projection = %#v", siding)
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || string(onDisk) != legacy {
		t.Fatalf("LoadApp rewrote state: %q, %v", onDisk, err)
	}
}

func TestEnsureV2AndBaseSelection(t *testing.T) {
	app := App{RepoPath: "/legacy", ConfigDir: "/cfg", Sidings: map[string]Siding{
		"one": {Name: "one"},
		"two": {Name: "two", WorktreeRepoPath: "/managed", MaterializationPhase: PhaseParked},
	}}
	if !NeedsBaseSelection(app) {
		t.Fatal("multiple legacy sidings should require base selection")
	}
	if !EnsureV2(&app) || app.Version != StateVersion {
		t.Fatalf("EnsureV2() app = %#v", app)
	}
	if got := WorktreeOwner(app, app.Sidings["one"]); got != "/legacy" {
		t.Fatalf("legacy owner = %q", got)
	}
	if got := WorktreeOwner(app, app.Sidings["two"]); got != "/managed" {
		t.Fatalf("managed owner = %q", got)
	}
	if app.Sidings["one"].MaterializationPhase != PhaseGuest {
		t.Fatalf("legacy phase = %q", app.Sidings["one"].MaterializationPhase)
	}
}

func TestV2SidingDefaultsToManagedWorktreeOwner(t *testing.T) {
	app := App{Version: StateVersion, RepoPath: "/legacy", ConfigDir: "/cfg", ControlRepoPath: "/cfg/.control.git", Sidings: map[string]Siding{
		"new": {Name: "new"},
	}}
	EnsureV2(&app)
	if got := app.Sidings["new"].WorktreeRepoPath; got != app.ControlRepoPath {
		t.Fatalf("v2 worktree owner = %q, want %q", got, app.ControlRepoPath)
	}
	app.BaseSiding = "missing"
	if !NeedsBaseSelection(app) {
		t.Fatal("missing designated base should require selection")
	}
}

func TestSaveLoadAppRoundTrip(t *testing.T) {
	dir := t.TempDir()
	app := App{
		Name:        "myapp",
		RepoPath:    "/repo",
		AppHostPath: "src/App.csproj",
		ConfigDir:   dir,
		FrontDoor: []Route{
			{Key: "frontend", Kind: KindHTTP, ListenPort: 5000, Resource: "web", CaddyID: "app_myapp_http_frontend"},
		},
		Sidings: map[string]Siding{
			"exp1": {Name: "exp1", Container: "shuntdev_myapp_exp1", Bridges: map[string]int{"frontend": 39001}},
		},
		LiveSiding: "exp1",
	}
	if err := SaveApp(app); err != nil {
		t.Fatal(err)
	}
	got, err := LoadApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "myapp" || got.LiveSiding != "exp1" || len(got.FrontDoor) != 1 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	s := got.Sidings["exp1"]
	if s.Bridges["frontend"] != 39001 {
		t.Errorf("siding roundtrip mismatch: %+v", s)
	}
}

func TestRemovalJournalRoundTripPreservesSafetyPolicy(t *testing.T) {
	dir := t.TempDir()
	app := App{ConfigDir: dir, Sidings: map[string]Siding{"one": {Name: "one"}}, Removal: &RemovalOperation{
		ID: "remove-one", Siding: "one", Stage: RemovalBasePinned, StartedAt: "2026-01-01T00:00:00Z",
		Force: false, Safety: "content-sensitive-fingerprint", Removing: []string{"one", "two"},
	}}
	if err := SaveApp(app); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Removal == nil || loaded.Removal.Force || loaded.Removal.Safety != "content-sensitive-fingerprint" || len(loaded.Removal.Removing) != 2 {
		t.Fatalf("removal journal = %#v", loaded.Removal)
	}
}

func TestRemovalJournalRoundTripPreservesOptionalBranchEvidence(t *testing.T) {
	dir := t.TempDir()
	app := App{Name: "app", ConfigDir: dir, Sidings: map[string]Siding{}, Removal: &RemovalOperation{
		ID: "remove-one", Siding: "one", Stage: RemovalBaselinePromoted, StartedAt: "2026-08-18T00:00:00Z",
		ObservedWorktreeBranch: "gb/shunt/actual", PreservationFingerprint: "preserved-fingerprint",
	}}
	if err := SaveApp(app); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Removal == nil || loaded.Removal.ObservedWorktreeBranch != "gb/shunt/actual" || loaded.Removal.PreservationFingerprint != "preserved-fingerprint" {
		t.Fatalf("loaded removal evidence = %#v", loaded.Removal)
	}
}

func TestUpdateAppRollsBackCallbackFailure(t *testing.T) {
	dir := t.TempDir()
	app := App{ConfigDir: dir, Memory: "4g", Sidings: map[string]Siding{}}
	if err := SaveApp(app); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("injected update failure")
	_, err := UpdateApp(context.Background(), dir, func(current *App) error {
		current.Memory = "8g"
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("UpdateApp() error = %v", err)
	}
	loaded, err := LoadApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Memory != "4g" {
		t.Fatalf("failed update was persisted: %#v", loaded)
	}
}

func TestUpdateAppSurfacesPublicationFailure(t *testing.T) {
	dir := t.TempDir()
	app := App{ConfigDir: dir, Memory: "4g", Sidings: map[string]Siding{}}
	if err := SaveApp(app); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(dir, stateFilename)
	if err := os.Remove(stateFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stateFile, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateApp(context.Background(), dir, func(current *App) error {
		current.Memory = "8g"
		return nil
	}); err == nil {
		t.Fatal("UpdateApp did not surface a state publication failure")
	}
}

func TestWithLockSecuresExistingLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	lockPath := path + ".lock"
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := withLock(context.Background(), path, func() error { return nil }); err != nil {
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

func TestWithLockCancellationIdentifiesLockPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	locked, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	if err := syscall.Flock(int(locked.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(locked.Fd()), syscall.LOCK_UN)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = withLock(ctx, path, func() error { return nil })
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), path+".lock") {
		t.Fatalf("withLock() error = %v", err)
	}
}

func TestLoadAppNotFound(t *testing.T) {
	if _, err := LoadApp(t.TempDir()); err == nil {
		t.Error("expected ErrNotFound for empty dir")
	}
}

func TestSaveAppRequiresConfigDir(t *testing.T) {
	if err := SaveApp(App{Name: "x"}); err == nil {
		t.Error("expected error when ConfigDir is empty")
	}
}

func TestStateV2IgnoresLaterLegacyWrites(t *testing.T) {
	dir := t.TempDir()
	legacyPath := legacyStatePath(dir)
	legacy := `{"name":"app","configDir":"` + dir + `","memory":"4g","sidings":{}}`
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	app, err := LoadApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveApp(app); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath(dir)); err != nil {
		t.Fatalf("versioned state was not published: %v", err)
	}

	legacy = `{"name":"app","configDir":"` + dir + `","memory":"16g","sidings":{}}`
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Memory != "4g" || loaded.Version != StateVersion {
		t.Fatalf("LoadApp() used rewritten legacy state: %#v", loaded)
	}

}

func TestLoadAppRejectsUnsupportedVersionedState(t *testing.T) {
	dir := t.TempDir()
	path := statePath(dir)
	if err := os.WriteFile(path, []byte(`{"version":3,"configDir":"`+dir+`","sidings":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApp(dir); err == nil || !strings.Contains(err.Error(), "unsupported state version 3") {
		t.Fatalf("LoadApp() error = %v", err)
	}
	if err := SaveApp(App{Version: StateVersion, ConfigDir: dir, Sidings: map[string]Siding{}}); err == nil || !strings.Contains(err.Error(), "unsupported state version 3") {
		t.Fatalf("SaveApp() error = %v", err)
	}
}

func TestSaveAppRejectsUnsupportedNewState(t *testing.T) {
	dir := t.TempDir()
	err := SaveApp(App{Version: StateVersion + 1, ConfigDir: dir, Sidings: map[string]Siding{}})
	if err == nil || !strings.Contains(err.Error(), "unsupported state version 3") {
		t.Fatalf("SaveApp() error = %v", err)
	}
	if _, statErr := os.Stat(statePath(dir)); !os.IsNotExist(statErr) {
		t.Fatalf("unsupported state was published: %v", statErr)
	}
}

func TestWriteJSONReportsCommittedDurabilityFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), stateFilename)
	sentinel := errors.New("directory sync denied")
	err := writeJSONWithDirectorySync(path, map[string]string{"value": "published"}, func(string) error {
		return sentinel
	})
	var committed *CommittedDurabilityError
	if !errors.As(err, &committed) || !errors.Is(err, sentinel) {
		t.Fatalf("writeJSONWithDirectorySync() error = %v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || !strings.Contains(string(data), "published") {
		t.Fatalf("published state = %q, %v", data, readErr)
	}
}

func TestRegistryFindProjectCaseInsensitive(t *testing.T) {
	reg := Registry{Projects: map[string]string{"MyApp": "/cfg/MyApp"}}
	cases := []struct {
		name          string
		wantCanonical string
		wantOK        bool
	}{
		{"MyApp", "MyApp", true}, // exact
		{"myApp", "MyApp", true}, // cwd basename with different case (macOS)
		{"MYAPP", "MyApp", true}, // fold match
		{"Other", "", false},     // genuinely absent
	}
	for _, c := range cases {
		gotName, gotDir, ok := reg.FindProject(c.name)
		if ok != c.wantOK {
			t.Errorf("FindProject(%q) ok = %v, want %v", c.name, ok, c.wantOK)
		}
		if gotName != c.wantCanonical {
			t.Errorf("FindProject(%q) canonical = %q, want %q", c.name, gotName, c.wantCanonical)
		}
		if c.wantOK && gotDir != "/cfg/MyApp" {
			t.Errorf("FindProject(%q) dir = %q, want /cfg/MyApp", c.name, gotDir)
		}
	}
}

func TestNeedsBaseSelectionSeparatesLegacyStateFromADetachedBase(t *testing.T) {
	two := map[string]Siding{"one": {Name: "one"}, "two": {Name: "two"}}
	cases := []struct {
		name string
		app  App
		want bool
	}{
		// The migration case this exists for: several sidings, and nothing on
		// record saying which one was the base. Only a person can answer that.
		{"legacy multi-siding with no commit", App{Sidings: two}, true},
		// The detached base. Pinning the commit is what detaching does, so a
		// commit with no base siding is a deliberate state, not an unmigrated one.
		{"detached with a pinned commit", App{Sidings: two, BaseCommit: "abc123"}, false},
		// One siding and no base was already unambiguous.
		{"single siding with no commit", App{Sidings: map[string]Siding{"one": {Name: "one"}}}, false},
		// A base naming a siding that is gone is corrupt either way.
		{"base names a missing siding", App{Sidings: two, BaseSiding: "gone", BaseCommit: "abc123"}, true},
		{"no sidings at all", App{BaseCommit: "abc123"}, false},
	}
	for _, c := range cases {
		if got := NeedsBaseSelection(c.app); got != c.want {
			t.Errorf("%s: NeedsBaseSelection() = %v, want %v", c.name, got, c.want)
		}
	}
}
