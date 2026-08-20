package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/databaseline"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestAppAddFreshRegistrationPublishesPinnedStateAndRegistry(t *testing.T) {
	repo, configDir := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	withWorkingDirectory(t, repo)
	if err := newAppAddCmd().ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	app, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if app.Version != state.StateVersion || app.BaseCommit != "pinned-seed" || app.ControlRepoPath != filepath.Join(configDir, ".control.git") || app.LiveSiding != "" {
		t.Fatalf("fresh registration = %#v", app)
	}
	reg, err := state.LoadRegistry()
	if err != nil || reg.Projects[app.Name] != configDir {
		t.Fatalf("registry = %#v, %v", reg, err)
	}
}

func TestAppAddReregistrationPreservesLifecycleStateAndClearsLegacyHost(t *testing.T) {
	repo, configDir := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	existing := state.App{Version: state.StateVersion, Name: filepath.Base(repo), RepoPath: repo, ConfigDir: configDir,
		ControlRepoPath: filepath.Join(configDir, ".control.git"), BaseSiding: "one", BaseCommit: "existing-base", LiveSiding: state.HostTarget,
		Sidings: map[string]state.Siding{"one": {Name: "one", Branch: "feature", MaterializationPhase: state.PhaseParked}}}
	if err := state.SaveApp(existing); err != nil {
		t.Fatal(err)
	}
	withWorkingDirectory(t, repo)
	if err := newAppAddCmd().ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	app, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if app.BaseSiding != "one" || app.BaseCommit != "existing-base" || app.LiveSiding != "" || app.Sidings["one"].MaterializationPhase != state.PhaseParked {
		t.Fatalf("re-registration reset lifecycle state: %#v", app)
	}
}

func TestAppAddFencesRemovalBeforeControlRepoOrCaddySideEffects(t *testing.T) {
	repo, configDir := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	existing := state.App{Version: state.StateVersion, Name: filepath.Base(repo), RepoPath: repo, ConfigDir: configDir, Sidings: map[string]state.Siding{},
		Removal: &state.RemovalOperation{ID: "remove-one", Siding: "one", Stage: state.RemovalStarted}}
	if err := state.SaveApp(existing); err != nil {
		t.Fatal(err)
	}
	called := false
	appAddEnsureControl = func(context.Context, string, string, string, string) (string, error) { called = true; return "", nil }
	withWorkingDirectory(t, repo)
	err := newAppAddCmd().ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "removal") || called {
		t.Fatalf("app add during removal = %v, control called=%t", err, called)
	}
}

func TestAppAddDoesNotPublishStateWhenFrontDoorSetupFails(t *testing.T) {
	repo, configDir := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	sentinel := errors.New("injected front-door failure")
	restored := false
	appAddEnsureFrontDoor = func(context.Context, *caddy.Admin, state.App) error { return sentinel }
	appAddRestoreCaddy = func(context.Context, *caddy.Admin, caddy.RouteSnapshot) error { restored = true; return nil }
	withWorkingDirectory(t, repo)
	err := newAppAddCmd().ExecuteContext(context.Background())
	if !errors.Is(err, sentinel) || !restored {
		t.Fatalf("app add = %v", err)
	}
	if _, err := state.LoadApp(configDir); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("partial app state was published: %v", err)
	}
	reg, err := state.LoadRegistry()
	if err != nil || len(reg.Projects) != 0 {
		t.Fatalf("partial registry was published: %#v, %v", reg, err)
	}
}

func TestAppAddRestoresCaddyWhenAppPublicationIsUncommitted(t *testing.T) {
	repo, _ := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	sentinel := errors.New("save app failed before rename")
	restored := false
	registryCalled := false
	appAddSaveApp = func(state.App) error { return sentinel }
	appAddUpdateRegistry = func(context.Context, func(*state.Registry) error) (state.Registry, error) {
		registryCalled = true
		return state.Registry{}, nil
	}
	appAddRestoreCaddy = func(context.Context, *caddy.Admin, caddy.RouteSnapshot) error { restored = true; return nil }
	withWorkingDirectory(t, repo)
	err := newAppAddCmd().ExecuteContext(context.Background())
	if !errors.Is(err, sentinel) || !restored || registryCalled {
		t.Fatalf("err=%v restored=%t registryCalled=%t", err, restored, registryCalled)
	}
}

func TestAppAddCaddyRollbackSurvivesCallerCancellation(t *testing.T) {
	repo, _ := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	ctx, cancel := context.WithCancel(context.Background())
	sentinel := errors.New("partial Caddy mutation")
	appAddEnsureFrontDoor = func(context.Context, *caddy.Admin, state.App) error {
		cancel()
		return sentinel
	}
	restoreContextActive := false
	appAddRestoreCaddy = func(ctx context.Context, _ *caddy.Admin, _ caddy.RouteSnapshot) error {
		restoreContextActive = ctx.Err() == nil
		return nil
	}
	withWorkingDirectory(t, repo)
	err := newAppAddCmd().ExecuteContext(ctx)
	if !errors.Is(err, sentinel) || !restoreContextActive {
		t.Fatalf("err=%v restoreContextActive=%t", err, restoreContextActive)
	}
}

func TestAppAddRollsBackFreshStateWhenRegistryPublicationFails(t *testing.T) {
	repo, configDir := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	sentinel := errors.New("injected registry failure")
	caddyRestored := false
	appAddUpdateRegistry = func(context.Context, func(*state.Registry) error) (state.Registry, error) {
		return state.Registry{}, sentinel
	}
	appAddRestoreCaddy = func(context.Context, *caddy.Admin, caddy.RouteSnapshot) error { caddyRestored = true; return nil }
	withWorkingDirectory(t, repo)
	if err := newAppAddCmd().ExecuteContext(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("app add = %v", err)
	}
	if _, err := state.LoadApp(configDir); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("fresh state remained after registry failure: %v", err)
	}
	if !caddyRestored {
		t.Fatal("Caddy routes were not restored")
	}
}

func TestAppAddRestoresExistingStateWhenRegistryPublicationFails(t *testing.T) {
	repo, configDir := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	existing := state.App{Version: state.StateVersion, Name: filepath.Base(repo), RepoPath: repo, ConfigDir: configDir,
		ControlRepoPath: filepath.Join(configDir, ".control.git"), BaseSiding: "one", BaseCommit: "original-base", Memory: "3g",
		Sidings: map[string]state.Siding{"one": {Name: "one", MaterializationPhase: state.PhaseParked}}}
	if err := state.SaveApp(existing); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("injected registry failure")
	appAddUpdateRegistry = func(context.Context, func(*state.Registry) error) (state.Registry, error) {
		return state.Registry{}, sentinel
	}
	withWorkingDirectory(t, repo)
	if err := newAppAddCmd().ExecuteContext(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("app add = %v", err)
	}
	got, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseCommit != "original-base" || got.Memory != "3g" || got.Sidings["one"].MaterializationPhase != state.PhaseParked {
		t.Fatalf("existing state was not restored: %#v", got)
	}
}

func TestAppAddCombinesRegistryAndRollbackFailures(t *testing.T) {
	repo, _ := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	registryFailure := errors.New("registry failed")
	rollbackFailure := errors.New("rollback failed")
	caddyFailure := errors.New("caddy rollback failed")
	appAddUpdateRegistry = func(context.Context, func(*state.Registry) error) (state.Registry, error) {
		return state.Registry{}, registryFailure
	}
	appAddRollbackState = func(bool, state.App, state.App) error { return rollbackFailure }
	appAddRestoreCaddy = func(context.Context, *caddy.Admin, caddy.RouteSnapshot) error { return caddyFailure }
	withWorkingDirectory(t, repo)
	err := newAppAddCmd().ExecuteContext(context.Background())
	if !errors.Is(err, registryFailure) || !errors.Is(err, rollbackFailure) || !errors.Is(err, caddyFailure) {
		t.Fatalf("combined publication error = %v", err)
	}
}

func TestAppAddContinuesRegistryAfterCommittedAppPublication(t *testing.T) {
	repo, configDir := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	realSave := appAddSaveApp
	durabilityFailure := errors.New("directory sync failed")
	appAddSaveApp = func(app state.App) error {
		if err := realSave(app); err != nil {
			return err
		}
		return &state.CommittedDurabilityError{Path: filepath.Join(app.ConfigDir, "state-v2.json"), Err: durabilityFailure}
	}
	caddyRestored := false
	appAddRestoreCaddy = func(context.Context, *caddy.Admin, caddy.RouteSnapshot) error { caddyRestored = true; return nil }
	withWorkingDirectory(t, repo)
	err := newAppAddCmd().ExecuteContext(context.Background())
	var committed *state.CommittedDurabilityError
	if !errors.As(err, &committed) || caddyRestored {
		t.Fatalf("err=%v caddyRestored=%t", err, caddyRestored)
	}
	registry, loadErr := state.LoadRegistry()
	if loadErr != nil || registry.Projects[filepath.Base(repo)] != configDir {
		t.Fatalf("registry=%#v err=%v", registry, loadErr)
	}
}

func TestAppAddDoesNotCompensateCommittedRegistryPublication(t *testing.T) {
	repo, configDir := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	realRegistry := appAddUpdateRegistry
	durabilityFailure := errors.New("registry directory sync failed")
	appAddUpdateRegistry = func(ctx context.Context, mutate func(*state.Registry) error) (state.Registry, error) {
		registry, err := realRegistry(ctx, mutate)
		if err != nil {
			return registry, err
		}
		return registry, &state.CommittedDurabilityError{Path: "registry.json", Err: durabilityFailure}
	}
	stateRolledBack := false
	caddyRestored := false
	appAddRollbackState = func(bool, state.App, state.App) error { stateRolledBack = true; return nil }
	appAddRestoreCaddy = func(context.Context, *caddy.Admin, caddy.RouteSnapshot) error { caddyRestored = true; return nil }
	withWorkingDirectory(t, repo)
	err := newAppAddCmd().ExecuteContext(context.Background())
	var committed *state.CommittedDurabilityError
	if !errors.As(err, &committed) || stateRolledBack || caddyRestored {
		t.Fatalf("err=%v stateRolledBack=%t caddyRestored=%t", err, stateRolledBack, caddyRestored)
	}
	if _, err := state.LoadApp(configDir); err != nil {
		t.Fatalf("published app was compensated: %v", err)
	}
}

func TestAppAddCommittedAppAndUncommittedRegistryKeepsVisibleApp(t *testing.T) {
	originalChannel := config.Channel
	config.Channel = "beta"
	defer func() { config.Channel = originalChannel }()
	repo, configDir := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	realSave := appAddSaveApp
	appDurability := errors.New("app durability")
	registryFailure := errors.New("registry failed before rename")
	appAddSaveApp = func(app state.App) error {
		if err := realSave(app); err != nil {
			return err
		}
		return &state.CommittedDurabilityError{Path: filepath.Join(configDir, "state-v2.json"), Err: appDurability}
	}
	appAddUpdateRegistry = func(context.Context, func(*state.Registry) error) (state.Registry, error) {
		return state.Registry{}, registryFailure
	}
	stateRolledBack := false
	caddyRestored := false
	appAddRollbackState = func(bool, state.App, state.App) error { stateRolledBack = true; return nil }
	appAddRestoreCaddy = func(context.Context, *caddy.Admin, caddy.RouteSnapshot) error { caddyRestored = true; return nil }
	withWorkingDirectory(t, repo)
	err := newAppAddCmd().ExecuteContext(context.Background())
	if !errors.Is(err, appDurability) || !errors.Is(err, registryFailure) || stateRolledBack || caddyRestored {
		t.Fatalf("err=%v stateRolledBack=%t caddyRestored=%t", err, stateRolledBack, caddyRestored)
	}
	message := err.Error()
	if !strings.Contains(message, "`shunt-beta app add`") || !strings.Contains(message, "again from this repository") || !strings.Contains(message, "idempotent registration") {
		t.Fatalf("recovery guidance = %q", message)
	}
	for _, leaked := range []string{configDir, "state-v2.json", "registry.json", "do not retry"} {
		if strings.Contains(message, leaked) {
			t.Fatalf("recovery guidance leaked or contradicted recovery with %q: %q", leaked, message)
		}
	}
	var recovery *appAddRecoveryError
	if !errors.As(err, &recovery) {
		t.Fatalf("error type = %T, want *appAddRecoveryError", err)
	}
	var committed *state.CommittedDurabilityError
	if !errors.As(err, &committed) || strings.Count(message, "`shunt-beta app add`") != 1 {
		t.Fatalf("recovery causes or command are ambiguous: %q", message)
	}
	if _, err := state.LoadApp(configDir); err != nil {
		t.Fatalf("committed app was compensated: %v", err)
	}
}

func TestAppAddReturnsBothCommittedDurabilityFailuresAfterCoherentPublication(t *testing.T) {
	repo, configDir := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	realSave := appAddSaveApp
	realRegistry := appAddUpdateRegistry
	appDurability := errors.New("app directory sync failed")
	registryDurability := errors.New("registry directory sync failed")
	appAddSaveApp = func(app state.App) error {
		if err := realSave(app); err != nil {
			return err
		}
		return &state.CommittedDurabilityError{Path: filepath.Join(configDir, "state-v2.json"), Err: appDurability}
	}
	appAddUpdateRegistry = func(ctx context.Context, mutate func(*state.Registry) error) (state.Registry, error) {
		registry, err := realRegistry(ctx, mutate)
		if err != nil {
			return registry, err
		}
		return registry, &state.CommittedDurabilityError{Path: "registry.json", Err: registryDurability}
	}
	compensated := false
	appAddRollbackState = func(bool, state.App, state.App) error { compensated = true; return nil }
	appAddRestoreCaddy = func(context.Context, *caddy.Admin, caddy.RouteSnapshot) error { compensated = true; return nil }
	withWorkingDirectory(t, repo)
	err := newAppAddCmd().ExecuteContext(context.Background())
	if !errors.Is(err, appDurability) || !errors.Is(err, registryDurability) || compensated {
		t.Fatalf("err=%v compensated=%t", err, compensated)
	}
	registry, registryErr := state.LoadRegistry()
	if _, appErr := state.LoadApp(configDir); appErr != nil || registryErr != nil || registry.Projects[filepath.Base(repo)] != configDir {
		t.Fatalf("appErr=%v registry=%#v registryErr=%v", appErr, registry, registryErr)
	}
}

func TestAppAddSnapshotsOldAndNewRoutesAndRestoresLiveRouteBeforePublication(t *testing.T) {
	repo, configDir := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	existing := state.App{Version: state.StateVersion, Name: filepath.Base(repo), RepoPath: repo, ConfigDir: configDir,
		FrontDoor:  []state.Route{{Key: "old", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "old-route"}},
		LiveSiding: "one", Sidings: map[string]state.Siding{"one": {Name: "one", Container: "sample-one", Bridges: map[string]int{"web": 3200}}}}
	if err := state.SaveApp(existing); err != nil {
		t.Fatal(err)
	}
	var snapshotRoutes []state.Route
	pointed := false
	deletedOldRoute := false
	appAddSnapshotCaddy = func(_ context.Context, _ *caddy.Admin, _ string, routes []state.Route) (caddy.RouteSnapshot, error) {
		snapshotRoutes = append([]state.Route(nil), routes...)
		oldPath, _, err := caddy.ServerForRoute(filepath.Base(repo), existing.FrontDoor[0], false)
		return caddy.RouteSnapshot{Entries: []caddy.RouteSnapshotEntry{{Path: oldPath, Exists: true, Body: json.RawMessage(`{"old":true}`)}}}, err
	}
	appAddDeleteRoute = func(context.Context, *caddy.Admin, string) error { deletedOldRoute = true; return nil }
	appAddPointCaddy = func(_ context.Context, _ state.App, siding *state.Siding) error {
		pointed = true
		siding.LastIP = "192.0.2.10"
		return nil
	}
	withWorkingDirectory(t, repo)
	if err := newAppAddCmd().ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(snapshotRoutes) != 2 || !deletedOldRoute || !pointed {
		t.Fatalf("snapshot routes=%#v deletedOldRoute=%t pointed=%t", snapshotRoutes, deletedOldRoute, pointed)
	}
	published, err := state.LoadApp(configDir)
	if err != nil || published.Sidings["one"].LastIP != "192.0.2.10" {
		t.Fatalf("published=%#v err=%v", published, err)
	}
}

func TestAppAddRestoresSnapshotWhenLiveRouteRepointFails(t *testing.T) {
	repo, configDir := appAddFixture(t)
	restore := stubAppAddDependencies(t)
	defer restore()
	existing := state.App{Version: state.StateVersion, Name: filepath.Base(repo), RepoPath: repo, ConfigDir: configDir,
		FrontDoor:  []state.Route{{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "old-route"}},
		LiveSiding: "one", Sidings: map[string]state.Siding{"one": {Name: "one", Bridges: map[string]int{"web": 3200}}}}
	if err := state.SaveApp(existing); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("live route repoint failed")
	restored := false
	saved := false
	appAddPointCaddy = func(context.Context, state.App, *state.Siding) error { return sentinel }
	appAddRestoreCaddy = func(context.Context, *caddy.Admin, caddy.RouteSnapshot) error { restored = true; return nil }
	appAddSaveApp = func(state.App) error { saved = true; return nil }
	withWorkingDirectory(t, repo)
	err := newAppAddCmd().ExecuteContext(context.Background())
	if !errors.Is(err, sentinel) || !restored || saved {
		t.Fatalf("err=%v restored=%t saved=%t", err, restored, saved)
	}
}

func appAddFixture(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "sample")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	contract := `{"runner":"node","start":"npm start","frontDoor":[{"key":"web","kind":"http","listenPort":3000,"resource":"web","guestPort":3000}]}`
	if err := os.WriteFile(filepath.Join(repo, ".shunt.app.json"), []byte(contract), 0o600); err != nil {
		t.Fatal(err)
	}
	configDir, err := config.ProjectConfigDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	return repo, configDir
}

func stubAppAddDependencies(t *testing.T) func() {
	t.Helper()
	originalEnsure, originalPrepare := appAddEnsureControl, appAddPrepareCaddy
	originalDelete, originalFrontDoor, originalPoint := appAddDeleteRoute, appAddEnsureFrontDoor, appAddPointCaddy
	originalSnapshot, originalRestoreCaddy := appAddSnapshotCaddy, appAddRestoreCaddy
	originalSave, originalRegistry, originalRollback := appAddSaveApp, appAddUpdateRegistry, appAddRollbackState
	appAddEnsureControl = func(context.Context, string, string, string, string) (string, error) { return "pinned-seed", nil }
	appAddPrepareCaddy = func(context.Context) (*caddy.Admin, error) { return nil, nil }
	appAddDeleteRoute = func(context.Context, *caddy.Admin, string) error { return nil }
	appAddEnsureFrontDoor = func(context.Context, *caddy.Admin, state.App) error { return nil }
	appAddSnapshotCaddy = func(context.Context, *caddy.Admin, string, []state.Route) (caddy.RouteSnapshot, error) {
		return caddy.RouteSnapshot{}, nil
	}
	appAddRestoreCaddy = func(context.Context, *caddy.Admin, caddy.RouteSnapshot) error { return nil }
	appAddPointCaddy = func(context.Context, state.App, *state.Siding) error { return nil }
	return func() {
		appAddEnsureControl, appAddPrepareCaddy = originalEnsure, originalPrepare
		appAddDeleteRoute, appAddEnsureFrontDoor, appAddPointCaddy = originalDelete, originalFrontDoor, originalPoint
		appAddSnapshotCaddy, appAddRestoreCaddy = originalSnapshot, originalRestoreCaddy
		appAddSaveApp, appAddUpdateRegistry, appAddRollbackState = originalSave, originalRegistry, originalRollback
	}
}

func withWorkingDirectory(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func TestValidateBaselineVolumeChangeRejectsInitializedBaseline(t *testing.T) {
	dir := t.TempDir()
	baseline, err := databaseline.New(dir, []string{"database"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := baseline.InitializeEmpty(context.Background()); err != nil {
		t.Fatal(err)
	}
	existing := state.App{ConfigDir: dir, Volumes: []string{"database"}}
	err = validateBaselineVolumeChange(context.Background(), existing, []string{"database", "files"})
	if err == nil || !strings.Contains(err.Error(), "cannot change") {
		t.Fatalf("validateBaselineVolumeChange() error = %v", err)
	}
}

func TestValidateBaselineVolumeChangeAllowsUninitializedBaselineAndReordering(t *testing.T) {
	existing := state.App{ConfigDir: t.TempDir(), Volumes: []string{"database", "files"}}
	if err := validateBaselineVolumeChange(context.Background(), existing, []string{"files", "database"}); err != nil {
		t.Fatalf("reordered volume set error = %v", err)
	}
	if err := validateBaselineVolumeChange(context.Background(), existing, []string{"database", "cache"}); err != nil {
		t.Fatalf("uninitialized volume change error = %v", err)
	}
}

func TestWarnStaleGuestsOnlyForGuestBackedSidings(t *testing.T) {
	base := state.App{
		Env:    map[string]string{"A": "1"},
		Mounts: []state.MountSpec{{Host: "~/x", Guest: "/x", ReadOnly: true}},
		Memory: "4g",
		CPUs:   "4",
	}
	guest := map[string]state.Siding{
		"one": {MaterializationPhase: state.PhaseGuest},
		"two": {MaterializationPhase: ""}, // pre-dates the phase field: still a guest
	}
	worktree := map[string]state.Siding{"one": {MaterializationPhase: state.PhaseWorktree}}

	tests := []struct {
		name    string
		mutate  func(*state.App)
		sidings map[string]state.Siding
		want    []string
		quiet   bool
	}{
		{name: "env change with guests", mutate: func(a *state.App) { a.Env = map[string]string{"A": "2"} }, sidings: guest, want: []string{"env", "reapply one", "reapply two"}},
		{name: "mounts change with guests", mutate: func(a *state.App) { a.Mounts = nil }, sidings: guest, want: []string{"mounts"}},
		{name: "memory and cpus change", mutate: func(a *state.App) { a.Memory, a.CPUs = "12g", "8" }, sidings: guest, want: []string{"memory and cpus"}},
		{name: "no guest-fixed change", mutate: func(a *state.App) { a.Start = "different" }, sidings: guest, quiet: true},
		{name: "change but only worktree sidings", mutate: func(a *state.App) { a.Env = map[string]string{"A": "2"} }, sidings: worktree, quiet: true},
		{name: "change but no sidings at all", mutate: func(a *state.App) { a.Env = nil }, sidings: nil, quiet: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			existing := base
			existing.Sidings = tc.sidings
			updated := base
			updated.Sidings = tc.sidings
			tc.mutate(&updated)

			var out strings.Builder
			warnStaleGuests(&out, existing, updated)
			if tc.quiet {
				if out.Len() != 0 {
					t.Fatalf("expected no warning, got %q", out.String())
				}
				return
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("warning missing %q:\n%s", want, out.String())
				}
			}
		})
	}
}
