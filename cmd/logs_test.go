package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/state"
)

// newLogsTestApp writes a minimal one-siding project whose siding is recorded as
// guest-materialized, so sidingLog reaches the guest read rather than failing the
// phase check first.
func newLogsTestApp(t *testing.T) string {
	t.Helper()
	configDir := t.TempDir()
	app := state.App{
		ConfigDir: configDir,
		Sidings: map[string]state.Siding{
			"one": {Name: "one", Container: "guest-one", MaterializationPhase: state.PhaseGuest},
		},
	}
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	return configDir
}

// A guest can list as running while refusing every exec, and it stays that way
// until something restarts it. Reading a file is no reason to do that restart
// silently, so `logs` names the recovery instead of performing it.
func TestSidingLogNamesTheRecoveryForARunningButUnreachableGuest(t *testing.T) {
	originalRead, originalObserve := readGuestLog, observeGuestState
	defer func() { readGuestLog, observeGuestState = originalRead, originalObserve }()

	readGuestLog = func(context.Context, string, ...string) (string, error) {
		return "", errors.New("cannot exec: container is not running")
	}

	tests := []struct {
		name       string
		observed   container.GuestObservationState
		wantHint   bool
		wantSubstr string
	}{
		{name: "running but unreachable", observed: container.GuestRunning, wantHint: true, wantSubstr: "refuses commands"},
		{name: "genuinely stopped", observed: container.GuestStopped, wantSubstr: "is the guest running?"},
		{name: "runtime unavailable", observed: container.GuestUnavailable, wantSubstr: "is the guest running?"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			observeGuestState = func(context.Context, string) container.GuestObservationState { return tc.observed }
			dir := newLogsTestApp(t)

			_, err := sidingLog(context.Background(), dir, "one", 0)
			if err == nil {
				t.Fatal("expected an error when the guest refuses exec")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantSubstr)
			}
			// The recovery command has to name this channel's binary, or a
			// copy-paste from a dev/nightly build runs the wrong one.
			if tc.wantHint && !strings.Contains(err.Error(), bin()+" up one") {
				t.Fatalf("error = %v, want the `%s up one` recovery hint", err, bin())
			}
		})
	}
}
