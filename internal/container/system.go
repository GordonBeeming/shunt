// Package container wraps Apple's `container` CLI: starting the runtime and
// managing siding guests (run, inspect, exec, stop, rm).
package container

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/proc"
)

// Bin is the Apple container CLI binary name. proc augments PATH at startup with
// the standard install dirs, so the bare name resolves even under launchd's
// minimal PATH (the dashboard agent).
const Bin = "container"

const systemProbeTimeout = 5 * time.Second

// RuntimeState is the bounded result of observing Apple's container runtime.
// Unavailable means the probe could not establish a running or stopped state.
type RuntimeState string

const (
	RuntimeRunning     RuntimeState = "running"
	RuntimeStopped     RuntimeState = "stopped"
	RuntimeUnavailable RuntimeState = "unavailable"
)

// RuntimeObservation is safe to display in status surfaces. Detail is bounded
// so a failed CLI invocation cannot turn a health response into an error dump.
type RuntimeObservation struct {
	State  RuntimeState `json:"state"`
	Detail string       `json:"detail,omitempty"`
}

// SystemDiskUsage is a read-only observation of Apple's container runtime and
// its authoritative disk accounting. Data is the untouched JSON emitted by
// `container system df --format json`; callers must not manufacture
// reclaimable values from host directory scans when this observation is
// unavailable.
type SystemDiskUsage struct {
	Observation string          `json:"observation"` // observed | unavailable
	Running     bool            `json:"running"`
	Detail      string          `json:"detail,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
}

// EnsureSystemStarted starts the container runtime (apiserver + default
// network) if it isn't already up. `container system start` is idempotent, so
// calling it when already running is harmless.
func EnsureSystemStarted(ctx context.Context) error {
	if !proc.Look(Bin) {
		return fmt.Errorf("%q not found on PATH; install Apple's container CLI", Bin)
	}
	if running, err := systemRunning(ctx); err == nil && running {
		return nil
	}
	if _, err := proc.Run(ctx, Bin, "system", "start"); err != nil {
		return fmt.Errorf("container system start: %w", err)
	}
	return nil
}

// SystemRunning reports whether the container runtime is up (for `shunt status`
// health checks), without starting it.
func SystemRunning(ctx context.Context) bool {
	return ObserveSystem(ctx).State == RuntimeRunning
}

// ObserveSystem reports the runtime's bounded health without starting it.
func ObserveSystem(ctx context.Context) RuntimeObservation {
	return observeSystem(ctx, proc.Look, proc.Run)
}

func observeSystem(ctx context.Context, look func(string) bool, run systemCommandRunner) RuntimeObservation {
	if !look(Bin) {
		return RuntimeObservation{State: RuntimeUnavailable, Detail: "container CLI not found on PATH"}
	}
	probeCtx, cancel := context.WithTimeout(ctx, systemProbeTimeout)
	defer cancel()
	result, err := run(probeCtx, Bin, "system", "status")
	if err != nil {
		statusOutput := strings.Join(nonEmptyStrings(result.Stdout, result.Stderr), "; ")
		if statusExplicitlyStopped(statusOutput) {
			return RuntimeObservation{State: RuntimeStopped, Detail: boundedDetail(statusOutput)}
		}
		detail := strings.Join(nonEmptyStrings(result.Stdout, result.Stderr, err.Error()), "; ")
		return RuntimeObservation{State: RuntimeUnavailable, Detail: boundedDetail(detail)}
	}
	if statusIsRunning(result.Stdout) {
		return RuntimeObservation{State: RuntimeRunning}
	}
	detail := strings.TrimSpace(result.Stdout)
	if detail == "" {
		detail = "container system is not running"
	}
	return RuntimeObservation{State: RuntimeStopped, Detail: boundedDetail(detail)}
}

func statusExplicitlyStopped(output string) bool {
	return strings.Contains(strings.ToLower(output), "not running")
}

// ObserveSystemDiskUsage obtains runtime status and, only when it is already
// running, Apple's official disk-usage report. It never starts the service.
func ObserveSystemDiskUsage(ctx context.Context) SystemDiskUsage {
	return observeSystemDiskUsage(ctx, proc.Look, proc.Run)
}

type systemCommandRunner func(context.Context, string, ...string) (proc.Result, error)

func observeSystemDiskUsage(ctx context.Context, look func(string) bool, run systemCommandRunner) SystemDiskUsage {
	if !look(Bin) {
		return SystemDiskUsage{Observation: "unavailable", Detail: "container CLI not found on PATH"}
	}
	statusCtx, cancelStatus := context.WithTimeout(ctx, systemProbeTimeout)
	status, err := run(statusCtx, Bin, "system", "status")
	cancelStatus()
	if err != nil {
		detail := strings.TrimSpace(strings.Join(nonEmptyStrings(status.Stdout, status.Stderr), "; "))
		if detail == "" {
			detail = err.Error()
		}
		return SystemDiskUsage{Observation: "unavailable", Detail: detail}
	}
	if !statusIsRunning(status.Stdout) {
		detail := strings.TrimSpace(status.Stdout)
		if detail == "" {
			detail = "container system is not running"
		}
		return SystemDiskUsage{Observation: "unavailable", Detail: detail}
	}

	dfCtx, cancelDF := context.WithTimeout(ctx, systemProbeTimeout)
	df, err := run(dfCtx, Bin, "system", "df", "--format", "json")
	cancelDF()
	if err != nil {
		detail := strings.TrimSpace(strings.Join(nonEmptyStrings(df.Stdout, df.Stderr), "; "))
		if detail == "" {
			detail = err.Error()
		}
		return SystemDiskUsage{Observation: "unavailable", Running: true, Detail: detail}
	}
	raw := json.RawMessage(strings.TrimSpace(df.Stdout))
	if len(raw) == 0 || !json.Valid(raw) {
		return SystemDiskUsage{Observation: "unavailable", Running: true, Detail: "container system df returned invalid JSON"}
	}
	return SystemDiskUsage{Observation: "observed", Running: true, Data: append(json.RawMessage(nil), raw...)}
}

func nonEmptyStrings(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func statusIsRunning(output string) bool {
	status := strings.ToLower(output)
	return !strings.Contains(status, "not running") && strings.Contains(status, "running")
}

// systemRunning reports whether the apiserver is up via `container system status`.
func systemRunning(ctx context.Context) (bool, error) {
	return systemRunningWith(ctx, proc.Run)
}

func systemRunningWith(ctx context.Context, run systemCommandRunner) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, systemProbeTimeout)
	defer cancel()
	res, err := run(probeCtx, Bin, "system", "status")
	if err != nil {
		// A non-zero exit here usually means "not running" rather than a hard
		// failure, so treat it as not-running and let the caller start it.
		return false, nil
	}
	return statusIsRunning(res.Stdout), nil
}

func boundedDetail(detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	const maxDetailBytes = 256
	if len(detail) > maxDetailBytes {
		return detail[:maxDetailBytes] + "…"
	}
	return detail
}
