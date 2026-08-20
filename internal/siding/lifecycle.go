package siding

import (
	"context"
	"fmt"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/state"
)

var parkRemoveGuest = container.Remove

// RequireGuest prevents guest-only commands from silently materializing a
// small or parked siding. Only Up is allowed to grow it.
func RequireGuest(sd state.Siding) error {
	if sd.MaterializationPhase == "" || sd.MaterializationPhase == state.PhaseGuest {
		return nil
	}
	return fmt.Errorf("siding %q is %s; run `%s up %s` first", sd.Name, sd.MaterializationPhase, config.Current().BinaryName, sd.Name)
}

// Park removes only the recreatable Apple guest. Code, branch, data, and output
// remain in place for the next Up.
func Park(ctx context.Context, app state.App, name string) (state.Siding, error) {
	var parked state.Siding
	err := WithProjectSidingOperation(ctx, app.ConfigDir, name, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		if err := EnsureNoRemovalInProgress(current, "park"); err != nil {
			return err
		}
		if current.LiveSiding == state.HostTarget {
			current.LiveSiding = ""
		}
		sd, ok := current.Sidings[name]
		if !ok {
			return fmt.Errorf("no siding %q", name)
		}
		if current.LiveSiding == name {
			return fmt.Errorf("siding %q is live; switch away before parking it", name)
		}
		if sd.MaterializationPhase == state.PhaseWorktree {
			return fmt.Errorf("siding %q is already worktree-only; there is no guest to park", name)
		}
		if sd.MaterializationPhase == state.PhaseParked {
			parked = sd
			return nil
		}
		if sd.MaterializationPhase == state.PhaseGuest || sd.MaterializationPhase == "" {
			if err := parkRemoveGuest(ctx, sd.Container); err != nil {
				return err
			}
		}
		sd.MaterializationPhase = state.PhaseParked
		sd.Stopped = false
		sd.Bridges = map[string]int{}
		sd.LastIP = ""
		current.Sidings[name] = sd
		if err := state.SaveApp(current); err != nil {
			return err
		}
		parked = sd
		return nil
	})
	return parked, err
}
