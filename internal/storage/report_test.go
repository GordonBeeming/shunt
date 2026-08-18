package storage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestInspectGitReportsActualEvidence(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "one")
	gitTest(t, repo, "add", "tracked.txt")
	gitTestCommit(t, repo, "first")
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "changed")
	writeTestFile(t, filepath.Join(repo, "untracked.txt"), "new")

	evidence := InspectGit(context.Background(), repo)
	if evidence.Observation != "observed" || evidence.Head == "" || evidence.Branch == "" || evidence.LastCommit == "" {
		t.Fatalf("missing git evidence: %+v", evidence)
	}
	if !evidence.Dirty || evidence.Untracked != 1 {
		t.Fatalf("working tree evidence = %+v", evidence)
	}
	if evidence.UniqueCommits != 1 {
		t.Fatalf("unique commits = %d, want 1", evidence.UniqueCommits)
	}
}

func TestCollectProjectKeepsLogicalCategoriesExplicitlyOverlapping(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "init", "-q")
	gitTest(t, repo, "config", "user.email", "test@example.com")
	gitTest(t, repo, "config", "user.name", "Test")
	writeTestFile(t, filepath.Join(repo, "README.md"), "source")
	gitTest(t, repo, "add", "README.md")
	gitTestCommit(t, repo, "base")

	configDir := filepath.Join(root, ".shunt-dev", "repo")
	src := filepath.Join(configDir, "one", "src")
	gitTest(t, repo, "worktree", "add", "-q", "-b", "shunt/one", src)
	writeTestFile(t, filepath.Join(src, ".gitignore"), "bin/\n")
	writeTestFile(t, filepath.Join(src, "app", "bin", "generated"), "artifact")
	writeTestFile(t, filepath.Join(configDir, "one", "out", "shot.png"), "output")
	writeTestFile(t, filepath.Join(configDir, "one", "vol", "db", "data"), "database")
	generation := filepath.Join(configDir, ".shunt-baseline-generations", "generation-1")
	writeTestFile(t, filepath.Join(generation, "db", "data"), "baseline")
	writeTestFile(t, filepath.Join(configDir, ".shunt-baseline-state.json"), `{"current":"generation-1"}`)

	app := state.App{
		Name:      "repo",
		RepoPath:  repo,
		ConfigDir: configDir,
		Sidings: map[string]state.Siding{
			"one": {Name: "one", Branch: "shunt/one"},
		},
	}
	project := collectProject(context.Background(), app)
	if project.Filesystem.Measurement != "physical" || project.Filesystem.TotalBytes == 0 {
		t.Fatalf("filesystem = %+v", project.Filesystem)
	}
	if len(project.Sidings) != 1 || project.Sidings[0].Generated.LogicalBytes == 0 {
		t.Fatalf("siding report = %+v", project.Sidings)
	}
	if project.Sidings[0].Generated.Overlaps != "source" || !project.Sidings[0].Data.Shared {
		t.Fatalf("measurement semantics = %+v", project.Sidings[0])
	}
	if len(project.Baselines) != 1 || !project.Baselines[0].Protected || project.Baselines[0].Name != "current" {
		t.Fatalf("baselines = %+v", project.Baselines)
	}
}

func TestInspectGitFailureAndBaselineStateFailureRemainObservable(t *testing.T) {
	gitEvidence := InspectGit(context.Background(), t.TempDir())
	if gitEvidence.Observation != "error" || gitEvidence.Detail == "" {
		t.Fatalf("git evidence = %+v", gitEvidence)
	}

	root := t.TempDir()
	configDir := filepath.Join(root, ".shunt", "repo")
	baseline := filepath.Join(configDir, ".shunt-baseline-generations", "one")
	writeTestFile(t, filepath.Join(baseline, "db", "data"), "baseline")
	writeTestFile(t, filepath.Join(configDir, ".shunt-baseline-state.json"), "not-json")
	measurements := collectBaselineMeasurements(configDir)
	var stateFailure *Measurement
	for index := range measurements {
		if measurements[index].Name == "baseline state" {
			stateFailure = &measurements[index]
		}
	}
	if stateFailure == nil || stateFailure.Observation != "error" || stateFailure.Detail == "" {
		t.Fatalf("baseline measurements = %+v", measurements)
	}
}

func TestCollectProjectReportsRecordedAndObservedTargetsConservatively(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "tracked.txt"), "base")
	gitTest(t, repo, "add", "tracked.txt")
	gitTestCommit(t, repo, "base")
	gitTest(t, repo, "branch", "recorded")
	config := filepath.Join(t.TempDir(), "config")
	src := filepath.Join(config, "one", "src")
	gitTest(t, repo, "worktree", "add", "-b", "observed", src, "HEAD")
	app := state.App{Name: "repo", RepoPath: repo, ConfigDir: config, Sidings: map[string]state.Siding{"one": {Name: "one", Branch: "recorded", WorktreeRepoPath: repo}}}
	project := collectProject(context.Background(), app)
	evidence := project.Sidings[0].Source.Git
	if evidence.Preservation == nil || len(evidence.Preservation.Targets) != 2 {
		t.Fatalf("preservation = %#v", evidence.Preservation)
	}
	if evidence.Preservation.MatchingRef != "" || evidence.Preservation.MatchingCommit != "" {
		t.Fatalf("multi-target aggregate exposed one witness: %#v", evidence.Preservation)
	}
	if evidence.UniqueCommits != InspectGit(context.Background(), src).UniqueCommits {
		t.Fatalf("unique commits changed: %d", evidence.UniqueCommits)
	}
}

func TestInspectGitMarksMalformedDivergenceAsError(t *testing.T) {
	worktree := t.TempDir()
	evidence := inspectGit(context.Background(), worktree, func(_ context.Context, _ string, args ...string) (proc.Result, error) {
		switch args[2] {
		case "symbolic-ref":
			return proc.Result{Stdout: "main"}, nil
		case "status":
			return proc.Result{}, nil
		case "show":
			return proc.Result{Stdout: "2026-08-07T00:00:00Z"}, nil
		case "for-each-ref":
			return proc.Result{}, nil
		case "rev-parse":
			if args[len(args)-1] == "@{upstream}" {
				return proc.Result{Stdout: "origin/main"}, nil
			}
			return proc.Result{Stdout: "abc123"}, nil
		case "rev-list":
			if args[len(args)-1] == "HEAD...@{upstream}" {
				return proc.Result{Stdout: "not-a-number 2"}, nil
			}
			return proc.Result{Stdout: "0"}, nil
		default:
			return proc.Result{}, errors.New("unexpected git command")
		}
	})
	if evidence.Observation != "error" || evidence.Detail == "" || evidence.Ahead != 0 || evidence.Behind != 0 {
		t.Fatalf("git evidence = %+v", evidence)
	}
}

func TestInspectGitForcesSignatureFreeSingleTimestamp(t *testing.T) {
	worktree := t.TempDir()
	showDisabledSignature := false
	evidence := inspectGit(context.Background(), worktree, func(_ context.Context, _ string, args ...string) (proc.Result, error) {
		switch args[2] {
		case "symbolic-ref":
			return proc.Result{Stdout: "main"}, nil
		case "status", "for-each-ref":
			return proc.Result{}, nil
		case "rev-parse":
			if args[len(args)-1] == "@{upstream}" {
				return proc.Result{ExitCode: 128}, errors.New("no upstream")
			}
			return proc.Result{Stdout: "abc123"}, nil
		case "rev-list":
			return proc.Result{Stdout: "0"}, nil
		case "show":
			for _, arg := range args {
				if arg == "--no-show-signature" {
					showDisabledSignature = true
				}
			}
			if !showDisabledSignature {
				return proc.Result{Stdout: "Good git signature\nNo principal matched\n2026-08-07T09:00:00+10:00"}, nil
			}
			return proc.Result{Stdout: "2026-08-07T09:00:00+10:00\n"}, nil
		default:
			return proc.Result{}, errors.New("unexpected git command")
		}
	})
	if evidence.Observation != "observed" || !showDisabledSignature || evidence.LastCommit != "2026-08-07T09:00:00+10:00" || strings.Contains(evidence.LastCommit, "signature") {
		t.Fatalf("git evidence = %+v (signature disabled=%t)", evidence, showDisabledSignature)
	}
	if _, err := parseLastCommitTimestamp("Good git signature\nNo principal matched\n2026-08-07T09:00:00+10:00"); err == nil {
		t.Fatal("signature-polluted timestamp was accepted")
	}
}

func TestCollectSidingCandidatesRunsBoundedScansConcurrentlyAndKeepsOrder(t *testing.T) {
	configDir := t.TempDir()
	names := []string{"alpha", "bravo", "charlie", "delta", "echo"}
	reports := make([]SidingReport, len(names))
	collector := &generatedCollector{candidates: make(map[string][]*discoveredCandidate)}
	for index, name := range names {
		src := filepath.Join(configDir, name, "src")
		reports[index] = SidingReport{Name: name, Source: SourceReport{Measurement: Measurement{Path: src}}}
	}
	oldFilter := filterTrimCandidatesForReport
	t.Cleanup(func() { filterTrimCandidatesForReport = oldFilter })
	started := make(chan string, len(reports))
	release := make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})
	filterTrimCandidatesForReport = func(_ context.Context, src string, _ []*discoveredCandidate) ([]TrimCandidate, error) {
		started <- filepath.Base(filepath.Dir(src))
		<-release
		return []TrimCandidate{}, nil
	}
	done := make(chan error, 1)
	go func() { done <- collectSidingCandidates(context.Background(), reports, collector) }()
	for index := 0; index < projectSidingScanLimit; index++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("only %d scans started concurrently", index)
		}
	}
	select {
	case name := <-started:
		t.Fatalf("scan limit exceeded by %q", name)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	released = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent siding scans did not finish")
	}
	if len(reports) != len(names) {
		t.Fatalf("reports = %d, want %d", len(reports), len(names))
	}
	for index, report := range reports {
		if report.Name != names[index] {
			t.Fatalf("report %d = %q, want %q", index, report.Name, names[index])
		}
	}
}

func TestCollectSidingCandidatesDoesNotScheduleQueuedWorkAfterCancellation(t *testing.T) {
	reports := make([]SidingReport, projectSidingScanLimit+1)
	collector := &generatedCollector{candidates: make(map[string][]*discoveredCandidate)}
	for index := range reports {
		reports[index] = SidingReport{Name: strconv.Itoa(index), Source: SourceReport{Measurement: Measurement{Path: filepath.Join(t.TempDir(), strconv.Itoa(index))}}}
	}
	oldFilter := filterTrimCandidatesForReport
	t.Cleanup(func() { filterTrimCandidatesForReport = oldFilter })
	started := make(chan struct{}, projectSidingScanLimit)
	release := make(chan struct{})
	filterTrimCandidatesForReport = func(_ context.Context, _ string, _ []*discoveredCandidate) ([]TrimCandidate, error) {
		started <- struct{}{}
		<-release
		return nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- collectSidingCandidates(ctx, reports, collector) }()
	for range projectSidingScanLimit {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	cancel()
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	select {
	case <-started:
		t.Fatal("queued siding scan started after cancellation")
	default:
	}
}

func TestCollectProjectPreservesBothFilesystemProbeFailures(t *testing.T) {
	oldProbe := reportFilesystemProbe
	t.Cleanup(func() { reportFilesystemProbe = oldProbe })
	var calls int
	reportFilesystemProbe = func(string) (FilesystemView, error) {
		calls++
		return FilesystemView{}, errors.New("probe failed")
	}
	project := collectProject(context.Background(), state.App{Name: "app", ConfigDir: t.TempDir(), RepoPath: t.TempDir(), Sidings: map[string]state.Siding{}})
	if calls != 2 || project.Filesystem.Observation != "unavailable" || project.Filesystem.Detail == "" || project.Filesystem.TotalBytes != 0 {
		t.Fatalf("filesystem = %+v (calls=%d)", project.Filesystem, calls)
	}
}

func TestMeasureClassifiedWalksOnce(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "one", "a.txt"), "one")
	writeTestFile(t, filepath.Join(root, "two", "b.txt"), "two")
	all := newMeasurement("all", root, false, false, "")
	one := newMeasurement("one", filepath.Join(root, "one"), false, false, "")
	oldWalk := walkStorageTree
	t.Cleanup(func() { walkStorageTree = oldWalk })
	var walks int
	walkStorageTree = func(path string, fn fs.WalkDirFunc) error {
		walks++
		return oldWalk(path, fn)
	}
	if err := measureClassified(context.Background(), root, nil, &all, &one); err != nil {
		t.Fatal(err)
	}
	if walks != 1 || all.LogicalBytes != 6 || one.LogicalBytes != 3 {
		t.Fatalf("walks=%d all=%+v one=%+v", walks, all, one)
	}
}

func TestMeasureClassifiedUsesComponentAncestorsNotMeasurementCount(t *testing.T) {
	root := t.TempDir()
	project := newMeasurement("project", root, false, false, "")
	measurements := []*Measurement{&project}
	for index := 0; index < 64; index++ {
		path := filepath.Join(root, "sibling", strconv.Itoa(index))
		writeTestFile(t, filepath.Join(path, "value"), "x")
		measurement := newMeasurement("sibling", path, false, false, "")
		measurements = append(measurements, &measurement)
	}
	oldCounter := onClassifiedAncestor
	t.Cleanup(func() { onClassifiedAncestor = oldCounter })
	var operations int
	onClassifiedAncestor = func() { operations++ }
	if err := measureClassified(context.Background(), root, nil, measurements...); err != nil {
		t.Fatal(err)
	}
	if measurements[0].LogicalBytes != 64 || operations > 64*4 {
		t.Fatalf("logical=%d ancestor operations=%d", measurements[0].LogicalBytes, operations)
	}
	for index := 1; index < len(measurements); index++ {
		if measurements[index].LogicalBytes != 1 {
			t.Fatalf("measurement %d=%d, want 1", index, measurements[index].LogicalBytes)
		}
	}
}

func TestMeasureClassifiedStopsOnCancellation(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "first"), "x")
	writeTestFile(t, filepath.Join(root, "later"), "x")
	ctx, cancel := context.WithCancel(context.Background())
	oldWalk := walkStorageTree
	t.Cleanup(func() { walkStorageTree = oldWalk })
	seenLater := false
	walkStorageTree = func(path string, fn fs.WalkDirFunc) error {
		return oldWalk(path, func(path string, entry fs.DirEntry, err error) error {
			if filepath.Base(path) == "first" {
				cancel()
			}
			if filepath.Base(path) == "later" {
				seenLater = true
			}
			return fn(path, entry, err)
		})
	}
	all := newMeasurement("all", root, false, false, "")
	err := measureClassified(ctx, root, nil, &all)
	if !errors.Is(err, context.Canceled) || seenLater {
		t.Fatalf("err=%v seenLater=%t", err, seenLater)
	}
}

func TestLogicalSizeStopsOnCancellation(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "first"), "x")
	writeTestFile(t, filepath.Join(root, "later"), "x")
	ctx, cancel := context.WithCancel(context.Background())
	oldWalk := walkLogicalTree
	t.Cleanup(func() { walkLogicalTree = oldWalk })
	seenLater := false
	walkLogicalTree = func(path string, fn fs.WalkDirFunc) error {
		return oldWalk(path, func(path string, entry fs.DirEntry, err error) error {
			if filepath.Base(path) == "first" {
				cancel()
			}
			if filepath.Base(path) == "later" {
				seenLater = true
			}
			return fn(path, entry, err)
		})
	}
	_, err := LogicalSize(ctx, root)
	if !errors.Is(err, context.Canceled) || seenLater {
		t.Fatalf("err=%v seenLater=%t", err, seenLater)
	}
}

func TestCollectProjectVisitsSourceEntryOnce(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "init", "-q")
	gitTest(t, repo, "config", "user.email", "test@example.com")
	gitTest(t, repo, "config", "user.name", "Test")
	writeTestFile(t, filepath.Join(repo, "README.md"), "source")
	gitTest(t, repo, "add", "README.md")
	gitTestCommit(t, repo, "base")
	configDir := filepath.Join(root, ".shunt", "repo")
	src := filepath.Join(configDir, "one", "src")
	gitTest(t, repo, "worktree", "add", "-q", "-b", "shunt/one", src)
	writeTestFile(t, filepath.Join(src, ".gitignore"), "bin/\n")
	target := filepath.Join(src, "bin", "generated")
	writeTestFile(t, target, "generated")

	oldClassified := walkStorageTree
	t.Cleanup(func() { walkStorageTree = oldClassified })
	var classifiedVisits int
	walkStorageTree = func(path string, fn fs.WalkDirFunc) error {
		return oldClassified(path, func(path string, entry fs.DirEntry, err error) error {
			if path == target {
				classifiedVisits++
			}
			return fn(path, entry, err)
		})
	}
	project := collectProject(context.Background(), state.App{Name: "repo", RepoPath: repo, ConfigDir: configDir, Sidings: map[string]state.Siding{"one": {Name: "one", Branch: "shunt/one"}}})
	if len(project.Sidings) != 1 || project.Sidings[0].Generated.Observation != "observed" {
		t.Fatalf("report = %+v", project)
	}
	if classifiedVisits != 1 {
		t.Fatalf("classified visits=%d", classifiedVisits)
	}
}

func TestCollectReturnsFatalTraversalError(t *testing.T) {
	root := t.TempDir()
	sentinel := errors.New("fatal traversal failure")
	oldWalk, oldProbe := walkStorageTree, reportFilesystemProbe
	t.Cleanup(func() { walkStorageTree, reportFilesystemProbe = oldWalk, oldProbe })
	walkStorageTree = func(string, fs.WalkDirFunc) error { return sentinel }
	reportFilesystemProbe = func(string) (FilesystemView, error) {
		return FilesystemView{Observation: "observed", Measurement: "physical"}, nil
	}
	report, err := Collect(context.Background(), []state.App{{Name: "app", RepoPath: filepath.Join(root, "missing"), ConfigDir: root, Sidings: map[string]state.Siding{}}})
	if !errors.Is(err, sentinel) || len(report.Projects) != 1 || report.Projects[0].Logical.Observation != "error" {
		t.Fatalf("report=%#v err=%v", report, err)
	}
}

func TestCollectProjectExplainsManagedAndLegacyLayoutEntries(t *testing.T) {
	configDir := t.TempDir()
	writeTestFile(t, filepath.Join(configDir, ".control.git", "objects", "pack"), "control")
	writeTestFile(t, filepath.Join(configDir, "base", "images", "generation", "blob"), "cache")
	writeTestFile(t, filepath.Join(configDir, "base", "images.tar"), "archive")
	writeTestFile(t, filepath.Join(configDir, "legacy-baseline-20260803T012923Z", "db", "data"), "legacy")
	writeTestFile(t, filepath.Join(configDir, "state-v2.json"), "metadata")

	project := collectProject(context.Background(), state.App{Name: "repo", ConfigDir: configDir, Sidings: map[string]state.Siding{}})
	managed := measurementByName(project.Managed)
	unknown := measurementByName(project.Unclassified)
	if managed["source control store"].LogicalBytes != int64(len("control")) || !managed["source control store"].Protected {
		t.Fatalf("managed control = %+v", managed["source control store"])
	}
	if managed["image cache"].LogicalBytes != int64(len("cache")) {
		t.Fatalf("managed cache = %+v", managed["image cache"])
	}
	if unknown["base/images.tar"].LogicalBytes != int64(len("archive")) || unknown["legacy-baseline-20260803T012923Z"].LogicalBytes != int64(len("legacy")) {
		t.Fatalf("unclassified = %+v", project.Unclassified)
	}
	if _, found := unknown["state-v2.json"]; found {
		t.Fatalf("state metadata was classified as unknown: %+v", project.Unclassified)
	}
	for _, measurement := range append(append([]Measurement{}, project.Managed...), project.Unclassified...) {
		if measurement.Measurement != "logical" || measurement.Overlaps != "project" {
			t.Fatalf("measurement could be mistaken for independent reclaimability: %+v", measurement)
		}
	}
}

func measurementByName(measurements []Measurement) map[string]Measurement {
	result := make(map[string]Measurement, len(measurements))
	for _, measurement := range measurements {
		result[measurement.Name] = measurement
	}
	return result
}
