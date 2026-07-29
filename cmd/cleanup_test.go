package cmd

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestParseCleanupSelection(t *testing.T) {
	candidates := []cleanupCandidate{{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"}}
	tests := []struct {
		name    string
		input   string
		want    []string
		wantErr bool
	}{
		{name: "empty selects nothing", input: "", want: nil},
		{name: "one", input: "2", want: []string{"beta"}},
		{name: "multiple", input: "1, 3", want: []string{"alpha", "gamma"}},
		{name: "duplicates", input: "2 2", want: []string{"beta"}},
		{name: "all", input: "ALL", want: []string{"alpha", "beta", "gamma"}},
		{name: "invalid number", input: "4", wantErr: true},
		{name: "invalid token", input: "alpha", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseCleanupSelection(test.input, candidates)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseCleanupSelection() error = %v, wantErr %v", err, test.wantErr)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parseCleanupSelection() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestPickCleanupByNumberShowsDirtyStateAndSelectsSeveral(t *testing.T) {
	candidates := []cleanupCandidate{
		{Name: "alpha", Status: "idle"},
		{Name: "beta", Status: "stopped", Dirty: true},
		{Name: "gamma", Status: "up"},
	}
	var out bytes.Buffer
	got, err := pickCleanupByNumber(candidates, bufio.NewReader(strings.NewReader("1,3\n")), &out)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "gamma"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pickCleanupByNumber() = %#v, want %#v", got, want)
	}
	if !strings.Contains(out.String(), "beta  (stopped, work not safely saved)") {
		t.Fatalf("picker output did not mark dirty siding:\n%s", out.String())
	}
}

func TestConfirmDirtyCleanupDefaultsToNo(t *testing.T) {
	var out bytes.Buffer
	confirmed, err := confirmDirtyCleanup([]string{"beta"}, bufio.NewReader(strings.NewReader("\n")), &out)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed {
		t.Fatal("empty confirmation should not discard changes")
	}
	if !strings.Contains(out.String(), "beta") {
		t.Fatalf("confirmation did not name dirty siding:\n%s", out.String())
	}
}

func TestConfirmDirtyCleanupAcceptsYes(t *testing.T) {
	confirmed, err := confirmDirtyCleanup([]string{"beta"}, bufio.NewReader(strings.NewReader("yes\n")), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !confirmed {
		t.Fatal("yes confirmation was rejected")
	}
}

func TestNumberedCleanupPreservesPipedConfirmation(t *testing.T) {
	candidates := []cleanupCandidate{{Name: "alpha"}}
	in := bufio.NewReader(strings.NewReader("1\nyes\n"))

	selected, err := pickCleanupByNumber(candidates, in, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selected, []string{"alpha"}) {
		t.Fatalf("pickCleanupByNumber() = %#v, want alpha", selected)
	}

	confirmed, err := confirmDirtyCleanup(selected, in, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !confirmed {
		t.Fatal("piped confirmation was lost")
	}
}

func TestWorktreeHasChanges(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	tracked := filepath.Join(repo, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "tracked.txt")
	runGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "initial")
	runGit(t, repo, "checkout", "-b", "siding")

	dirty, err := worktreeHasChanges(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("clean worktree reported as dirty")
	}

	if err := os.WriteFile(tracked, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err = worktreeHasChanges(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("tracked change was not detected")
	}
}

func TestWorktreeHasChangesDetectsOnlyReachableFromSidingBranch(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "tracked.txt")
	runGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "initial")
	runGit(t, repo, "checkout", "-b", "siding")
	runGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "unpushed")

	dirty, err := worktreeHasChanges(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("commits reachable only from the siding branch must be protected")
	}
}

func TestWorktreeHasChangesDetectsUntrackedFile(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err := worktreeHasChanges(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("untracked file was not detected")
	}
}

func TestWorktreeHasChangesAllowsMissingWorktree(t *testing.T) {
	dirty, err := worktreeHasChanges(context.Background(), filepath.Join(t.TempDir(), "missing"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("missing worktree must be protected")
	}
}

func TestSidingBaseRejectsPathsOutsideConfigDir(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "project")
	tests := []struct {
		name    string
		siding  string
		wantErr bool
	}{
		{name: "direct child", siding: "feature", wantErr: false},
		{name: "config dir itself", siding: ".", wantErr: true},
		{name: "parent", siding: "..", wantErr: true},
		{name: "outside config dir", siding: filepath.Join("..", "other-project", "feature"), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := sidingBase(state.App{ConfigDir: configDir}, test.siding)
			if (err != nil) != test.wantErr {
				t.Fatalf("sidingBase(%q) error = %v, wantErr %v", test.siding, err, test.wantErr)
			}
			if !test.wantErr && got != filepath.Join(configDir, test.siding) {
				t.Fatalf("sidingBase(%q) = %q, want %q", test.siding, got, filepath.Join(configDir, test.siding))
			}
		})
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
