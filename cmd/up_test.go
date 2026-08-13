package cmd

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestUpRejectsWorktreeOnlyStateBeforeGuestEffects(t *testing.T) {
	repo, configDir, _ := newShellOnlyCommandRepo(t)
	withWorkingDirectory(t, repo)
	newCmd := newNewCmd()
	newCmd.SetArgs([]string{"shell"})
	if err := newCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}

	original := runSidingUp
	called := false
	runSidingUp = func(_ context.Context, _ state.App, sd state.Siding, _ bool, _ io.Writer) (state.Siding, error) {
		called = true
		return sd, nil
	}
	t.Cleanup(func() { runSidingUp = original })

	upCmd := newUpCmd()
	upCmd.SetArgs([]string{"shell"})
	err := upCmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), ".shunt.app.json") || !strings.Contains(err.Error(), bin()+" app add") {
		t.Fatalf("shell-only up error = %v", err)
	}
	if called {
		t.Fatal("shell-only up reached guest lifecycle")
	}
	app, loadErr := state.LoadApp(configDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if app.Sidings["shell"].MaterializationPhase != state.PhaseWorktree {
		t.Fatalf("shell-only up changed phase to %q", app.Sidings["shell"].MaterializationPhase)
	}
}

func TestUpContinuesForRegisteredState(t *testing.T) {
	repo, configDir, _ := newShellOnlyCommandRepo(t)
	withWorkingDirectory(t, repo)
	newCmd := newNewCmd()
	newCmd.SetArgs([]string{"registered"})
	if err := newCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	app, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	app.Runner = "node"
	app.Start = "npm start"
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}

	original := runSidingUp
	called := false
	runSidingUp = func(_ context.Context, _ state.App, sd state.Siding, bridge bool, _ io.Writer) (state.Siding, error) {
		called = true
		if !bridge {
			t.Fatal("registered up did not request the default bridge")
		}
		return sd, nil
	}
	t.Cleanup(func() { runSidingUp = original })

	upCmd := newUpCmd()
	upCmd.SetArgs([]string{"registered"})
	if err := upCmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("registered up did not reach guest lifecycle")
	}
}
