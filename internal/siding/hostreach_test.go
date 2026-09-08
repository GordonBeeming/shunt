package siding

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/hostreach"
	"github.com/gordonbeeming/shunt/internal/state"
)

type hostReachCalls struct {
	started int
	stopped int
	probed  int
	scripts []string
}

func withHostReachSeams(t *testing.T) *hostReachCalls {
	t.Helper()
	origResolve, origToken := resolveHostReach, newHostReachToken
	origStart, origStop, origProbe := startHostReachServer, stopHostReachServer, probeHostReach
	origExec := execGuest
	t.Cleanup(func() {
		resolveHostReach, newHostReachToken = origResolve, origToken
		startHostReachServer, stopHostReachServer, probeHostReach = origStart, origStop, origProbe
		execGuest = origExec
	})
	calls := &hostReachCalls{}
	resolveHostReach = func(context.Context, []state.HostReach) ([]hostreach.Resolved, error) {
		return []hostreach.Resolved{{Name: "vm-sql-dev", Port: 1433, Address: "10.0.0.4"}}, nil
	}
	newHostReachToken = func() (string, error) { return "a-token", nil }
	startHostReachServer = func(context.Context, state.App, state.Siding, string, []hostreach.Relay) error {
		calls.started++
		return nil
	}
	stopHostReachServer = func(state.App, state.Siding) { calls.stopped++ }
	probeHostReach = func(context.Context, state.Siding) error {
		calls.probed++
		return nil
	}
	execGuest = func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "cat" {
			return "127.0.0.1\tlocalhost\n", nil
		}
		if len(args) == 3 && args[0] == "sh" && args[1] == "-c" {
			calls.scripts = append(calls.scripts, args[2])
		}
		return "", nil
	}
	return calls
}

func testApp() state.App {
	return state.App{Name: "alpha", HostReach: []state.HostReach{{Name: "vm-sql-dev", Port: 1433}}}
}

func testSiding() state.Siding {
	return state.Siding{Name: "one", Container: "guest", LastIP: "192.168.64.8"}
}

func TestApplyHostReachStartsBothEndsAndProvesTheChain(t *testing.T) {
	calls := withHostReachSeams(t)
	if err := applyHostReach(context.Background(), testApp(), testSiding()); err != nil {
		t.Fatal(err)
	}
	if calls.started != 1 {
		t.Errorf("host end started %d times, want 1", calls.started)
	}
	if calls.probed != 1 {
		t.Errorf("chain probed %d times, want 1", calls.probed)
	}
	if len(calls.scripts) != 1 {
		t.Fatalf("guest configured %d times, want 1", len(calls.scripts))
	}
	// The guest is configured before the host process starts, so the host has a
	// listener to connect to rather than backing off against a closed port.
	if !strings.Contains(calls.scripts[0], hostreach.RelayProgram) {
		t.Error("the guest script does not start the relay")
	}
	if !strings.Contains(calls.scripts[0], "a-token") {
		t.Error("the guest was not given the token, so it would refuse the host")
	}
}

func TestApplyHostReachStopsTheHostEndWhenTheChainDoesNotCarry(t *testing.T) {
	// Leaving the host process running after a failed probe would keep a token
	// and a route to a private endpoint alive with nothing using them.
	calls := withHostReachSeams(t)
	probeHostReach = func(context.Context, state.Siding) error {
		calls.probed++
		return errors.New("nothing came back")
	}
	err := applyHostReach(context.Background(), testApp(), testSiding())
	if err == nil {
		t.Fatal("applyHostReach() = nil error, want the broken chain reported")
	}
	if calls.stopped == 0 {
		t.Error("the host end was left running after a failed probe")
	}
}

func TestApplyHostReachTouchesNothingWhenAHostNameCannotResolve(t *testing.T) {
	calls := withHostReachSeams(t)
	resolveHostReach = func(context.Context, []state.HostReach) ([]hostreach.Resolved, error) {
		return nil, errors.New("the host cannot resolve it")
	}
	if err := applyHostReach(context.Background(), testApp(), testSiding()); err == nil {
		t.Fatal("applyHostReach() = nil error, want the resolution failure surfaced")
	}
	// Resolution happens first, so a bad name leaves no host process and no hosts
	// entry half-applied.
	if calls.started != 0 || len(calls.scripts) != 0 {
		t.Errorf("started %d host processes and wrote %d guest configs despite a resolution failure", calls.started, len(calls.scripts))
	}
}

func TestApplyHostReachIsANoOpWithoutDeclaredEntries(t *testing.T) {
	calls := withHostReachSeams(t)
	resolveHostReach = func(context.Context, []state.HostReach) ([]hostreach.Resolved, error) {
		t.Error("resolved with nothing declared")
		return nil, nil
	}
	if err := applyHostReach(context.Background(), state.App{Name: "alpha"}, testSiding()); err != nil {
		t.Fatal(err)
	}
	if calls.started != 0 {
		t.Error("started the host end with nothing declared")
	}
}

func TestRemoveHostReachStopsTheHostEnd(t *testing.T) {
	calls := withHostReachSeams(t)
	removeHostReach(context.Background(), testApp(), testSiding())
	if calls.stopped != 1 {
		t.Errorf("host end stopped %d times, want 1", calls.stopped)
	}
}

// stubUpExternals neutralises everything up() reaches outside itself, so a test
// can drive its real control flow without a guest. The relay seams are left to
// each test: they are what the tests are about.
func stubUpExternals(t *testing.T) {
	t.Helper()
	origRuntime, origMat, origLive := ensureGuestRuntime, upMaterialize, upEnsureGuestLive
	origFD, origProbe, origExec := upResolveFrontDoor, upProbeAppRunning, execGuest
	origPrep, origStop, origStart, origWait := upPrepareGuest, upStopApp, upStartApp, upWaitReady
	t.Cleanup(func() {
		ensureGuestRuntime, upMaterialize, upEnsureGuestLive = origRuntime, origMat, origLive
		upResolveFrontDoor, upProbeAppRunning, execGuest = origFD, origProbe, origExec
		upPrepareGuest, upStopApp, upStartApp, upWaitReady = origPrep, origStop, origStart, origWait
	})
	ensureGuestRuntime = func(context.Context) error { return nil }
	upMaterialize = func(_ context.Context, _ state.App, sd state.Siding, _ io.Writer) (state.Siding, error) {
		return sd, nil
	}
	upEnsureGuestLive = func(context.Context, state.Siding) error { return nil }
	upResolveFrontDoor = func(state.App, state.Siding) ([]state.Route, error) { return nil, nil }
	upProbeAppRunning = func(context.Context, state.App, state.Siding) (bool, error) { return false, nil }
	execGuest = func(context.Context, string, ...string) (string, error) { return "", nil }
	upPrepareGuest = func(context.Context, state.App, state.Siding) error { return nil }
	upStopApp = func(context.Context, state.App, state.Siding) error { return nil }
	upStartApp = func(context.Context, state.App, state.Siding) error { return nil }
	upWaitReady = func(context.Context, state.App, state.Siding, time.Duration) error { return nil }
}

// TestUpAppliesHostReachEvenWithoutBridging is the regression for the bug that
// made this feature do nothing at all: applyHostReach was hooked into Activate,
// which `up --no-bridge` returns before ever reaching, so the relay never ran
// and never complained.
//
// It drives the real up() rather than calling the seams in sequence. Calling
// them directly would pass even if up stopped invoking host-reach again, which
// is the exact failure this exists to catch.
func TestUpAppliesHostReachEvenWithoutBridging(t *testing.T) {
	stubUpExternals(t)
	origIP, origApply, origActivate := upGuestIP, upApplyHostReach, upActivate
	t.Cleanup(func() { upGuestIP, upApplyHostReach, upActivate = origIP, origApply, origActivate })

	app := testApp()
	for _, bridge := range []bool{false, true} {
		applied, activated := false, false
		upGuestIP = func(context.Context, string) (string, error) { return "192.168.64.8", nil }
		upApplyHostReach = func(_ context.Context, _ state.App, sd state.Siding) error {
			applied = true
			if sd.LastIP == "" {
				t.Error("host-reach ran before the guest address was resolved")
			}
			return nil
		}
		upActivate = func(context.Context, state.App, *state.Siding) error {
			activated = true
			return nil
		}

		if _, err := up(context.Background(), app, state.Siding{Name: "one", Container: "guest"}, bridge, io.Discard); err != nil {
			t.Fatalf("bridge=%v: up() = %v", bridge, err)
		}
		if !applied {
			t.Errorf("bridge=%v: up did not apply host-reach", bridge)
		}
		if activated != bridge {
			t.Errorf("bridge=%v: bridging ran = %v, want %v", bridge, activated, bridge)
		}
	}
}

// TestUpSurfacesAGuestAddressFailureForHostReach checks the error names its real
// cause. Swallowing it left applyHostReach refusing with "start the siding
// first", which points at the wrong thing entirely.
func TestUpSurfacesAGuestAddressFailureForHostReach(t *testing.T) {
	stubUpExternals(t)
	origIP := upGuestIP
	t.Cleanup(func() { upGuestIP = origIP })
	upGuestIP = func(context.Context, string) (string, error) {
		return "", errors.New("guest has no network address yet")
	}

	_, err := up(context.Background(), testApp(), state.Siding{Name: "one", Container: "guest"}, false, io.Discard)
	if err == nil {
		t.Fatal("up() = nil error, want the guest address failure surfaced")
	}
	if !strings.Contains(err.Error(), "no network address") {
		t.Errorf("error = %v, want the underlying cause named", err)
	}
}
