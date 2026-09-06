package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/state"
)

// Shared fixtures that outlived the base command they were written for.
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
