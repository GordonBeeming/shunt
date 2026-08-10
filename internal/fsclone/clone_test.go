package fsclone

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/proc"
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

func TestEnsureControlRepoIsIndependentAndPinsBase(t *testing.T) {
	repo, _, workspaceCommit := newWorktreeTestRepo(t)
	control := filepath.Join(t.TempDir(), ".control.git")
	const origin = "https://example.test/acme/project.git"

	commit, err := EnsureControlRepo(context.Background(), control, repo, origin, "gitbutler/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if commit != workspaceCommit {
		t.Fatalf("base commit = %s, want %s", commit, workspaceCommit)
	}
	if got := gitOutput(t, control, "remote", "get-url", "origin"); got != origin {
		t.Fatalf("origin = %q", got)
	}
	if got := gitOutput(t, control, "config", "--local", "--get", "user.email"); got != "shunt@example.test" {
		t.Fatalf("copied identity email = %q", got)
	}
	if got := gitOutput(t, control, "rev-parse", BaseRef); got != workspaceCommit {
		t.Fatalf("base ref = %s, want %s", got, workspaceCommit)
	}
	if _, err := os.Lstat(filepath.Join(control, "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("control repo has alternates dependency: %v", err)
	}
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, control, "cat-file", "-t", workspaceCommit); got != "commit" {
		t.Fatalf("independent object type = %q", got)
	}
	if again, err := EnsureControlRepo(context.Background(), control, "/now/missing", origin, "HEAD"); err != nil || again != workspaceCommit {
		t.Fatalf("existing EnsureControlRepo() = %q, %v", again, err)
	}
}

func TestPinBaseCommitImportsNewOwnerCommit(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	control := filepath.Join(t.TempDir(), ".control.git")
	if _, err := EnsureControlRepo(context.Background(), control, repo, "https://example.test/project.git", "HEAD"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, repo, "new.txt", "new\n")
	gitOutput(t, repo, "add", "new.txt")
	gitOutput(t, repo, "commit", "-m", "new owner commit")
	want := gitOutput(t, repo, "rev-parse", "HEAD")

	got, err := PinBaseCommit(context.Background(), control, repo, want)
	if err != nil {
		t.Fatal(err)
	}
	if got != want || gitOutput(t, control, "rev-parse", BaseRef) != want {
		t.Fatalf("PinBaseCommit() = %q, want %q", got, want)
	}
}

func TestResolveStartPointImportsWithoutMovingBase(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	control := filepath.Join(t.TempDir(), ".control.git")
	base, err := EnsureControlRepo(context.Background(), control, repo, "", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "checkout", "-b", "alternate")
	writeTestFile(t, repo, "alternate.txt", "alternate\n")
	gitOutput(t, repo, "add", "alternate.txt")
	gitOutput(t, repo, "commit", "-m", "alternate start")
	want := gitOutput(t, repo, "rev-parse", "HEAD")

	got, err := ResolveStartPoint(context.Background(), control, repo, "alternate")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ResolveStartPoint() = %q, want %q", got, want)
	}
	if pinned := gitOutput(t, control, "rev-parse", BaseRef); pinned != base {
		t.Fatalf("explicit start moved source base to %q, want %q", pinned, base)
	}
}

func TestEnsureControlRepoSupportsRepositoryWithoutOrigin(t *testing.T) {
	repo, _, workspaceCommit := newWorktreeTestRepo(t)
	control := filepath.Join(t.TempDir(), ".control.git")
	commit, err := EnsureControlRepo(context.Background(), control, repo, "", "HEAD")
	if err != nil || commit != workspaceCommit {
		t.Fatalf("EnsureControlRepo() = %q, %v", commit, err)
	}
	remotes := gitOutput(t, control, "remote")
	if remotes != "" {
		t.Fatalf("control remotes = %q, want none", remotes)
	}
}

func TestRemoveWorktreeSurfacesBranchDeletionFailure(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	err := RemoveWorktree(context.Background(), repo, filepath.Join(t.TempDir(), "absent"), "gitbutler/workspace")
	if err == nil || !strings.Contains(err.Error(), "delete branch") {
		t.Fatalf("RemoveWorktree() error = %v", err)
	}
}

func TestRemoveWorktreeRefusesUnregisteredDestination(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	dest := filepath.Join(t.TempDir(), "unregistered")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	err := RemoveWorktree(context.Background(), repo, dest, "")
	if err == nil || !strings.Contains(err.Error(), "unregistered") {
		t.Fatalf("RemoveWorktree() error = %v", err)
	}
}

func TestRemoveWorktreePreservesUnrelatedStaleRegistration(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	root := t.TempDir()
	target := filepath.Join(root, "target")
	stale := filepath.Join(root, "unrelated-stale")
	if err := AddWorktree(context.Background(), repo, target, "shunt/remove-target", "HEAD"); err != nil {
		t.Fatal(err)
	}
	makeWorktreeRegistrationStale(t, repo, stale, "shunt/remove-unrelated")

	if err := RemoveWorktree(context.Background(), repo, target, "shunt/remove-target"); err != nil {
		t.Fatal(err)
	}
	assertWorktreeRegistration(t, repo, target, false)
	assertWorktreeRegistration(t, repo, stale, true)
}

func TestRemoveWorktreeRetiresExactMissingRegistrationOnly(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	root := t.TempDir()
	target := filepath.Join(root, "missing-target")
	if err := AddWorktree(context.Background(), repo, target, "shunt/remove-missing-target", "HEAD"); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "unrelated-stale")
	makeWorktreeRegistrationStale(t, repo, stale, "shunt/remove-missing-unrelated")
	if err := os.Rename(target, target+"-moved"); err != nil {
		t.Fatal(err)
	}

	if err := RemoveWorktree(context.Background(), repo, target, "shunt/remove-missing-target"); err != nil {
		t.Fatal(err)
	}
	assertWorktreeRegistration(t, repo, target, false)
	assertWorktreeRegistration(t, repo, stale, true)
}

func TestAddWorktreeCleanupPreservesUnrelatedStaleRegistration(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := AddWorktree(context.Background(), repo, target, "shunt/add-old-target", "HEAD"); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "unrelated-stale")
	makeWorktreeRegistrationStale(t, repo, stale, "shunt/add-unrelated")
	if err := os.Rename(target, target+"-moved"); err != nil {
		t.Fatal(err)
	}

	if err := AddWorktree(context.Background(), repo, target, "shunt/add-new-target", "HEAD"); err != nil {
		t.Fatal(err)
	}
	assertWorktreeRegistration(t, repo, target, true)
	assertWorktreeRegistration(t, repo, stale, true)
}

func TestAddWorktreeTrackingCleanupPreservesUnrelatedStaleRegistration(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	remote := filepath.Join(t.TempDir(), "origin.git")
	gitOutput(t, filepath.Dir(remote), "clone", "--bare", repo, remote)
	gitOutput(t, repo, "remote", "add", "origin", remote)

	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := AddWorktree(context.Background(), repo, target, "shunt/tracking-old-target", "HEAD"); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "unrelated-stale")
	makeWorktreeRegistrationStale(t, repo, stale, "shunt/tracking-unrelated")
	if err := os.Rename(target, target+"-moved"); err != nil {
		t.Fatal(err)
	}

	if err := AddWorktreeTracking(context.Background(), repo, target, "main"); err != nil {
		t.Fatal(err)
	}
	assertWorktreeRegistration(t, repo, target, true)
	assertWorktreeRegistration(t, repo, stale, true)
}

func TestQuarantineWorktreeRestoresWithoutRetiringRegistration(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	dest := filepath.Join(t.TempDir(), "siding")
	if err := AddWorktree(context.Background(), repo, dest, "shunt/quarantine-restore", "HEAD"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dest, "late.txt", "keep me")

	quarantine, err := QuarantineWorktree(context.Background(), repo, dest, "shunt/quarantine-restore", "op-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repairQuarantinedWorktreeRegistration(context.Background(), quarantine); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("original path still exists: %v", err)
	}
	if err := RestoreQuarantinedWorktree(quarantine); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "late.txt")); err != nil || string(got) != "keep me" {
		t.Fatalf("restored file = %q, %v", got, err)
	}
	registered, err := worktreeRegistered(context.Background(), repo, dest)
	if err != nil || !registered {
		t.Fatalf("registered = %t, %v", registered, err)
	}
}

func TestRetireQuarantinedWorktreeRemovesExactRegistrationAndBranch(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	root := t.TempDir()
	dest := filepath.Join(root, "siding")
	const branch = "shunt/quarantine-retire"
	if err := AddWorktree(context.Background(), repo, dest, branch, "HEAD"); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "unrelated-stale")
	if err := AddWorktree(context.Background(), repo, stale, "shunt/unrelated-stale", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stale, stale+"-moved"); err != nil {
		t.Fatal(err)
	}
	if registered, err := worktreeRegistered(context.Background(), repo, stale); err != nil || !registered {
		t.Fatalf("unrelated stale registration setup = %t, %v", registered, err)
	}
	quarantine, err := QuarantineWorktree(context.Background(), repo, dest, branch, "op-2")
	if err != nil {
		t.Fatal(err)
	}
	if err := RetireQuarantinedWorktree(context.Background(), quarantine); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(quarantine.RecoveryPath); !os.IsNotExist(err) {
		t.Fatalf("recovery path still exists: %v", err)
	}
	registered, err := worktreeRegistered(context.Background(), repo, dest)
	if err != nil || registered {
		t.Fatalf("registered = %t, %v", registered, err)
	}
	result, err := proc.Run(context.Background(), "git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil || result.ExitCode != 1 {
		t.Fatalf("branch remains: result=%+v err=%v", result, err)
	}
	if registered, err := worktreeRegistered(context.Background(), repo, stale); err != nil || !registered {
		t.Fatalf("unrelated stale registration was retired = %t, %v", registered, err)
	}
}

func TestRetireQuarantinedWorktreePreservesUsableGitRecoveryOnCleanupFailure(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	root := t.TempDir()
	dest := filepath.Join(root, "siding")
	const branch = "shunt/quarantine-recovery"
	if err := AddWorktree(context.Background(), repo, dest, branch, "HEAD"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dest, "staged.txt", "recover staged state\n")
	gitOutput(t, dest, "add", "staged.txt")
	stale := filepath.Join(root, "unrelated-stale")
	if err := AddWorktree(context.Background(), repo, stale, "shunt/unrelated-stale-failure", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stale, stale+"-moved"); err != nil {
		t.Fatal(err)
	}
	quarantine, err := QuarantineWorktree(context.Background(), repo, dest, branch, "op-3")
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("injected cleanup failure")
	err = retireQuarantinedWorktree(context.Background(), quarantine, func(string) error { return wantErr })
	if err == nil || !strings.Contains(err.Error(), quarantine.RecoveryPath) || !errors.Is(err, wantErr) {
		t.Fatalf("retire error = %v", err)
	}
	if _, statErr := os.Lstat(quarantine.RecoveryPath); statErr != nil {
		t.Fatalf("recovery path was not preserved: %v", statErr)
	}
	if status := gitOutput(t, quarantine.RecoveryPath, "status", "--short"); status != "A  staged.txt" {
		t.Fatalf("recovery status = %q, want staged file", status)
	}
	if staged := gitOutput(t, quarantine.RecoveryPath, "diff", "--cached", "--name-only"); staged != "staged.txt" {
		t.Fatalf("recovery index = %q, want staged.txt", staged)
	}
	if got := gitOutput(t, quarantine.RecoveryPath, "symbolic-ref", "--short", "HEAD"); got != branch {
		t.Fatalf("recovery branch = %q, want %q", got, branch)
	}
	if _, err := proc.Run(context.Background(), "git", "-C", repo, "show-ref", "--verify", "refs/heads/"+branch); err != nil {
		t.Fatalf("recovery branch ref was deleted: %v", err)
	}
	if registered, err := worktreeRegistered(context.Background(), repo, quarantine.RecoveryPath); err != nil || !registered {
		t.Fatalf("recovery registration = %t, %v", registered, err)
	}
	if registered, err := worktreeRegistered(context.Background(), repo, stale); err != nil || !registered {
		t.Fatalf("unrelated stale registration was retired = %t, %v", registered, err)
	}
}

func TestLinkedWorktreeAdminValidationRejectsPathOutsideOwnerMetadata(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	dest := filepath.Join(t.TempDir(), "siding")
	if err := AddWorktree(context.Background(), repo, dest, "shunt/outside-admin", "HEAD"); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-admin")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, ".git"), []byte("gitdir: "+outside+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validatedLinkedWorktreeAdminPath(context.Background(), repo, dest); err == nil || !strings.Contains(err.Error(), "not an exact child") {
		t.Fatalf("outside administration path validation = %v", err)
	}
}

func TestRestoreQuarantinedWorktreePreservesRecoveryWhenOriginalReappears(t *testing.T) {
	repo, _, _ := newWorktreeTestRepo(t)
	dest := filepath.Join(t.TempDir(), "siding")
	if err := AddWorktree(context.Background(), repo, dest, "shunt/quarantine-conflict", "HEAD"); err != nil {
		t.Fatal(err)
	}
	quarantine, err := QuarantineWorktree(context.Background(), repo, dest, "shunt/quarantine-conflict", "op-4")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	err = RestoreQuarantinedWorktree(quarantine)
	if err == nil || !strings.Contains(err.Error(), quarantine.RecoveryPath) {
		t.Fatalf("restore error = %v", err)
	}
	if _, statErr := os.Lstat(quarantine.RecoveryPath); statErr != nil {
		t.Fatalf("recovery path was not preserved: %v", statErr)
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

func TestCloneVolumeSetReportsCommittedCleanupFailure(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "baseline")
	destination := filepath.Join(root, "siding", "vol")
	writeTestFile(t, filepath.Join(source, "db"), "value", "new")
	writeTestFile(t, filepath.Join(destination, "db"), "value", "old")
	var retired string
	result, err := cloneVolumeSet(context.Background(), source, destination, []string{"db"}, volumeSetOps{
		rename: os.Rename,
		renameSwap: func(stage, current string) error {
			retired = stage
			return renameSwap(stage, current)
		},
		remove: func(path string) error {
			if path == retired {
				return errors.New("cleanup denied")
			}
			return os.RemoveAll(path)
		},
	})
	var cleanup *VolumeSetCleanupError
	if !result.Committed || !errors.As(err, &cleanup) {
		t.Fatalf("cloneVolumeSet() = %#v, %v", result, err)
	}
	if len(result.RecoveryPaths) != 1 || result.RecoveryPaths[0] != retired {
		t.Fatalf("recovery paths = %v, want %s", result.RecoveryPaths, retired)
	}
	got, readErr := os.ReadFile(filepath.Join(destination, "db", "value"))
	if readErr != nil || string(got) != "new" {
		t.Fatalf("destination = %q, %v", got, readErr)
	}
	old, readErr := os.ReadFile(filepath.Join(retired, "db", "value"))
	if readErr != nil || string(old) != "old" {
		t.Fatalf("retired root = %q, %v", old, readErr)
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

func makeWorktreeRegistrationStale(t *testing.T, repo, path, branch string) {
	t.Helper()
	if err := AddWorktree(context.Background(), repo, path, branch, "HEAD"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+"-moved"); err != nil {
		t.Fatal(err)
	}
	assertWorktreeRegistration(t, repo, path, true)
}

func assertWorktreeRegistration(t *testing.T, repo, path string, want bool) {
	t.Helper()
	registered, err := worktreeRegistered(context.Background(), repo, path)
	if err != nil || registered != want {
		t.Fatalf("worktree registration for %q = %t, %v; want %t", path, registered, err, want)
	}
}
