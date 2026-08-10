package siding

import (
	"context"
	"fmt"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/state"
)

// StopResult describes the persisted result of stopping one siding guest.
type StopResult struct {
	Siding  state.Siding
	Forced  bool
	WasLive bool
}

var (
	stopOrForce       = container.StopOrForce
	mergeStoppedState = MergeSidingState
)

// Stop is the shared CLI and dashboard stop path. The guest operation and state
// merge run under the same siding lock, while unrelated sidings remain free.
func Stop(ctx context.Context, app state.App, name string) (StopResult, error) {
	if _, err := SidingBase(app, name); err != nil {
		return StopResult{}, err
	}
	var result StopResult
	err := WithSidingOperation(ctx, app.ConfigDir, name, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		if err := EnsureNoRemovalInProgress(current, "stop"); err != nil {
			return err
		}
		sd, ok := current.Sidings[name]
		if !ok {
			return fmt.Errorf("no siding %q", name)
		}
		if err := RequireGuest(sd); err != nil {
			return err
		}
		result.WasLive = current.LiveSiding == name
		result.Forced, err = stopOrForce(ctx, sd.Container)
		if err != nil {
			return err
		}
		sd.Stopped = true
		// Every successful stop terminates the in-guest bridge processes and may
		// assign a different IP when the guest starts again. Never persist routing
		// metadata that can only describe the stopped guest.
		sd.Bridges = nil
		sd.LastIP = ""
		if result.Forced {
			if len(current.Volumes) > 0 {
				sd.MaterializationPhase = state.PhaseData
			} else {
				sd.MaterializationPhase = state.PhaseWorktree
			}
		}
		result.Siding = sd
		if _, err := mergeStoppedState(ctx, current.ConfigDir, sd, false); err != nil {
			return fmt.Errorf("guest stopped, but siding state could not be saved: %w", err)
		}
		return nil
	})
	return result, err
}
