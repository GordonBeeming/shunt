package siding

import (
	"testing"

	"github.com/gordonbeeming/shunt/internal/contract"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestEffRoutesFallsBackToApp(t *testing.T) {
	app := state.App{FrontDoor: []state.Route{{Key: "a", Kind: state.KindHTTP, ListenPort: 1}}}
	// No per-siding set → the app-level routes are used.
	if got := EffRoutes(app, state.Siding{}); len(got) != 1 || got[0].Key != "a" {
		t.Fatalf("expected app-level fallback, got %+v", got)
	}
	// A per-siding set wins (the guest runs the siding's code).
	sd := state.Siding{FrontDoor: []state.Route{
		{Key: "b", Kind: state.KindHTTP, ListenPort: 2},
		{Key: "c", Kind: state.KindHTTP, ListenPort: 3},
	}}
	if got := EffRoutes(app, sd); len(got) != 2 || got[1].Key != "c" {
		t.Fatalf("expected the siding front door, got %+v", got)
	}
}

func TestExtraRoutesOnlyReturnsSidingAdditions(t *testing.T) {
	app := state.App{FrontDoor: []state.Route{{Key: "a", Kind: state.KindHTTP}}}
	sd := state.Siding{FrontDoor: []state.Route{
		{Key: "a", Kind: state.KindHTTP},                    // shared with the app
		{Key: "gw", Kind: state.KindHTTP, ListenPort: 7250}, // only in the siding
	}}
	extra := extraRoutes(app, sd)
	if len(extra) != 1 || extra[0].Key != "gw" || extra[0].ListenPort != 7250 {
		t.Fatalf("expected only the siding-added gw route, got %+v", extra)
	}
}

func TestRouteFromContractMapping(t *testing.T) {
	r := contract.FrontDoorRoute{
		Key: "gw", Kind: state.KindHTTP, ListenPort: 7250,
		Resource: "gw", Endpoint: "https", GuestPort: 7250, TLS: true,
	}
	got := RouteFromContract("myapp", r, 7250)
	if got.Key != "gw" || got.ListenPort != 7250 || got.GuestPort != 7250 || !got.TLS || got.CaddyID == "" {
		t.Fatalf("unexpected route mapping: %+v", got)
	}
}
