package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestStateIsHostlessAndSeparatesPhaseFromRuntime(t *testing.T) {
	app := state.App{
		Name:       "app",
		LiveSiding: state.HostTarget,
		BaseSiding: "work",
		Sidings: map[string]state.Siding{
			"work":    {Name: "work", MaterializationPhase: state.PhaseWorktree},
			"parked":  {Name: "parked", MaterializationPhase: state.PhaseParked},
			"up":      {Name: "up", MaterializationPhase: state.PhaseGuest, Container: "up-guest", LastIP: "192.0.2.10"},
			"stopped": {Name: "stopped", MaterializationPhase: state.PhaseGuest, Container: "stopped-guest"},
			"missing": {Name: "missing", MaterializationPhase: state.PhaseGuest, Container: "missing-guest"},
			"broken":  {Name: "broken", MaterializationPhase: state.PhaseGuest, Container: "broken-guest"},
		},
	}
	deps := testServerDeps(app)
	deps.runtime = func(context.Context) container.RuntimeObservation {
		return container.RuntimeObservation{State: container.RuntimeRunning}
	}
	deps.guest = func(_ context.Context, name string) container.GuestObservation {
		switch name {
		case "up-guest":
			return container.GuestObservation{State: container.GuestRunning}
		case "stopped-guest":
			return container.GuestObservation{State: container.GuestStopped}
		case "missing-guest":
			return container.GuestObservation{State: container.GuestAbsent}
		case "broken-guest":
			return container.GuestObservation{State: container.GuestUnavailable}
		default:
			return container.GuestObservation{State: container.GuestUnavailable}
		}
	}
	deps.healthOK = func(context.Context, state.App, state.Siding) bool { return true }
	server := newServerWithDeps(deps)

	response := httptest.NewRecorder()
	request := stateRequest()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var view stateView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Apps) != 1 || view.Apps[0].LiveSiding != "" {
		t.Fatalf("legacy host was exposed as live: %+v", view.Apps)
	}
	rows := make(map[string]sidingView)
	for _, row := range view.Apps[0].Sidings {
		if row.Name == state.HostTarget {
			t.Fatal("dashboard exposed a host row")
		}
		rows[row.Name] = row
	}
	if !rows["work"].Base || rows["work"].Phase != string(state.PhaseWorktree) || rows["work"].Runtime != "missing" {
		t.Fatalf("worktree row = %+v", rows["work"])
	}
	if rows["parked"].Phase != string(state.PhaseParked) || rows["parked"].Runtime != "missing" {
		t.Fatalf("parked row = %+v", rows["parked"])
	}
	if rows["up"].Phase != string(state.PhaseGuest) || rows["up"].Runtime != "running" || !rows["up"].Serving {
		t.Fatalf("running row = %+v", rows["up"])
	}
	if rows["stopped"].Runtime != "stopped" || rows["missing"].Runtime != "missing" {
		t.Fatalf("stopped/missing rows = %+v / %+v", rows["stopped"], rows["missing"])
	}
	if rows["broken"].Runtime != "runtime-unavailable" || rows["broken"].RuntimeDetail == "" {
		t.Fatalf("inspection failure was misreported: %+v", rows["broken"])
	}
}

func TestStateReportsRuntimeUnavailableWithoutInspectingGuests(t *testing.T) {
	app := state.App{Name: "app", Sidings: map[string]state.Siding{
		"work": {Name: "work", MaterializationPhase: state.PhaseWorktree},
		"up":   {Name: "up", MaterializationPhase: state.PhaseGuest, Container: "up-guest"},
	}}
	deps := testServerDeps(app)
	deps.runtime = func(context.Context) container.RuntimeObservation {
		return container.RuntimeObservation{State: container.RuntimeUnavailable}
	}
	deps.guest = func(context.Context, string) container.GuestObservation {
		t.Fatal("guest inspect ran while runtime was unavailable")
		return container.GuestObservation{}
	}
	server := newServerWithDeps(deps)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, stateRequest())
	var view stateView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	rows := make(map[string]sidingView)
	for _, row := range view.Apps[0].Sidings {
		rows[row.Name] = row
	}
	if rows["work"].Runtime != "missing" || rows["work"].RuntimeDetail != "no guest is materialized for this phase" {
		t.Fatalf("worktree row = %+v", rows["work"])
	}
	if rows["up"].Runtime != "runtime-unavailable" {
		t.Fatalf("guest row = %+v", rows["up"])
	}
}

func TestStateDoesNotProbeRuntimeForNonGuestSidings(t *testing.T) {
	app := state.App{Name: "app", Sidings: map[string]state.Siding{
		"work":   {Name: "work", MaterializationPhase: state.PhaseWorktree},
		"data":   {Name: "data", MaterializationPhase: state.PhaseData},
		"parked": {Name: "parked", MaterializationPhase: state.PhaseParked},
	}}
	deps := testServerDeps(app)
	runtimeCalls := 0
	deps.runtime = func(context.Context) container.RuntimeObservation {
		runtimeCalls++
		return container.RuntimeObservation{State: container.RuntimeUnavailable}
	}
	deps.guest = func(context.Context, string) container.GuestObservation {
		t.Fatal("guest inspection ran when no guest was materialized")
		return container.GuestObservation{}
	}
	server := newServerWithDeps(deps)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, stateRequest())
	if runtimeCalls != 0 {
		t.Fatalf("runtime availability was probed %d times", runtimeCalls)
	}
	var view stateView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	for _, row := range view.Apps[0].Sidings {
		if row.Runtime != "missing" || row.RuntimeDetail != "no guest is materialized for this phase" {
			t.Fatalf("non-guest row = %+v", row)
		}
	}
}

func TestStateReturnsEmptyAppsArrayForEmptyRegistry(t *testing.T) {
	deps := testServerDeps(state.App{Name: "app"})
	deps.loadRegistry = func() (state.Registry, error) { return state.Registry{Projects: map[string]string{}}, nil }
	server := newServerWithDeps(deps)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, stateRequest())
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var raw struct {
		Apps json.RawMessage `json:"apps"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw.Apps) != "[]" {
		t.Fatalf("apps JSON = %s, want []", raw.Apps)
	}
}

func TestStateRequiresLoopbackHostAndDisablesCaching(t *testing.T) {
	deps := testServerDeps(state.App{Name: "app"})
	loads := 0
	deps.loadRegistry = func() (state.Registry, error) {
		loads++
		return state.Registry{Projects: map[string]string{}}, nil
	}
	server := newServerWithDeps(deps)
	blocked := httptest.NewRecorder()
	request := stateRequest()
	request.Host = "attacker.example"
	server.Handler().ServeHTTP(blocked, request)
	if blocked.Code != http.StatusForbidden || loads != 0 {
		t.Fatalf("blocked state = %d loads=%d body=%q", blocked.Code, loads, blocked.Body.String())
	}

	allowed := httptest.NewRecorder()
	server.Handler().ServeHTTP(allowed, stateRequest())
	if allowed.Code != http.StatusOK || allowed.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("allowed state = %d cache=%q", allowed.Code, allowed.Header().Get("Cache-Control"))
	}
}

func TestStateKeepsStoppedRuntimeDistinctFromUnavailable(t *testing.T) {
	app := state.App{Name: "app", Sidings: map[string]state.Siding{
		"one": {Name: "one", MaterializationPhase: state.PhaseGuest, Container: "one-guest"},
	}}
	deps := testServerDeps(app)
	deps.runtime = func(context.Context) container.RuntimeObservation {
		return container.RuntimeObservation{State: container.RuntimeStopped, Detail: "service is stopped"}
	}
	deps.guest = func(context.Context, string) container.GuestObservation {
		t.Fatal("guest inspection ran while the runtime was stopped")
		return container.GuestObservation{}
	}
	server := newServerWithDeps(deps)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, stateRequest())
	var view stateView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	row := view.Apps[0].Sidings[0]
	if row.Runtime != "stopped" || row.RuntimeDetail != "service is stopped" || row.Guest != "stopped" {
		t.Fatalf("runtime row = %+v", row)
	}
}

func TestStartUsesSharedUpForWorktreeAndParked(t *testing.T) {
	for _, phase := range []state.MaterializationPhase{state.PhaseWorktree, state.PhaseParked} {
		t.Run(string(phase), func(t *testing.T) {
			app := state.App{Name: "app", Sidings: map[string]state.Siding{
				"one": {Name: "one", MaterializationPhase: phase},
			}}
			deps := testServerDeps(app)
			called := make(chan state.MaterializationPhase, 1)
			deps.upSiding = func(_ context.Context, _ state.App, sd state.Siding, bridge bool, progress io.Writer) (state.Siding, error) {
				if !bridge || progress == nil {
					t.Errorf("shared Up arguments: bridge=%t progress=%v", bridge, progress)
				}
				called <- sd.MaterializationPhase
				return sd, nil
			}
			server := newServerWithDeps(deps)
			response := postAction(t, server, "/api/start", "app", "one")
			if response.Code != http.StatusAccepted {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
			select {
			case got := <-called:
				if got != phase {
					t.Fatalf("Up phase = %q, want %q", got, phase)
				}
			case <-time.After(time.Second):
				t.Fatal("shared Up was not called")
			}
		})
	}
}

func TestParkRefusesLiveAndNonGuestSidings(t *testing.T) {
	tests := []struct {
		name string
		app  state.App
		want string
	}{
		{
			name: "live",
			app: state.App{Name: "app", LiveSiding: "one", Sidings: map[string]state.Siding{
				"one": {Name: "one", MaterializationPhase: state.PhaseGuest},
			}},
			want: "switch away",
		},
		{
			name: "worktree",
			app: state.App{Name: "app", Sidings: map[string]state.Siding{
				"one": {Name: "one", MaterializationPhase: state.PhaseWorktree},
			}},
			want: "shunt up one",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deps := testServerDeps(test.app)
			deps.parkSiding = func(context.Context, state.App, string) (state.Siding, error) {
				t.Fatal("Park ran after server validation failed")
				return state.Siding{}, nil
			}
			response := postAction(t, newServerWithDeps(deps), "/api/park", "app", "one")
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("status/body = %d %q, want conflict containing %q", response.Code, response.Body.String(), test.want)
			}
		})
	}
}

func TestParkCallsSharedPathForNonLiveGuest(t *testing.T) {
	app := state.App{Name: "app", Sidings: map[string]state.Siding{
		"one": {Name: "one", MaterializationPhase: state.PhaseGuest},
	}}
	deps := testServerDeps(app)
	called := make(chan string, 1)
	deps.parkSiding = func(_ context.Context, _ state.App, name string) (state.Siding, error) {
		called <- name
		return state.Siding{Name: name, MaterializationPhase: state.PhaseParked}, nil
	}
	response := postAction(t, newServerWithDeps(deps), "/api/park", "app", "one")
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	select {
	case name := <-called:
		if name != "one" {
			t.Fatalf("parked %q", name)
		}
	case <-time.After(time.Second):
		t.Fatal("shared Park was not called")
	}
}

func TestGuestOnlyActionsReturnUpGuidance(t *testing.T) {
	app := state.App{Name: "app", Sidings: map[string]state.Siding{
		"one": {Name: "one", MaterializationPhase: state.PhaseParked},
	}}
	for _, path := range []string{"/api/switch", "/api/stop"} {
		response := postAction(t, newServerWithDeps(testServerDeps(app)), path, "app", "one")
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "shunt up one") {
			t.Fatalf("%s status/body = %d %q", path, response.Code, response.Body.String())
		}
	}
}

func TestHostActionsAreNotAccepted(t *testing.T) {
	app := state.App{Name: "app", Sidings: map[string]state.Siding{}}
	for _, path := range []string{"/api/switch", "/api/start", "/api/stop", "/api/park"} {
		response := postAction(t, newServerWithDeps(testServerDeps(app)), path, "app", state.HostTarget)
		if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "no siding host") {
			t.Fatalf("%s status/body = %d %q", path, response.Code, response.Body.String())
		}
	}
}

func TestDashboardHTMLShowsHostlessLifecycleControls(t *testing.T) {
	response := httptest.NewRecorder()
	handleIndex(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if got := response.Header().Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
		t.Fatalf("Content-Security-Policy = %q", got)
	}
	if got := response.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options = %q", got)
	}
	html := response.Body.String()
	for _, want := range []string{
		`rel="icon" href="data:image/svg+xml,`,
		"badge base",
		"persisted siding phase",
		"runtime-unavailable",
		"onclick=\"doPark",
		"removes only its recreatable Apple guest",
		"logical clone totals are not reclaimable",
		`role="log"`,
		`aria-relevant="additions"`,
		"terminal-status",
		"reconcileProgress",
		"No apps registered yet",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("dashboard HTML missing %q", want)
		}
	}
	for _, obsolete := range []string{"sd==='host'", "starting host", "stopping host"} {
		if strings.Contains(html, obsolete) {
			t.Fatalf("dashboard HTML retains host action %q", obsolete)
		}
	}
	if strings.Contains(html, "$('#apps').innerHTML = st.apps") {
		t.Fatal("dashboard replaces the full app subtree on each refresh")
	}
}

func TestAcceptedActionsUseJSONAndWritePersistentCompletionLog(t *testing.T) {
	app := state.App{Name: "app", Sidings: map[string]state.Siding{
		"one": {Name: "one", MaterializationPhase: state.PhaseGuest},
	}}
	deps := testServerDeps(app)
	deps.stopSiding = func(context.Context, state.App, string) (siding.StopResult, error) {
		return siding.StopResult{Forced: true}, errors.New("stop failed at /private/secret")
	}
	logs := make(chan string, 1)
	deps.actionLog = func(entry string) { logs <- entry }
	response := postAction(t, newServerWithDeps(deps), "/api/stop", "app", "one")
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	select {
	case entry := <-logs:
		for _, want := range []string{`action="stop"`, `project="app"`, `siding="one"`, `forced_stop=true`, `result="failed"`} {
			if !strings.Contains(entry, want) {
				t.Fatalf("log %q missing %q", entry, want)
			}
		}
		if strings.Contains(entry, "/private/secret") {
			t.Fatalf("log exposes error path: %q", entry)
		}
	case <-time.After(time.Second):
		t.Fatal("detached action did not write a completion log")
	}
}

func TestSwitchWritesPersistentSuccessAndFailureLogs(t *testing.T) {
	app := state.App{Name: "app", Sidings: map[string]state.Siding{
		"one": {Name: "one", MaterializationPhase: state.PhaseGuest},
	}}
	for _, test := range []struct {
		name       string
		switchErr  error
		wantStatus int
		wantResult string
	}{
		{name: "success", wantStatus: http.StatusOK, wantResult: `result="completed"`},
		{name: "failure", switchErr: errors.New("switch failed"), wantStatus: http.StatusInternalServerError, wantResult: `result="failed"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps := testServerDeps(app)
			deps.switchSiding = func(context.Context, *state.App, string) error { return test.switchErr }
			var entry string
			deps.actionLog = func(value string) { entry = value }
			response := postAction(t, newServerWithDeps(deps), "/api/switch", "app", "one")
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
			for _, want := range []string{`action="switch"`, `project="app"`, `siding="one"`, test.wantResult} {
				if !strings.Contains(entry, want) {
					t.Fatalf("log %q missing %q", entry, want)
				}
			}
		})
	}
}

func TestStateReportsRemovalAndPreservesLegacyGuestEnum(t *testing.T) {
	app := state.App{Name: "app", Removal: &state.RemovalOperation{ID: "remove-one", Siding: "one", Stage: state.RemovalBaselinePromoted, GenerationID: "gen-1", StartedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}, Sidings: map[string]state.Siding{
		"one": {Name: "one", MaterializationPhase: state.PhaseGuest, Container: "one-guest"},
	}}
	deps := testServerDeps(app)
	deps.runtime = func(context.Context) container.RuntimeObservation {
		return container.RuntimeObservation{State: container.RuntimeUnavailable, Detail: "permission denied"}
	}
	server := newServerWithDeps(deps)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, stateRequest())
	var view stateView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	row := view.Apps[0].Sidings[0]
	if row.Runtime != "runtime-unavailable" || row.Guest != "stopped" {
		t.Fatalf("runtime/legacy guest = %+v", row)
	}
	removal := view.Apps[0].Removal
	if removal == nil || removal.Resume != config.Current().BinaryName+" rm one" || removal.Generation != "gen-1" || removal.Age == "unknown" {
		t.Fatalf("removal = %+v", removal)
	}
}

func TestRemovalFencesEveryDashboardAction(t *testing.T) {
	app := state.App{Name: "app", Removal: &state.RemovalOperation{ID: "remove-one", Siding: "one", Stage: state.RemovalGuestRemoved, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}, Sidings: map[string]state.Siding{
		"one": {Name: "one", MaterializationPhase: state.PhaseGuest},
	}}
	deps := testServerDeps(app)
	deps.switchSiding = func(context.Context, *state.App, string) error { t.Fatal("switch ran during removal"); return nil }
	deps.upSiding = func(context.Context, state.App, state.Siding, bool, io.Writer) (state.Siding, error) {
		t.Fatal("up ran during removal")
		return state.Siding{}, nil
	}
	deps.stopSiding = func(context.Context, state.App, string) (siding.StopResult, error) {
		t.Fatal("stop ran during removal")
		return siding.StopResult{}, nil
	}
	deps.parkSiding = func(context.Context, state.App, string) (state.Siding, error) {
		t.Fatal("park ran during removal")
		return state.Siding{}, nil
	}
	server := newServerWithDeps(deps)
	for _, path := range []string{"/api/switch", "/api/start", "/api/stop", "/api/park"} {
		response := postAction(t, server, path, "app", "one")
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), config.Current().BinaryName+" rm one") {
			t.Fatalf("%s status/body = %d %q", path, response.Code, response.Body.String())
		}
	}
}

func TestActionRequestHardening(t *testing.T) {
	server := newServerWithDeps(testServerDeps(state.App{Name: "app", Sidings: map[string]state.Siding{
		"one": {Name: "one", MaterializationPhase: state.PhaseGuest},
	}}))

	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{
			name: "wrong method",
			mutate: func(r *http.Request) {
				r.Method = http.MethodGet
			},
			want: http.StatusMethodNotAllowed,
		},
		{
			name: "cross origin",
			mutate: func(r *http.Request) {
				r.Header.Set("Origin", "http://attacker.example")
			},
			want: http.StatusForbidden,
		},
		{
			name: "host rebinding",
			mutate: func(r *http.Request) {
				r.Host = "attacker.example"
				r.Header.Set("Origin", "http://attacker.example")
			},
			want: http.StatusForbidden,
		},
		{
			name: "missing token",
			mutate: func(r *http.Request) {
				r.Header.Del(csrfHeader)
			},
			want: http.StatusForbidden,
		},
		{
			name: "wrong content type",
			mutate: func(r *http.Request) {
				r.Header.Set("Content-Type", "text/plain")
			},
			want: http.StatusUnsupportedMediaType,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := actionRequest(t, server, "/api/start", "app", "one")
			test.mutate(request)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestActionRejectsOversizedBodyAndUnknownSiding(t *testing.T) {
	app := state.App{Name: "app", Sidings: map[string]state.Siding{
		"one": {Name: "one", MaterializationPhase: state.PhaseGuest},
	}}
	deps := testServerDeps(app)
	deps.upSiding = func(context.Context, state.App, state.Siding, bool, io.Writer) (state.Siding, error) {
		t.Fatal("Up ran for an invalid action")
		return state.Siding{}, nil
	}
	server := newServerWithDeps(deps)

	oversized := actionRequest(t, server, "/api/start", "app", "one")
	oversized.Body = io.NopCloser(strings.NewReader(`{"project":"app","siding":"one","padding":"` + strings.Repeat("x", maxActionBodyBytes) + `"}`))
	oversized.ContentLength = -1
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, oversized)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d: %s", response.Code, response.Body.String())
	}

	response = postAction(t, server, "/api/start", "app", "gone")
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown siding status = %d: %s", response.Code, response.Body.String())
	}
}

func TestStateKeepsCompatibilityGuestAndBoundedProgress(t *testing.T) {
	app := state.App{Name: "app", Sidings: map[string]state.Siding{
		"one": {Name: "one", MaterializationPhase: state.PhaseWorktree},
	}}
	server := newServerWithDeps(testServerDeps(app))
	server.setStatus("app", "one", "starting…")
	writer := server.progressWriter("app/one")
	for i := 0; i < maxProgressEntries+3; i++ {
		if _, err := writer.Write([]byte("phase update\n")); err != nil {
			t.Fatal(err)
		}
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, stateRequest())
	var view stateView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	row := view.Apps[0].Sidings[0]
	if row.Phase != string(state.PhaseWorktree) || row.Guest != "stopped" {
		t.Fatalf("compatibility state = %+v", row)
	}
	if len(row.Progress) != maxProgressEntries {
		t.Fatalf("progress entries = %d, want %d", len(row.Progress), maxProgressEntries)
	}
}

func TestNewActionClearsStaleProgress(t *testing.T) {
	server := newServerWithDeps(testServerDeps(state.App{Name: "app"}))
	server.setStatus("app", "one", "starting…")
	server.addProgress("app/one", "old materialization step")
	server.setStatus("app", "one", "stopping…")
	action := server.getAction("app", "one")
	if action.message != "stopping…" || len(action.progress) != 0 {
		t.Fatalf("new action retained stale progress: %+v", action)
	}
}

func TestTerminalActionEntriesAreRemovedOrExpired(t *testing.T) {
	server := newServerWithDeps(testServerDeps(state.App{Name: "app"}))
	key := "app/one"
	if !server.acquire(key) {
		t.Fatal("first action was not acquired")
	}
	server.release(key, "")
	if len(server.busy) != 0 || len(server.status) != 0 {
		t.Fatalf("completed success leaked action maps: busy=%v status=%v", server.busy, server.status)
	}

	server.complete(key, "error: failed")
	server.mu.Lock()
	entry := server.status[key]
	entry.expiresAt = time.Now().Add(-time.Second)
	server.status[key] = entry
	server.mu.Unlock()
	if action := server.getAction("app", "one"); action.message != "" {
		t.Fatalf("expired action = %+v", action)
	}
	if len(server.status) != 0 {
		t.Fatalf("expired action leaked status map: %v", server.status)
	}
}

func testServerDeps(app state.App) serverDeps {
	return serverDeps{
		loadRegistry: func() (state.Registry, error) {
			return state.Registry{Projects: map[string]string{app.Name: "/state/app"}}, nil
		},
		loadAppDir:  func(string) (state.App, error) { return app, nil },
		loadProject: func(string) (state.App, error) { return app, nil },
		runtime: func(context.Context) container.RuntimeObservation {
			return container.RuntimeObservation{State: container.RuntimeRunning}
		},
		guest: func(context.Context, string) container.GuestObservation {
			return container.GuestObservation{State: container.GuestRunning}
		},
		healthOK:     func(context.Context, state.App, state.Siding) bool { return true },
		switchSiding: func(context.Context, *state.App, string) error { return nil },
		upSiding: func(_ context.Context, _ state.App, sd state.Siding, _ bool, _ io.Writer) (state.Siding, error) {
			return sd, nil
		},
		stopSiding: func(_ context.Context, _ state.App, name string) (siding.StopResult, error) {
			return siding.StopResult{Siding: state.Siding{Name: name}}, nil
		},
		parkSiding: func(_ context.Context, _ state.App, name string) (state.Siding, error) {
			return state.Siding{Name: name, MaterializationPhase: state.PhaseParked}, nil
		},
		actionLog: func(string) {},
	}
}

func postAction(t *testing.T, server *Server, path, project, name string) *httptest.ResponseRecorder {
	t.Helper()
	request := actionRequest(t, server, path, project, name)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func actionRequest(t *testing.T, server *Server, path, project, name string) *http.Request {
	t.Helper()
	body, err := json.Marshal(actionReq{Project: project, Siding: name})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request.Host = "localhost"
	request.Header.Set("Origin", "http://localhost")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(csrfHeader, server.csrfToken)
	return request
}

func stateRequest() *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	request.Host = "localhost"
	return request
}
