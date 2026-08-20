package siding

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/dockerdpolicy"
	"github.com/gordonbeeming/shunt/internal/imagecache"
	"github.com/gordonbeeming/shunt/internal/runner"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestStopAppDoesNotUseFullCommandLineProcessKills(t *testing.T) {
	for _, unsafe := range []string{"pkill -9 -f dotnet", "pkill -9 -f aspire", "pkill -9 -f dcp"} {
		if strings.Contains(aspireProcessKillScript, unsafe) {
			t.Fatalf("StopApp contains self-matching process kill %q", unsafe)
		}
	}
	// dcp is the orchestrator tree; it outlives the AppHost and binds a random
	// API-server port, so nothing else in StopApp reaps it.
	for _, safe := range []string{"pkill -9 -x dotnet", "pkill -9 -x aspire", "pkill -9 -x dcp"} {
		if !strings.Contains(aspireProcessKillScript, safe) {
			t.Fatalf("StopApp is missing executable-name process kill %q", safe)
		}
	}
}

func TestRecreateAssuresCacheBeforeRemovingGuest(t *testing.T) {
	originalAssure, originalRemove := assureImageSources, removeGuest
	defer func() {
		assureImageSources = originalAssure
		removeGuest = originalRemove
	}()
	assureImageSources = func(context.Context, string, []string, []imagecache.LocalBuildSource) ([]imagecache.Change, error) {
		return nil, errors.New("injected assurance failure")
	}
	removed := false
	removeGuest = func(context.Context, string) error {
		removed = true
		return nil
	}
	app := state.App{ConfigDir: t.TempDir(), PrebakeImages: []string{"example/db:latest"}}
	_, err := recreate(context.Background(), app, state.Siding{Name: "alpha", Container: "guest"}, false)
	if err == nil || !strings.Contains(err.Error(), "assure dependency image cache") {
		t.Fatalf("recreate() error = %v", err)
	}
	if removed {
		t.Fatal("guest was removed before cache assurance completed")
	}
}

func TestRecreateEnsuresBaseImageBeforeRemovingGuest(t *testing.T) {
	originalEnsureBase, originalRemove := ensureBaseImage, removeGuest
	defer func() { ensureBaseImage, removeGuest = originalEnsureBase, originalRemove }()
	ensureBaseImage = func(context.Context, bool) error { return errors.New("injected base build failure") }
	removed := false
	removeGuest = func(context.Context, string) error { removed = true; return nil }

	app := state.App{ConfigDir: t.TempDir()}
	_, err := recreate(context.Background(), app, state.Siding{Name: "alpha", Container: "guest"}, false)
	if err == nil || !strings.Contains(err.Error(), "ensure native base image") {
		t.Fatalf("recreate() error = %v", err)
	}
	if removed {
		t.Fatal("guest was removed before the native base image was ready")
	}
}

func TestAssureImageCacheContinuesAfterCommittedCleanupWarning(t *testing.T) {
	originalAssure, originalWarnings := assureImageSources, cacheWarningWriter
	defer func() { assureImageSources, cacheWarningWriter = originalAssure, originalWarnings }()
	var warnings bytes.Buffer
	cacheWarningWriter = &warnings
	assureImageSources = func(context.Context, string, []string, []imagecache.LocalBuildSource) ([]imagecache.Change, error) {
		return []imagecache.Change{{Ref: "example/db:latest", Action: "added"}}, &imagecache.CommittedCleanupError{Err: errors.New("injected GC failure")}
	}
	app := state.App{ConfigDir: t.TempDir(), PrebakeImages: []string{"example/db:latest"}}
	if err := AssureImageCache(context.Background(), app); err != nil {
		t.Fatalf("AssureImageCache() error = %v", err)
	}
	if !strings.Contains(warnings.String(), "automatic collection failed") || !strings.Contains(warnings.String(), WarmTarPath(app)) {
		t.Fatalf("cache warning = %q", warnings.String())
	}
}

func TestEnsureGuestLiveRejectsStaleBaseWithoutRestartingIt(t *testing.T) {
	originalExec, originalCapability, originalStop, originalStart := execGuest, guestCapabilityProbe, stopGuest, startGuest
	defer func() {
		execGuest, guestCapabilityProbe, stopGuest, startGuest = originalExec, originalCapability, originalStop, originalStart
	}()
	execGuest = func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) != 1 || args[0] != "true" {
			t.Fatalf("liveness args = %#v", args)
		}
		return "", nil
	}
	guestCapabilityProbe = func(context.Context, string) (bool, error) { return false, nil }
	restarted := false
	stopGuest = func(context.Context, string) error { restarted = true; return nil }
	startGuest = func(context.Context, string) error { restarted = true; return nil }
	err := EnsureGuestLive(context.Background(), state.Siding{Name: "alpha", Container: "guest"})
	if err == nil || !strings.Contains(err.Error(), "stale base image") {
		t.Fatalf("EnsureGuestLive() error = %v", err)
	}
	if restarted {
		t.Fatal("stale guest was restarted instead of being recreated")
	}
}

func TestEnsureGuestLiveFailsClosedOnTransientRuntimeObservation(t *testing.T) {
	originalExec, originalObserve, originalStop, originalStart := execGuest, observeGuest, stopGuest, startGuest
	defer func() {
		execGuest, observeGuest, stopGuest, startGuest = originalExec, originalObserve, originalStop, originalStart
	}()
	execGuest = func(context.Context, string, ...string) (string, error) {
		return "", errors.New("transient exec failure")
	}
	observeGuest = func(context.Context, string) container.GuestObservation {
		return container.GuestObservation{State: container.GuestUnavailable}
	}
	mutated := false
	stopGuest = func(context.Context, string) error { mutated = true; return nil }
	startGuest = func(context.Context, string) error { mutated = true; return nil }
	err := EnsureGuestLive(context.Background(), state.Siding{Name: "alpha", Container: "guest"})
	if err == nil || !strings.Contains(err.Error(), "runtime unavailable") {
		t.Fatalf("EnsureGuestLive() = %v", err)
	}
	if mutated {
		t.Fatal("runtime uncertainty caused a guest mutation")
	}
}

func TestRestartDoesNotRecreateOnTransientLivenessFailure(t *testing.T) {
	originalExec, originalObserve, originalRemove := execGuest, observeGuest, removeGuest
	defer func() { execGuest, observeGuest, removeGuest = originalExec, originalObserve, originalRemove }()
	execGuest = func(context.Context, string, ...string) (string, error) { return "", errors.New("permission denied") }
	observeGuest = func(context.Context, string) container.GuestObservation {
		return container.GuestObservation{State: container.GuestUnavailable}
	}
	removed := false
	removeGuest = func(context.Context, string) error { removed = true; return nil }
	dir := t.TempDir()
	app := state.App{ConfigDir: dir, Sidings: map[string]state.Siding{
		"alpha": {Name: "alpha", Container: "guest", MaterializationPhase: state.PhaseGuest},
	}}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	err := Restart(context.Background(), app, app.Sidings["alpha"])
	if err == nil || !strings.Contains(err.Error(), "runtime unavailable") {
		t.Fatalf("Restart() = %v", err)
	}
	if removed {
		t.Fatal("Restart recreated a guest after transient liveness uncertainty")
	}
}

func TestCapabilityPredicateMismatchDiffersFromTransportFailure(t *testing.T) {
	originalExec, originalCapability := execGuest, guestCapabilityProbe
	defer func() { execGuest, guestCapabilityProbe = originalExec, originalCapability }()
	guestCapabilityProbe = probeGuestCapability

	t.Run("predicate-mismatch-authorizes-recreation", func(t *testing.T) {
		calls := 0
		execGuest = func(context.Context, string, ...string) (string, error) {
			calls++
			if calls == 1 {
				return "", nil
			}
			return "shunt-capability:mismatch", nil
		}
		err := EnsureGuestLive(context.Background(), state.Siding{Name: "alpha", Container: "guest"})
		var recreate *GuestRecreateRequiredError
		if !errors.As(err, &recreate) || !strings.Contains(err.Error(), "stale base image") {
			t.Fatalf("predicate mismatch = %v", err)
		}
	})

	t.Run("xpc-transport-fails-closed", func(t *testing.T) {
		calls := 0
		execGuest = func(context.Context, string, ...string) (string, error) {
			calls++
			if calls == 1 {
				return "", nil
			}
			return "", errors.New("XPC connection invalid")
		}
		err := EnsureGuestLive(context.Background(), state.Siding{Name: "alpha", Container: "guest"})
		var recreate *GuestRecreateRequiredError
		if err == nil || errors.As(err, &recreate) || !strings.Contains(err.Error(), "XPC") {
			t.Fatalf("transport failure = %v", err)
		}
	})
}

func TestLivenessRetryExhaustionFailsClosed(t *testing.T) {
	originalExec, originalObserve, originalStop, originalStart := execGuest, observeGuest, stopGuest, startGuest
	originalAttempts, originalPoll := guestLivenessAttempts, guestLivenessPoll
	defer func() {
		execGuest, observeGuest, stopGuest, startGuest = originalExec, originalObserve, originalStop, originalStart
		guestLivenessAttempts, guestLivenessPoll = originalAttempts, originalPoll
	}()
	execGuest = func(context.Context, string, ...string) (string, error) {
		return "", errors.New("transient exec failure")
	}
	observeGuest = func(context.Context, string) container.GuestObservation {
		return container.GuestObservation{State: container.GuestRunning}
	}
	stopGuest = func(context.Context, string) error { return nil }
	startGuest = func(context.Context, string) error { return nil }
	guestLivenessAttempts = 2
	guestLivenessPoll = 0
	err := EnsureGuestLive(context.Background(), state.Siding{Name: "alpha", Container: "guest"})
	var recreate *GuestRecreateRequiredError
	if err == nil || errors.As(err, &recreate) || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("retry exhaustion = %v", err)
	}
}

func TestEnsureDockerdWaitsForEntrypointBeforeRepairing(t *testing.T) {
	originalExec, originalWait, originalPoll := execGuest, dockerdStartupWait, dockerdReadyPoll
	defer func() {
		execGuest, dockerdStartupWait, dockerdReadyPoll = originalExec, originalWait, originalPoll
	}()
	dockerdStartupWait = time.Second
	dockerdReadyPoll = 0
	calls := 0
	execGuest = func(_ context.Context, _ string, args ...string) (string, error) {
		calls++
		if len(args) > 0 && args[0] == dockerdpolicy.EnsureCommand {
			t.Fatal("entrypoint startup was interrupted by a competing repair")
		}
		if calls == 1 {
			return "", errors.New("entrypoint is still starting")
		}
		return "", nil
	}
	if err := EnsureDockerd(context.Background(), state.Siding{Container: "guest"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("readiness checks = %d, want 2", calls)
	}
}

func TestRecreateStopsAfterRemovedStateCannotBeSaved(t *testing.T) {
	originalEnsureBase, originalRemove, originalRun, originalMerge := ensureBaseImage, removeGuest, runGuest, mergeSiding
	defer func() {
		ensureBaseImage, removeGuest, runGuest, mergeSiding = originalEnsureBase, originalRemove, originalRun, originalMerge
	}()
	ensureBaseImage = func(context.Context, bool) error { return nil }
	removed := false
	ran := false
	removeGuest = func(context.Context, string) error { removed = true; return nil }
	runGuest = func(context.Context, container.RunOpts) error { ran = true; return nil }
	mergeSiding = func(context.Context, string, state.Siding, bool) (state.App, error) {
		return state.App{}, errors.New("injected state failure")
	}
	app := state.App{ConfigDir: t.TempDir()}
	_, err := recreate(context.Background(), app, state.Siding{Name: "alpha", Container: "guest"}, false)
	if err == nil || !strings.Contains(err.Error(), "guest removed, but replacement state could not be saved") {
		t.Fatalf("recreate() error = %v", err)
	}
	if !removed || ran {
		t.Fatalf("recreate ordering: removed=%v ran=%v", removed, ran)
	}
}

func TestRecreateStopsReplacementWhenRunningStateCannotBeSaved(t *testing.T) {
	originalEnsureBase, originalRemove, originalRun, originalStop, originalMerge := ensureBaseImage, removeGuest, runGuest, stopGuest, mergeSiding
	defer func() {
		ensureBaseImage, removeGuest, runGuest, stopGuest, mergeSiding = originalEnsureBase, originalRemove, originalRun, originalStop, originalMerge
	}()
	ensureBaseImage = func(context.Context, bool) error { return nil }
	removeGuest = func(context.Context, string) error { return nil }
	runGuest = func(context.Context, container.RunOpts) error { return nil }
	stopped := false
	stopGuest = func(context.Context, string) error { stopped = true; return nil }
	merges := 0
	mergeSiding = func(context.Context, string, state.Siding, bool) (state.App, error) {
		merges++
		if merges == 2 {
			return state.App{}, errors.New("injected running-state failure")
		}
		return state.App{}, nil
	}
	app := state.App{ConfigDir: t.TempDir()}
	_, err := recreate(context.Background(), app, state.Siding{Name: "alpha", Container: "guest"}, false)
	if err == nil || !strings.Contains(err.Error(), "replacement guest started, but its state could not be saved") {
		t.Fatalf("recreate() error = %v", err)
	}
	if !stopped {
		t.Fatal("replacement guest remained running after its running state failed to persist")
	}
}

func TestRecreateKeepsReplacementWhenStatePublicationIsVisible(t *testing.T) {
	originalEnsureBase, originalRemove, originalRun, originalStop, originalMerge := ensureBaseImage, removeGuest, runGuest, stopGuest, mergeSiding
	defer func() {
		ensureBaseImage, removeGuest, runGuest, stopGuest, mergeSiding = originalEnsureBase, originalRemove, originalRun, originalStop, originalMerge
	}()
	ensureBaseImage = func(context.Context, bool) error { return nil }
	removeGuest = func(context.Context, string) error { return nil }
	runGuest = func(context.Context, container.RunOpts) error { return nil }
	stopped := false
	stopGuest = func(context.Context, string) error { stopped = true; return nil }
	merges := 0
	mergeSiding = func(context.Context, string, state.Siding, bool) (state.App, error) {
		merges++
		if merges == 2 {
			return state.App{}, &state.CommittedDurabilityError{Path: "state-v2.json", Err: errors.New("injected parent sync failure")}
		}
		return state.App{}, nil
	}
	app := state.App{ConfigDir: t.TempDir()}
	_, err := recreate(context.Background(), app, state.Siding{Name: "alpha", Container: "guest"}, false)
	if err == nil || !strings.Contains(err.Error(), "state is visible but durability is unconfirmed") {
		t.Fatalf("recreate() error = %v", err)
	}
	if stopped {
		t.Fatal("replacement guest was stopped after its running state was published")
	}
}

func TestRestartPreparesBeforeStoppingApplication(t *testing.T) {
	originalState, originalPrepare, originalStop := guestRuntimeState, prepareLifecycle, stopLifecycleApp
	defer func() {
		guestRuntimeState, prepareLifecycle, stopLifecycleApp = originalState, originalPrepare, originalStop
	}()
	guestRuntimeState = func(context.Context, string) (string, error) { return "running", nil }
	prepareLifecycle = func(context.Context, state.App, state.Siding) error {
		return errors.New("injected assurance failure")
	}
	stopped := false
	stopLifecycleApp = func(context.Context, state.App, state.Siding) error {
		stopped = true
		return nil
	}
	err := restart(context.Background(), state.App{}, state.Siding{Name: "alpha", Container: "guest"})
	if err == nil || !strings.Contains(err.Error(), "injected assurance failure") {
		t.Fatalf("restart() error = %v", err)
	}
	if stopped {
		t.Fatal("restart stopped the application before preparation succeeded")
	}
}

func TestRestartRejectsSidingParkedBeforeLockedReload(t *testing.T) {
	dir := t.TempDir()
	app := state.App{ConfigDir: dir, Sidings: map[string]state.Siding{
		"alpha": {Name: "alpha", Container: "guest", MaterializationPhase: state.PhaseParked},
	}}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	err := Restart(context.Background(), app, state.Siding{Name: "alpha", Container: "guest", MaterializationPhase: state.PhaseGuest})
	if err == nil || !strings.Contains(err.Error(), "parked") {
		t.Fatalf("Restart() error = %v, want parked-phase rejection", err)
	}
}

func TestRecreateRejectsSidingParkedBeforeLockedReload(t *testing.T) {
	dir := t.TempDir()
	app := state.App{ConfigDir: dir, Sidings: map[string]state.Siding{
		"alpha": {Name: "alpha", Container: "guest", MaterializationPhase: state.PhaseParked},
	}}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	_, err := Recreate(context.Background(), app, state.Siding{Name: "alpha", Container: "guest", MaterializationPhase: state.PhaseGuest}, false)
	if err == nil || !strings.Contains(err.Error(), "parked") {
		t.Fatalf("Recreate() error = %v, want parked-phase rejection", err)
	}
}

// Readiness is one rule for every runner: the required routes are listening.
// An optional route is still bridged and still served, it just does not get to
// hold the app back, which is what a slow dev server needs.
func TestProbeAppRunningWaitsForRequiredRoutesOnly(t *testing.T) {
	tests := []struct {
		name    string
		routes  []state.Route
		up      map[int]bool
		want    bool
		probing []int
	}{
		{
			name:    "optional route down does not block",
			routes:  []state.Route{{Key: "api", GuestPort: 7260}, {Key: "webapp", GuestPort: 5173, Optional: true}},
			up:      map[int]bool{7260: true},
			want:    true,
			probing: []int{7260},
		},
		{
			name:    "required route down blocks",
			routes:  []state.Route{{Key: "api", GuestPort: 7260}, {Key: "db", GuestPort: 1500}},
			up:      map[int]bool{7260: true},
			want:    false,
			probing: []int{7260, 1500},
		},
		{
			name:    "every required route up",
			routes:  []state.Route{{Key: "api", GuestPort: 7260}, {Key: "db", GuestPort: 1500}},
			up:      map[int]bool{7260: true, 1500: true},
			want:    true,
			probing: []int{7260, 1500},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var script string
			restore := probeExec
			defer func() { probeExec = restore }()
			probeExec = func(_ context.Context, _ string, args ...string) (string, error) {
				script = args[len(args)-1]
				for port, listening := range tc.up {
					if !listening && strings.Contains(script, fmt.Sprintf(":%d ", port)) {
						return "down", nil
					}
				}
				for _, r := range tc.routes {
					if r.Optional {
						continue
					}
					if !tc.up[r.GuestPort] {
						return "down", nil
					}
				}
				return "up", nil
			}

			app := state.App{Runner: runner.Custom, FrontDoor: tc.routes}
			got, err := ProbeAppRunning(context.Background(), app, state.Siding{Container: "guest"})
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("ready = %v, want %v", got, tc.want)
			}
			for _, port := range tc.probing {
				if !strings.Contains(script, fmt.Sprintf(":%d ", port)) {
					t.Fatalf("required port %d was not probed:\n%s", port, script)
				}
			}
			for _, r := range tc.routes {
				if r.Optional && strings.Contains(script, fmt.Sprintf(":%d ", r.GuestPort)) {
					t.Fatalf("optional port %d must not be probed:\n%s", r.GuestPort, script)
				}
			}
		})
	}
}
