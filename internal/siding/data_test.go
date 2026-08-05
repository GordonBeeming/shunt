package siding

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

func TestSnapshotHostVolumeRejectsMissingSource(t *testing.T) {
	lifecycle := NewDataPromotionLifecycle(
		state.App{ConfigDir: t.TempDir()},
		state.Siding{Name: "alpha"},
		io.Discard,
	)
	err := lifecycle.SnapshotHostVolume(context.Background(), "db", filepath.Join(t.TempDir(), "snapshot"))
	if err == nil || !strings.Contains(err.Error(), "host-backed source") {
		t.Fatalf("SnapshotHostVolume() error = %v", err)
	}
}

func TestPromotionLifecycleCapturesOriginalRoutingState(t *testing.T) {
	app := state.App{LiveSiding: "alpha"}
	live := NewDataPromotionLifecycle(app, state.Siding{Name: "alpha", Bridges: map[string]int{"web": 5000}}, io.Discard)
	nonLive := NewDataPromotionLifecycle(app, state.Siding{Name: "beta"}, io.Discard)
	if !live.wasLive || !live.wasBridged || nonLive.wasLive || nonLive.wasBridged {
		t.Fatalf("routing preservation = live(%v,%v), non-live(%v,%v)", live.wasLive, live.wasBridged, nonLive.wasLive, nonLive.wasBridged)
	}
}

func TestRestoreBatchesVolumeConsumerRestartAndReportsPartialFailure(t *testing.T) {
	originalExec := dataExecGuest
	defer func() { dataExecGuest = originalExec }()
	lifecycle := NewDataPromotionLifecycle(state.App{}, state.Siding{Container: "guest"}, io.Discard)
	lifecycle.stoppedContainers = []string{"first", "second"}
	var calls [][]string
	dataExecGuest = func(_ context.Context, _ string, args ...string) (string, error) {
		calls = append(calls, append([]string(nil), args...))
		return "", errors.New("daemon unavailable")
	}
	result, err := lifecycle.Restore(context.Background())
	if err == nil || result.Restored {
		t.Fatalf("Restore() = %#v, %v", result, err)
	}
	if len(calls) != 1 || strings.Join(calls[0], "\x00") != strings.Join([]string{"docker", "start", "first", "second"}, "\x00") {
		t.Fatalf("restart calls = %#v", calls)
	}
}

func TestFailedCaptureStartupRestoresOriginallyStoppedGuest(t *testing.T) {
	originalState, originalEnsure, originalStop := dataGuestState, dataEnsureGuestLive, dataStopGuest
	defer func() {
		dataGuestState, dataEnsureGuestLive, dataStopGuest = originalState, originalEnsure, originalStop
	}()
	stateCalls := 0
	dataGuestState = func(context.Context, string) (string, error) {
		stateCalls++
		if stateCalls == 1 {
			return "stopped", nil
		}
		return "running", nil
	}
	dataEnsureGuestLive = func(context.Context, state.Siding) error {
		return errors.New("capability validation failed after start")
	}
	stopped := false
	dataStopGuest = func(context.Context, string) error {
		stopped = true
		return nil
	}

	lifecycle := NewDataPromotionLifecycle(state.App{}, state.Siding{Name: "alpha", Container: "guest", Stopped: true}, io.Discard)
	if err := lifecycle.Quiesce(context.Background()); err == nil {
		t.Fatal("Quiesce() unexpectedly succeeded")
	}
	result, err := lifecycle.Restore(context.Background())
	if err != nil || !result.Restored || !stopped || !lifecycle.Siding().Stopped {
		t.Fatalf("Restore() result=%#v error=%v stopped=%v siding=%#v", result, err, stopped, lifecycle.Siding())
	}
}
