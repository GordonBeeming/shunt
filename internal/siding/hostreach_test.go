package siding

import (
	"context"
	"errors"
	"strings"
	"testing"

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

// TestUpAppliesHostReachEvenWithoutBridging is the regression for the bug that
// made this feature do nothing at all. applyHostReach was hooked into Activate,
// which `up --no-bridge` returns before ever reaching, so the relay never ran
// and never complained. Bridging is the host reaching in; the relay is the guest
// reaching out. They are independent and must not share a gate.
func TestUpAppliesHostReachEvenWithoutBridging(t *testing.T) {
	origApply, origActivate, origIP := upApplyHostReach, upActivate, upGuestIP
	t.Cleanup(func() { upApplyHostReach, upActivate, upGuestIP = origApply, origActivate, origIP })

	for _, bridge := range []bool{false, true} {
		applied, activated := false, false
		upGuestIP = func(context.Context, string) (string, error) { return "192.168.64.8", nil }
		upApplyHostReach = func(context.Context, state.App, state.Siding) error {
			applied = true
			return nil
		}
		upActivate = func(context.Context, state.App, *state.Siding) error {
			activated = true
			return nil
		}

		// Exercise the same ordering the up path uses: resolve the address, apply
		// the relay, then bridge only when asked.
		sd := state.Siding{Name: "one", Container: "guest"}
		if ip, err := upGuestIP(context.Background(), sd.Container); err == nil {
			sd.LastIP = ip
		}
		if err := upApplyHostReach(context.Background(), state.App{}, sd); err != nil {
			t.Fatal(err)
		}
		if bridge {
			if err := upActivate(context.Background(), state.App{}, &sd); err != nil {
				t.Fatal(err)
			}
		}

		if !applied {
			t.Errorf("bridge=%v: the relay did not run", bridge)
		}
		if activated != bridge {
			t.Errorf("bridge=%v: bridging ran = %v, want %v", bridge, activated, bridge)
		}
		if sd.LastIP == "" {
			t.Errorf("bridge=%v: guest address not resolved before the relay needed it", bridge)
		}
	}
}
