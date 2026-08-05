package siding

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/dockerdpolicy"
	"github.com/gordonbeeming/shunt/internal/image"
	"github.com/gordonbeeming/shunt/internal/imagecache"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestStopAppDoesNotUseFullCommandLineProcessKills(t *testing.T) {
	for _, unsafe := range []string{"pkill -9 -f dotnet", "pkill -9 -f aspire"} {
		if strings.Contains(aspireProcessKillScript, unsafe) {
			t.Fatalf("StopApp contains self-matching process kill %q", unsafe)
		}
	}
	for _, safe := range []string{"pkill -9 -x dotnet", "pkill -9 -x aspire"} {
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
	originalExec, originalStop, originalStart := execGuest, stopGuest, startGuest
	defer func() {
		execGuest, stopGuest, startGuest = originalExec, originalStop, originalStart
	}()
	calls := 0
	execGuest = func(_ context.Context, _ string, args ...string) (string, error) {
		calls++
		if calls == 1 {
			if len(args) != 1 || args[0] != "true" {
				t.Fatalf("liveness args = %#v", args)
			}
			return "", nil
		}
		want := image.GuestCapabilityCheck()
		if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("capability args = %#v, want %#v", args, want)
		}
		return "", errors.New("version mismatch")
	}
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
