package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/gordonbeeming/shunt/internal/storage"
)

func TestSpaceCommandShape(t *testing.T) {
	command := newSpaceCmd()
	if command.Use != "space" {
		t.Fatalf("Use = %q", command.Use)
	}
	for _, name := range []string{"all", "json"} {
		if command.Flags().Lookup(name) == nil {
			t.Fatalf("missing --%s", name)
		}
	}
}

func TestTrimCommandShape(t *testing.T) {
	command := newTrimCmd()
	if command.Use != "trim <siding>" {
		t.Fatalf("Use = %q", command.Use)
	}
	for _, name := range []string{"dry-run", "yes"} {
		if command.Flags().Lookup(name) == nil {
			t.Fatalf("missing --%s", name)
		}
	}
}

func TestConfirmTrimRequiresYesOutsideTerminal(t *testing.T) {
	err := confirmTrim(strings.NewReader("yes\n"), &bytes.Buffer{}, false, 1, 10)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error = %v", err)
	}
	if err := confirmTrim(strings.NewReader(""), &bytes.Buffer{}, true, 1, 10); err != nil {
		t.Fatalf("--yes failed: %v", err)
	}
}

func TestPrintSpaceLabelsCloneScansAndOfficialContainerData(t *testing.T) {
	report := storage.Report{
		Projects: []storage.ProjectReport{{
			Name:       "app",
			Filesystem: storage.FilesystemView{Measurement: "physical", Observation: "observed", TotalBytes: 100, UsedBytes: 75, AvailableBytes: 25},
			Logical:    storage.Measurement{Observation: "observed", LogicalBytes: 200},
			Source: storage.SourceReport{
				Measurement: storage.Measurement{Path: "/repo", Observation: "observed", LogicalBytes: 80},
				Git: storage.GitEvidence{
					Observation: "observed", Branch: "main", Head: "0123456789abcdef", Upstream: "origin/main",
					Ahead: 1, Behind: 2, Dirty: true, Untracked: 3, UniqueCommits: 4, LastCommit: "2026-08-07T09:00:00+10:00",
				},
			},
			Sidings: []storage.SidingReport{{
				Name: "feature",
				Source: storage.SourceReport{Measurement: storage.Measurement{Observation: "observed"}, Git: storage.GitEvidence{
					Observation: "observed", Branch: "shunt/feature", Head: "abcdef0123456789", UniqueCommits: 1,
				}}, Generated: storage.Measurement{Observation: "observed"}, Output: storage.Measurement{Observation: "observed"}, Data: storage.Measurement{Observation: "observed"},
			}},
		}},
		Container: container.SystemDiskUsage{Observation: "observed", Running: true, Data: json.RawMessage(`[{"reclaimableBytes":42}]`)},
	}
	var output bytes.Buffer
	if err := printSpace(&output, report); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{
		"physical",
		"logical, clone-shared; not reclaimable",
		"legacy checkout reference only; never a runtime host",
		"branch main; HEAD 0123456789ab; upstream origin/main (+1/-2); dirty, 3 untracked; unique 4; last 2026-08-07",
		"branch shunt/feature; HEAD abcdef012345; upstream (none) (+0/-0); clean; unique 1",
		"the only reclaimable figures",
		"reclaimableBytes",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}

func TestPrintSpaceShowsEveryMeasurementObservationInsteadOfFalseZero(t *testing.T) {
	report := storage.Report{Projects: []storage.ProjectReport{{
		Name:    "app",
		Logical: storage.Measurement{Observation: "error", Detail: "walk denied"},
		Source:  storage.SourceReport{Measurement: storage.Measurement{Path: "/repo", Observation: "missing", Detail: "not found"}},
		Sidings: []storage.SidingReport{{
			Name:      "one",
			Source:    storage.SourceReport{Measurement: storage.Measurement{Observation: "error", Detail: "source failed"}},
			Generated: storage.Measurement{Observation: "error", Detail: "generated failed"},
			Output:    storage.Measurement{Observation: "missing"},
			Data:      storage.Measurement{Observation: "error", Detail: "data failed"},
		}},
		Baselines: []storage.Measurement{{Name: "current", Observation: "error", Detail: "baseline failed"}},
	}}}
	var output bytes.Buffer
	if err := printSpace(&output, report); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{"error (walk denied)", "missing (not found)", "error (source failed)", "error (generated failed)", "missing", "error (data failed)", "error (baseline failed)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "project scan (logical, clone-shared; not reclaimable): 0 B") {
		t.Fatalf("failed measurement rendered as zero:\n%s", text)
	}
}

func TestSpaceCommandReturnsCollectionCancellation(t *testing.T) {
	oldLoad, oldCollect := spaceLoadApps, spaceCollect
	t.Cleanup(func() { spaceLoadApps, spaceCollect = oldLoad, oldCollect })
	spaceLoadApps = func(bool) ([]state.App, error) { return []state.App{{Name: "app"}}, nil }
	spaceCollect = func(context.Context, []state.App) (storage.Report, error) { return storage.Report{}, context.Canceled }
	command := newSpaceCmd()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	if err := command.ExecuteContext(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestPrintSpaceExplainsManagedAndUnclassifiedStorageWithoutReclaimClaim(t *testing.T) {
	report := storage.Report{Projects: []storage.ProjectReport{{
		Name: "app",
		Managed: []storage.Measurement{
			{Name: "source control store", Measurement: "logical", Observation: "observed", LogicalBytes: 7, Protected: true, Overlaps: "project"},
			{Name: "image cache", Measurement: "logical", Observation: "observed", LogicalBytes: 5, Overlaps: "project"},
		},
		Unclassified: []storage.Measurement{
			{Name: "base/images.tar", Measurement: "logical", Observation: "observed", LogicalBytes: 7, Overlaps: "project"},
			{Name: "legacy-baseline-20260803T012923Z", Measurement: "logical", Observation: "observed", LogicalBytes: 6, Overlaps: "project"},
		},
	}}}
	var output bytes.Buffer
	if err := printSpace(&output, report); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{
		"managed/protected source control store (logical; not reclaimable): 7 B",
		"managed image cache (logical; not reclaimable): 5 B",
		"unclassified base/images.tar (logical; ownership/reclaimability unverified): 7 B",
		"unclassified legacy-baseline-20260803T012923Z (logical; ownership/reclaimability unverified): 6 B",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}

func TestPrintSpaceDoesNotRenderUnavailableFilesystemAsZero(t *testing.T) {
	report := storage.Report{Projects: []storage.ProjectReport{{
		Name:       "app",
		Filesystem: storage.FilesystemView{Measurement: "physical", Observation: "unavailable", Detail: "config probe: denied; source fallback: denied"},
	}}}
	var output bytes.Buffer
	if err := printSpace(&output, report); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "filesystem (physical): unavailable (config probe: denied; source fallback: denied)") || strings.Contains(text, "0 B used of 0 B") {
		t.Fatalf("output = %s", text)
	}
}

func TestPrintSpaceDoesNotClaimUnavailableContainerServiceIsStopped(t *testing.T) {
	report := storage.Report{Container: container.SystemDiskUsage{Observation: "unavailable", Running: true, Detail: "df timed out"}}
	var output bytes.Buffer
	if err := printSpace(&output, report); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "this command did not start the service") || strings.Contains(text, "the service was not started") {
		t.Fatalf("output = %s", text)
	}
}

func TestRemoveConfirmedTrimBlocksRemovalPublishedWhileWaiting(t *testing.T) {
	preview := []storage.TrimCandidate{{Path: "/src/bin", RelativePath: "bin", LogicalBytes: 10}}
	app := state.App{ConfigDir: "/config", Sidings: map[string]state.Siding{"one": {Name: "one"}}}
	removed := false
	deps := trimLockedDependencies{
		withSidingOperation: func(_ context.Context, _, _ string, operation func() error) error {
			app.Removal = &state.RemovalOperation{Siding: "one", Stage: state.RemovalStarted}
			return operation()
		},
		loadApp:         func(string) (state.App, error) { return app, nil },
		ensureNoRemoval: siding.EnsureNoRemovalInProgress,
		paths:           siding.Paths,
		scan:            func(context.Context, string) ([]storage.TrimCandidate, error) { return preview, nil },
		remove: func(context.Context, string, []storage.TrimCandidate) (storage.TrimResult, error) {
			removed = true
			return storage.TrimResult{}, nil
		},
	}
	if _, err := removeConfirmedTrimWith(context.Background(), app.ConfigDir, "one", preview, deps); err == nil || !strings.Contains(err.Error(), "removal") {
		t.Fatalf("error = %v", err)
	}
	if removed {
		t.Fatal("trim removed candidates after removal was published")
	}
}

func TestRemoveConfirmedTrimRejectsChangedCandidateSet(t *testing.T) {
	preview := []storage.TrimCandidate{{Path: "/src/bin", RelativePath: "bin", LogicalBytes: 10}}
	current := []storage.TrimCandidate{{Path: "/src/obj", RelativePath: "obj", LogicalBytes: 12}}
	removed := false
	deps := successfulTrimDependencies(current, func(context.Context, string, []storage.TrimCandidate) (storage.TrimResult, error) {
		removed = true
		return storage.TrimResult{}, nil
	})
	if _, err := removeConfirmedTrimWith(context.Background(), "/config", "one", preview, deps); err == nil || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("error = %v", err)
	}
	if removed {
		t.Fatal("trim removed an unconfirmed candidate set")
	}
}

func TestRemoveConfirmedTrimUsesFreshUnchangedCandidateSet(t *testing.T) {
	preview := []storage.TrimCandidate{{Path: "/src/bin", RelativePath: "bin", LogicalBytes: 10}}
	current := append([]storage.TrimCandidate(nil), preview...)
	deps := successfulTrimDependencies(current, func(_ context.Context, src string, candidates []storage.TrimCandidate) (storage.TrimResult, error) {
		if src != "/src" || len(candidates) != 1 || candidates[0].RelativePath != "bin" {
			return storage.TrimResult{}, errors.New("remove received stale inputs")
		}
		return storage.TrimResult{CandidateBytes: 10, RemovedBytes: 10}, nil
	})
	result, err := removeConfirmedTrimWith(context.Background(), "/config", "one", preview, deps)
	if err != nil || result.RemovedBytes != 10 {
		t.Fatalf("result = %+v, %v", result, err)
	}
}

func successfulTrimDependencies(current []storage.TrimCandidate, remove func(context.Context, string, []storage.TrimCandidate) (storage.TrimResult, error)) trimLockedDependencies {
	app := state.App{ConfigDir: "/config", Sidings: map[string]state.Siding{"one": {Name: "one"}}}
	return trimLockedDependencies{
		withSidingOperation: func(_ context.Context, _, _ string, operation func() error) error { return operation() },
		loadApp:             func(string) (state.App, error) { return app, nil },
		ensureNoRemoval:     siding.EnsureNoRemovalInProgress,
		paths:               func(state.App, string) (string, string, error) { return "/src", "/vol", nil },
		scan:                func(context.Context, string) ([]storage.TrimCandidate, error) { return current, nil },
		remove:              remove,
	}
}
