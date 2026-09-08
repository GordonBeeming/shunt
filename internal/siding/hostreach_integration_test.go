//go:build integration

package siding

import (
	"context"
	"testing"

	"github.com/gordonbeeming/shunt/internal/caddy"
)

// TestProbeBridgeDetectsWhetherAListenerCarriesBytes runs the reachability check
// against a real Caddy on loopback, where nothing blocks it.
//
// It guards the mistake the check exists to avoid making itself: an earlier
// version only dialled, and a dial succeeds even when the listener is blocked,
// because the kernel completes the handshake and the program never accepts. A
// check that cannot pass here cannot be trusted to fail anywhere else.
func TestProbeBridgeDetectsWhetherAListenerCarriesBytes(t *testing.T) {
	ctx := context.Background()
	admin := caddy.NewAdmin()
	if err := admin.Ping(ctx); err != nil {
		t.Skipf("caddy admin not reachable (%v) — run `shunt init`", err)
	}
	if err := probeBridgeReachable(ctx, admin, "127.0.0.1"); err != nil {
		t.Fatalf("probe failed on loopback, where nothing blocks it: %v", err)
	}
	// The probe listener is removed whether the check passes or fails, so a run
	// cannot leave a bridge port held by a check nobody is making any more.
	if _, err := admin.GetID(ctx, caddy.HostReachProbeServerName()); !caddy.IsNotFound(err) {
		t.Errorf("probe listener still present after the check: %v", err)
	}
}
