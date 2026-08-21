package siding

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/runner"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestProbeRoutesParsesPerPortOutputInDeclarationOrder(t *testing.T) {
	restore := probeExec
	defer func() { probeExec = restore }()
	probeExec = func(context.Context, string, ...string) (string, error) {
		return "7260 up\n5173 down\n", nil
	}
	app := state.App{Runner: runner.Custom, FrontDoor: []state.Route{
		{Key: "webapp", GuestPort: 5173, Optional: true},
		{Key: "api", GuestPort: 7260},
	}}
	got, err := ProbeRoutes(context.Background(), app, state.Siding{Container: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	want := []RouteState{
		{Key: "webapp", GuestPort: 5173, Optional: true, Listening: false},
		{Key: "api", GuestPort: 7260, Listening: true},
	}
	if len(got) != len(want) {
		t.Fatalf("ProbeRoutes() = %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("route[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestProbeRoutesErrorsWhenAPortIsMissingFromTheOutput(t *testing.T) {
	restore := probeExec
	defer func() { probeExec = restore }()
	probeExec = func(context.Context, string, ...string) (string, error) {
		return "7260 up\n", nil // 5173 never reported back
	}
	app := state.App{Runner: runner.Custom, FrontDoor: []state.Route{
		{Key: "api", GuestPort: 7260},
		{Key: "webapp", GuestPort: 5173},
	}}
	_, err := ProbeRoutes(context.Background(), app, state.Siding{Name: "alpha", Container: "guest"})
	if err == nil || !strings.Contains(err.Error(), "5173") {
		t.Fatalf("ProbeRoutes() error = %v, want a missing-port error naming 5173", err)
	}
}

func TestProbeRoutesReportsAContractGapWithoutProbingIt(t *testing.T) {
	restore := probeExec
	defer func() { probeExec = restore }()
	var probed string
	probeExec = func(_ context.Context, _ string, args ...string) (string, error) {
		probed = args[len(args)-1]
		return "7260 up\n", nil
	}
	app := state.App{Runner: runner.Custom, FrontDoor: []state.Route{
		{Key: "api", GuestPort: 7260},
		{Key: "legacy", GuestPort: 0},
	}}
	got, err := ProbeRoutes(context.Background(), app, state.Siding{Container: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(probed, "legacy") {
		t.Fatalf("route with no guest port was probed: %s", probed)
	}
	if len(got) != 2 || got[1].Key != "legacy" || got[1].GuestPort != 0 || got[1].Listening {
		t.Fatalf("unprobeable route = %#v", got)
	}
}

func TestProbeRoutesReturnsNilForNoDeclaredRoutes(t *testing.T) {
	restore := probeExec
	defer func() { probeExec = restore }()
	called := false
	probeExec = func(context.Context, string, ...string) (string, error) {
		called = true
		return "", nil
	}
	got, err := ProbeRoutes(context.Background(), state.App{Runner: runner.Custom}, state.Siding{Container: "guest"})
	if err != nil || got != nil {
		t.Fatalf("ProbeRoutes() = %#v, %v, want nil, nil", got, err)
	}
	if called {
		t.Fatal("no declared routes should not need a probe exec")
	}
}

func TestProbeAppRunningReturnsTrueWhenNoRoutesAreDeclared(t *testing.T) {
	restore := probeExec
	defer func() { probeExec = restore }()
	probeExec = func(context.Context, string, ...string) (string, error) {
		t.Fatal("no declared routes should not need a probe exec")
		return "", nil
	}
	got, err := ProbeAppRunning(context.Background(), state.App{Runner: runner.Custom}, state.Siding{Container: "guest"})
	if err != nil || !got {
		t.Fatalf("ProbeAppRunning() = %v, %v, want true, nil", got, err)
	}
}

// A probe stall (the guest's exec path wedged, per ExecBounded) is distinct
// evidence from an ordinary probe failure — ProbeRoutes must hand it back
// unwrapped-enough for errors.As to still find it, not fold it into a generic
// "probe routes for" error a caller can't tell apart from any other failure.
func TestProbeRoutesPropagatesAStallWithoutMaskingIt(t *testing.T) {
	restore := probeExec
	defer func() { probeExec = restore }()
	stall := &container.ExecStalledError{Guest: "guest", Timeout: time.Second}
	probeExec = func(context.Context, string, ...string) (string, error) {
		return "", stall
	}
	app := state.App{Runner: runner.Custom, FrontDoor: []state.Route{{Key: "api", GuestPort: 7260}}}
	_, err := ProbeRoutes(context.Background(), app, state.Siding{Container: "guest"})
	var stalled *container.ExecStalledError
	if !errors.As(err, &stalled) || stalled != stall {
		t.Fatalf("ProbeRoutes() error = %v, want the stall propagated", err)
	}
}
