//go:build integration

package caddy

import (
	"context"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

// TestAdminRoundTrip exercises the real Caddy admin API: register a route's
// server, repoint its upstream dial, and read it back. Requires a running shunt
// Caddy (run `shunt init` first); skips otherwise.
func TestAdminRoundTrip(t *testing.T) {
	ctx := context.Background()
	admin := NewAdmin()
	if err := admin.Ping(ctx); err != nil {
		t.Skipf("caddy admin not reachable (%v) — run `shunt init`", err)
	}

	route := state.Route{
		Key:        "itest",
		Kind:       state.KindHTTP,
		ListenPort: 59999, // unlikely to collide
		CaddyID:    "app_itest_http_itest",
	}
	path, body, err := ServerForRoute("itest", route, false)
	if err != nil {
		t.Fatal(err)
	}
	// Clean any prior run, then register the server.
	_ = admin.do(ctx, "DELETE", path, nil)
	if err := admin.Put(ctx, path, body); err != nil {
		t.Fatalf("register server: %v", err)
	}
	t.Cleanup(func() { _ = admin.do(ctx, "DELETE", path, nil) })

	// Repoint the dial and read it back via the @id handle.
	dialPath, dialBody, err := DialPatch(route, "10.0.0.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Patch(ctx, dialPath, dialBody); err != nil {
		t.Fatalf("patch dial: %v", err)
	}
	got, err := admin.GetID(ctx, route.CaddyID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if !strings.Contains(string(got), "10.0.0.1:1234") {
		t.Errorf("expected dial 10.0.0.1:1234 in handler, got: %s", got)
	}
}
