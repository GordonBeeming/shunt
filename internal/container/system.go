// Package container wraps Apple's `container` CLI: starting the runtime and
// managing siding guests (run, inspect, exec, stop, rm).
package container

import (
	"context"
	"fmt"
	"strings"

	"github.com/gordonbeeming/shunt/internal/proc"
)

// Bin is the Apple container CLI binary name.
const Bin = "container"

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

// systemRunning reports whether the apiserver is up via `container system status`.
func systemRunning(ctx context.Context) (bool, error) {
	res, err := proc.Run(ctx, Bin, "system", "status")
	if err != nil {
		// A non-zero exit here usually means "not running" rather than a hard
		// failure, so treat it as not-running and let the caller start it.
		return false, nil
	}
	return strings.Contains(res.Stdout, "running"), nil
}
