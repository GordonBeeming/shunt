package container

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/proc"
)

func TestObserveSystemDiskUsageDoesNotStartStoppedService(t *testing.T) {
	var calls [][]string
	got := observeSystemDiskUsage(context.Background(), func(string) bool { return true }, func(_ context.Context, name string, args ...string) (proc.Result, error) {
		calls = append(calls, append([]string{name}, args...))
		return proc.Result{Stderr: "apiserver is not running"}, errors.New("stopped")
	})

	if got.Observation != "unavailable" || got.Running {
		t.Fatalf("unexpected observation: %+v", got)
	}
	want := [][]string{{Bin, "system", "status"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("commands = %#v, want %#v", calls, want)
	}
}

func TestObserveSystemDiskUsageBoundsEachProbe(t *testing.T) {
	got := observeSystemDiskUsage(context.Background(), func(string) bool { return true }, func(ctx context.Context, _ string, args ...string) (proc.Result, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > systemProbeTimeout || time.Until(deadline) <= 0 {
			return proc.Result{}, errors.New("probe has no usable deadline")
		}
		if len(args) == 2 && args[1] == "status" {
			return proc.Result{Stdout: "running"}, nil
		}
		return proc.Result{}, context.DeadlineExceeded
	})
	if got.Observation != "unavailable" || !got.Running || !strings.Contains(got.Detail, context.DeadlineExceeded.Error()) {
		t.Fatalf("observation = %+v", got)
	}
}

func TestObserveSystemDiskUsageDoesNotMistakeNotRunningForRunning(t *testing.T) {
	var calls int
	got := observeSystemDiskUsage(context.Background(), func(string) bool { return true }, func(_ context.Context, _ string, _ ...string) (proc.Result, error) {
		calls++
		return proc.Result{Stdout: "apiserver is not running"}, nil
	})
	if got.Observation != "unavailable" || got.Running || calls != 1 {
		t.Fatalf("unexpected observation: %+v (calls=%d)", got, calls)
	}
}

func TestObserveSystemDiskUsageExplainsEmptyStoppedStatus(t *testing.T) {
	got := observeSystemDiskUsage(context.Background(), func(string) bool { return true }, func(_ context.Context, _ string, _ ...string) (proc.Result, error) {
		return proc.Result{}, nil
	})
	if got.Observation != "unavailable" || got.Detail != "container system is not running" {
		t.Fatalf("observation = %+v", got)
	}
}

func TestSystemRunningBoundsStatusProbe(t *testing.T) {
	running, err := systemRunningWith(context.Background(), func(ctx context.Context, _ string, _ ...string) (proc.Result, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > systemProbeTimeout || time.Until(deadline) <= 0 {
			return proc.Result{}, errors.New("probe has no usable deadline")
		}
		return proc.Result{Stdout: "running"}, nil
	})
	if err != nil || !running {
		t.Fatalf("SystemRunning() = %t, %v", running, err)
	}
}

func TestObserveSystemKeepsStoppedDistinctFromUnavailable(t *testing.T) {
	stopped := observeSystem(context.Background(), func(string) bool { return true }, func(context.Context, string, ...string) (proc.Result, error) {
		return proc.Result{Stdout: "apiserver is not running"}, nil
	})
	if stopped.State != RuntimeStopped || stopped.Detail == "" {
		t.Fatalf("stopped observation = %+v", stopped)
	}
	unavailable := observeSystem(context.Background(), func(string) bool { return false }, nil)
	if unavailable.State != RuntimeUnavailable || unavailable.Detail == "" {
		t.Fatalf("unavailable observation = %+v", unavailable)
	}
}

func TestObserveSystemRecognizesStoppedServiceFromNonZeroStatus(t *testing.T) {
	observation := observeSystem(context.Background(), func(string) bool { return true }, func(context.Context, string, ...string) (proc.Result, error) {
		return proc.Result{Stderr: "apiserver is not running and not registered with launchd"}, errors.New("exit status 1")
	})
	if observation.State != RuntimeStopped || observation.Detail != "apiserver is not running and not registered with launchd" {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestObserveSystemDiskUsageUsesOfficialJSON(t *testing.T) {
	var calls [][]string
	got := observeSystemDiskUsage(context.Background(), func(string) bool { return true }, func(_ context.Context, name string, args ...string) (proc.Result, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) == 2 && args[1] == "status" {
			return proc.Result{Stdout: "apiserver is running"}, nil
		}
		return proc.Result{Stdout: `[{"type":"Images","reclaimableBytes":42}]`}, nil
	})

	if got.Observation != "observed" || !got.Running {
		t.Fatalf("unexpected observation: %+v", got)
	}
	if string(got.Data) != `[{"type":"Images","reclaimableBytes":42}]` {
		t.Fatalf("data = %s", got.Data)
	}
	want := [][]string{
		{Bin, "system", "status"},
		{Bin, "system", "df", "--format", "json"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("commands = %#v, want %#v", calls, want)
	}
}

func TestObserveSystemDiskUsageRejectsInvalidJSON(t *testing.T) {
	got := observeSystemDiskUsage(context.Background(), func(string) bool { return true }, func(_ context.Context, _ string, args ...string) (proc.Result, error) {
		if len(args) == 2 && args[1] == "status" {
			return proc.Result{Stdout: "running"}, nil
		}
		return proc.Result{Stdout: "not-json"}, nil
	})

	if got.Observation != "unavailable" || !got.Running || got.Detail == "" {
		t.Fatalf("unexpected observation: %+v", got)
	}
}
