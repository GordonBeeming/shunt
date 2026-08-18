package gitpreservation

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
}

func (f fakeRunner) run(_ context.Context, args ...string) (string, error) { return f.runFn(args...) }
func (f fakeRunner) patchID(_ context.Context, args ...string) (string, error) {
	return f.patchFn(args...)
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
	if got.Preserved || !strings.Contains(got.Reason, "parse") {
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
