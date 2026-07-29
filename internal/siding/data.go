package siding

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/databaseline"
	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/state"
)

const dataRestoreTimeout = 15 * time.Minute

var (
	dataExecGuest       = container.Exec
	dataGuestState      = container.State
	dataEnsureGuestLive = EnsureGuestLive
	dataStopGuest       = container.Stop
)

// DataPromotionLifecycle adapts guest and runner operations to the data
// generation manager without putting runtime policy in the command package.
type DataPromotionLifecycle struct {
	app                   state.App
	sd                    state.Siding
	progress              io.Writer
	wasRunning            bool
	wasLive               bool
	wasGuestStopped       bool
	wasBridged            bool
	captureStartAttempted bool
	stoppedContainers     []string
}

func NewDataPromotionLifecycle(app state.App, sd state.Siding, progress io.Writer) *DataPromotionLifecycle {
	if progress == nil {
		progress = io.Discard
	}
	return &DataPromotionLifecycle{
		app:        app,
		sd:         sd,
		progress:   progress,
		wasLive:    app.LiveSiding == sd.Name,
		wasBridged: len(sd.Bridges) > 0,
	}
}

func (l *DataPromotionLifecycle) Siding() state.Siding { return l.sd }

func (l *DataPromotionLifecycle) Quiesce(ctx context.Context) error {
	fmt.Fprintln(l.progress, "• checking the source siding state…")
	guestState, err := dataGuestState(ctx, l.sd.Container)
	if err != nil {
		return fmt.Errorf("inspect source guest: %w", err)
	}
	appWasRunning := false
	if guestState == "running" {
		appWasRunning, err = ProbeAppRunning(ctx, l.app, l.sd)
		if err != nil {
			return fmt.Errorf("inspect source application: %w", err)
		}
	}
	l.wasGuestStopped = guestState != "running"
	l.wasRunning = !l.wasGuestStopped && appWasRunning
	if l.wasGuestStopped {
		l.captureStartAttempted = true
		if err := dataEnsureGuestLive(ctx, l.sd); err != nil {
			return fmt.Errorf("start guest for data capture: %w", err)
		}
		l.sd.Stopped = false
	}
	if !l.wasRunning {
		return nil
	}
	fmt.Fprintln(l.progress, "• stopping the application for a consistent snapshot…")
	if err := StopApp(ctx, l.app, l.sd); err != nil {
		return fmt.Errorf("stop application: %w", err)
	}
	running, err := ProbeAppRunning(ctx, l.app, l.sd)
	if err != nil {
		return fmt.Errorf("verify application stopped: %w", err)
	}
	if running {
		return errors.New("application is still running after stop")
	}
	return nil
}

func (l *DataPromotionLifecycle) StopVolumeConsumers(ctx context.Context) error {
	args := []string{"docker", "ps", "-q"}
	for _, volume := range l.app.Volumes {
		args = append(args, "--filter", "volume="+volume)
	}
	out, err := dataExecGuest(ctx, l.sd.Container, args...)
	if err != nil {
		return fmt.Errorf("list data-volume consumers: %w", err)
	}
	l.stoppedContainers = append(l.stoppedContainers, strings.Fields(out)...)
	if len(l.stoppedContainers) > 0 {
		fmt.Fprintf(l.progress, "• stopping %d data-volume consumer(s)…\n", len(l.stoppedContainers))
		stopArgs := append([]string{"docker", "stop"}, l.stoppedContainers...)
		if _, err := dataExecGuest(ctx, l.sd.Container, stopArgs...); err != nil {
			return fmt.Errorf("stop data-volume consumers: %w", err)
		}
	}
	out, err = dataExecGuest(ctx, l.sd.Container, args...)
	if err != nil {
		return fmt.Errorf("verify data-volume consumers stopped: %w", err)
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("data-volume consumers are still running: %s", strings.Join(strings.Fields(out), ", "))
	}
	return nil
}

func (l *DataPromotionLifecycle) Sync(ctx context.Context) error {
	if _, err := dataExecGuest(ctx, l.sd.Container, "sync"); err != nil {
		return fmt.Errorf("sync guest data: %w", err)
	}
	return nil
}

func (l *DataPromotionLifecycle) SnapshotHostVolume(ctx context.Context, volume, destination string) error {
	_, volumeRoot, err := Paths(l.app, l.sd.Name)
	if err != nil {
		return err
	}
	source := filepath.Join(volumeRoot, volume)
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("host-backed source for volume %q is unavailable at %s: %w", volume, source, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("host-backed source for volume %q is not a directory: %s", volume, source)
	}
	fmt.Fprintf(l.progress, "• snapshotting data volume %q…\n", volume)
	return fsclone.CloneVolume(ctx, source, destination)
}

func (l *DataPromotionLifecycle) Restore(ctx context.Context) (databaseline.RestoreResult, error) {
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dataRestoreTimeout)
	defer cancel()
	details := []string{}
	var restoreErrs []error

	if len(l.stoppedContainers) > 0 {
		fmt.Fprintf(l.progress, "• restarting %d data-volume consumer(s)…\n", len(l.stoppedContainers))
		args := append([]string{"docker", "start"}, l.stoppedContainers...)
		if _, err := dataExecGuest(restoreCtx, l.sd.Container, args...); err != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("restart data-volume consumers: %w", err))
		} else {
			for _, id := range l.stoppedContainers {
				details = append(details, "restarted Docker container "+id)
			}
		}
	}
	if l.wasRunning {
		fmt.Fprintln(l.progress, "• preparing the restored application…")
		prepared := true
		if err := PrepareGuest(restoreCtx, l.app, l.sd); err != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("prepare application restart: %w", err))
			prepared = false
		}
		if prepared {
			fmt.Fprintln(l.progress, "• restarting the restored application…")
			if err := StartApp(restoreCtx, l.app, l.sd); err != nil {
				restoreErrs = append(restoreErrs, fmt.Errorf("restart application: %w", err))
			} else {
				details = append(details, "restarted application")
				if err := WaitReady(restoreCtx, l.app, l.sd, dataRestoreTimeout); err != nil {
					restoreErrs = append(restoreErrs, fmt.Errorf("wait for restored application: %w", err))
				}
			}
		}
	}
	if l.wasBridged || l.wasLive {
		fmt.Fprintln(l.progress, "• restoring host bridges…")
		if err := Activate(restoreCtx, l.app, &l.sd); err != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("restore bridges: %w", err))
		} else {
			details = append(details, "restored host bridges")
		}
	}
	if l.wasLive {
		fmt.Fprintln(l.progress, "• restoring the live front door…")
		if err := PointCaddy(restoreCtx, l.app, &l.sd); err != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("restore live route: %w", err))
		} else {
			details = append(details, "restored live front door")
		}
	}
	if l.wasGuestStopped && l.captureStartAttempted {
		fmt.Fprintln(l.progress, "• restoring the guest's stopped state…")
		guestState, stateErr := dataGuestState(restoreCtx, l.sd.Container)
		switch {
		case stateErr != nil:
			restoreErrs = append(restoreErrs, fmt.Errorf("inspect guest while restoring stopped state: %w", stateErr))
		case guestState == "running":
			if err := dataStopGuest(restoreCtx, l.sd.Container); err != nil {
				restoreErrs = append(restoreErrs, fmt.Errorf("restore stopped guest: %w", err))
			} else {
				l.sd.Stopped = true
				details = append(details, "restored stopped guest")
			}
		default:
			l.sd.Stopped = true
			details = append(details, "guest remained stopped")
		}
	}
	err := errors.Join(restoreErrs...)
	return databaseline.RestoreResult{Restored: err == nil, Details: details}, err
}

// PromoteData runs promotion under the project operation lock and merges the
// restored siding fields into the latest saved application state.
func PromoteData(ctx context.Context, app state.App, name string, progress io.Writer) (databaseline.Result, error) {
	var result databaseline.Result
	err := WithProjectSidingOperation(ctx, app.ConfigDir, name, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		sd, ok := current.Sidings[name]
		if !ok {
			return fmt.Errorf("no siding %q", name)
		}
		manager, err := databaseline.New(current.ConfigDir, current.Volumes)
		if err != nil {
			return err
		}
		lifecycle := NewDataPromotionLifecycle(current, sd, progress)
		result, err = manager.PromoteWithLifecycle(ctx, name, lifecycle)
		if _, saveErr := MergeSidingState(ctx, current.ConfigDir, lifecycle.Siding(), false); saveErr != nil {
			if err != nil {
				return fmt.Errorf("data promotion failed (%v); saving restored siding state also failed: %w", err, saveErr)
			}
			return fmt.Errorf("data baseline committed, but restored siding state could not be saved: %w", saveErr)
		}
		return err
	})
	return result, err
}

// RollbackData swaps the current and previous generation under the project lock
// and then removes any recoverable transaction leftovers.
func RollbackData(ctx context.Context, app state.App) (databaseline.Result, error) {
	var result databaseline.Result
	err := WithProjectOperation(ctx, app.ConfigDir, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		manager, err := databaseline.New(current.ConfigDir, current.Volumes)
		if err != nil {
			return err
		}
		result, err = manager.RollbackContext(ctx)
		if err != nil {
			return err
		}
		cleanupResult, cleanupErr := manager.Cleanup(ctx)
		result.RecoveryPaths = append(result.RecoveryPaths, cleanupResult.RecoveryPaths...)
		return cleanupErr
	})
	return result, err
}
