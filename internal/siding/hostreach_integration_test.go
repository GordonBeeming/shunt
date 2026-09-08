//go:build integration

package siding

import (
	"context"
	"strings"
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
	const app, siding = "itest", "probe"
	path := "/config/apps/layer4/servers/" + caddy.HostReachProbeServerName(app, siding)
	t.Cleanup(func() { _ = admin.DeleteIfExists(context.Background(), path) })

	if err := probeBridgeReachable(ctx, admin, app, siding, "127.0.0.1"); err != nil {
		t.Fatalf("probe failed on loopback, where nothing blocks it: %v", err)
	}
	// The probe listener is removed whether the check passes or fails, so a run
	// cannot leave a bridge port held by a check nobody is making any more.
	//
	// Absence is read from the body, not from a status. Caddy answers a missing
	// config key with 200 and "null", and the probe server sets no "@id" for an
	// id lookup to miss on, so both of the obvious checks here report absent
	// whatever the truth is.
	body, err := admin.Get(ctx, path)
	if err != nil {
		t.Fatalf("read the probe listener's config path: %v", err)
	}
	if got := strings.TrimSpace(string(body)); got != "null" {
		t.Errorf("probe listener still present after the check: %s", got)
	}
}
