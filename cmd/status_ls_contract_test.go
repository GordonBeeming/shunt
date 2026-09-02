package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestRemovalHealthIncludesResumeGuidance(t *testing.T) {
	removal := makeRemovalHealth(&state.RemovalOperation{
		ID:           "remove-one",
		Siding:       "one",
		Stage:        state.RemovalBaselinePromoted,
		GenerationID: "generation-1",
		StartedAt:    time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
	})
	if removal == nil || removal.Resume != bin()+" rm one" || removal.Age == "unknown" || removal.Generation != "generation-1" {
		t.Fatalf("removal health = %+v", removal)
	}
}

func TestLsCompatibilityStatusPreservesPreChangeSemantics(t *testing.T) {
	tests := []struct {
		name            string
		app             state.App
		siding          state.Siding
		system          container.RuntimeObservation
		guest           container.GuestObservation
		wantRuntime     string
		wantLegacyGuest string
		wantStatus      string
	}{
		{
			name:            "fresh worktree is idle",
			siding:          state.Siding{Name: "one", MaterializationPhase: state.PhaseWorktree},
			wantRuntime:     "missing",
			wantLegacyGuest: "stopped",
			wantStatus:      "idle",
		},
		{
			name:            "parked siding is idle",
			siding:          state.Siding{Name: "one", MaterializationPhase: state.PhaseParked},
			wantRuntime:     "missing",
			wantLegacyGuest: "stopped",
			wantStatus:      "idle",
		},
		{
			name:            "explicitly killed siding is stopped",
			siding:          state.Siding{Name: "one", MaterializationPhase: state.PhaseGuest, Stopped: true},
			system:          container.RuntimeObservation{State: container.RuntimeRunning},
			guest:           container.GuestObservation{State: container.GuestStopped},
			wantRuntime:     "stopped",
			wantLegacyGuest: "stopped",
			wantStatus:      "stopped",
		},
		{
			name:            "running unbridged siding is idle",
			siding:          state.Siding{Name: "one", MaterializationPhase: state.PhaseGuest},
			system:          container.RuntimeObservation{State: container.RuntimeRunning},
			guest:           container.GuestObservation{State: container.GuestRunning},
			wantRuntime:     "running",
			wantLegacyGuest: "running",
			wantStatus:      "idle",
		},
		{
			name:            "running bridged siding is up",
			siding:          state.Siding{Name: "one", MaterializationPhase: state.PhaseGuest, Bridges: map[string]int{"web": 5000}},
			system:          container.RuntimeObservation{State: container.RuntimeRunning},
			guest:           container.GuestObservation{State: container.GuestRunning},
			wantRuntime:     "running",
			wantLegacyGuest: "running",
			wantStatus:      "up",
		},
		{
			name:            "explicitly stopped siding stays stopped with stale running bridges",
			siding:          state.Siding{Name: "one", MaterializationPhase: state.PhaseGuest, Stopped: true, Bridges: map[string]int{"web": 5000}},
			system:          container.RuntimeObservation{State: container.RuntimeRunning},
			guest:           container.GuestObservation{State: container.GuestRunning},
			wantRuntime:     "running",
			wantLegacyGuest: "running",
			wantStatus:      "stopped",
		},
		{
			name:            "live siding remains live",
			app:             state.App{LiveSiding: "one"},
			siding:          state.Siding{Name: "one", MaterializationPhase: state.PhaseGuest, Bridges: map[string]int{"web": 5000}},
			system:          container.RuntimeObservation{State: container.RuntimeRunning},
			guest:           container.GuestObservation{State: container.GuestRunning},
			wantRuntime:     "running",
			wantLegacyGuest: "running",
			wantStatus:      "live",
		},
		{
			name:            "released front door still reports the deprecated-compatible live status",
			app:             state.App{LiveSiding: "one", FrontDoorReleased: true},
			siding:          state.Siding{Name: "one", MaterializationPhase: state.PhaseGuest, Bridges: map[string]int{"web": 5000}},
			system:          container.RuntimeObservation{State: container.RuntimeRunning},
			guest:           container.GuestObservation{State: container.GuestRunning},
			wantRuntime:     "running",
			wantLegacyGuest: "running",
			wantStatus:      "live",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			phase := effectiveLsPhase(test.siding)
			runtime, _, legacyGuest := classifyLsRuntime(phase, test.system, test.guest)
			if runtime != test.wantRuntime || legacyGuest != test.wantLegacyGuest {
				t.Fatalf("runtime/guest = %q/%q, want %q/%q", runtime, legacyGuest, test.wantRuntime, test.wantLegacyGuest)
			}
			if got := compatibilityLsStatus(test.app, test.siding.Name, test.siding, legacyGuest); got != test.wantStatus {
				t.Fatalf("status = %q, want %q", got, test.wantStatus)
			}
		})
	}
	if lsSchemaVersion != 2 {
		t.Fatalf("schema version = %d", lsSchemaVersion)
	}
}

func TestLsRuntimeContractUsesOnlyBoundedTypedStates(t *testing.T) {
	tests := []struct {
		name        string
		phase       state.MaterializationPhase
		system      container.RuntimeObservation
		guest       container.GuestObservation
		wantRuntime string
		wantGuest   string
	}{
		{name: "worktree", phase: state.PhaseWorktree, wantRuntime: "missing", wantGuest: "stopped"},
		{name: "system stopped", phase: state.PhaseGuest, system: container.RuntimeObservation{State: container.RuntimeStopped}, wantRuntime: "stopped", wantGuest: "stopped"},
		{name: "system unavailable", phase: state.PhaseGuest, system: container.RuntimeObservation{State: container.RuntimeUnavailable}, wantRuntime: "runtime-unavailable", wantGuest: "stopped"},
		{name: "guest running", phase: state.PhaseGuest, system: container.RuntimeObservation{State: container.RuntimeRunning}, guest: container.GuestObservation{State: container.GuestRunning}, wantRuntime: "running", wantGuest: "running"},
		{name: "guest stopped", phase: state.PhaseGuest, system: container.RuntimeObservation{State: container.RuntimeRunning}, guest: container.GuestObservation{State: container.GuestStopped}, wantRuntime: "stopped", wantGuest: "stopped"},
		{name: "guest absent", phase: state.PhaseGuest, system: container.RuntimeObservation{State: container.RuntimeRunning}, guest: container.GuestObservation{State: container.GuestAbsent}, wantRuntime: "missing", wantGuest: "stopped"},
		{name: "guest unavailable", phase: state.PhaseGuest, system: container.RuntimeObservation{State: container.RuntimeRunning}, guest: container.GuestObservation{State: container.GuestUnavailable}, wantRuntime: "runtime-unavailable", wantGuest: "stopped"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime, _, guest := classifyLsRuntime(test.phase, test.system, test.guest)
			if runtime != test.wantRuntime || guest != test.wantGuest {
				t.Fatalf("runtime/guest = %q/%q, want %q/%q", runtime, guest, test.wantRuntime, test.wantGuest)
			}
		})
	}
}

func TestStatusReportsEveryAppRemovalInJSONAndText(t *testing.T) {
	first := makeRemovalHealth(&state.RemovalOperation{ID: "remove-one", Siding: "one", Stage: state.RemovalStarted, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	second := makeRemovalHealth(&state.RemovalOperation{ID: "remove-two", Siding: "two", Stage: state.RemovalGuestRemoved, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	health := healthView{Apps: []appHealth{{Name: "first", Removal: first}, {Name: "second", Removal: second}}}
	encoded, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(string(encoded), `"name":"first"`, `"id":"remove-one"`, `"name":"second"`, `"id":"remove-two"`) {
		t.Fatalf("status JSON = %s", encoded)
	}
	text := statusText(health)
	if !containsAll(text, "first", "remove-one", bin()+" rm one", "second", "remove-two", bin()+" rm two") {
		t.Fatalf("status text = %q", text)
	}
}

func TestLsJSONRemainsTopLevelArrayWithAdditiveSchemaVersion(t *testing.T) {
	encoded, err := json.Marshal([]lsApp{{SchemaVersion: lsSchemaVersion, Name: "app", Sidings: []lsSiding{}}})
	if err != nil {
		t.Fatal(err)
	}
	var legacy []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(encoded, &legacy); err != nil || len(legacy) != 1 || legacy[0].Name != "app" {
		t.Fatalf("legacy array decoder: apps=%+v err=%v", legacy, err)
	}
	var empty []struct{}
	if err := json.Unmarshal([]byte(`[]`), &empty); err != nil || len(empty) != 0 {
		t.Fatalf("empty legacy array decoder: rows=%+v err=%v", empty, err)
	}
}

func TestLsTableStatusShowsReleasedOnlyForTheLiveSidingOfAReleasedApp(t *testing.T) {
	released := lsApp{FrontDoorReleased: true, Sidings: []lsSiding{
		{Name: "one", Live: true, Status: "live"},
		{Name: "two", Live: false, Status: "idle"},
	}}
	if got := lsTableStatus(released, released.Sidings[0]); got != "released" {
		t.Fatalf("live siding of a released app = %q, want released", got)
	}
	if got := lsTableStatus(released, released.Sidings[1]); got != "idle" {
		t.Fatalf("non-live siding of a released app = %q, want unchanged idle", got)
	}

	notReleased := lsApp{Sidings: []lsSiding{{Name: "one", Live: true, Status: "live"}}}
	if got := lsTableStatus(notReleased, notReleased.Sidings[0]); got != "live" {
		t.Fatalf("live siding of a non-released app = %q, want unchanged live", got)
	}
}

func TestLsJSONIncludesFrontDoorReleasedAdditively(t *testing.T) {
	encoded, err := json.Marshal([]lsApp{{SchemaVersion: lsSchemaVersion, Name: "app", FrontDoorReleased: true, Sidings: []lsSiding{}}})
	if err != nil {
		t.Fatal(err)
	}
	var current []struct {
		FrontDoorReleased bool `json:"frontDoorReleased"`
	}
	if err := json.Unmarshal(encoded, &current); err != nil || len(current) != 1 || !current[0].FrontDoorReleased {
		t.Fatalf("ls JSON = %s, want frontDoorReleased true: %+v (err %v)", encoded, current, err)
	}

	// A pre-flag consumer that only knows the schemaVersion-2 shape must still
	// decode the array without error.
	var legacy []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(encoded, &legacy); err != nil || len(legacy) != 1 || legacy[0].Name != "app" {
		t.Fatalf("legacy array decoder: apps=%+v err=%v", legacy, err)
	}
}

func TestAppFrontDoorBoundSkipsDriftScanAndFixForAReleasedApp(t *testing.T) {
	tests := []struct {
		name         string
		app          state.App
		caddyAdminUp bool
		want         bool
	}{
		{name: "nothing live", app: state.App{}, caddyAdminUp: true, want: false},
		{name: "caddy admin down", app: state.App{LiveSiding: "one"}, caddyAdminUp: false, want: false},
		{name: "live and bound", app: state.App{LiveSiding: "one"}, caddyAdminUp: true, want: true},
		{name: "released front door has no binding to check", app: state.App{LiveSiding: "one", FrontDoorReleased: true}, caddyAdminUp: true, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := appFrontDoorBound(test.app, test.caddyAdminUp); got != test.want {
				t.Fatalf("appFrontDoorBound = %t, want %t", got, test.want)
			}
		})
	}
}

func TestStatusReportsFrontDoorReleasedInJSONAndText(t *testing.T) {
	health := healthView{Apps: []appHealth{{Name: "app", LiveSiding: "one", FrontDoorReleased: true}}}
	encoded, err := json.Marshal(health)
	if err != nil {
		t.Fatal(err)
	}
	var current struct {
		Apps []struct {
			FrontDoorReleased bool `json:"frontDoorReleased"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(encoded, &current); err != nil || len(current.Apps) != 1 || !current.Apps[0].FrontDoorReleased {
		t.Fatalf("status JSON = %s, want frontDoorReleased true: %+v (err %v)", encoded, current, err)
	}
	text := statusText(health)
	if !containsAll(text, "app", "one", "front door released") {
		t.Fatalf("status text = %q, want a front-door-released note", text)
	}
}

func containsAll(value string, values ...string) bool {
	for _, expected := range values {
		if !strings.Contains(value, expected) {
			return false
		}
	}
	return true
}

func TestSidingReclaimableNeverOffersLiveBaseOrARunningGuest(t *testing.T) {
	app := state.App{
		Name:       "Alpha",
		LiveSiding: "live",
		BaseSiding: "base",
		Sidings: map[string]state.Siding{
			"live": {Name: "live"}, "base": {Name: "base"}, "busy": {Name: "busy"},
		},
	}
	// Each of these is excluded before any Git work happens, so the analyzer is
	// never reached and the answer cannot depend on the worktree's state.
	for _, c := range []struct {
		name  string
		guest container.GuestObservationState
	}{
		{"live", container.GuestAbsent},  // the front-door target: switch away first
		{"base", container.GuestAbsent},  // removing the base needs a successor
		{"busy", container.GuestRunning}, // a run inside leaves no trace in Git
	} {
		got := sidingReclaimable(context.Background(), app, c.name, container.GuestObservation{State: c.guest})
		if got == nil || *got {
			t.Errorf("sidingReclaimable(%q, guest=%s) = %v, want a definite false", c.name, c.guest, got)
		}
	}
}

func TestReclaimableFooterIsSilentUnlessAsked(t *testing.T) {
	yes := true
	apps := []lsApp{{Name: "Alpha", Sidings: []lsSiding{{Name: "one", Reclaimable: &yes}}}}

	var quiet strings.Builder
	printReclaimableFooter(&quiet, false, apps)
	if quiet.String() != "" {
		t.Errorf("footer printed without --reclaimable: %q", quiet.String())
	}

	var asked strings.Builder
	printReclaimableFooter(&asked, true, apps)
	if !strings.Contains(asked.String(), "Alpha/one") {
		t.Errorf("footer did not name the reclaimable siding:\n%s", asked.String())
	}
}

func TestReclaimableFooterSeparatesHeldFromUnchecked(t *testing.T) {
	no := false

	// Every siding was checked and none is free: a definite answer is safe.
	var definite strings.Builder
	printReclaimableFooter(&definite, true, []lsApp{{Name: "Alpha", Sidings: []lsSiding{
		{Name: "held", Reclaimable: &no},
	}}})
	if !strings.Contains(definite.String(), "no siding is reclaimable") {
		t.Errorf("expected the definite nothing-free line, got:\n%s", definite.String())
	}

	// One check failed, so the same claim would overstate what is known. An
	// unchecked siding is named on its own line rather than folded into either
	// answer, because calling it held is a guess and calling it free is worse.
	var partial strings.Builder
	printReclaimableFooter(&partial, true, []lsApp{{Name: "Alpha", Sidings: []lsSiding{
		{Name: "held", Reclaimable: &no}, {Name: "unchecked"},
	}}})
	out := partial.String()
	if !strings.Contains(out, "could not be checked") {
		t.Errorf("expected the hedged line when a check failed, got:\n%s", out)
	}
	if !strings.Contains(out, "Alpha/unchecked") {
		t.Errorf("expected the unchecked siding to be named, got:\n%s", out)
	}
	if strings.Contains(out, "no siding is reclaimable") {
		t.Errorf("stated a definite answer despite a failed check:\n%s", out)
	}
}
