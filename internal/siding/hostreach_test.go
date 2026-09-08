package siding

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/hostreach"
	"github.com/gordonbeeming/shunt/internal/state"
)

func withHostReachSeams(t *testing.T) (published *[]string, deleted *[]string) {
	t.Helper()
	origResolve, origPrepare := resolveHostReach, hostReachPrepare
	origPut, origDelete, origExec := hostReachPutServer, hostReachDeleteServer, execGuest
	t.Cleanup(func() {
		resolveHostReach, hostReachPrepare = origResolve, origPrepare
		hostReachPutServer, hostReachDeleteServer, execGuest = origPut, origDelete, origExec
	})
	put, del := []string{}, []string{}
	hostReachPrepare = func(context.Context) (*caddy.Admin, error) { return nil, nil }
	hostReachPutServer = func(_ context.Context, _ *caddy.Admin, path string, _ []byte) error {
		put = append(put, path)
		return nil
	}
	hostReachDeleteServer = func(_ context.Context, _ *caddy.Admin, path string) error {
		del = append(del, path)
		return nil
	}
	execGuest = func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "cat" {
			return "127.0.0.1\tlocalhost\n", nil
		}
		return "", nil
	}
	return &put, &del
}

func TestApplyHostReachPublishesOneListenerPerEntry(t *testing.T) {
	published, _ := withHostReachSeams(t)
	resolveHostReach = func(context.Context, []state.HostReach) ([]hostreach.Resolved, error) {
		return []hostreach.Resolved{
			{Name: "kv-dev", Port: 443, Address: "10.0.0.5"},
			{Name: "vm-sql-dev", Port: 1433, Address: "10.0.0.4"},
		}, nil
	}
	app := state.App{Name: "alpha", HostReach: []state.HostReach{{Name: "kv-dev", Port: 443}, {Name: "vm-sql-dev", Port: 1433}}}
	sd := state.Siding{Name: "one", Container: "guest", LastIP: "192.168.64.8"}

	if err := applyHostReach(context.Background(), app, sd); err != nil {
		t.Fatal(err)
	}
	if len(*published) != 2 {
		t.Fatalf("published %d listeners, want one per entry: %v", len(*published), *published)
	}
	for _, path := range *published {
		if !strings.Contains(path, "/layer4/servers/") {
			t.Errorf("listener %q is not a raw TCP server", path)
		}
	}
}

func TestApplyHostReachPublishesNothingWhenAHostNameCannotResolve(t *testing.T) {
	published, _ := withHostReachSeams(t)
	resolveHostReach = func(context.Context, []state.HostReach) ([]hostreach.Resolved, error) {
		return nil, errors.New("the host cannot resolve it")
	}
	app := state.App{Name: "alpha", HostReach: []state.HostReach{{Name: "gone", Port: 443}}}
	sd := state.Siding{Name: "one", Container: "guest", LastIP: "192.168.64.8"}

	if err := applyHostReach(context.Background(), app, sd); err == nil {
		t.Fatal("applyHostReach() = nil error, want the resolution failure surfaced")
	}
	// Resolution happens before anything is published, so a bad name leaves no
	// listener and no hosts entry half-applied.
	if len(*published) != 0 {
		t.Fatalf("published %v despite a resolution failure", *published)
	}
}

func TestApplyHostReachIsANoOpWithoutDeclaredEntries(t *testing.T) {
	published, _ := withHostReachSeams(t)
	resolveHostReach = func(context.Context, []state.HostReach) ([]hostreach.Resolved, error) {
		t.Error("resolved with nothing declared")
		return nil, nil
	}
	if err := applyHostReach(context.Background(), state.App{Name: "alpha"}, state.Siding{Name: "one"}); err != nil {
		t.Fatal(err)
	}
	if len(*published) != 0 {
		t.Fatalf("published %v with nothing declared", *published)
	}
}

func TestRemoveHostReachDropsEveryListenerItCreated(t *testing.T) {
	_, deleted := withHostReachSeams(t)
	app := state.App{Name: "alpha", HostReach: []state.HostReach{
		{Name: "kv-dev", Port: 443}, {Name: "vm-sql-dev", Port: 1433},
	}}
	removeHostReach(context.Background(), app, state.Siding{Name: "one"})
	if len(*deleted) != 2 {
		t.Fatalf("deleted %d listeners, want one per entry: %v", len(*deleted), *deleted)
	}
	// Namespaced by app and siding so a teardown cannot reach another siding's.
	for _, path := range *deleted {
		if !strings.Contains(path, "alpha") || !strings.Contains(path, "one") {
			t.Errorf("listener name %q is not scoped to this app and siding", path)
		}
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
// them directly would pass even if up stopped invoking the relay again, which is
// the exact failure this exists to catch.
func TestUpAppliesHostReachEvenWithoutBridging(t *testing.T) {
	stubUpExternals(t)
	origIP, origApply, origActivate := upGuestIP, upApplyHostReach, upActivate
	t.Cleanup(func() { upGuestIP, upApplyHostReach, upActivate = origIP, origApply, origActivate })

	app := state.App{Name: "alpha", HostReach: []state.HostReach{{Name: "vm-sql-dev", Port: 1433}}}
	for _, bridge := range []bool{false, true} {
		applied, activated := false, false
		upGuestIP = func(context.Context, string) (string, error) { return "192.168.64.8", nil }
		upApplyHostReach = func(_ context.Context, _ state.App, sd state.Siding) error {
			applied = true
			if sd.LastIP == "" {
				t.Error("relay ran before the guest address was resolved")
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
			t.Errorf("bridge=%v: up did not run the relay", bridge)
		}
		if activated != bridge {
			t.Errorf("bridge=%v: bridging ran = %v, want %v", bridge, activated, bridge)
		}
	}
}

// TestUpSurfacesAGuestAddressFailureForHostReach checks the error names its real
// cause. Swallowing it left applyHostReach refusing with "activate the siding
// first", which points at the wrong thing entirely.
func TestUpSurfacesAGuestAddressFailureForHostReach(t *testing.T) {
	stubUpExternals(t)
	origIP := upGuestIP
	t.Cleanup(func() { upGuestIP = origIP })
	upGuestIP = func(context.Context, string) (string, error) {
		return "", errors.New("guest has no network address yet")
	}

	app := state.App{Name: "alpha", HostReach: []state.HostReach{{Name: "vm-sql-dev", Port: 1433}}}
	_, err := up(context.Background(), app, state.Siding{Name: "one", Container: "guest"}, false, io.Discard)
	if err == nil {
		t.Fatal("up() = nil error, want the guest address failure surfaced")
	}
	if !strings.Contains(err.Error(), "no network address") {
		t.Errorf("error = %v, want the underlying cause named", err)
	}
}
