package container

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/proc"
)

// stubRun answers a --version probe with the given streams, so no real CLI is
// needed. It mirrors the injectable runner observeSystem already uses.
func stubRun(stdout, stderr string, err error) systemCommandRunner {
	return func(context.Context, string, ...string) (proc.Result, error) {
		return proc.Result{Stdout: stdout, Stderr: stderr}, err
	}
}

func found(string) bool   { return true }
func missing(string) bool { return false }

func TestVersionParsesTheRealCLIOutput(t *testing.T) {
	// The exact string this CLI prints, including the build/commit suffix that
	// must not be mistaken for version components.
	got, ok := probeVersion(context.Background(), found,
		stubRun("container CLI version 1.0.0 (build: release, commit: ee848e3)", "", nil))
	if !ok || got != "1.0.0" {
		t.Fatalf("version = %q ok=%v, want \"1.0.0\" true", got, ok)
	}
}

func TestVersionReadsStderrWhenStdoutIsEmpty(t *testing.T) {
	// Which stream carries the version varies by release, so both are read.
	got, ok := probeVersion(context.Background(), found,
		stubRun("", "container CLI version 1.2.2", nil))
	if !ok || got != "1.2.2" {
		t.Fatalf("version = %q ok=%v, want \"1.2.2\" true", got, ok)
	}
}

func TestCheckMinimumRejectsOnlyVersionsBelowTheFloor(t *testing.T) {
	cases := []struct {
		version string
		wantErr bool
	}{
		{"1.0.0", true},  // the version that produced the exit-64 report
		{"1.1.9", true},  // just below the floor
		{"1.2.0", false}, // the floor itself: --read-only-path arrives here
		{"1.2.2", false}, // the tested release
		{"1.3.0", false}, // newer than tested is still accepted
		{"2.0.0", false}, // a major bump must not read as older
	}
	for _, c := range cases {
		err := checkMinimum(context.Background(), found,
			stubRun("container CLI version "+c.version, "", nil))
		if (err != nil) != c.wantErr {
			t.Errorf("checkMinimum(%s) err = %v, wantErr %v", c.version, err, c.wantErr)
		}
	}
}

func TestCheckMinimumTreatsAnUnknownVersionAsUsable(t *testing.T) {
	// Refusing every guest because a future release renamed its version string
	// would be a worse failure than the one this check exists to prevent, so an
	// unreadable version means proceed rather than block.
	unknown := []struct {
		name string
		look func(string) bool
		run  systemCommandRunner
	}{
		{"no version in the output", found, stubRun("container CLI (build: release)", "", nil)},
		{"probe failed", found, stubRun("", "", errors.New("boom"))},
		{"CLI absent from PATH", missing, stubRun("", "", nil)},
	}
	for _, c := range unknown {
		if err := checkMinimum(context.Background(), c.look, c.run); err != nil {
			t.Errorf("%s: err = %v, want nil", c.name, err)
		}
	}
}

func TestUnsupportedVersionErrorNamesBothVersionsAndRulesOutTheWrongFixes(t *testing.T) {
	err := checkMinimum(context.Background(), found, stubRun("container CLI version 1.0.0", "", nil))
	var unsupported *UnsupportedVersionError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %T, want *UnsupportedVersionError", err)
	}
	if unsupported.Found != "1.0.0" || unsupported.Required != MinimumVersion {
		t.Fatalf("error carries found=%q required=%q", unsupported.Found, unsupported.Required)
	}
	// The message is the point of the change: it has to name what was found, what
	// is needed, and — most importantly — that reapply and rm are the wrong
	// reaches, since following the old error's advice destroys a worktree.
	for _, want := range []string{"1.0.0", MinimumVersion, testedVersion, "reapply", "rm", "host CLI"} {
		if !strings.Contains(unsupported.Error(), want) {
			t.Errorf("message is missing %q:\n%s", want, unsupported.Error())
		}
	}
}

func TestCompareVersionsHandlesUnequalLengthsAndJunk(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2", "1.2.0", 0},    // a missing component reads as zero
		{"1.2.1", "1.2", 1},    // and does not make the shorter one win
		{"1.10.0", "1.9.0", 1}, // numeric, not lexical
		{"x.y.z", "1.2.0", -1}, // junk never sorts above a real version
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
