package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestWithLatestSidingBlocksRemovalPublishedWhileWaiting(t *testing.T) {
	configDir := t.TempDir()
	app := state.App{ConfigDir: configDir, Sidings: map[string]state.Siding{"one": {Name: "one", Container: "guest"}}}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	lockedDone := make(chan error, 1)
	go func() {
		lockedDone <- siding.WithProjectOperation(context.Background(), configDir, func() error {
			if _, err := state.UpdateApp(context.Background(), configDir, func(current *state.App) error {
				current.Removal = &state.RemovalOperation{ID: "remove-one", Siding: "one", Stage: state.RemovalStarted}
				return nil
			}); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	called := false
	done := make(chan error, 1)
	go func() {
		done <- withLatestSiding(context.Background(), configDir, "one", "test", func(state.App, state.Siding) error {
			called = true
			return nil
		})
	}()
	close(release)
	if err := <-lockedDone; err != nil {
		t.Fatal(err)
	}
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "removal") {
		t.Fatalf("withLatestSiding() error = %v, want removal fence", err)
	}
	if called {
		t.Fatal("underlying siding action ran after removal was published")
	}
}

func TestLockedGitAndGuestOperationsDoNotRunDuringRemoval(t *testing.T) {
	configDir := t.TempDir()
	app := state.App{
		ConfigDir: configDir,
	}
	app.Sidings = map[string]state.Siding{"one": {Name: "one", Container: "guest"}}
	app.Removal = &state.RemovalOperation{ID: "remove-one", Siding: "one", Stage: state.RemovalStarted}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	originalGit, originalLog := runSidingGitCommand, readGuestLog
	t.Cleanup(func() {
		runSidingGitCommand = originalGit
		readGuestLog = originalLog
	})
	gitCalled, guestCalled := false, false
	runSidingGitCommand = func(context.Context, string, ...string) error {
		gitCalled = true
		return nil
	}
	readGuestLog = func(context.Context, string, ...string) (string, error) {
		guestCalled = true
		return "", nil
	}
	if err := runSidingGit(context.Background(), configDir, "one", []string{"status"}); err == nil || !strings.Contains(err.Error(), "removal") {
		t.Fatalf("runSidingGit() error = %v, want removal fence", err)
	}
	if _, err := sidingLog(context.Background(), configDir, "one", 0); err == nil || !strings.Contains(err.Error(), "removal") {
		t.Fatalf("sidingLog() error = %v, want removal fence", err)
	}
	if gitCalled || guestCalled {
		t.Fatalf("underlying operations ran: git=%v guest=%v", gitCalled, guestCalled)
	}
}

func TestSidingStatus(t *testing.T) {
	app := state.App{LiveSiding: "liveone"}
	bridged := map[string]int{"web": 5000}

	cases := []struct {
		desc  string
		name  string
		sd    state.Siding
		guest string
		want  string
	}{
		{"live wins over running+bridged", "liveone", state.Siding{Bridges: bridged}, "running", "live"},
		{"stopped even if bridged", "b", state.Siding{Stopped: true, Bridges: bridged}, "running", "stopped"},
		{"up when running and bridged", "b", state.Siding{Bridges: bridged}, "running", "up"},
		{"idle when running but unbridged", "b", state.Siding{}, "running", "idle"},
		{"stopped is observed separately", "b", state.Siding{Bridges: bridged}, "stopped", "stopped"},
		{"idle when guest state unknown", "b", state.Siding{}, "", "idle"},
		{"runtime unavailable is not stopped", "b", state.Siding{}, "unavailable", "unavailable"},
		{"worktree phase needs no runtime", "b", state.Siding{MaterializationPhase: state.PhaseWorktree}, "unavailable", "worktree"},
		{"parked phase needs no runtime", "b", state.Siding{MaterializationPhase: state.PhaseParked}, "unavailable", "parked"},
	}
	for _, c := range cases {
		if got := sidingStatus(app, c.name, c.sd, c.guest); got != c.want {
			t.Errorf("%s: sidingStatus = %q, want %q", c.desc, got, c.want)
		}
	}
}
