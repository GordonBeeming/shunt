package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestObserveGuestReturnsTypedStates(t *testing.T) {
	original := observeGuestInspect
	defer func() { observeGuestInspect = original }()

	tests := []struct {
		name  string
		state string
		err   error
		want  GuestObservationState
	}{
		{name: "running", state: "running", want: GuestRunning},
		{name: "stopped", state: "stopped", want: GuestStopped},
		{name: "unknown-runtime-state", state: "creating", want: GuestUnavailable},
		{name: "absent", err: &guestNotFoundError{name: "guest"}, want: GuestAbsent},
		{name: "ambiguous-not-running-error", err: errors.New("runtime not running"), want: GuestUnavailable},
		{name: "xpc-service-not-found", err: errors.New("XPC service not found"), want: GuestUnavailable},
		{name: "apiserver-does-not-exist", err: errors.New("apiserver does not exist"), want: GuestUnavailable},
		{name: "another-guest", err: &guestNotFoundError{name: "other"}, want: GuestUnavailable},
		{name: "unavailable", err: errors.New("XPC connection invalid"), want: GuestUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			observeGuestInspect = func(context.Context, string) (inspectDoc, error) {
				var doc inspectDoc
				doc.Status.State = tt.state
				return doc, tt.err
			}
			if got := ObserveGuest(context.Background(), "guest").State; got != tt.want {
				t.Fatalf("ObserveGuest() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDestructiveMutationsRequireExactNamedGuestPostcondition(t *testing.T) {
	binDir := t.TempDir()
	script := `#!/bin/sh
case "$SHUNT_MUTATION_CASE:$1" in
  target-absent:stop|target-absent:rm) echo 'mutation failed' >&2; exit 1 ;;
  target-absent:inspect) echo 'container target not found' >&2; exit 1 ;;
  xpc:stop|xpc:rm|xpc:inspect) echo 'XPC service not found' >&2; exit 1 ;;
  other:stop|other:rm|other:inspect) echo 'container other not found' >&2; exit 1 ;;
  remove-*:stop) exit 0 ;;
  remove-target-absent:rm) echo 'mutation failed' >&2; exit 1 ;;
  remove-target-absent:inspect) echo 'container target not found' >&2; exit 1 ;;
  remove-xpc:rm|remove-xpc:inspect) echo 'XPC service not found' >&2; exit 1 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, Bin), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, tt := range []struct {
		name string
		run  func(context.Context, string) error
		ok   bool
	}{
		{name: "target-absent", run: Stop, ok: true},
		{name: "xpc", run: Stop},
		{name: "other", run: Stop},
		{name: "remove-target-absent", run: Remove, ok: true},
		{name: "remove-xpc", run: Remove},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SHUNT_MUTATION_CASE", tt.name)
			err := tt.run(context.Background(), "target")
			if tt.ok && err != nil {
				t.Fatalf("mutation = %v, want typed postcondition success", err)
			}
			if !tt.ok && (err == nil || !strings.Contains(err.Error(), "target")) {
				t.Fatalf("mutation = %v, want fail-closed target error", err)
			}
		})
	}
}

func TestInspectErrorNamesOnlyRequestedAbsentGuest(t *testing.T) {
	for _, message := range []string{
		"container not found: target",
		"Error: container not found: target",
		"error: CONTAINER NOT FOUND: TARGET",
		"container target not found",
		`container "target" not found`,
		"no such container: target",
		"no such container target",
		"container target does not exist",
		`container "target" does not exist`,
	} {
		if !inspectErrorNamesAbsentGuest(errors.New(message), "target") {
			t.Fatalf("target-specific not-found error %q was not recognized", message)
		}
	}
	for _, message := range []string{
		"container not found: other",
		"Error: container not found: target-other",
		"Error: container not found: target and more",
		"Error: container not found: target: detail",
		"XPC service not found",
		"apiserver does not exist",
		"container other not found",
	} {
		if inspectErrorNamesAbsentGuest(errors.New(message), "target") {
			t.Fatalf("unrelated or ambiguous error %q proved target absence", message)
		}
	}
}

func TestSuccessfulMutationsRequireBoundedExactPostconditions(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, Bin), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	originalObserve, originalAttempts, originalPoll := postconditionObserveGuest, postconditionAttempts, postconditionPoll
	defer func() {
		postconditionObserveGuest, postconditionAttempts, postconditionPoll = originalObserve, originalAttempts, originalPoll
	}()
	postconditionAttempts = 3
	postconditionPoll = 0

	t.Run("successful-stop-wrong-state", func(t *testing.T) {
		postconditionObserveGuest = func(context.Context, string) GuestObservation {
			return GuestObservation{State: GuestRunning}
		}
		if err := Stop(context.Background(), "target"); err == nil || !strings.Contains(err.Error(), "state=running") {
			t.Fatalf("Stop() = %v", err)
		}
	})

	t.Run("eventual-stop-state", func(t *testing.T) {
		calls := 0
		postconditionObserveGuest = func(context.Context, string) GuestObservation {
			calls++
			if calls < 3 {
				return GuestObservation{State: GuestRunning}
			}
			return GuestObservation{State: GuestStopped}
		}
		if err := Stop(context.Background(), "target"); err != nil || calls != 3 {
			t.Fatalf("Stop() = %v after %d observations", err, calls)
		}
	})

	t.Run("successful-remove-wrong-state", func(t *testing.T) {
		calls := 0
		postconditionObserveGuest = func(context.Context, string) GuestObservation {
			calls++
			if calls == 1 {
				return GuestObservation{State: GuestStopped}
			}
			return GuestObservation{State: GuestRunning}
		}
		if err := Remove(context.Background(), "target"); err == nil || !strings.Contains(err.Error(), "state=running") {
			t.Fatalf("Remove() = %v", err)
		}
	})
}

func TestHostileCgroupKillGuestNameCannotTriggerForceRemove(t *testing.T) {
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "rm-called")
	script := "#!/bin/sh\nif [ \"$1\" = stop ]; then echo 'ordinary permission failure' >&2; exit 1; fi\nif [ \"$1\" = inspect ]; then echo '[{\"status\":{\"state\":\"running\"}}]'; exit 0; fi\nif [ \"$1\" = rm ]; then touch \"$SHUNT_RM_MARKER\"; fi\n"
	if err := os.WriteFile(filepath.Join(binDir, Bin), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SHUNT_RM_MARKER", marker)
	forced, err := StopOrForce(context.Background(), "hostile-cgroup.kill-name")
	if err == nil || forced {
		t.Fatalf("StopOrForce() = forced %t, %v", forced, err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("hostile name triggered force remove: %v", statErr)
	}
}

func TestSuccessfulForceRemoveStillRequiresAbsentPostcondition(t *testing.T) {
	binDir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = stop ]; then echo 'cgroup.kill operation not supported' >&2; exit 1; fi\nif [ \"$1\" = inspect ]; then echo '[{\"status\":{\"state\":\"running\"}}]'; exit 0; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, Bin), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	originalObserve, originalAttempts, originalPoll := postconditionObserveGuest, postconditionAttempts, postconditionPoll
	defer func() {
		postconditionObserveGuest, postconditionAttempts, postconditionPoll = originalObserve, originalAttempts, originalPoll
	}()
	postconditionObserveGuest = func(context.Context, string) GuestObservation {
		return GuestObservation{State: GuestRunning}
	}
	postconditionAttempts = 2
	postconditionPoll = 0
	forced, err := StopOrForce(context.Background(), "target")
	if err == nil || forced || !strings.Contains(err.Error(), "state=running") {
		t.Fatalf("StopOrForce() = forced %t, %v", forced, err)
	}
}
