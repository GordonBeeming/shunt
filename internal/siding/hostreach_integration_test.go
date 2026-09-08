//go:build integration

package siding

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/caddy"
)

// TestProbeBridgeRemovesItsListener checks that the reachability check takes its
// own listener back down, whatever verdict it reaches.
//
// The verdict itself is a property of the machine rather than of the code: on a
// host with the firewall on it is a failure, and on one where shunt is allowed
// it is a success. Both are correct, so the test asserts the part that is always
// true. A listener left behind holds a bridge port, which is the exposure this
// design keeps narrow.
func TestProbeBridgeRemovesItsListener(t *testing.T) {
	if os.Getenv("SHUNT_CONTAINER_INTEGRATION") == "" {
		t.Skip("needs a guest; set SHUNT_CONTAINER_INTEGRATION=1")
	}
	container := os.Getenv("SHUNT_PROBE_GUEST")
	if container == "" {
		t.Skip("set SHUNT_PROBE_GUEST to a running siding guest")
	}
	ctx := context.Background()
	admin := caddy.NewAdmin()
	if err := admin.Ping(ctx); err != nil {
		t.Skipf("caddy admin not reachable (%v) — run `shunt init`", err)
	}

	const app, siding = "itest", "probe"
	path := "/config/apps/layer4/servers/" + caddy.HostReachProbeServerName(app, siding)
	t.Cleanup(func() { _ = admin.DeleteIfExists(context.Background(), path) })

	t.Logf("probe verdict: %v", probeBridgeReachable(ctx, admin, app, siding, container, "192.168.64.1"))

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
