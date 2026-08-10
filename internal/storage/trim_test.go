package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestScanTrimCandidatesRequiresAllowlistedIgnoredAndUntracked(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "bin/\nobj/\nnode_modules/\nbuild/\n")
	writeTestFile(t, filepath.Join(repo, "src", "bin", "app.dll"), "generated")
	writeTestFile(t, filepath.Join(repo, "src", "obj", "state"), "generated")
	writeTestFile(t, filepath.Join(repo, "web", "node_modules", "pkg", "index.js"), "generated")
	writeTestFile(t, filepath.Join(repo, "coverage", "report"), "not ignored")
	writeTestFile(t, filepath.Join(repo, "tracked", "build", "keep.txt"), "tracked")
	gitTest(t, repo, "add", "-f", "tracked/build/keep.txt")
	gitTest(t, repo, "add", ".gitignore")
	if err := os.Symlink(filepath.Join(repo, "src", "bin"), filepath.Join(repo, "linked-bin")); err != nil {
		t.Fatal(err)
	}

	candidates, err := ScanTrimCandidates(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"src/bin", "src/obj", "web/node_modules"}
	if len(candidates) != len(want) {
		t.Fatalf("candidates = %#v, want %v", candidates, want)
	}
	for index, candidate := range candidates {
		if candidate.RelativePath != want[index] {
			t.Fatalf("candidate %d = %q, want %q", index, candidate.RelativePath, want[index])
		}
		if candidate.LogicalBytes == 0 {
			t.Fatalf("candidate %q has no measured bytes", candidate.RelativePath)
		}
	}
}

func TestRemoveTrimCandidatesRevalidatesGitSafety(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "bin/\n")
	writeTestFile(t, filepath.Join(repo, "bin", "artifact"), "generated")
	candidates, err := ScanTrimCandidates(context.Background(), repo)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("scan = %#v, %v", candidates, err)
	}
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "")

	if _, err := RemoveTrimCandidates(context.Background(), repo, candidates); err == nil {
		t.Fatal("expected changed ignore state to fail closed")
	}
	if _, err := os.Stat(filepath.Join(repo, "bin", "artifact")); err != nil {
		t.Fatalf("candidate was removed after failed revalidation: %v", err)
	}
}

func TestRemoveTrimCandidatesReportsLogicalAndPhysicalSeparately(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "dist/\n")
	writeTestFile(t, filepath.Join(repo, "dist", "bundle.js"), "generated bundle")
	candidates, err := ScanTrimCandidates(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	result, err := RemoveTrimCandidates(context.Background(), repo, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if result.CandidateBytes == 0 || result.RemovedBytes == 0 {
		t.Fatalf("logical measurements missing: %+v", result)
	}
	if result.FilesystemFreeBefore == 0 || result.FilesystemFreeAfter == 0 {
		t.Fatalf("physical filesystem observations missing: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(repo, "dist")); !os.IsNotExist(err) {
		t.Fatalf("dist still exists: %v", err)
	}
}

func TestRemoveTrimCandidatesFailsClosedForSubstitutedPaths(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "dist/\n")
	writeTestFile(t, filepath.Join(repo, "dist", "bundle.js"), "generated")
	candidates, err := ScanTrimCandidates(context.Background(), repo)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("scan = %#v, %v", candidates, err)
	}
	if err := os.RemoveAll(filepath.Join(repo, "dist")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repo, "dist", "replacement.js"), "replacement")
	if _, err := RemoveTrimCandidates(context.Background(), repo, candidates); err == nil || !strings.Contains(err.Error(), "changed identity") {
		t.Fatalf("substitution error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "dist", "replacement.js")); err != nil {
		t.Fatalf("replacement was removed: %v", err)
	}
}

func TestRemoveTrimCandidatesRejectsSymlinkOutsideAndNewlyTrackedPaths(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		repo := newTestRepo(t)
		writeTestFile(t, filepath.Join(repo, ".gitignore"), "dist/\n")
		writeTestFile(t, filepath.Join(repo, "dist", "bundle.js"), "generated")
		candidates, err := ScanTrimCandidates(context.Background(), repo)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("scan = %#v, %v", candidates, err)
		}
		if err := os.RemoveAll(filepath.Join(repo, "dist")); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(repo, "dist")); err != nil {
			t.Fatal(err)
		}
		if _, err := RemoveTrimCandidates(context.Background(), repo, candidates); err == nil || !strings.Contains(err.Error(), "not a real directory") {
			t.Fatalf("symlink error = %v", err)
		}
	})
	t.Run("outside", func(t *testing.T) {
		repo := newTestRepo(t)
		outside := filepath.Join(t.TempDir(), "dist")
		writeTestFile(t, filepath.Join(outside, "bundle.js"), "generated")
		candidate := TrimCandidate{Path: outside, RelativePath: "dist", LogicalBytes: 9}
		if _, err := RemoveTrimCandidates(context.Background(), repo, []TrimCandidate{candidate}); err == nil || !strings.Contains(err.Error(), "outside worktree") {
			t.Fatalf("outside error = %v", err)
		}
	})
	t.Run("newly tracked", func(t *testing.T) {
		repo := newTestRepo(t)
		writeTestFile(t, filepath.Join(repo, ".gitignore"), "dist/\n")
		writeTestFile(t, filepath.Join(repo, "dist", "bundle.js"), "generated")
		candidates, err := ScanTrimCandidates(context.Background(), repo)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("scan = %#v, %v", candidates, err)
		}
		gitTest(t, repo, "add", "-f", "dist/bundle.js")
		if _, err := RemoveTrimCandidates(context.Background(), repo, candidates); err == nil || !strings.Contains(err.Error(), "no longer safely") {
			t.Fatalf("tracked error = %v", err)
		}
	})
}

func TestScanTrimCandidatesBatchesGitChecksAndWalksOnce(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "**/dist/\n**/build/\n")
	for index := 0; index < 8; index++ {
		writeTestFile(t, filepath.Join(repo, "project", string(rune('a'+index)), "dist", "bundle.js"), "generated")
		writeTestFile(t, filepath.Join(repo, "project", string(rune('a'+index)), "build", "bundle.js"), "generated")
	}
	oldWalk, oldRunner := walkTrimTree, runGitBatch
	t.Cleanup(func() { walkTrimTree, runGitBatch = oldWalk, oldRunner })
	var walks, calls int
	walkTrimTree = func(root string, fn fs.WalkDirFunc) error {
		walks++
		return oldWalk(root, fn)
	}
	runGitBatch = func(ctx context.Context, root string, args []string, input []byte) (gitBatchResult, error) {
		calls++
		return oldRunner(ctx, root, args, input)
	}
	candidates, err := ScanTrimCandidates(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if walks != 1 || calls != 2 || len(candidates) != 16 {
		t.Fatalf("walks=%d calls=%d candidates=%d", walks, calls, len(candidates))
	}
}

func TestScanTrimCandidatesUsesActiveAncestorOperations(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "**/dist/\n")
	for index := 0; index < 64; index++ {
		writeTestFile(t, filepath.Join(repo, fmt.Sprintf("sibling-%03d", index), "dist", "artifact"), "x")
	}
	oldCounter := onTrimCandidateAncestor
	t.Cleanup(func() { onTrimCandidateAncestor = oldCounter })
	var operations int
	onTrimCandidateAncestor = func() { operations++ }
	candidates, err := ScanTrimCandidates(context.Background(), repo)
	if err != nil || len(candidates) != 64 || operations > 64*3 {
		t.Fatalf("candidates=%d operations=%d err=%v", len(candidates), operations, err)
	}
}

func TestScanTrimCandidatesStopsOnCancellation(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, "first", "dist", "artifact"), "x")
	writeTestFile(t, filepath.Join(repo, "later", "dist", "artifact"), "x")
	ctx, cancel := context.WithCancel(context.Background())
	oldWalk := walkTrimTree
	t.Cleanup(func() { walkTrimTree = oldWalk })
	seenLater := false
	walkTrimTree = func(path string, fn fs.WalkDirFunc) error {
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
	_, err := ScanTrimCandidates(ctx, repo)
	if !errors.Is(err, context.Canceled) || seenLater {
		t.Fatalf("err=%v seenLater=%t", err, seenLater)
	}
}

func TestScanTrimCandidatesChunksTrackedPathspecsAndMergesResults(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "**/dist/\n")
	for index := 0; index < maxGitPathspecs+2; index++ {
		name := fmt.Sprintf("project-%03d", index)
		writeTestFile(t, filepath.Join(repo, name, "dist", "bundle.js"), "generated")
	}
	tracked := filepath.Join(repo, fmt.Sprintf("project-%03d", maxGitPathspecs+1), "dist", "bundle.js")
	gitTest(t, repo, "add", "-f", tracked)
	oldRunner := runGitBatch
	t.Cleanup(func() { runGitBatch = oldRunner })
	var ignoreCalls, trackedCalls int
	runGitBatch = func(ctx context.Context, root string, args []string, input []byte) (gitBatchResult, error) {
		if args[0] == "check-ignore" {
			ignoreCalls++
		} else if args[0] == "ls-files" {
			trackedCalls++
		}
		return oldRunner(ctx, root, args, input)
	}
	candidates, err := ScanTrimCandidates(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if ignoreCalls != 1 || trackedCalls != 2 || len(candidates) != maxGitPathspecs+1 {
		t.Fatalf("ignoreCalls=%d trackedCalls=%d candidates=%d", ignoreCalls, trackedCalls, len(candidates))
	}
}

func TestChunkPathspecsRespectsByteBudget(t *testing.T) {
	paths := []string{strings.Repeat("a", maxGitPathspecBytes/2), strings.Repeat("b", maxGitPathspecBytes/2), "c"}
	chunks := chunkPathspecs(paths)
	if len(chunks) != 2 || len(chunks[0]) != 1 || len(chunks[1]) != 2 {
		t.Fatalf("chunks = %#v", chunks)
	}
	if chunks[0][0] != paths[0] || chunks[1][0] != paths[1] || chunks[1][1] != paths[2] {
		t.Fatalf("chunk ordering changed: %#v", chunks)
	}
}

func TestRemoveTrimCandidatesRestoresAllQuarantinesWhenPostCheckFindsTrackedFile(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "dist/\n")
	writeTestFile(t, filepath.Join(repo, "dist", "bundle.js"), "generated")
	candidates, err := ScanTrimCandidates(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	oldRunner := runGitBatch
	t.Cleanup(func() { runGitBatch = oldRunner })
	var trackedCalls int
	runGitBatch = func(ctx context.Context, root string, args []string, input []byte) (gitBatchResult, error) {
		if args[0] == "ls-files" {
			trackedCalls++
			if trackedCalls == 2 {
				entries, err := os.ReadDir(repo)
				if err != nil {
					t.Fatal(err)
				}
				var quarantined string
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".dist.shunt-trim-") {
						quarantined = filepath.Join(repo, entry.Name(), "bundle.js")
					}
				}
				if quarantined == "" {
					t.Fatal("quarantine directory not found")
				}
				hash := strings.TrimSpace(gitTest(t, repo, "hash-object", "-w", quarantined))
				gitTest(t, repo, "update-index", "--add", "--cacheinfo", "100644,"+hash+",dist/bundle.js")
			}
		}
		return oldRunner(ctx, root, args, input)
	}
	if _, err := RemoveTrimCandidates(context.Background(), repo, candidates); err == nil || !strings.Contains(err.Error(), "changed Git eligibility") {
		t.Fatalf("remove error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "dist", "bundle.js")); err != nil {
		t.Fatalf("candidate was not restored: %v", err)
	}
}

func TestRemoveTrimCandidatesRestoresAllQuarantinesWhenPostCheckLosesIgnoreRule(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "dist/\n")
	writeTestFile(t, filepath.Join(repo, "dist", "bundle.js"), "generated")
	candidates, err := ScanTrimCandidates(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	oldRunner := runGitBatch
	t.Cleanup(func() { runGitBatch = oldRunner })
	var ignoreCalls int
	runGitBatch = func(ctx context.Context, root string, args []string, input []byte) (gitBatchResult, error) {
		if args[0] == "check-ignore" {
			ignoreCalls++
			if ignoreCalls == 2 {
				writeTestFile(t, filepath.Join(repo, ".gitignore"), "")
			}
		}
		return oldRunner(ctx, root, args, input)
	}
	if _, err := RemoveTrimCandidates(context.Background(), repo, candidates); err == nil || !strings.Contains(err.Error(), "changed Git eligibility") {
		t.Fatalf("remove error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "dist", "bundle.js")); err != nil {
		t.Fatalf("candidate was not restored: %v", err)
	}
}

func TestRemoveTrimCandidatesRestoresEarlierQuarantinesBeforeAnyDeletion(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "**/dist/\n")
	writeTestFile(t, filepath.Join(repo, "one", "dist", "bundle.js"), "generated")
	writeTestFile(t, filepath.Join(repo, "two", "dist", "bundle.js"), "generated")
	candidates, err := ScanTrimCandidates(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	oldQuarantine := quarantineTrimCandidate
	t.Cleanup(func() { quarantineTrimCandidate = oldQuarantine })
	var calls int
	quarantineTrimCandidate = func(candidate TrimCandidate) (string, error) {
		calls++
		if calls == 2 {
			return "", errors.New("forced quarantine failure")
		}
		return oldQuarantine(candidate)
	}
	if _, err := RemoveTrimCandidates(context.Background(), repo, candidates); err == nil || !strings.Contains(err.Error(), "forced quarantine failure") {
		t.Fatalf("remove error = %v", err)
	}
	for _, path := range []string{"one/dist/bundle.js", "two/dist/bundle.js"} {
		if _, err := os.Stat(filepath.Join(repo, path)); err != nil {
			t.Fatalf("candidate %s was not restored: %v", path, err)
		}
	}
}

func TestRemoveTrimCandidatesKeepsPhysicalObservationUnavailableWhenAProbeFails(t *testing.T) {
	repo := newTestRepo(t)
	writeTestFile(t, filepath.Join(repo, ".gitignore"), "dist/\n")
	writeTestFile(t, filepath.Join(repo, "dist", "bundle.js"), "generated")
	candidates, err := ScanTrimCandidates(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	oldProbe := filesystemProbe
	t.Cleanup(func() { filesystemProbe = oldProbe })
	var calls int
	filesystemProbe = func(string) (FilesystemView, error) {
		calls++
		if calls == 1 {
			return FilesystemView{}, errors.New("first probe unavailable")
		}
		return FilesystemView{AvailableBytes: 99}, nil
	}
	result, err := RemoveTrimCandidates(context.Background(), repo, candidates)
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesystemObservation != "unavailable" || result.FilesystemFreeBefore != 0 || result.FilesystemFreeAfter != 0 || result.FilesystemFreeDelta != 0 {
		t.Fatalf("physical observation = %+v", result)
	}
}

func BenchmarkScanTrimCandidatesMultiProject(b *testing.B) {
	repo := newBenchmarkRepo(b)
	// Benchmarks run in an isolated process; build a realistic multi-project
	// tree once, then report the two batched Git subprocesses per scan.
	for project := 0; project < 12; project++ {
		writeBenchmarkFile(b, filepath.Join(repo, "project", fmt.Sprintf("%02d", project), "dist", "bundle.js"), strings.Repeat("x", 4096))
	}
	writeBenchmarkFile(b, filepath.Join(repo, ".gitignore"), "**/dist/\n")
	oldRunner := runGitBatch
	b.Cleanup(func() { runGitBatch = oldRunner })
	var calls int64
	runGitBatch = func(ctx context.Context, root string, args []string, input []byte) (gitBatchResult, error) {
		atomic.AddInt64(&calls, 1)
		return oldRunner(ctx, root, args, input)
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := ScanTrimCandidates(context.Background(), repo); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(atomic.LoadInt64(&calls))/float64(b.N), "git-procs/op")
}

func newBenchmarkRepo(b *testing.B) string {
	b.Helper()
	dir := b.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test"}} {
		command := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			b.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	return dir
}

func writeBenchmarkFile(b *testing.B, path, contents string) {
	b.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		b.Fatal(err)
	}
}

func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	gitTest(t, dir, "config", "user.name", "Test")
	return dir
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}

// gitTestCommit builds a fixture commit directly. It avoids inheriting the
// developer's commit-signing policy while never weakening or overriding that
// policy in repository configuration.
func gitTestCommit(t *testing.T, dir, message string) string {
	t.Helper()
	tree := strings.TrimSpace(gitTest(t, dir, "write-tree"))
	commit := strings.TrimSpace(gitTest(t, dir, "commit-tree", tree, "-m", message))
	gitTest(t, dir, "update-ref", "HEAD", commit)
	return commit
}
