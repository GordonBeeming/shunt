package siding

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/caddy"
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

// TestCreateMissingRoutesLeavesAnExistingServerAlone is the guard that protects
// a live route's upstream: EnsureFrontDoor is delete-then-put, so touching a
// route whose server already exists would reset it to the placeholder dial —
// a brief outage that also clobbers PointCaddy's rollback capture. The handler
// records every request, so a stray PUT or DELETE against an existing server
// fails the test even if createMissingRoutes still returns success.
func TestCreateMissingRoutesLeavesAnExistingServerAlone(t *testing.T) {
	route := state.Route{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "app_x_http_web"}
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet && r.URL.Path == "/id/"+route.CaddyID {
			_, _ = io.WriteString(w, `{"upstreams":[{"dial":"placeholder:0"}]}`)
			return
		}
		t.Fatalf("unexpected request to an already-existing route: %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(server.Close)
	admin := caddy.NewAdminAt(server.URL)

	if err := createMissingRoutes(context.Background(), admin, state.App{Name: "x"}, []state.Route{route}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0] != "GET /id/"+route.CaddyID {
		t.Fatalf("requests = %v, want exactly the one @id lookup and nothing else", requests)
	}
}

// TestCreateMissingRoutesCreatesAMissingServer covers the other half of the
// guard: a route whose @id lookup 404s (e.g. after `app switch --release`
// deleted it) must actually be recreated, via a PUT to its server path.
func TestCreateMissingRoutesCreatesAMissingServer(t *testing.T) {
	route := state.Route{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "app_x_http_web"}
	wantPath := "/config/apps/http/servers/srv_x_web"
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.Method {
		case http.MethodGet:
			http.NotFound(w, r) // no server registered for this @id yet
		case http.MethodDelete:
			http.NotFound(w, r) // EnsureFrontDoor's delete-then-put; nothing to clear
		case http.MethodPut:
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	admin := caddy.NewAdminAt(server.URL)

	if err := createMissingRoutes(context.Background(), admin, state.App{Name: "x"}, []state.Route{route}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(requests, "PUT "+wantPath) {
		t.Fatalf("requests = %v, want a PUT to %s", requests, wantPath)
	}
}

// TestCreateMissingRoutesInAMixedSetOnlyTouchesTheMissingOne exercises both
// guards in a single call, matching how switchLocked invokes it against the
// app's whole front door: one route stays untouched, the other gets created.
func TestCreateMissingRoutesInAMixedSetOnlyTouchesTheMissingOne(t *testing.T) {
	existing := state.Route{Key: "web", Kind: state.KindHTTP, ListenPort: 4100, CaddyID: "app_x_http_web"}
	missing := state.Route{Key: "api", Kind: state.KindHTTP, ListenPort: 4101, CaddyID: "app_x_http_api"}
	existingServerPath := "/config/apps/http/servers/srv_x_web"
	missingServerPath := "/config/apps/http/servers/srv_x_api"

	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/id/"+existing.CaddyID:
			_, _ = io.WriteString(w, `{"upstreams":[{"dial":"placeholder:0"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/id/"+missing.CaddyID:
			http.NotFound(w, r)
		case r.Method == http.MethodDelete:
			http.NotFound(w, r)
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	admin := caddy.NewAdminAt(server.URL)

	if err := createMissingRoutes(context.Background(), admin, state.App{Name: "x"}, []state.Route{existing, missing}); err != nil {
		t.Fatal(err)
	}
	for _, req := range requests {
		if strings.Contains(req, existingServerPath) {
			t.Fatalf("the existing route's server was touched: %v", requests)
		}
	}
	if !slices.Contains(requests, "PUT "+missingServerPath) {
		t.Fatalf("requests = %v, want a PUT to the missing route's server %s", requests, missingServerPath)
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
