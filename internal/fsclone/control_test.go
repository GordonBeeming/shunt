package fsclone

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureControlRepoUsesAndRepairsOwnerOnlyPermissions(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	control := filepath.Join(t.TempDir(), ".control.git")
	const origin = "https://example.test/project.git"
	if _, err := EnsureControlRepo(context.Background(), control, repo, origin, "HEAD"); err != nil {
		t.Fatal(err)
	}
	assertControlMode(t, control, 0o700)
	assertControlMode(t, filepath.Join(control, "config"), 0o600)

	if err := os.Chmod(control, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(control, "config"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureControlRepo(context.Background(), control, repo, origin, "HEAD"); err != nil {
		t.Fatal(err)
	}
	assertControlMode(t, control, 0o700)
	assertControlMode(t, filepath.Join(control, "config"), 0o600)
}

func TestEnsureControlRepoRedactsCredentialBearingOrigins(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	control := filepath.Join(t.TempDir(), ".control.git")
	const current = "https://alice:current-secret@example.test/acme/project.git?access_token=current-token"
	const requested = "https://bob:requested-secret@example.test/acme/project.git?access_token=requested-token"
	if _, err := EnsureControlRepo(context.Background(), control, repo, current, "HEAD"); err != nil {
		t.Fatal(err)
	}
	_, err := EnsureControlRepo(context.Background(), control, repo, requested, "HEAD")
	if err == nil {
		t.Fatal("EnsureControlRepo() error = nil, want origin mismatch")
	}
	message := err.Error()
	for _, secret := range []string{"alice", "current-secret", "current-token", "bob", "requested-secret", "requested-token"} {
		if strings.Contains(message, secret) {
			t.Fatalf("origin mismatch exposed %q in %q", secret, message)
		}
	}
	if !strings.Contains(message, "example.test/acme/project.git") {
		t.Fatalf("origin mismatch lost host and path context: %q", message)
	}
}

func TestAddWorktreeFromRemoteBranchUsesFetchedCommit(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	writer := filepath.Join(root, "writer")
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(writer, 0o755); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, remote, "init", "--bare", "-b", "main")
	gitOutput(t, writer, "init", "-b", "main")
	gitOutput(t, writer, "config", "user.name", "Shunt Test")
	gitOutput(t, writer, "config", "user.email", "shunt@example.test")
	gitOutput(t, writer, "config", "commit.gpgsign", "false")
	writeTestFile(t, writer, "state.txt", "main\n")
	gitOutput(t, writer, "add", "state.txt")
	gitOutput(t, writer, "commit", "-m", "main")
	gitOutput(t, writer, "checkout", "-b", "feature")
	writeTestFile(t, writer, "state.txt", "feature one\n")
	gitOutput(t, writer, "commit", "-am", "feature one")
	gitOutput(t, writer, "remote", "add", "origin", remote)
	gitOutput(t, writer, "push", "-u", "origin", "main", "feature")

	control := filepath.Join(root, "control.git")
	if _, err := EnsureControlRepo(context.Background(), control, writer, remote, "main"); err != nil {
		t.Fatal(err)
	}
	stale := gitOutput(t, control, "rev-parse", "refs/heads/feature")

	writeTestFile(t, writer, "state.txt", "feature two\n")
	gitOutput(t, writer, "commit", "-am", "feature two")
	gitOutput(t, writer, "push", "origin", "feature")
	want := gitOutput(t, writer, "rev-parse", "HEAD")
	if stale == want {
		t.Fatal("test setup did not create a stale local branch")
	}

	destination := filepath.Join(root, "siding")
	got, err := AddWorktreeFromRemoteBranch(context.Background(), control, destination, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if head := gitOutput(t, destination, "rev-parse", "HEAD"); got != want || head != want {
		t.Fatalf("remote worktree commit = %q, HEAD = %q, want %q", got, head, want)
	}
	if upstream := gitOutput(t, destination, "rev-parse", "--abbrev-ref", "@{upstream}"); upstream != "origin/feature" {
		t.Fatalf("upstream = %q, want origin/feature", upstream)
	}
}

func assertControlMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s permissions = %o, want %o", path, got, want)
	}
}
