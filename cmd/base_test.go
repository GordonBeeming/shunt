package cmd

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestValidateCleanBaseChecksWorktreeState(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
		wantErr string
	}{
		{name: "clean"},
		{name: "dirty", prepare: func(t *testing.T, src string) {
			if err := os.WriteFile(filepath.Join(src, "untracked.txt"), []byte("dirty\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, wantErr: "uncommitted or untracked"},
		{name: "wrong branch", prepare: func(t *testing.T, src string) {
			sourceGitOutput(t, src, "checkout", "-b", "wrong")
		}, wantErr: "not on its recorded branch"},
		{name: "detached", prepare: func(t *testing.T, src string) {
			sourceGitOutput(t, src, "checkout", "--detach")
		}, wantErr: "not on its recorded branch"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := newSourceStateTestApp(t, true)
			src, _, err := siding.Paths(app, app.BaseSiding)
			if err != nil {
				t.Fatal(err)
			}
			if test.prepare != nil {
				test.prepare(t, src)
			}
			got, err := validateCleanBase(context.Background(), &app)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("validateCleanBase() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if head := sourceGitOutput(t, src, "rev-parse", "HEAD"); got != head || app.BaseCommit != head {
				t.Fatalf("validateCleanBase() = %q, BaseCommit = %q, want %q", got, app.BaseCommit, head)
			}
		})
	}
}

func TestSetBaseSidingPinsDirtyCandidate(t *testing.T) {
	app := newSourceStateTestApp(t, true)
	src, _, err := siding.Paths(app, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	const branch = "feature/candidate"
	if err := fsclone.AddWorktree(context.Background(), app.ControlRepoPath, src, branch, app.BaseCommit); err != nil {
		t.Fatal(err)
	}
	app.Sidings["candidate"] = state.Siding{Name: "candidate", Branch: branch, WorktreeRepoPath: app.ControlRepoPath, MaterializationPhase: state.PhaseWorktree}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "untracked.txt"), []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dirty, err := setBaseSiding(context.Background(), &app, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	want := sourceGitOutput(t, src, "rev-parse", "HEAD")
	if !dirty || app.BaseSiding != "candidate" || app.BaseCommit != want {
		t.Fatalf("setBaseSiding() dirty = %t, app = %#v, want commit %q", dirty, app, want)
	}
	loaded, err := state.LoadApp(app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BaseSiding != "candidate" || loaded.BaseCommit != want {
		t.Fatalf("persisted base = %q @ %q, want candidate @ %q", loaded.BaseSiding, loaded.BaseCommit, want)
	}
}

func TestSetBaseSidingMigratesLegacyControlRepository(t *testing.T) {
	repo, commit := newSourceStateGitRepo(t)
	configDir := filepath.Join(sourceStateTempDir(t), "config")
	legacy := state.App{
		Version:   1,
		Name:      "app",
		RepoPath:  repo,
		ConfigDir: configDir,
		Sidings: map[string]state.Siding{
			"legacy": {Name: "legacy", Branch: "feature"},
		},
	}
	src, _, err := siding.Paths(legacy, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := fsclone.AddWorktree(context.Background(), repo, src, "feature", commit); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	app, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setBaseSiding(context.Background(), &app, "legacy"); err != nil {
		t.Fatal(err)
	}
	if app.Version != state.StateVersion || app.BaseCommit != commit {
		t.Fatalf("migrated app = %#v, want version %d and commit %q", app, state.StateVersion, commit)
	}
	if got := sourceGitOutput(t, app.ControlRepoPath, "rev-parse", fsclone.BaseRef); got != commit {
		t.Fatalf("control base = %q, want %q", got, commit)
	}
	if _, err := os.Stat(filepath.Join(configDir, "state-v2.json")); err != nil {
		t.Fatalf("versioned state was not published: %v", err)
	}
}

func TestSetBaseSidingFencesRemovalFromCurrentState(t *testing.T) {
	stale := newSourceStateTestApp(t, true)
	current, err := state.LoadApp(stale.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	candidateSource, _, err := siding.Paths(current, "candidate")
	if err != nil {
		t.Fatal(err)
	}
	const candidateBranch = "feature/candidate"
	if err := fsclone.AddWorktree(context.Background(), current.ControlRepoPath, candidateSource, candidateBranch, current.BaseCommit); err != nil {
		t.Fatal(err)
	}
	current.Sidings["candidate"] = state.Siding{Name: "candidate", Branch: candidateBranch, WorktreeRepoPath: current.ControlRepoPath, MaterializationPhase: state.PhaseWorktree}
	current.Removal = &state.RemovalOperation{ID: "remove-base", Siding: current.BaseSiding, Stage: state.RemovalBasePinned}
	if err := state.SaveApp(current); err != nil {
		t.Fatal(err)
	}
	baseBefore := sourceGitOutput(t, current.ControlRepoPath, "rev-parse", fsclone.BaseRef)

	dirty, err := setBaseSiding(context.Background(), &stale, "candidate")
	if err == nil || !strings.Contains(err.Error(), "blocked while siding") {
		t.Fatalf("setBaseSiding() dirty = %t, error = %v", dirty, err)
	}
	if stale.BaseSiding == "candidate" {
		t.Fatal("removal-fenced base set changed the stale caller state")
	}
	if baseAfter := sourceGitOutput(t, current.ControlRepoPath, "rev-parse", fsclone.BaseRef); baseAfter != baseBefore {
		t.Fatalf("removal-fenced base set moved protected ref from %q to %q", baseBefore, baseAfter)
	}
	loaded, loadErr := state.LoadApp(current.ConfigDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if loaded.BaseSiding != current.BaseSiding || loaded.BaseCommit != current.BaseCommit {
		t.Fatalf("removal-fenced base set changed persisted base to %q @ %q", loaded.BaseSiding, loaded.BaseCommit)
	}
}

func newSourceStateTestApp(t *testing.T, withBase bool) state.App {
	t.Helper()
	repo, commit := newSourceStateGitRepo(t)
	configDir := filepath.Join(sourceStateTempDir(t), "config")
	control := filepath.Join(configDir, ".control.git")
	if _, err := fsclone.EnsureControlRepo(context.Background(), control, repo, "", commit); err != nil {
		t.Fatal(err)
	}
	app := state.App{
		Version:         state.StateVersion,
		Name:            "app",
		RepoPath:        repo,
		ControlRepoPath: control,
		BaseCommit:      commit,
		ConfigDir:       configDir,
		Sidings:         map[string]state.Siding{},
	}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	if !withBase {
		return app
	}
	app, _, err := createSiding(context.Background(), configDir, "base", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func newSourceStateGitRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := filepath.Join(sourceStateTempDir(t), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
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
	return repo, sourceGitOutput(t, repo, "rev-parse", "HEAD")
}

func sourceStateTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func sourceGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
