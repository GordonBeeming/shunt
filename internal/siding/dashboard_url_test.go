package siding

import (
	"testing"

	"github.com/gordonbeeming/shunt/internal/runner"
	"github.com/gordonbeeming/shunt/internal/state"
)

// The dashboard is reachable because the contract says where it is, exactly like
// every other route. There is no runner that gets a port for free.
func TestDashboardURLComesFromTheContract(t *testing.T) {
	sd := state.Siding{LastIP: "10.0.0.2"}
	for _, r := range []string{runner.Custom, runner.Aspire} {
		if got := DashboardURL(state.App{Runner: r}, sd); got != "" {
			t.Fatalf("%s runner with no declared dashboard route = %q, want no URL", r, got)
		}
	}
	app := state.App{Runner: runner.Custom, FrontDoor: []state.Route{{Key: "dashboard", Kind: state.KindHTTP, GuestPort: 4321}}}
	if got := DashboardURL(app, sd); got != "http://10.0.0.2:4321" {
		t.Fatalf("declared dashboard = %q", got)
	}
	// A dashboard served over TLS has to be advertised as https, or the link
	// points at a port that refuses a plaintext request.
	secure := state.App{Runner: runner.Aspire, FrontDoor: []state.Route{{Key: "aspire-dashboard", Kind: state.KindHTTP, GuestPort: 17162, TLS: true}}}
	if got := DashboardURL(secure, sd); got != "https://10.0.0.2:17162" {
		t.Fatalf("tls dashboard = %q", got)
	}
}

func TestDashboardGuestPortUsesSidingContract(t *testing.T) {
	app := state.App{
		Runner:    runner.Aspire,
		FrontDoor: []state.Route{{Key: "aspire-dashboard", Kind: state.KindHTTP, GuestPort: 15072}},
	}
	if got, _ := dashboardGuestPort(app, state.Siding{}); got != 15072 {
		t.Fatalf("app dashboard port = %d", got)
	}
	sd := state.Siding{FrontDoor: []state.Route{{Key: "aspire-dashboard", Kind: state.KindHTTP, GuestPort: 16072}}}
	if got, _ := dashboardGuestPort(app, sd); got != 16072 {
		t.Fatalf("siding dashboard port = %d", got)
	}
}
