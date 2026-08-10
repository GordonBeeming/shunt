package siding

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestStopPersistsForcedGuestState(t *testing.T) {
	originalStop := stopOrForce
	defer func() { stopOrForce = originalStop }()
	stopOrForce = func(context.Context, string) (bool, error) { return true, nil }
	configDir := t.TempDir()
	app := state.App{
		ConfigDir:  configDir,
		LiveSiding: "alpha",
		Volumes:    []string{"db"},
		Sidings: map[string]state.Siding{
			"alpha": {Name: "alpha", Container: "guest", MaterializationPhase: state.PhaseGuest, LastIP: "10.0.0.1", Bridges: map[string]int{"web": 5000}},
		},
	}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	result, err := Stop(context.Background(), app, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Forced || !result.WasLive || !result.Siding.Stopped || result.Siding.MaterializationPhase != state.PhaseData || result.Siding.LastIP != "" || result.Siding.Bridges != nil {
		t.Fatalf("Stop() result = %#v", result)
	}
	loaded, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Sidings["alpha"].Stopped || loaded.Sidings["alpha"].MaterializationPhase != state.PhaseData || loaded.Sidings["alpha"].LastIP != "" {
		t.Fatalf("saved stop state = %#v", loaded.Sidings["alpha"])
	}
}

func TestStopClearsRoutingStateAfterGracefulStop(t *testing.T) {
	originalStop := stopOrForce
	defer func() { stopOrForce = originalStop }()
	stopOrForce = func(context.Context, string) (bool, error) { return false, nil }
	configDir := t.TempDir()
	app := state.App{
		ConfigDir: configDir,
		Sidings: map[string]state.Siding{
			"alpha": {Name: "alpha", Container: "guest", LastIP: "10.0.0.1", Bridges: map[string]int{"web": 5000}},
		},
	}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	result, err := Stop(context.Background(), app, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if result.Forced || !result.Siding.Stopped || result.Siding.LastIP != "" || result.Siding.Bridges != nil {
		t.Fatalf("Stop() result = %#v", result)
	}
	loaded, err := state.LoadApp(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sidings["alpha"].LastIP != "" || loaded.Sidings["alpha"].Bridges != nil {
		t.Fatalf("saved stop state = %#v", loaded.Sidings["alpha"])
	}
}

func TestStopSurfacesStateMergeFailure(t *testing.T) {
	originalStop, originalMerge := stopOrForce, mergeStoppedState
	defer func() {
		stopOrForce, mergeStoppedState = originalStop, originalMerge
	}()
	stopOrForce = func(context.Context, string) (bool, error) { return false, nil }
	mergeStoppedState = func(context.Context, string, state.Siding, bool) (state.App, error) {
		return state.App{}, errors.New("injected save failure")
	}
	configDir := t.TempDir()
	app := state.App{ConfigDir: configDir, Sidings: map[string]state.Siding{
		"alpha": {Name: "alpha", Container: "guest"},
	}}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	_, err := Stop(context.Background(), app, "alpha")
	if err == nil || !strings.Contains(err.Error(), "guest stopped, but siding state could not be saved") {
		t.Fatalf("Stop() error = %v", err)
	}
}
