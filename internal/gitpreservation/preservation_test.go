package gitpreservation

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type repoFixture struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repoFixture {
	t.Helper()
	dir := t.TempDir()
	r := &repoFixture{t: t, dir: dir}
	r.git("init", "-b", "main")
	r.git("config", "user.name", "Shunt Test")
	r.git("config", "user.email", "shunt@example.invalid")
	r.git("config", "commit.gpgSign", "false")
	r.git("config", "tag.gpgSign", "false")
	r.write("base.txt", []byte("base\n"), 0o644)
	r.git("add", ".")
	r.git("commit", "-m", "base")
	r.git("remote", "add", "origin", dir)
	return r
}

func (r *repoFixture) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repoFixture) write(name string, data []byte, mode os.FileMode) {
	r.t.Helper()
	path := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repoFixture) commit(name, contents, message string) string {
	r.write(name, []byte(contents), 0o644)
	r.git("add", ".")
	r.git("commit", "-m", message)
	return r.git("rev-parse", "HEAD")
}

func (r *repoFixture) integrationRef() {
	r.git("update-ref", "refs/remotes/origin/main", "refs/heads/main")
}

func analyzeRef(r *repoFixture, target string, deleted ...string) Result {
	return Analyze(context.Background(), Request{Repo: r.dir, TargetRef: target, DeletionRefs: deleted})
}

func TestAnalyzeReachableFromSurvivingRef(t *testing.T) {
	r := newRepo(t)
	r.git("switch", "-c", "topic")
	r.commit("topic.txt", "work\n", "work")
	r.git("branch", "keeper")
	got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic")
	if !got.Preserved || got.Kind != KindReachable || got.MatchingRef != "refs/heads/keeper" {
		t.Fatalf("result = %+v", got)
	}
}

func TestAnalyzeReachableFromTag(t *testing.T) {
	r := newRepo(t)
	r.git("switch", "-c", "topic")
	r.commit("topic.txt", "work\n", "work")
	r.git("tag", "saved")
	got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic")
	if !got.Preserved || got.Kind != KindReachable || got.MatchingRef != "refs/tags/saved" {
		t.Fatalf("result = %+v", got)
	}
}

func TestAnalyzeExcludesEveryDeletionRef(t *testing.T) {
	r := newRepo(t)
	r.git("switch", "-c", "topic")
	r.commit("topic.txt", "work\n", "work")
	r.git("branch", "also-deleted")
	r.git("switch", "main")
	r.integrationRef()
	got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic", "refs/heads/also-deleted")
	if got.Preserved {
		t.Fatalf("selected refs preserved each other: %+v", got)
	}
}

func TestAnalyzeEquivalentCherryPick(t *testing.T) {
	r := newRepo(t)
	base := r.git("rev-parse", "HEAD")
	r.git("switch", "-c", "topic")
	old := r.commit("work.txt", "one\n", "topic")
	r.git("switch", "main")
	r.commit("main.txt", "advance\n", "advance")
	r.git("cherry-pick", old)
	r.integrationRef()
	got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic")
	if !got.Preserved || got.Kind != KindEquivalent || got.MatchingRef != "refs/remotes/origin/main" {
		t.Fatalf("base %s result = %+v", base, got)
	}
}

func TestAnalyzeEquivalentRebase(t *testing.T) {
	r := newRepo(t)
	r.git("switch", "-c", "topic")
	r.commit("one.txt", "one\n", "one")
	r.commit("two.txt", "two\n", "two")
	r.git("branch", "integrated")
	r.git("switch", "main")
	r.commit("main.txt", "advance\n", "advance")
	r.git("switch", "integrated")
	r.git("rebase", "main")
	r.git("update-ref", "refs/remotes/origin/main", "refs/heads/integrated")
	got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic", "refs/heads/integrated")
	if !got.Preserved || got.Kind != KindEquivalent {
		t.Fatalf("result = %+v", got)
	}
}

func TestAnalyzeSquash(t *testing.T) {
	r := newRepo(t)
	r.git("switch", "-c", "topic")
	r.commit("one.txt", "one\n", "one")
	r.commit("two.txt", "two\n", "two")
	r.git("switch", "main")
	r.git("merge", "--squash", "topic")
	squash := r.git("commit", "-m", "squash")
	_ = squash
	r.integrationRef()
	got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic")
	if !got.Preserved || got.Kind != KindSquash || got.MatchingCommit == "" {
		t.Fatalf("result = %+v", got)
	}
}

func TestAnalyzePrefersOriginHEADAndFallsBackToOriginMain(t *testing.T) {
	r := newRepo(t)
	r.git("switch", "-c", "topic")
	r.commit("one.txt", "one\n", "one")
	r.git("switch", "main")
	r.git("merge", "--squash", "topic")
	r.git("commit", "-m", "squash")
	r.integrationRef()
	r.git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic")
	if !got.Preserved || got.MatchingRef != "refs/remotes/origin/HEAD" {
		t.Fatalf("origin HEAD result = %+v", got)
	}
	r.git("symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	got = analyzeRef(r, "refs/heads/topic", "refs/heads/topic")
	if !got.Preserved || got.MatchingRef != "refs/remotes/origin/main" {
		t.Fatalf("origin main result = %+v", got)
	}
}

func TestAnalyzeStrictPatchForms(t *testing.T) {
	t.Run("whitespace difference is unproven", func(t *testing.T) {
		r := newRepo(t)
		r.git("switch", "-c", "topic")
		r.commit("work.txt", "a b\n", "topic")
		r.git("switch", "main")
		r.commit("work.txt", "a  b\n", "different whitespace")
		r.integrationRef()
		if got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic"); got.Preserved {
			t.Fatalf("result = %+v", got)
		}
	})
	t.Run("binary squash", func(t *testing.T) {
		r := newRepo(t)
		r.git("switch", "-c", "topic")
		r.write("blob.bin", []byte{0, 1, 2, 0xff}, 0o644)
		r.git("add", ".")
		r.git("commit", "-m", "binary")
		r.git("switch", "main")
		r.git("merge", "--squash", "topic")
		r.git("commit", "-m", "squash")
		r.integrationRef()
		if got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic"); !got.Preserved {
			t.Fatalf("result = %+v", got)
		}
	})
	t.Run("mode squash", func(t *testing.T) {
		r := newRepo(t)
		r.write("script.sh", []byte("#!/bin/sh\n"), 0o644)
		r.git("add", ".")
		r.git("commit", "-m", "script")
		r.git("switch", "-c", "topic")
		if err := os.Chmod(filepath.Join(r.dir, "script.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
		r.git("update-index", "--chmod=+x", "script.sh")
		r.git("commit", "-m", "mode")
		r.git("switch", "main")
		r.git("merge", "--squash", "topic")
		r.git("commit", "-m", "squash")
		r.integrationRef()
		if got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic"); !got.Preserved {
			t.Fatalf("result = %+v", got)
		}
	})
}

func TestAnalyzeSubmoduleSquash(t *testing.T) {
	sub := newRepo(t)
	r := newRepo(t)
	r.git("-c", "protocol.file.allow=always", "submodule", "add", sub.dir, "module")
	r.git("commit", "-m", "add submodule")
	r.git("switch", "-c", "topic")
	sub.commit("next.txt", "next\n", "next")
	r.git("-C", "module", "fetch")
	r.git("-C", "module", "checkout", sub.git("rev-parse", "HEAD"))
	r.git("add", "module")
	r.git("commit", "-m", "bump submodule")
	r.git("switch", "main")
	r.git("merge", "--squash", "topic")
	r.git("commit", "-m", "squash")
	r.integrationRef()
	if got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic"); !got.Preserved {
		t.Fatalf("result = %+v", got)
	}
}

func TestAnalyzeIntegrationHistoryAllowsMergeAndEmptyCommitRowsToBeAbsent(t *testing.T) {
	r := newRepo(t)
	r.git("switch", "-c", "topic")
	r.commit("one", "1\n", "one")
	r.commit("two", "2\n", "two")
	r.git("switch", "main")
	r.git("commit", "--allow-empty", "-m", "empty integration commit")
	r.git("switch", "-c", "integration-side")
	r.commit("side", "side\n", "side")
	r.git("switch", "main")
	r.git("merge", "--no-ff", "integration-side", "-m", "integration merge")
	r.git("merge", "--squash", "topic")
	r.git("commit", "-m", "squash topic")
	r.integrationRef()
	got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic", "refs/heads/integration-side")
	if !got.Preserved || got.Kind != KindSquash {
		t.Fatalf("result = %+v", got)
	}
}

func TestAnalyzeRejectsMergeAndPartialOrMultipleSquashes(t *testing.T) {
	t.Run("topic merge", func(t *testing.T) {
		r := newRepo(t)
		r.git("switch", "-c", "topic")
		r.commit("one", "1", "one")
		r.git("switch", "-c", "side", "main")
		r.commit("side", "s", "side")
		r.git("switch", "topic")
		r.git("merge", "--no-ff", "side", "-m", "merge")
		r.git("switch", "main")
		r.integrationRef()
		if got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic", "refs/heads/side"); got.Preserved {
			t.Fatalf("result = %+v", got)
		}
	})
	t.Run("partial", func(t *testing.T) {
		r := newRepo(t)
		r.git("switch", "-c", "topic")
		first := r.commit("one", "1", "one")
		r.commit("two", "2", "two")
		r.git("switch", "main")
		r.git("cherry-pick", first)
		r.integrationRef()
		if got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic"); got.Preserved {
			t.Fatalf("result = %+v", got)
		}
	})
	t.Run("multiple squashes", func(t *testing.T) {
		r := newRepo(t)
		r.git("switch", "-c", "topic")
		r.commit("one", "1", "one")
		r.commit("two", "2", "two")
		r.git("switch", "main")
		r.write("one", []byte("1"), 0o644)
		r.write("temporary", []byte("extra"), 0o644)
		r.git("add", ".")
		r.git("commit", "-m", "squash one")
		r.write("two", []byte("2"), 0o644)
		if err := os.Remove(filepath.Join(r.dir, "temporary")); err != nil {
			t.Fatal(err)
		}
		r.git("add", "-A")
		r.git("commit", "-m", "squash two")
		r.integrationRef()
		if got := analyzeRef(r, "refs/heads/topic", "refs/heads/topic"); got.Preserved {
			t.Fatalf("result = %+v", got)
		}
	})
}

type fakeRunner struct {
	runFn   func(...string) (string, error)
	patchFn func(...string) (string, error)
	batchFn func(string) (string, error)
}

func (f fakeRunner) run(_ context.Context, args ...string) (string, error) { return f.runFn(args...) }
func (f fakeRunner) patchID(_ context.Context, args ...string) (string, error) {
	return f.patchFn(args...)
}
func (f fakeRunner) patchIDs(_ context.Context, revision string) (string, error) {
	if f.batchFn == nil {
		return "", fmt.Errorf("unexpected patchIDs(%s)", revision)
	}
	return f.batchFn(revision)
}

func TestAnalyzeMalformedRefsAndCancellation(t *testing.T) {
	f := fakeRunner{runFn: func(args ...string) (string, error) {
		if args[0] == "rev-parse" {
			return strings.Repeat("a", 40), nil
		}
		if args[0] == "for-each-ref" && len(args) > 2 && strings.HasPrefix(args[2], "--contains=") {
			return "", nil
		}
		return "malformed", nil
	}, patchFn: func(...string) (string, error) { return "", nil }}
	got := analyze(context.Background(), f, Request{TargetRef: "refs/heads/topic"})
	if got.Preserved || !strings.Contains(got.Reason, "surviving ref enumeration") {
		t.Fatalf("result = %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := analyze(ctx, f, Request{TargetRef: "x"}); got.Preserved || !strings.Contains(got.Reason, "cancelled") {
		t.Fatalf("result = %+v", got)
	}
}

func TestAnalyzeFailsClosedOnReachabilityCommandError(t *testing.T) {
	f := fakeRunner{runFn: func(args ...string) (string, error) {
		if args[0] == "rev-parse" {
			return strings.Repeat("a", 40), nil
		}
		if args[0] == "for-each-ref" && len(args) > 2 && strings.HasPrefix(args[2], "--contains=") {
			return "", fmt.Errorf("git failed")
		}
		return "refs/remotes/origin/main\x00" + strings.Repeat("b", 40) + "\x00", nil
	}, patchFn: func(...string) (string, error) { return "", nil }}
	got := analyze(context.Background(), f, Request{TargetRef: "refs/heads/topic"})
	if got.Preserved || !strings.Contains(got.Reason, "reachability") {
		t.Fatalf("result = %+v", got)
	}
}

func TestCommitsEnforcesBothScanLimits(t *testing.T) {
	for _, limit := range []int{MaxTopicCommits, MaxIntegrationCommits} {
		t.Run(fmt.Sprintf("limit-%d", limit), func(t *testing.T) {
			f := fakeRunner{runFn: func(...string) (string, error) {
				var b strings.Builder
				for i := 0; i <= limit; i++ {
					fmt.Fprintf(&b, "%040x\n", i+1)
				}
				return b.String(), nil
			}, patchFn: func(...string) (string, error) { return "", nil }}
			list, overflow, err := commits(f, context.Background(), limit, "range")
			if err != nil || !overflow || len(list) != limit+1 {
				t.Fatalf("len=%d overflow=%v err=%v", len(list), overflow, err)
			}
		})
	}
}

func TestHistoryPatchIDsRejectsMalformedMissingAndDuplicateRows(t *testing.T) {
	commitA := strings.Repeat("a", 40)
	commitB := strings.Repeat("b", 40)
	patchA := strings.Repeat("1", 40)
	patchB := strings.Repeat("2", 40)
	tests := []struct {
		name string
		out  string
	}{
		{name: "malformed", out: "not-a-patch-row\n" + patchB + " " + commitB},
		{name: "duplicate", out: patchA + " " + commitA + "\n" + patchB + " " + commitA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := fakeRunner{
				runFn:   func(...string) (string, error) { return "", fmt.Errorf("unexpected run") },
				patchFn: func(...string) (string, error) { return "", fmt.Errorf("unexpected patch") },
				batchFn: func(string) (string, error) { return tt.out, nil },
			}
			if _, err := historyPatchIDs(context.Background(), f, "range", []string{commitA, commitB}); err == nil {
				t.Fatal("historyPatchIDs unexpectedly accepted invalid rows")
			}
		})
	}
	f := fakeRunner{
		runFn:   func(...string) (string, error) { return "", fmt.Errorf("unexpected run") },
		patchFn: func(...string) (string, error) { return "", fmt.Errorf("unexpected patch") },
		batchFn: func(string) (string, error) { return patchA + " " + commitA, nil },
	}
	patches, err := historyPatchIDs(context.Background(), f, "range", []string{commitA, commitB})
	if err != nil || len(patches) != 1 || patches[0].commit != commitA {
		t.Fatalf("ordered missing row subset = %+v, err=%v", patches, err)
	}
}

type countingRunner struct {
	inner      runner
	runCalls   int
	patchCalls int
	batchCalls int
	runArgs    [][]string
}

func (c *countingRunner) run(ctx context.Context, args ...string) (string, error) {
	c.runCalls++
	c.runArgs = append(c.runArgs, append([]string(nil), args...))
	return c.inner.run(ctx, args...)
}
func (c *countingRunner) patchID(ctx context.Context, args ...string) (string, error) {
	c.patchCalls++
	return c.inner.patchID(ctx, args...)
}
func (c *countingRunner) patchIDs(ctx context.Context, revision string) (string, error) {
	c.batchCalls++
	return c.inner.patchIDs(ctx, revision)
}

func TestAnalyzeBatchesPatchProcessesIndependentOfCommitCount(t *testing.T) {
	r := newRepo(t)
	r.git("switch", "-c", "topic")
	for i := 0; i < 24; i++ {
		r.commit(fmt.Sprintf("file-%02d", i), fmt.Sprintf("value-%02d\n", i), fmt.Sprintf("topic %02d", i))
	}
	r.git("switch", "main")
	r.git("merge", "--squash", "topic")
	r.git("commit", "-m", "squash")
	r.integrationRef()
	c := &countingRunner{inner: gitRunner{repo: r.dir}}
	got := analyze(context.Background(), c, Request{TargetRef: "refs/heads/topic", DeletionRefs: []string{"refs/heads/topic"}})
	if !got.Preserved || got.Kind != KindSquash {
		t.Fatalf("result = %+v", got)
	}
	if c.batchCalls != 2 || c.patchCalls != 1 {
		t.Fatalf("process calls: run=%d batch=%d aggregate=%d", c.runCalls, c.batchCalls, c.patchCalls)
	}
	if c.runCalls > 8 {
		t.Fatalf("git metadata calls grew with commit count: %d", c.runCalls)
	}
}

func TestAnalyzerReusesRefAndIntegrationEvidenceAcrossTargets(t *testing.T) {
	r := newRepo(t)
	r.git("switch", "-c", "topic-one")
	r.commit("one", "one\n", "one")
	r.git("switch", "main")
	r.git("switch", "-c", "topic-two")
	r.commit("two", "two\n", "two")
	r.git("switch", "main")
	for _, branch := range []string{"topic-one", "topic-two"} {
		r.git("merge", "--squash", branch)
		r.git("commit", "-m", "squash "+branch)
	}
	r.integrationRef()
	c := &countingRunner{inner: gitRunner{repo: r.dir}}
	a := &Analyzer{repo: r.dir, git: c, refs: make(map[string][]refInfo), patches: make(map[string]integrationEvidence)}
	deleted := []string{"refs/heads/topic-one", "refs/heads/topic-two"}
	for _, target := range deleted {
		got := a.analyze(context.Background(), Request{Repo: r.dir, TargetRef: target, DeletionRefs: deleted})
		if !got.Preserved {
			t.Fatalf("%s result = %+v", target, got)
		}
	}
	if c.batchCalls != 3 {
		t.Fatalf("batch calls = %d, want two topic histories plus one cached integration history", c.batchCalls)
	}
	enumerations := 0
	for _, args := range c.runArgs {
		if len(args) >= 1 && args[0] == "for-each-ref" && !slices.ContainsFunc(args, func(arg string) bool { return strings.HasPrefix(arg, "--contains=") }) {
			enumerations++
		}
	}
	if enumerations != 1 {
		t.Fatalf("surviving ref enumerations = %d, want 1", enumerations)
	}
}

func TestAnalyzeReportsPatchSizeAndTimeoutDistinctly(t *testing.T) {
	r := newRepo(t)
	r.git("switch", "-c", "topic")
	r.commit("large", strings.Repeat("payload", 2048), "large")
	r.git("switch", "main")
	r.integrationRef()
	got := NewAnalyzer(r.dir, Options{MaxPatchBytes: 64}).Analyze(context.Background(), "refs/heads/topic", []string{"refs/heads/topic"})
	if got.Preserved || !strings.Contains(got.Reason, "size limit during topic patch analysis") {
		t.Fatalf("size result = %+v", got)
	}
	got = NewAnalyzer(r.dir, Options{Timeout: time.Nanosecond}).Analyze(context.Background(), "refs/heads/topic", []string{"refs/heads/topic"})
	if got.Preserved || !strings.Contains(got.Reason, "timed out during") {
		t.Fatalf("timeout result = %+v", got)
	}
}

func TestAnalyzeEndToEndHistoryOverflow(t *testing.T) {
	for _, tc := range []struct {
		name             string
		topicCount       int
		integrationCount int
		want             string
	}{
		{name: "topic", topicCount: MaxTopicCommits + 1, integrationCount: 1, want: "topic history exceeds scan limit"},
		{name: "integration", topicCount: 1, integrationCount: MaxIntegrationCommits + 1, want: "integration history exceeds scan limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git := scriptedAnalysisRunner(tc.topicCount, tc.integrationCount, "")
			got := analyze(context.Background(), git, Request{TargetRef: testTargetRef, DeletionRefs: []string{testTargetRef}})
			if got.Preserved || got.Reason != tc.want {
				t.Fatalf("result = %+v", got)
			}
		})
	}
}

func TestAnalyzePerStageFailureMatrix(t *testing.T) {
	tests := []struct{ failure, want string }{
		{failure: "target", want: "target resolution"},
		{failure: "refs", want: "surviving ref enumeration"},
		{failure: "reachability", want: "reachability inspection"},
		{failure: "merge-base", want: "merge-base resolution"},
		{failure: "topic-history", want: "topic history scan"},
		{failure: "topic-merge", want: "topic merge scan"},
		{failure: "topic-patch", want: "topic patch analysis"},
		{failure: "integration-history", want: "integration history scan"},
		{failure: "integration-patch", want: "integration patch parsing"},
		{failure: "aggregate", want: "aggregate patch analysis"},
	}
	for _, tt := range tests {
		t.Run(tt.failure, func(t *testing.T) {
			got := analyze(context.Background(), scriptedAnalysisRunner(1, 1, tt.failure), Request{TargetRef: testTargetRef, DeletionRefs: []string{testTargetRef}})
			if got.Preserved || !strings.Contains(got.Reason, tt.want) {
				t.Fatalf("result = %+v, want stage %q", got, tt.want)
			}
		})
	}
}

const testTargetRef = "refs/heads/topic"

func testOID(n int) string { return fmt.Sprintf("%040x", n) }

func scriptedAnalysisRunner(topicCount, integrationCount int, failure string) fakeRunner {
	target, main, base := testOID(1), testOID(2), testOID(3)
	topicCommits := make([]string, topicCount)
	for i := range topicCommits {
		topicCommits[i] = testOID(100 + i)
	}
	integrationCommits := make([]string, integrationCount)
	for i := range integrationCommits {
		integrationCommits[i] = testOID(10000 + i)
	}
	failed := func(stage string) (string, error) {
		if failure == stage {
			return "", fmt.Errorf("injected %s failure", stage)
		}
		return "", nil
	}
	return fakeRunner{
		runFn: func(args ...string) (string, error) {
			switch args[0] {
			case "rev-parse":
				if out, err := failed("target"); err != nil {
					return out, err
				}
				return target, nil
			case "for-each-ref":
				if slices.ContainsFunc(args, func(arg string) bool { return strings.HasPrefix(arg, "--contains=") }) {
					if out, err := failed("reachability"); err != nil {
						return out, err
					}
					return "", nil
				}
				if out, err := failed("refs"); err != nil {
					return out, err
				}
				return "refs/remotes/origin/main\x00" + main + "\x00", nil
			case "merge-base":
				if out, err := failed("merge-base"); err != nil {
					return out, err
				}
				return base, nil
			case "rev-list":
				if slices.Contains(args, "--min-parents=2") {
					return failed("topic-merge")
				}
				if slices.Contains(args, fmt.Sprintf("--max-count=%d", MaxTopicCommits+1)) {
					if out, err := failed("topic-history"); err != nil {
						return out, err
					}
					return strings.Join(topicCommits, "\n"), nil
				}
				if out, err := failed("integration-history"); err != nil {
					return out, err
				}
				return strings.Join(integrationCommits, "\n"), nil
			}
			return "", fmt.Errorf("unexpected git command %v", args)
		},
		batchFn: func(revision string) (string, error) {
			commits := integrationCommits
			patch := testOID(22)
			stage := "integration-patch"
			if strings.HasSuffix(revision, target) {
				commits = topicCommits
				patch = testOID(11)
				stage = "topic-patch"
			}
			if out, err := failed(stage); err != nil {
				return out, err
			}
			rows := make([]string, len(commits))
			for i, commit := range commits {
				rows[i] = patch + " " + commit
			}
			return strings.Join(rows, "\n"), nil
		},
		patchFn: func(...string) (string, error) {
			if out, err := failed("aggregate"); err != nil {
				return out, err
			}
			return testOID(33), nil
		},
	}
}

func TestAnalyzeScanLimits(t *testing.T) {
	topicOID := strings.Repeat("a", 40)
	mainOID := strings.Repeat("b", 40)
	baseOID := strings.Repeat("c", 40)
	call := 0
	f := fakeRunner{runFn: func(args ...string) (string, error) {
		switch args[0] {
		case "rev-parse":
			return topicOID, nil
		case "for-each-ref":
			if len(args) > 2 && strings.HasPrefix(args[2], "--contains=") {
				return "", nil
			}
			return "refs/remotes/origin/main\x00" + mainOID + "\x00", nil
		case "merge-base":
			return baseOID, nil
		case "rev-list":
			call++
			if call == 1 {
				return strings.Repeat("d", 40) + "\n", nil
			}
			var b strings.Builder
			for i := 0; i <= MaxIntegrationCommits; i++ {
				fmt.Fprintf(&b, "%040x\n", i+1)
			}
			return b.String(), nil
		}
		return "", fmt.Errorf("unexpected %v", args)
	}, patchFn: func(...string) (string, error) { return "", nil }}
	got := analyze(context.Background(), f, Request{TargetRef: "refs/heads/topic"})
	// The second rev-list call above is the parent inspection; malformed parent
	// data fails closed before integration scanning, which is independently
	// bounded by commits().
	if got.Preserved {
		t.Fatalf("result = %+v", got)
	}
	list, overflow, err := commits(f, context.Background(), MaxIntegrationCommits, "range")
	if err != nil || !overflow || len(list) != MaxIntegrationCommits+1 {
		t.Fatalf("len=%d overflow=%v err=%v", len(list), overflow, err)
	}
}
