package fsclone

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddWorktreeGitButlerHeadUsesOriginDefault(t *testing.T) {
	repo, mainCommit, _ := newWorktreeTestRepo(t)
	dest := filepath.Join(t.TempDir(), "siding")

	if err := AddWorktree(context.Background(), repo, dest, "shunt/test", ""); err != nil {
		t.Fatalf("AddWorktree() error = %v", err)
	}

	if got := gitOutput(t, dest, "rev-parse", "HEAD"); got != mainCommit {
		t.Fatalf("siding HEAD = %s, want origin default %s", got, mainCommit)
	}
}

func TestAddWorktreeGitButlerHeadFallsBackToOriginMain(t *testing.T) {
	repo, mainCommit, _ := newWorktreeTestRepo(t)
	gitOutput(t, repo, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	dest := filepath.Join(t.TempDir(), "siding")

	if err := AddWorktree(context.Background(), repo, dest, "shunt/test", ""); err != nil {
		t.Fatalf("AddWorktree() error = %v", err)
	}

	if got := gitOutput(t, dest, "rev-parse", "HEAD"); got != mainCommit {
		t.Fatalf("siding HEAD = %s, want origin/main fallback %s", got, mainCommit)
	}
}

func TestAddWorktreeOrdinaryHeadUsesCurrentHead(t *testing.T) {
	repo, _, workspaceCommit := newWorktreeTestRepo(t)
	gitOutput(t, repo, "checkout", "-b", "feature")
	dest := filepath.Join(t.TempDir(), "siding")

	if err := AddWorktree(context.Background(), repo, dest, "shunt/test", ""); err != nil {
		t.Fatalf("AddWorktree() error = %v", err)
	}

	if got := gitOutput(t, dest, "rev-parse", "HEAD"); got != workspaceCommit {
		t.Fatalf("siding HEAD = %s, want ordinary HEAD %s", got, workspaceCommit)
	}
}

func TestAddWorktreeExplicitBaseWinsInGitButlerRepo(t *testing.T) {
	repo, _, workspaceCommit := newWorktreeTestRepo(t)
	dest := filepath.Join(t.TempDir(), "siding")

	if err := AddWorktree(context.Background(), repo, dest, "shunt/test", "gitbutler/workspace"); err != nil {
		t.Fatalf("AddWorktree() error = %v", err)
	}

	if got := gitOutput(t, dest, "rev-parse", "HEAD"); got != workspaceCommit {
		t.Fatalf("siding HEAD = %s, want explicit base %s", got, workspaceCommit)
	}
}

func TestVerifyCommitNamesMissingRef(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)

	err := verifyCommit(context.Background(), repo, "origin/missing")
	if err == nil {
		t.Fatal("verifyCommit() error = nil, want missing ref error")
	}
	if !strings.Contains(err.Error(), `verify commit ref "origin/missing"`) {
		t.Fatalf("verifyCommit() error = %q, want ref-specific message", err)
	}
}

func TestCloneVolumeSetReplacesWholeRoot(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "baseline")
	destination := filepath.Join(root, "siding", "vol")
	writeTestFile(t, filepath.Join(source, "db"), "value", "new db")
	writeTestFile(t, filepath.Join(source, "cache"), "value", "new cache")
	writeTestFile(t, filepath.Join(destination, "db"), "value", "old db")
	writeTestFile(t, filepath.Join(destination, "cache"), "value", "old cache")

	if err := CloneVolumeSet(context.Background(), source, destination, []string{"db", "cache"}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ volume, want string }{{"db", "new db"}, {"cache", "new cache"}} {
		got, err := os.ReadFile(filepath.Join(destination, test.volume, "value"))
		if err != nil || string(got) != test.want {
			t.Fatalf("%s = %q, %v; want %q", test.volume, got, err, test.want)
		}
	}
}

func TestCloneVolumeSetRejectsUnsafeNames(t *testing.T) {
	if err := CloneVolumeSet(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "dest"), []string{"../db"}); err == nil {
		t.Fatal("CloneVolumeSet() error = nil")
	}
}

func newWorktreeTestRepo(t *testing.T) (repo, mainCommit, workspaceCommit string) {
	t.Helper()
	repo = filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "init", "-b", "main")
	gitOutput(t, repo, "config", "user.name", "Shunt Test")
	gitOutput(t, repo, "config", "user.email", "shunt@example.test")
	gitOutput(t, repo, "config", "commit.gpgsign", "false")
	writeTestFile(t, repo, "state.txt", "main\n")
	gitOutput(t, repo, "add", "state.txt")
	gitOutput(t, repo, "commit", "-m", "main")
	mainCommit = gitOutput(t, repo, "rev-parse", "HEAD")

	// Remote-tracking refs are enough for worktree base resolution; no network or
	// bare remote is needed for this focused unit regression.
	gitOutput(t, repo, "update-ref", "refs/remotes/origin/main", mainCommit)
	gitOutput(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	gitOutput(t, repo, "checkout", "-b", "gitbutler/workspace")
	writeTestFile(t, repo, "state.txt", "gitbutler workspace\n")
	gitOutput(t, repo, "commit", "-am", "workspace")
	workspaceCommit = gitOutput(t, repo, "rev-parse", "HEAD")
	return repo, mainCommit, workspaceCommit
}

func writeTestFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmdArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
