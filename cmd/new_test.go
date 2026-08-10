package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestCreateSidingPinsLatestCleanBaseCommit(t *testing.T) {
	app := newSourceStateTestApp(t, true)
	baseSource, _, err := siding.Paths(app, app.BaseSiding)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseSource, "source.txt"), []byte("new base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceGitOutput(t, baseSource, "add", "source.txt")
	sourceGitOutput(t, baseSource, "commit", "-m", "advance base")
	want := sourceGitOutput(t, baseSource, "rev-parse", "HEAD")
	if want == app.BaseCommit {
		t.Fatal("test setup did not advance the selected base")
	}

	app, _, err = createSiding(context.Background(), app.ConfigDir, "next", "", "")
	if err != nil {
		t.Fatal(err)
	}
	nextSource, _, err := siding.Paths(app, "next")
	if err != nil {
		t.Fatal(err)
	}
	if head := sourceGitOutput(t, nextSource, "rev-parse", "HEAD"); head != want || app.BaseCommit != want {
		t.Fatalf("new siding HEAD = %q, BaseCommit = %q, want %q", head, app.BaseCommit, want)
	}
	loaded, err := state.LoadApp(app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.BaseCommit != want {
		t.Fatalf("persisted BaseCommit = %q, want %q", loaded.BaseCommit, want)
	}
}

func TestCreateSidingRejectsDirtyBaseBeforeCreatingWorktree(t *testing.T) {
	app := newSourceStateTestApp(t, true)
	baseSource, _, err := siding.Paths(app, app.BaseSiding)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseSource, "untracked.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err = createSiding(context.Background(), app.ConfigDir, "blocked", "", "")
	if err == nil || !strings.Contains(err.Error(), "uncommitted or untracked") {
		t.Fatalf("createSiding() error = %v", err)
	}
	blockedSource, _, pathErr := siding.Paths(app, "blocked")
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(blockedSource); !os.IsNotExist(statErr) {
		t.Fatalf("dirty-base creation left worktree %q: %v", blockedSource, statErr)
	}
	loaded, loadErr := state.LoadApp(app.ConfigDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, exists := loaded.Sidings["blocked"]; exists {
		t.Fatal("dirty-base creation published siding state")
	}
}

func TestCreateSidingDoesNotCompensateCommittedDurabilityError(t *testing.T) {
	app := newSourceStateTestApp(t, true)
	sentinel := errors.New("directory sync denied")
	ops := createSidingOps{saveApp: func(app state.App) error {
		if err := state.SaveApp(app); err != nil {
			return err
		}
		return &state.CommittedDurabilityError{Path: filepath.Join(app.ConfigDir, "state-v2.json"), Err: sentinel}
	}}

	_, _, err := createSidingWithOps(context.Background(), app.ConfigDir, "committed", "", "", ops)
	var committed *state.CommittedDurabilityError
	if !errors.As(err, &committed) || !errors.Is(err, sentinel) {
		t.Fatalf("createSidingWithOps() error = %v", err)
	}
	loaded, loadErr := state.LoadApp(app.ConfigDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, exists := loaded.Sidings["committed"]; !exists {
		t.Fatal("committed durability error triggered state compensation")
	}
	source, _, pathErr := siding.Paths(loaded, "committed")
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(source); statErr != nil {
		t.Fatalf("committed durability error removed worktree: %v", statErr)
	}
}

func TestCreateSidingCleansUpUnpublishedStateFailure(t *testing.T) {
	app := newSourceStateTestApp(t, true)
	sentinel := errors.New("state write denied")
	ops := createSidingOps{saveApp: func(state.App) error { return sentinel }}

	_, _, err := createSidingWithOps(context.Background(), app.ConfigDir, "unpublished", "", "", ops)
	if !errors.Is(err, sentinel) {
		t.Fatalf("createSidingWithOps() error = %v", err)
	}
	loaded, loadErr := state.LoadApp(app.ConfigDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, exists := loaded.Sidings["unpublished"]; exists {
		t.Fatal("failed state write published siding")
	}
	source, _, pathErr := siding.Paths(app, "unpublished")
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(source); !os.IsNotExist(statErr) {
		t.Fatalf("failed state write left worktree %q: %v; creation error: %v", source, statErr, err)
	}
}

func TestCreateFirstSidingSelectsItAsBase(t *testing.T) {
	app := newSourceStateTestApp(t, false)
	app, created, err := createSiding(context.Background(), app.ConfigDir, "first", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if app.BaseSiding != "first" || app.BaseCommit == "" || created.MaterializationPhase != state.PhaseWorktree {
		t.Fatalf("first siding state = %#v, app base = %q @ %q", created, app.BaseSiding, app.BaseCommit)
	}
}

func TestCreateSidingFencesRemovalFromCurrentState(t *testing.T) {
	app := newSourceStateTestApp(t, true)
	blockedControl := filepath.Join(app.ConfigDir, "blocked-control.git")
	app.ControlRepoPath = blockedControl
	app.Removal = &state.RemovalOperation{ID: "remove-base", Siding: app.BaseSiding, Stage: state.RemovalBasePinned}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}

	_, _, err := createSiding(context.Background(), app.ConfigDir, "blocked", "HEAD", "")
	if err == nil || !strings.Contains(err.Error(), "blocked while siding") {
		t.Fatalf("createSiding() error = %v", err)
	}
	if _, statErr := os.Stat(blockedControl); !os.IsNotExist(statErr) {
		t.Fatalf("removal-fenced create touched control repository: %v", statErr)
	}
	source, _, pathErr := siding.Paths(app, "blocked")
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if _, statErr := os.Stat(source); !os.IsNotExist(statErr) {
		t.Fatalf("removal-fenced create left worktree %q: %v", source, statErr)
	}
	loaded, loadErr := state.LoadApp(app.ConfigDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if _, exists := loaded.Sidings["blocked"]; exists {
		t.Fatal("removal-fenced create changed siding state")
	}
}
