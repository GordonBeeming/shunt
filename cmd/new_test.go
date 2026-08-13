package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestNewInitializesWorktreeOnlyStateWithoutContractOrRegistry(t *testing.T) {
	repo, configDir, commit := newShellOnlyCommandRepo(t)
	withWorkingDirectory(t, repo)

	cmd := newNewCmd()
	cmd.SetArgs([]string{"first"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}

	app, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if app.Name != filepath.Base(repo) || app.RepoPath != repo || app.ConfigDir != configDir {
		t.Fatalf("worktree-only project identity = %#v", app)
	}
	if app.ControlRepoPath != filepath.Join(configDir, ".control.git") || app.BaseCommit != commit || app.BaseSiding != "first" {
		t.Fatalf("worktree-only source state = control %q, base %q @ %q", app.ControlRepoPath, app.BaseSiding, app.BaseCommit)
	}
	if app.Runner != "" || app.Start != "" || app.Stop != "" || app.Workdir != "" || app.AppHostPath != "" || len(app.FrontDoor) != 0 || len(app.DataVolumes) != 0 || len(app.Env) != 0 || len(app.Mounts) != 0 || len(app.PrebakeImages) != 0 || len(app.PrebakeBuilds) != 0 || len(app.Volumes) != 0 || app.Memory != "" || app.CPUs != "" || app.HealthPort != 0 || app.HealthPath != "" {
		t.Fatalf("worktree-only state unexpectedly contains runtime configuration: %#v", app)
	}
	sd, ok := app.Sidings["first"]
	if !ok || sd.MaterializationPhase != state.PhaseWorktree {
		t.Fatalf("first siding = %#v, exists = %t", sd, ok)
	}
	src, _, err := siding.Paths(app, "first")
	if err != nil {
		t.Fatal(err)
	}
	if head := sourceGitOutput(t, src, "rev-parse", "HEAD"); head != commit {
		t.Fatalf("first siding HEAD = %q, want clean source commit %q", head, commit)
	}
	if _, err := os.Stat(filepath.Join(repo, ".shunt.app.json")); !os.IsNotExist(err) {
		t.Fatalf("new created or required a runtime contract: %v", err)
	}
	registry, err := state.LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, registered := registry.Projects[app.Name]; registered || len(registry.Projects) != 0 {
		t.Fatalf("worktree-only project was published to registry: %#v", registry.Projects)
	}
	registryPath, err := config.RegistryPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(registryPath); !os.IsNotExist(err) {
		t.Fatalf("worktree-only new wrote the channel registry: %v", err)
	}
}

func TestNewWithBranchWorksBeforeRegistration(t *testing.T) {
	repo, configDir, commit := newShellOnlyCommandRepo(t)
	withWorkingDirectory(t, repo)

	cmd := newNewCmd()
	cmd.SetArgs([]string{"explicit", "--branch", commit})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	app, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	src, _, err := siding.Paths(app, "explicit")
	if err != nil {
		t.Fatal(err)
	}
	if head := sourceGitOutput(t, src, "rev-parse", "HEAD"); head != commit {
		t.Fatalf("explicit siding HEAD = %q, want %q", head, commit)
	}
}

func TestNewFromSubdirectoryUsesGitTopLevel(t *testing.T) {
	repo, configDir, commit := newShellOnlyCommandRepo(t)
	nested := filepath.Join(repo, "one", "two")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	withWorkingDirectory(t, nested)

	cmd := newNewCmd()
	cmd.SetArgs([]string{"nested", "--branch", commit})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	app, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if app.RepoPath != repo || app.Name != filepath.Base(repo) {
		t.Fatalf("subdirectory initialized project as %#v", app)
	}
	wrongConfigDir, err := config.ProjectConfigDir(nested)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.LoadApp(wrongConfigDir); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("subdirectory unexpectedly received project state: %v", err)
	}
}

func TestNewOutsideGitRepositoryFailsClearly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	withWorkingDirectory(t, t.TempDir())
	cmd := newNewCmd()
	cmd.SetArgs([]string{"nowhere"})
	err := cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "requires a Git repository") {
		t.Fatalf("new outside Git error = %v", err)
	}
}

func TestAppAddEnrichesWorktreeOnlyStateAndRegisteredNewStillWorks(t *testing.T) {
	repo, configDir, commit := newShellOnlyCommandRepo(t)
	withWorkingDirectory(t, repo)
	first := newNewCmd()
	first.SetArgs([]string{"shell"})
	if err := first.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	contract := `{"runner":"node","start":"npm start","frontDoor":[{"key":"web","kind":"http","listenPort":3000,"resource":"web","guestPort":3000}]}`
	if err := os.WriteFile(filepath.Join(repo, ".shunt.app.json"), []byte(contract), 0o600); err != nil {
		t.Fatal(err)
	}
	restore := stubAppAddDependencies(t)
	t.Cleanup(restore)
	appAddEnsureControl = func(_ context.Context, control, source, _ string, _ string) (string, error) {
		if control != before.ControlRepoPath || source != repo {
			t.Fatalf("app add control/source = %q / %q", control, source)
		}
		return "different-seed", nil
	}
	if err := newAppAddCmd().ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}

	registered, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if registered.Runner != "node" || registered.Start != "npm start" || registered.ControlRepoPath != before.ControlRepoPath || registered.BaseSiding != before.BaseSiding || registered.BaseCommit != commit {
		t.Fatalf("enriched registration = %#v", registered)
	}
	if got, ok := registered.Sidings["shell"]; !ok || got.Branch != before.Sidings["shell"].Branch {
		t.Fatalf("app add did not preserve shell siding: %#v", registered.Sidings)
	}
	registry, err := state.LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if registry.Projects[registered.Name] != configDir {
		t.Fatalf("registered project entry = %#v", registry.Projects)
	}

	second := newNewCmd()
	second.SetArgs([]string{"registered", "--branch", commit})
	if err := second.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Sidings["registered"]; !ok || after.Runner != "node" {
		t.Fatalf("new on registered app = %#v", after)
	}
}

func TestConcurrentFirstNewCommandsShareOneProjectState(t *testing.T) {
	repo, configDir, commit := newShellOnlyCommandRepo(t)
	withWorkingDirectory(t, repo)
	errCh := make(chan error, 2)
	for _, name := range []string{"alpha", "beta"} {
		name := name
		go func() {
			cmd := newNewCmd()
			cmd.SetArgs([]string{name, "--branch", commit})
			errCh <- cmd.ExecuteContext(context.Background())
		}()
	}
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	app, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(app.Sidings) != 2 || app.Sidings["alpha"].Name != "alpha" || app.Sidings["beta"].Name != "beta" {
		t.Fatalf("concurrent first new state = %#v", app.Sidings)
	}
}

func newShellOnlyCommandRepo(t *testing.T) (repo, configDir, commit string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root := sourceStateTempDir(t)
	repo = filepath.Join(root, "sample")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	sourceGitOutput(t, repo, "init", "-b", "main")
	sourceGitOutput(t, repo, "config", "user.name", "Shunt Test")
	sourceGitOutput(t, repo, "config", "user.email", "shunt@example.test")
	sourceGitOutput(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceGitOutput(t, repo, "add", "source.txt")
	sourceGitOutput(t, repo, "commit", "-m", "base")
	commit = sourceGitOutput(t, repo, "rev-parse", "HEAD")
	configDir, err := config.ProjectConfigDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	return repo, configDir, commit
}

func TestCreateSidingPinsLatestCleanBaseCommit(t *testing.T) {
	app := newSourceStateTestApp(t, true)
	baseSource, _, err := siding.Paths(app, app.BaseSiding)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseSource, "source.txt"), []byte("new base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceGitOutput(t, baseSource, "add", "source.txt")
	sourceGitOutput(t, baseSource, "commit", "-m", "advance base")
	want := sourceGitOutput(t, baseSource, "rev-parse", "HEAD")
	if want == app.BaseCommit {
		t.Fatal("test setup did not advance the selected base")
	}

	app, _, err = createSiding(context.Background(), app.ConfigDir, "next", "", "")
	if err != nil {
		t.Fatal(err)
	}
	nextSource, _, err := siding.Paths(app, "next")
	if err != nil {
		t.Fatal(err)
	}
	if head := sourceGitOutput(t, nextSource, "rev-parse", "HEAD"); head != want || app.BaseCommit != want {
		t.Fatalf("new siding HEAD = %q, BaseCommit = %q, want %q", head, app.BaseCommit, want)
	}
	loaded, err := state.LoadApp(app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BaseCommit != want {
		t.Fatalf("persisted BaseCommit = %q, want %q", loaded.BaseCommit, want)
	}
}

func TestCreateSidingRejectsDirtyBaseBeforeCreatingWorktree(t *testing.T) {
	app := newSourceStateTestApp(t, true)
	baseSource, _, err := siding.Paths(app, app.BaseSiding)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseSource, "untracked.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err = createSiding(context.Background(), app.ConfigDir, "blocked", "", "")
	if err == nil || !strings.Contains(err.Error(), "uncommitted or untracked") {
		t.Fatalf("createSiding() error = %v", err)
	}
	blockedSource, _, pathErr := siding.Paths(app, "blocked")
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(blockedSource); !os.IsNotExist(statErr) {
		t.Fatalf("dirty-base creation left worktree %q: %v", blockedSource, statErr)
	}
	loaded, loadErr := state.LoadApp(app.ConfigDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, exists := loaded.Sidings["blocked"]; exists {
		t.Fatal("dirty-base creation published siding state")
	}
}

func TestCreateSidingDoesNotCompensateCommittedDurabilityError(t *testing.T) {
	app := newSourceStateTestApp(t, true)
	sentinel := errors.New("directory sync denied")
	ops := createSidingOps{saveApp: func(app state.App) error {
		if err := state.SaveApp(app); err != nil {
			return err
		}
		return &state.CommittedDurabilityError{Path: filepath.Join(app.ConfigDir, "state-v2.json"), Err: sentinel}
	}}

	_, _, err := createSidingWithOps(context.Background(), app.ConfigDir, "committed", "", "", ops)
	var committed *state.CommittedDurabilityError
	if !errors.As(err, &committed) || !errors.Is(err, sentinel) {
		t.Fatalf("createSidingWithOps() error = %v", err)
	}
	loaded, loadErr := state.LoadApp(app.ConfigDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, exists := loaded.Sidings["committed"]; !exists {
		t.Fatal("committed durability error triggered state compensation")
	}
	source, _, pathErr := siding.Paths(loaded, "committed")
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(source); statErr != nil {
		t.Fatalf("committed durability error removed worktree: %v", statErr)
	}
}

func TestCreateSidingCleansUpUnpublishedStateFailure(t *testing.T) {
	app := newSourceStateTestApp(t, true)
	sentinel := errors.New("state write denied")
	ops := createSidingOps{saveApp: func(state.App) error { return sentinel }}

	_, _, err := createSidingWithOps(context.Background(), app.ConfigDir, "unpublished", "", "", ops)
	if !errors.Is(err, sentinel) {
		t.Fatalf("createSidingWithOps() error = %v", err)
	}
	loaded, loadErr := state.LoadApp(app.ConfigDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, exists := loaded.Sidings["unpublished"]; exists {
		t.Fatal("failed state write published siding")
	}
	source, _, pathErr := siding.Paths(app, "unpublished")
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(source); !os.IsNotExist(statErr) {
		t.Fatalf("failed state write left worktree %q: %v; creation error: %v", source, statErr, err)
	}
}

func TestCreateFirstSidingSelectsItAsBase(t *testing.T) {
	app := newSourceStateTestApp(t, false)
	app, created, err := createSiding(context.Background(), app.ConfigDir, "first", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if app.BaseSiding != "first" || app.BaseCommit == "" || created.MaterializationPhase != state.PhaseWorktree {
		t.Fatalf("first siding state = %#v, app base = %q @ %q", created, app.BaseSiding, app.BaseCommit)
	}
}

func TestCreateSidingFencesRemovalFromCurrentState(t *testing.T) {
	app := newSourceStateTestApp(t, true)
	blockedControl := filepath.Join(app.ConfigDir, "blocked-control.git")
	app.ControlRepoPath = blockedControl
	app.Removal = &state.RemovalOperation{ID: "remove-base", Siding: app.BaseSiding, Stage: state.RemovalBasePinned}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}

	_, _, err := createSiding(context.Background(), app.ConfigDir, "blocked", "HEAD", "")
	if err == nil || !strings.Contains(err.Error(), "blocked while siding") {
		t.Fatalf("createSiding() error = %v", err)
	}
	if _, statErr := os.Stat(blockedControl); !os.IsNotExist(statErr) {
		t.Fatalf("removal-fenced create touched control repository: %v", statErr)
	}
	source, _, pathErr := siding.Paths(app, "blocked")
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(source); !os.IsNotExist(statErr) {
		t.Fatalf("removal-fenced create left worktree %q: %v", source, statErr)
	}
	loaded, loadErr := state.LoadApp(app.ConfigDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, exists := loaded.Sidings["blocked"]; exists {
		t.Fatal("removal-fenced create changed siding state")
	}
}
