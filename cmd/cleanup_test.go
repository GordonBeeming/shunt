package cmd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/gitpreservation"
	"github.com/gordonbeeming/shunt/internal/resolve"
	"github.com/gordonbeeming/shunt/internal/siding"
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

func TestOrderBaseLast(t *testing.T) {
	got := orderBaseLast([]string{"base", "alpha", "beta"}, "base")
	want := []string{"alpha", "beta", "base"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orderBaseLast() = %v, want %v", got, want)
	}
}

func TestUnrelatedRemovalDoesNotRequireLegacyBaseSelection(t *testing.T) {
	app := state.App{Sidings: map[string]state.Siding{"one": {Name: "one"}, "two": {Name: "two"}}}
	got, err := prepareBaseRemoval(app, []string{"one"}, "", bufio.NewReader(strings.NewReader("")))
	if err != nil || got != "" {
		t.Fatalf("unrelated removal = %q, %v", got, err)
	}
}

func TestConfiguredBaseRemovalStillRequiresSuccessor(t *testing.T) {
	app := state.App{BaseSiding: "one", Sidings: map[string]state.Siding{"one": {Name: "one"}, "two": {Name: "two"}}}
	_, err := prepareBaseRemoval(app, []string{"one"}, "", bufio.NewReader(strings.NewReader("")))
	if err == nil || !strings.Contains(err.Error(), "requires --next-base") {
		t.Fatalf("base removal error = %v", err)
	}
}

func TestRmCommandLegacyNoBaseDoesNotPromptForBase(t *testing.T) {
	app := persistedLegacyNoBaseApp(t)
	withRemovalCommandSeams(t, app, func(removed *[]string) error {
		command := newRmCmd()
		command.SetArgs([]string{"one", "--force"})
		return command.ExecuteContext(context.Background())
	}, "")
}

func TestCleanupCommandLegacyNoBaseConsumesOnlySidingSelection(t *testing.T) {
	app := persistedLegacyNoBaseApp(t)
	withRemovalCommandSeams(t, app, func(removed *[]string) error {
		command := newCleanupCmd()
		command.SetArgs([]string{"--force"})
		return command.ExecuteContext(context.Background())
	}, "1\n")
}

func TestAnalyzerFactoryCreatesOnePerOwnerPerObservationPhase(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "tracked"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "tracked")
	runGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "base")
	config := t.TempDir()
	app := state.App{ConfigDir: config, RepoPath: repo, Sidings: map[string]state.Siding{}}
	for _, name := range []string{"one", "two"} {
		src := filepath.Join(config, name, "src")
		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, repo, "worktree", "add", "-b", name, src, "main")
		app.Sidings[name] = state.Siding{Name: name, Branch: name, WorktreeRepoPath: repo}
	}
	old := newCommandAnalyzer
	t.Cleanup(func() { newCommandAnalyzer = old })
	calls := 0
	newCommandAnalyzer = func(owner string) *gitpreservation.Analyzer {
		calls++
		return gitpreservation.NewAnalyzer(owner, gitpreservation.Options{})
	}
	if _, err := buildCleanupCandidates(context.Background(), app, true); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("candidate analyzer creations = %d", calls)
	}
	calls = 0
	selected := []string{"one", "two"}
	refs, err := resolveSelectedRemovalRefs(context.Background(), app, selected)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := captureSelectedRemovalSafety(context.Background(), app, selected, refs); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("snapshot analyzer creations = %d", calls)
	}
}

func persistedLegacyNoBaseApp(t *testing.T) state.App {
	t.Helper()
	dir := t.TempDir()
	app := state.App{Name: "legacy", ConfigDir: dir, Sidings: map[string]state.Siding{"one": {Name: "one", Branch: "one"}, "two": {Name: "two", Branch: "two"}}}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	loaded, err := state.LoadApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded.BaseSiding = ""
	return loaded
}

func withRemovalCommandSeams(t *testing.T, app state.App, run func(*[]string) error, input string) {
	t.Helper()
	oldLoad, oldRemove, oldStdin, oldStdout := commandLoadCurrentApp, commandRemoveSiding, os.Stdin, os.Stdout
	t.Cleanup(func() {
		commandLoadCurrentApp, commandRemoveSiding, os.Stdin, os.Stdout = oldLoad, oldRemove, oldStdin, oldStdout
	})
	commandLoadCurrentApp = func() (state.App, resolve.Location, error) { return app, resolve.Location{}, nil }
	removed := []string{}
	commandRemoveSiding = func(_ context.Context, _ *state.App, name string, _ bool, successor string, _ ...*removalSafety) error {
		if successor != "" {
			return fmt.Errorf("unexpected base successor %q", successor)
		}
		removed = append(removed, name)
		return nil
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inW.WriteString(input); err != nil {
		t.Fatal(err)
	}
	inW.Close()
	os.Stdin = inR
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = outW
	err = run(&removed)
	outW.Close()
	output, _ := io.ReadAll(outR)
	if err != nil {
		t.Fatalf("command: %v; output=%s", err, output)
	}
	if len(removed) != 1 || removed[0] != "one" {
		t.Fatalf("removed = %v; output=%s", removed, output)
	}
	if strings.Contains(string(output), "Select a siding") || strings.Contains(string(output), "successor source base") {
		t.Fatalf("unexpected base/siding prompt: %s", output)
	}
}

func TestValidateFinalVolumeSet(t *testing.T) {
	root := filepath.Join(t.TempDir(), "vol")
	worktree := state.Siding{Name: "one", MaterializationPhase: state.PhaseWorktree}
	if promote, err := validateFinalVolumeSet(worktree, root, []string{"db"}); err != nil || promote {
		t.Fatalf("absent worktree data = %v, %v", promote, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := validateFinalVolumeSet(worktree, root, []string{"db"}); err == nil {
		t.Fatal("worktree-only siding accepted unexpected data root")
	}
	if err := os.MkdirAll(filepath.Join(root, "db"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := state.Siding{Name: "one", MaterializationPhase: state.PhaseData}
	if promote, err := validateFinalVolumeSet(data, root, []string{"db"}); err != nil || !promote {
		t.Fatalf("complete data phase = %v, %v", promote, err)
	}
	if _, err := validateFinalVolumeSet(data, root, []string{"db", "cache"}); err == nil {
		t.Fatal("partial data phase accepted")
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

func TestProtectionReasonsNameDirtyOwnershipAndUnprovenWork(t *testing.T) {
	t.Run("dirty untracked", func(t *testing.T) {
		config := t.TempDir()
		src := filepath.Join(config, "one", "src")
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, src, "init", "-b", "main")
		if err := os.WriteFile(filepath.Join(src, "new.txt"), []byte("new\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		app := state.App{ConfigDir: config, Sidings: map[string]state.Siding{"one": {Name: "one", Branch: "main"}}}
		protected, reason, err := sidingWorktreeProtection(context.Background(), app, "one", []string{"one"})
		if err != nil || !protected || !strings.Contains(reason, "untracked") {
			t.Fatalf("protection = %t, %q, %v", protected, reason, err)
		}
	})
	t.Run("surviving owner", func(t *testing.T) {
		config := t.TempDir()
		src := filepath.Join(config, "one", "src")
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, src, "init", "-b", "shared")
		app := state.App{ConfigDir: config, Sidings: map[string]state.Siding{"one": {Name: "one", Branch: "recorded"}, "two": {Name: "two", Branch: "shared"}}}
		protected, reason, err := sidingWorktreeProtection(context.Background(), app, "one", []string{"one"})
		if err != nil || !protected || !strings.Contains(reason, "owned by surviving siding") {
			t.Fatalf("protection = %t, %q, %v", protected, reason, err)
		}
	})
	t.Run("unproven commit", func(t *testing.T) {
		config := t.TempDir()
		src := filepath.Join(config, "one", "src")
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, src, "init", "-b", "topic")
		if err := os.WriteFile(filepath.Join(src, "tracked.txt"), []byte("topic\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, src, "add", "tracked.txt")
		runGit(t, src, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "topic")
		app := state.App{ConfigDir: config, Sidings: map[string]state.Siding{"one": {Name: "one", Branch: "topic"}}}
		protected, reason, err := sidingWorktreeProtection(context.Background(), app, "one", []string{"one"})
		if err != nil || !protected || !strings.Contains(reason, "not proven preserved") || !strings.Contains(reason, "no surviving origin/HEAD") {
			t.Fatalf("protection = %t, %q, %v", protected, reason, err)
		}
	})
}

func TestTruncateTerminalRowTinyAndNormalWidths(t *testing.T) {
	for _, width := range []int{0, 1} {
		if got := truncateTerminalRow("long reason", width); got != "…" {
			t.Fatalf("width %d = %q", width, got)
		}
	}
	if got := truncateTerminalRow("long", 2); got != "l…" {
		t.Fatalf("width 2 = %q", got)
	}
	if got := truncateTerminalRow("long", 4); got != "long" {
		t.Fatalf("width 4 = %q", got)
	}
	if got := truncateTerminalRow("short", 10); got != "short" {
		t.Fatalf("normal = %q", got)
	}
	if got := truncateTerminalRow("e\u0301x", 2); got != "e\u0301x" {
		t.Fatalf("combining width = %q", got)
	}
	if got := truncateTerminalRow("界x", 2); got != "…" {
		t.Fatalf("wide width = %q", got)
	}
	if got := terminalRuneWidth('界'); got != 2 {
		t.Fatalf("wide rune cells = %d", got)
	}
	if got := terminalRuneWidth('Ａ'); got != 2 {
		t.Fatalf("fullwidth form cells = %d", got)
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

	dirty, err := worktreeHasChanges(context.Background(), repo, "siding", nil)
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatal("clean worktree reported as dirty")
	}

	if err := os.WriteFile(tracked, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err = worktreeHasChanges(context.Background(), repo, "siding", nil)
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

	dirty, err := worktreeHasChanges(context.Background(), repo, "siding", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("commits reachable only from the siding branch must be protected")
	}
}

func TestWorktreeHasChangesAcceptsWholeBranchSquash(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "tracked.txt")
	runGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "initial")
	runGit(t, repo, "checkout", "-b", "siding")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("squashed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "tracked.txt")
	runGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "topic")
	runGit(t, repo, "checkout", "main")
	runGit(t, repo, "merge", "--squash", "siding")
	runGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "squash")
	runGit(t, repo, "update-ref", "refs/remotes/origin/main", "main")
	runGit(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runGit(t, repo, "checkout", "siding")
	protected, err := worktreeHasChanges(context.Background(), repo, "siding", []string{"siding"})
	if err != nil {
		t.Fatal(err)
	}
	if protected {
		t.Fatal("squash-preserved branch was marked protected")
	}
}

func TestSidingWorktreeChecksRecordedAndObservedBranches(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "tracked.txt")
	runGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "initial")
	runGit(t, repo, "branch", "recorded")
	configDir := filepath.Join(t.TempDir(), "config")
	src := filepath.Join(configDir, "one", "src")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "worktree", "add", "-b", "observed", src, "main")
	app := state.App{ConfigDir: configDir, RepoPath: repo, Sidings: map[string]state.Siding{"one": {Name: "one", Branch: "recorded", WorktreeRepoPath: repo}}}
	protected, err := sidingWorktreeHasChanges(context.Background(), app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	if protected {
		t.Fatal("independently preserved recorded and observed branches were marked protected")
	}
}

func TestWorktreeHasChangesDetectsUntrackedFile(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err := worktreeHasChanges(context.Background(), repo, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("untracked file was not detected")
	}
}

func TestWorktreeHasChangesAllowsMissingWorktree(t *testing.T) {
	dirty, err := worktreeHasChanges(context.Background(), filepath.Join(t.TempDir(), "missing"), "siding", nil)
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
			got, err := siding.SidingBase(state.App{ConfigDir: configDir}, test.siding)
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

// TestResolveSelectedRemovalRefsRejectsACollisionBetweenSelectedSidings covers
// the case where two sidings in the same batch share one branch: A's worktree is
// checked out on the branch recorded for B, and both are selected.
//
// The check used to look at surviving sidings only, so this pair went through.
// Removing A then deleted the ref B's worktree was still on, B could no longer
// resolve HEAD, and the batch aborted with A already gone — the worst outcome,
// because it is half-applied and not obviously recoverable.
func TestResolveSelectedRemovalRefsRejectsACollisionBetweenSelectedSidings(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "tracked"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "tracked")
	runGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "base")

	config := t.TempDir()
	app := state.App{ConfigDir: config, RepoPath: repo, Sidings: map[string]state.Siding{}}
	for _, name := range []string{"one", "two"} {
		src := filepath.Join(config, name, "src")
		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, repo, "worktree", "add", "-b", name, src, "main")
		app.Sidings[name] = state.Siding{Name: name, Branch: name, WorktreeRepoPath: repo}
	}
	// "one" is recorded against branch "one" but sits on "two"'s branch, which is
	// what `git checkout --ignore-other-worktrees` produces.
	app.Sidings["one"] = state.Siding{Name: "one", Branch: "two", WorktreeRepoPath: repo}

	_, err := resolveSelectedRemovalRefs(context.Background(), app, []string{"one", "two"})
	if err == nil {
		t.Fatal("resolveSelectedRemovalRefs() = nil error, want a refusal: removing one would delete the ref two is on")
	}
	if !strings.Contains(err.Error(), "selected siding") {
		t.Errorf("error = %v, want it to name the colliding selected siding", err)
	}
}
