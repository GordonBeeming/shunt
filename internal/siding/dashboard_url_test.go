package siding

import (
	"testing"

	"github.com/gordonbeeming/shunt/internal/runner"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestDashboardURLIsRunnerAware(t *testing.T) {
	sd := state.Siding{LastIP: "10.0.0.2"}
	if got := DashboardURL(state.App{Runner: runner.Custom}, sd); got != "" {
		t.Fatalf("custom runner dashboard = %q", got)
	}
	if got := DashboardURL(state.App{Runner: runner.Aspire}, sd); got != "http://10.0.0.2:18888" {
		t.Fatalf("Aspire dashboard = %q", got)
	}
	app := state.App{Runner: runner.Custom, FrontDoor: []state.Route{{Key: "dashboard", Kind: state.KindHTTP, GuestPort: 4321}}}
	if got := DashboardURL(app, sd); got != "http://10.0.0.2:4321" {
		t.Fatalf("explicit custom dashboard = %q", got)
	}
}

func TestDashboardGuestPortUsesSidingContract(t *testing.T) {
	app := state.App{
		Runner:    runner.Aspire,
		FrontDoor: []state.Route{{Key: "aspire-dashboard", Kind: state.KindHTTP, GuestPort: 15072}},
	}
	if got := dashboardGuestPort(app, state.Siding{}); got != 15072 {
		t.Fatalf("app dashboard port = %d", got)
	}
	sd := state.Siding{FrontDoor: []state.Route{{Key: "aspire-dashboard", Kind: state.KindHTTP, GuestPort: 16072}}}
	if got := dashboardGuestPort(app, sd); got != 16072 {
		t.Fatalf("siding dashboard port = %d", got)
	}
}
