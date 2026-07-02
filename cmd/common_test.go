package cmd

import (
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestSidingStatus(t *testing.T) {
	app := state.App{LiveSiding: "liveone"}
	bridged := map[string]int{"web": 5000}

	cases := []struct {
		desc  string
		name  string
		sd    state.Siding
		guest string
		want  string
	}{
		{"live wins over running+bridged", "liveone", state.Siding{Bridges: bridged}, "running", "live"},
		{"stopped even if bridged", "b", state.Siding{Stopped: true, Bridges: bridged}, "running", "stopped"},
		{"up when running and bridged", "b", state.Siding{Bridges: bridged}, "running", "up"},
		{"idle when running but unbridged", "b", state.Siding{}, "running", "idle"},
		{"idle when bridged but guest not running", "b", state.Siding{Bridges: bridged}, "stopped", "idle"},
		{"idle when guest state unknown", "b", state.Siding{}, "", "idle"},
	}
	for _, c := range cases {
		if got := sidingStatus(app, c.name, c.sd, c.guest); got != c.want {
			t.Errorf("%s: sidingStatus = %q, want %q", c.desc, got, c.want)
		}
	}
}
