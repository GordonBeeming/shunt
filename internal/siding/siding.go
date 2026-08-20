// Package siding orchestrates one application experiment end to end: clone the
// repo, prepare and launch its Apple container guest, bridge application
// endpoints to the guest IP, and point the host Caddy at them.
package siding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/gordonbeeming/shunt/internal/aspire"
	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/contract"
	"github.com/gordonbeeming/shunt/internal/databaseline"
	"github.com/gordonbeeming/shunt/internal/dockerdpolicy"
	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/image"
	"github.com/gordonbeeming/shunt/internal/imagecache"
	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/runner"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/gordonbeeming/shunt/internal/ui"
)

const (
	// Guest-internal ports shunt pins (each guest is isolated, so these are the
	// same across sidings).
	guestDashboardPort = 18888
	guestRSPort        = 18890

	// rsExtPort is the guest-external (0.0.0.0) port the in-guest socat bridge for
	// the resource service listens on. Front-door route bridges instead reuse each
	// route's own port on the guest IP (host == guest); see Activate.
	rsExtPort = 38890

	startedMarker        = "Distributed application started"
	guestImageMarkerPath = "/var/lib/shunt/image-cache.json"
)

var (
	planCachedImages         = imagecache.Plan
	execGuest                = container.Exec
	execGuestStdinFile       = container.ExecStdinFile
	execGuestStdinFileDigest = container.ExecStdinFileDigest
	removeGuest              = container.Remove
	runGuest                 = container.Run
	ensureBaseImage          = image.EnsureBuilt
	stopGuest                = container.Stop
	startGuest               = container.Start
	mergeSiding              = MergeSidingState
	guestRuntimeState        = container.State
	observeGuest             = container.ObserveGuest
	guestCapabilityProbe     = probeGuestCapability
	guestLivenessAttempts    = 20
	guestLivenessPoll        = time.Second
	prepareLifecycle         = PrepareGuest
	stopLifecycleApp         = StopApp
	startLifecycleApp        = StartApp
	waitLifecycleReady       = WaitReady
	upMaterialize            = materialize
	ensureGuestRuntime       = container.EnsureSystemStarted
	upEnsureGuestLive        = EnsureGuestLive
	upResolveFrontDoor       = resolveSidingFrontDoor
	upProbeAppRunning        = ProbeAppRunning
	upPrepareGuest           = PrepareGuest
	upStopApp                = StopApp
	upStartApp               = StartApp
	upWaitReady              = WaitReady
	upActivate               = Activate
	upGuestIP                = container.IP
	upClearAppLog            = func(ctx context.Context, guest string) {
		_, _ = container.Exec(ctx, guest, "sh", "-c", "> "+appLogPath)
	}
	upSiding           = up
	upRestoreLiveRoute = restoreLiveRoute
	cacheWarningWriter = io.Writer(os.Stderr)
	dockerdStartupWait = 30 * time.Second
	dockerdReadyPoll   = 500 * time.Millisecond
)

// WarmTarPath is the per-project content-addressed image-cache directory.
func WarmTarPath(app state.App) string {
	return filepath.Join(app.ConfigDir, "base", "images")
}

// IsWarmed reports whether a project warm cache exists.
func IsWarmed(app state.App) bool {
	_, err := os.Stat(WarmTarPath(app))
	return err == nil
}

// guestEnv is the proven Aspire-13 launch env: unsecured anonymous dashboard (so
// shunt can read the resource service without an API key), all endpoints pinned,
// http transport, in-guest Docker runtime.
func guestEnv(app state.App) map[string]string {
	// Base env for any .NET app (harmless for node): Development so user-secrets
	// load, polling watcher to cross the VM boundary, roll-forward for older TFMs.
	env := map[string]string{
		"DOTNET_USE_POLLING_FILE_WATCHER": "1",
		"ASPNETCORE_ENVIRONMENT":          "Development",
		"DOTNET_ENVIRONMENT":              "Development",
		"DOTNET_ROLL_FORWARD":             "Major",
	}
	// Aspire-only: just tell Aspire to use the in-guest Docker daemon. Everything
	// else — dashboard scheme/auth, ports, transport — comes from the app's own
	// launch profile / config (config-driven, no cleverness, no forced-insecure).
	if app.Runner == "" || app.Runner == runner.Aspire {
		env["DOTNET_ASPIRE_CONTAINER_RUNTIME"] = "docker"
	}
	// App-specific env from the contract (parameters, secrets) wins.
	for k, v := range app.Env {
		env[k] = v
	}
	return env
}

// Spin creates only the managed Git worktree. Up grows it through data and guest
// materialization when the application is actually needed.
func Spin(ctx context.Context, app state.App, name, branch, fromBranch string) (state.Siding, error) {
	if _, err := SidingBase(app, name); err != nil {
		return state.Siding{}, err
	}
	var created state.Siding
	err := WithProjectSidingOperation(ctx, app.ConfigDir, name, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		app = current
		if err := EnsureNoRemovalInProgress(app, "create siding"); err != nil {
			return err
		}
		if _, exists := app.Sidings[name]; exists {
			return fmt.Errorf("siding %q already exists", name)
		}
		created, err = spin(ctx, app, name, branch, fromBranch)
		if err != nil {
			return errors.Join(err, cleanupFailedSpin(app, name, created.Container, created.Branch, fromBranch != ""))
		}
		created.CreatedAt = time.Now().Format(time.RFC3339)
		if app.BaseSiding == "" && len(app.Sidings) == 0 {
			src, _, pathErr := Paths(app, name)
			if pathErr != nil {
				return pathErr
			}
			commit, commitErr := proc.Run(ctx, "git", "-C", src, "rev-parse", "--verify", "HEAD^{commit}")
			if commitErr != nil {
				return commitErr
			}
			pinned, pinErr := fsclone.PinBaseCommit(ctx, app.ControlRepoPath, created.WorktreeRepoPath, strings.TrimSpace(commit.Stdout))
			if pinErr != nil {
				return pinErr
			}
			app.BaseSiding, app.BaseCommit = name, pinned
		}
		app.Sidings[name] = created
		if err := state.SaveApp(app); err != nil {
			if isCommittedStatePublication(err) {
				return err
			}
			return errors.Join(err, cleanupFailedSpin(app, name, created.Container, created.Branch, fromBranch != ""))
		}
		return nil
	})
	return created, err
}

func spin(ctx context.Context, app state.App, name, branch, fromBranch string) (state.Siding, error) {
	src, _, err := Paths(app, name)
	if err != nil {
		return state.Siding{}, err
	}
	owner := app.ControlRepoPath
	if owner == "" {
		owner = app.RepoPath
	}
	wtBranch := config.BranchPrefix() + name
	if fromBranch != "" {
		wtBranch = fromBranch
		if _, err := fsclone.AddWorktreeFromRemoteBranch(ctx, owner, src, fromBranch); err != nil {
			return state.Siding{}, err
		}
	} else if err := fsclone.AddWorktree(ctx, owner, src, wtBranch, branch); err != nil {
		return state.Siding{}, err
	}
	return state.Siding{Name: name, Branch: wtBranch, WorktreeRepoPath: owner,
		MaterializationPhase: state.PhaseWorktree, Container: config.ContainerName(app.Name, name),
		RSPort: guestRSPort, Bridges: map[string]int{}}, nil
}

func cleanupFailedSpin(app state.App, name, guest, branch string, keepBranch bool) error {
	if _, err := SidingBase(app, name); err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var cleanupErrs []error
	if guest == "" {
		guest = config.ContainerName(app.Name, name)
	}
	src, _, err := Paths(app, name)
	if err != nil {
		cleanupErrs = append(cleanupErrs, err)
		return errors.Join(cleanupErrs...)
	}
	removeBranch := branch
	if keepBranch {
		removeBranch = ""
	} else if removeBranch == "" {
		removeBranch = config.BranchPrefix() + name
	}
	owner := app.ControlRepoPath
	if owner == "" {
		owner = app.RepoPath
	}
	if err := fsclone.RemoveWorktree(cleanupCtx, owner, src, removeBranch); err != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("remove failed worktree: %w", err))
	}
	if err := RemoveFiles(app, name); err != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("remove failed siding files: %w", err))
	}
	return errors.Join(cleanupErrs...)
}

// Up brings a siding online: it makes sure the guest is live, starts the app if
// it isn't already running, and (when bridge is true) bridges its routes to the
// host. Progress lines are written to progress (os.Stdout for the CLI, io.Discard
// for the dashboard, which runs it async and polls status). Up persists the latest
// siding state and restores the front-door route when this siding was already live.
func Up(ctx context.Context, app state.App, sd state.Siding, bridge bool, progress io.Writer) (state.Siding, error) {
	var updated state.Siding
	wasLive := false
	err := WithSidingOperation(ctx, app.ConfigDir, sd.Name, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		if err := EnsureNoRemovalInProgress(current, "up"); err != nil {
			return err
		}
		latest, ok := current.Sidings[sd.Name]
		if !ok {
			return fmt.Errorf("no siding %q", sd.Name)
		}
		wasLive = current.LiveSiding == sd.Name
		updated, err = upSiding(ctx, current, latest, bridge, progress)
		if err != nil {
			return err
		}
		_, err = MergeSidingState(ctx, current.ConfigDir, updated, false)
		return err
	})
	if err == nil && bridge && wasLive {
		err = upRestoreLiveRoute(ctx, app.ConfigDir, sd.Name)
	}
	return updated, err
}

func restoreLiveRoute(ctx context.Context, configDir, name string) error {
	return WithProjectOperation(ctx, configDir, func() error {
		current, err := state.LoadApp(configDir)
		if err != nil {
			return err
		}
		if err := EnsureNoRemovalInProgress(current, "restore live route"); err != nil {
			return err
		}
		if current.LiveSiding != "" && current.LiveSiding != name {
			return nil
		}
		return switchLocked(ctx, &current, name)
	})
}

func up(ctx context.Context, app state.App, sd state.Siding, bridge bool, progress io.Writer) (state.Siding, error) {
	var err error
	if err := ensureGuestRuntime(ctx); err != nil {
		return sd, fmt.Errorf("start container runtime: %w", err)
	}
	sd, err = upMaterialize(ctx, app, sd, progress)
	if err != nil {
		return sd, err
	}
	fmt.Fprintln(progress, "• checking the guest is up…")
	if err := upEnsureGuestLive(ctx, sd); err != nil {
		// A cancelled/timed-out context is the caller giving up, not a broken guest —
		// surface it rather than recreating.
		if ctx.Err() != nil {
			return sd, err
		}
		var recreateRequired *GuestRecreateRequiredError
		if !errors.As(err, &recreateRequired) {
			return sd, err
		}
		// The guest container is missing or wedged (e.g. a cgroup-kill wedge left it
		// un-startable). Recreate it in place from saved settings — keeps the worktree,
		// branch, and data clones — so `up` self-heals instead of making the user
		// reapply by hand.
		fmt.Fprintf(progress, "• guest wouldn't come up (%v) — recreating it (keeps code + data)…\n", err)
		healed, rerr := recreate(ctx, app, sd, false)
		if rerr != nil {
			return sd, fmt.Errorf("%w; auto-recreate also failed: %v", err, rerr)
		}
		sd = healed
		// Persist the new container reference + cleared bridges now, so a failure in a
		// later step doesn't leave disk state pointing at the old (removed) guest.
		if _, saveErr := MergeSidingState(ctx, app.ConfigDir, sd, false); saveErr != nil {
			return sd, fmt.Errorf("save auto-recreated guest state: %w", saveErr)
		}
		if err := upEnsureGuestLive(ctx, sd); err != nil {
			return sd, err
		}
	}
	// The guest is live again — clear any `stopped` marker `kill` left behind.
	sd.Stopped = false
	// Resolve this siding's own front door from its worktree contract (the guest runs
	// the siding's code), so a route it declares applies without an `app add` in root.
	// Always assign — nil (no contract) clears a stale set so EffRoutes falls back to
	// the app-level routes; a broken contract is a warning here, not a hard failure.
	fd, ferr := upResolveFrontDoor(app, sd)
	if ferr != nil {
		fmt.Fprintf(progress, "  (siding front door: %v — using the app-level routes)\n", ferr)
	}
	sd.FrontDoor = fd

	// Only (re)launch the app when it isn't already serving. A running AppHost that
	// never becomes healthy (its child projects all crashed) is half-dead — rebuild
	// it rather than skip. A genuinely healthy live app is left alone so a re-up just
	// re-activates instead of starting a second AppHost (port clash).
	appRunning, err := upProbeAppRunning(ctx, app, sd)
	if err != nil {
		return sd, fmt.Errorf("inspect application state: %w", err)
	}
	needStart := !appRunning
	if !needStart {
		fmt.Fprintln(progress, "• app process is up — checking it's actually serving…")
		if healthyWithin(ctx, app, sd, 20*time.Second) {
			fmt.Fprintf(progress, "• the app is already running + serving in %q\n", sd.Name)
		} else {
			fmt.Fprintln(progress, "• app is up but not serving (health check failed) — rebuilding it")
			needStart = true
		}
	}
	if needStart {
		fmt.Fprintln(progress, "• preparing offline Docker images + data volumes…")
		if err := upPrepareGuest(ctx, app, sd); err != nil {
			return sd, err
		}
		if err := upStopApp(ctx, app, sd); err != nil {
			return sd, err
		}
		upClearAppLog(ctx, sd.Container)

		fmt.Fprintf(progress, "• starting the app in %q…\n", sd.Name)
		if err := upStartApp(ctx, app, sd); err != nil {
			return sd, err
		}
		// Non-blocking: the app keeps building in the background; the eager bridges
		// serve each route as it comes up.
		fmt.Fprintln(progress, "• starting the app (it keeps building in the background)…")
		_ = upWaitReady(ctx, app, sd, 45*time.Second)
	}

	if !bridge {
		if ip, e := upGuestIP(ctx, sd.Container); e == nil {
			sd.LastIP = ip
		}
		return sd, nil
	}
	fmt.Fprintln(progress, "• discovering endpoints + bridging to the host…")
	if err := upActivate(ctx, app, &sd); err != nil {
		return sd, err
	}
	return sd, nil
}

// GuestRecreateRequiredError carries typed evidence that replacing the named
// guest is safe. Untyped runtime/control-plane errors must never trigger it.
type GuestRecreateRequiredError struct {
	Name   string
	Reason string
	Err    error
}

func (e *GuestRecreateRequiredError) Error() string {
	return fmt.Sprintf("guest for %q requires recreation (%s): %v", e.Name, e.Reason, e.Err)
}
func (e *GuestRecreateRequiredError) Unwrap() error { return e.Err }

func materialize(ctx context.Context, app state.App, sd state.Siding, progress io.Writer) (state.Siding, error) {
	phase := sd.MaterializationPhase
	if phase == "" {
		phase = state.PhaseGuest
	}
	if phase == state.PhaseWorktree {
		fmt.Fprintln(progress, "• creating the siding data set…")
		src, volRoot, err := Paths(app, sd.Name)
		if err != nil {
			return sd, err
		}
		if len(app.Volumes) > 0 {
			if err := EnsureVolumeBaselines(ctx, app); err != nil {
				return sd, err
			}
			manager, err := databaseline.New(app.ConfigDir, app.Volumes)
			if err != nil {
				return sd, err
			}
			if _, err := manager.ResetVolumeRoot(ctx, volRoot); err != nil {
				return sd, err
			}
		}
		if err := os.MkdirAll(filepath.Join(filepath.Dir(src), "out"), 0o755); err != nil {
			return sd, err
		}
		sd.MaterializationPhase = state.PhaseData
		if _, err := MergeSidingState(ctx, app.ConfigDir, sd, false); err != nil {
			return sd, err
		}
		phase = state.PhaseData
	}
	if phase == state.PhaseData || phase == state.PhaseParked {
		fmt.Fprintln(progress, "• creating the guest…")
		created, err := recreate(ctx, app, sd, false)
		if err != nil {
			return sd, err
		}
		sd = created
		sd.MaterializationPhase = state.PhaseGuest
		if _, err := MergeSidingState(ctx, app.ConfigDir, sd, false); err != nil {
			return sd, err
		}
	}
	return sd, nil
}

// appLogPath is where StartApp redirects the AppHost output inside the guest, so
// WaitStarted can read it (the AppHost runs as a detached exec, not pid 1, so it
// doesn't show up in `container logs`).
const appLogPath = "/var/log/apphost.log"

// EnsureGuestLive makes sure the guest actually runs — not just per stale
// metadata. After a host sleep/reboot the Apple container runtime can report a
// guest as "running" while its VM is dead, so `container exec` fails with
// "cannot exec: container is not running". Probe with a trivial exec; if it
// fails, bounce the guest (stop+start). That's non-destructive: the worktree and
// data volumes are host bind mounts and the guest's own disk (incl. loaded
// dependency images) persists across stop/start, so no work is lost — it just
// re-runs the entrypoint (dockerd + dev cert). Returns an error only if the guest
// genuinely can't be brought back (e.g. it no longer exists → the caller says new).
func EnsureGuestLive(ctx context.Context, sd state.Siding) error {
	if _, err := execGuest(ctx, sd.Container, "true"); err == nil {
		match, err := guestCapabilityProbe(ctx, sd.Container)
		if err != nil {
			return fmt.Errorf("check guest capability for %q: %w", sd.Name, err)
		}
		if !match {
			return &GuestRecreateRequiredError{Name: sd.Name, Reason: "stale base image", Err: errors.New("capability predicate mismatch")}
		}
		return nil
	}
	observation := observeGuest(ctx, sd.Container)
	if observation.State == container.GuestAbsent {
		return &GuestRecreateRequiredError{Name: sd.Name, Reason: "guest absent", Err: errors.New("named guest is absent")}
	}
	if observation.State == container.GuestUnavailable {
		return fmt.Errorf("inspect guest for %q after liveness failure: runtime unavailable", sd.Name)
	}
	if err := stopGuest(ctx, sd.Container); err != nil {
		return fmt.Errorf("stop guest for %q after liveness failure: %w", sd.Name, err)
	}
	if err := startGuest(ctx, sd.Container); err != nil {
		if observeGuest(ctx, sd.Container).State == container.GuestAbsent {
			return &GuestRecreateRequiredError{Name: sd.Name, Reason: "guest disappeared during restart", Err: err}
		}
		return fmt.Errorf("guest for %q wouldn't restart: %w", sd.Name, err)
	}
	for i := 0; i < guestLivenessAttempts; i++ {
		if _, err := execGuest(ctx, sd.Container, "true"); err == nil {
			match, err := guestCapabilityProbe(ctx, sd.Container)
			if err != nil {
				return fmt.Errorf("check guest capability for %q after restart: %w", sd.Name, err)
			}
			if !match {
				return &GuestRecreateRequiredError{Name: sd.Name, Reason: "stale base image", Err: errors.New("capability predicate mismatch")}
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(guestLivenessPoll):
		}
	}
	return fmt.Errorf("guest for %q did not become reachable after restart; runtime state remains uncertain", sd.Name)
}

func probeGuestCapability(ctx context.Context, guest string) (bool, error) {
	args := image.GuestCapabilityCheck()
	if len(args) < 4 || args[0] != "sh" || args[1] != "-c" {
		return false, errors.New("invalid guest capability predicate")
	}
	const match = "shunt-capability:match"
	const mismatch = "shunt-capability:mismatch"
	wrapped := "if " + args[2] + "; then printf '" + match + "'; else printf '" + mismatch + "'; fi"
	probeArgs := append([]string{"sh", "-c", wrapped}, args[3:]...)
	out, err := execGuest(ctx, guest, probeArgs...)
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(out) {
	case match:
		return true, nil
	case mismatch:
		return false, nil
	default:
		return false, fmt.Errorf("unexpected guest capability response")
	}
}

// EnsureDockerd makes sure the in-guest Docker daemon is healthy before Aspire
// needs it. A guest that was stopped and started again often fails to re-start
// dockerd from its entrypoint (stale pid/socket state), leaving it dead even
// though the guest is "running" — Aspire then reports the runtime unhealthy. This
// clears the stale state and (re)starts dockerd, waiting for it to answer.
func EnsureDockerd(ctx context.Context, sd state.Siding) error {
	check := fmt.Sprintf("test -s %s && grep -qx 'version=%s' %s && docker info >/dev/null 2>&1", dockerdpolicy.ReadyMarker, dockerdpolicy.PolicyVersion, dockerdpolicy.ReadyMarker)
	deadline := time.Now().Add(dockerdStartupWait)
	for {
		if _, err := execGuest(ctx, sd.Container, "sh", "-c", check); err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		timer := time.NewTimer(dockerdReadyPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if _, err := execGuest(ctx, sd.Container, dockerdpolicy.EnsureCommand); err != nil {
		return fmt.Errorf("ensure in-guest Docker offline policy: %w", err)
	}
	if _, err := execGuest(ctx, sd.Container, "sh", "-c", check); err != nil {
		return fmt.Errorf("verify in-guest Docker offline policy marker: %w", err)
	}
	return nil
}

// StartApp starts the configured runner inside an already-prepared guest. Image
// assurance and loading happen before this call. Output goes to appLogPath and
// the guest environment configured by Spin is inherited by the process.
//
// It runs plain `dotnet run` for reliability. `dotnet watch` was tried for
// hot-reload, but its static-web-assets watcher chokes on Linux under the .NET 10
// preview SDK (looks for an `obj\Debug\…` Windows path and blocks the UI/API
// projects from starting). `shunt restart` (StopApp + StartApp) is the rebuild
// path — it keeps the guest + dockerd + dependency containers + data running.
func StartApp(ctx context.Context, app state.App, sd state.Siding) error {
	var runCmd string
	if app.Runner == "" || app.Runner == runner.Aspire {
		// Run the AppHost via the Aspire CLI — `aspire start` runs it managed in the
		// background and reuses/does not stack a second instance on a running one.
		// (`dotnet run` launched a fresh AppHost every time, which piled up competing
		// instances on the fixed ports.) Each project still binds its launchSettings
		// ports, which the contract front-doors host==guest.
		// Raise the CLI's start timeout well past its 120s default — a heavy app's
		// cold start (restore + build + many resources) blows through it, and
		// `aspire start` would otherwise give up with "Timed out waiting … for
		// AppHost to start" before the front door has anything to serve. Default 1800s
		// (30m) for big solutions; overridable via ASPIRE_CLI_START_TIMEOUT in the
		// contract's env (the ${VAR:-default} keeps a contract-set value winning).
		runCmd = fmt.Sprintf(`export PATH="$PATH:/root/.dotnet/tools"; export ASPIRE_CLI_START_TIMEOUT="${ASPIRE_CLI_START_TIMEOUT:-1800}"; cd /workspace && aspire start --apphost "%s" --non-interactive > %s 2>&1`,
			app.AppHostPath, appLogPath)
	} else {
		_, err := container.Exec(ctx, sd.Container, "/bin/sh", "-lc", nonAspireStartScript(app))
		return err
	}
	return container.ExecDetached(ctx, sd.Container, "/bin/sh", "-lc", runCmd)
}

// aspirePortHex are the Aspire host ports (dashboard 18888, OTLP 18889, resource
// service 18890, MCP 18891) as the hex used in /proc/net/tcp local addresses.
const aspirePortHex = "49C8 49C9 49CA 49CB"

// dcp is Aspire's orchestrator: one `start-apiserver`, one `run-controllers`,
// and a `monitor-process` per resource. It outlives the AppHost that spawned it,
// and its API server binds a random port, so the fixed-port sweep below cannot
// reach it. Left running, the tree survives a restart and collides with the DCP
// the next AppHost starts, which hangs the app and eventually wedges the guest.
const aspireProcessKillScript = `pkill -9 -x dotnet 2>/dev/null
pkill -9 -x aspire 2>/dev/null
pkill -9 -x dcp 2>/dev/null`

// StopApp stops the app inside the guest WITHOUT touching the guest, dockerd, or
// the dependency containers (which run under dockerd on other ports), so a
// rebuild keeps SQL etc. + their data up.
//
// First it runs the app's configured clean-stop command if any (e.g. `aspire
// stop`). Then — always, as a fallback, because `dotnet run`/`watch` RESPAWN the
// compiled AppHost binary when killed — it SIGKILLs the run/watch wrappers so
// nothing respawns and frees the Aspire host ports by finding whatever holds
// them via the socket inode in /proc/net/tcp (name-agnostic; catches the
// compiled AppHost binary and the dashboard). That sweep only covers the fixed
// ports in aspirePortHex, which is why the process kills above have to name
// every executable that can outlive the AppHost on a port of its own choosing.
func StopApp(ctx context.Context, app state.App, sd state.Siding) error {
	if app.Runner != "" && app.Runner != runner.Aspire {
		var stopErrs []error
		if app.Stop != "" {
			wd := "/workspace"
			if app.Workdir != "" {
				wd += "/" + app.Workdir
			}
			if _, err := container.Exec(ctx, sd.Container, "/bin/sh", "-lc", fmt.Sprintf("cd %s && %s", shellQuote(wd), app.Stop)); err != nil {
				stopErrs = append(stopErrs, fmt.Errorf("run configured stop command: %w", err))
			}
		}
		if _, err := container.Exec(ctx, sd.Container, "/bin/sh", "-lc", nonAspireStopScript()); err != nil {
			stopErrs = append(stopErrs, fmt.Errorf("stop process group: %w", err))
		}
		return errors.Join(stopErrs...)
	}
	var stopErrs []error
	// Clean stop first (best-effort) — the force-kill below is the safety net.
	if app.Runner == "" || app.Runner == runner.Aspire {
		// Match `aspire start`: `aspire stop` cleanly shuts down the managed AppHost.
		// With a single managed instance it doesn't prompt, so it runs non-interactively.
		_, _ = container.Exec(ctx, sd.Container, "/bin/sh", "-lc",
			`export PATH="$PATH:/root/.dotnet/tools"; cd /workspace && aspire stop --non-interactive`)
	}
	if app.Stop != "" {
		wd := "/workspace"
		if app.Workdir != "" {
			wd = "/workspace/" + app.Workdir
		}
		if _, err := container.Exec(ctx, sd.Container, "/bin/sh", "-lc", fmt.Sprintf("cd %s && %s", shellQuote(wd), app.Stop)); err != nil {
			stopErrs = append(stopErrs, fmt.Errorf("run configured stop command: %w", err))
		}
	}
	script := `
# Reap the whole managed app tree. ` + "`aspire start`" + ` detaches the AppHost plus its
# dotnet build / MSBuild worker nodes / VBCSCompiler, which aren't ` + "`dotnet run`/`watch`" + ` and
# don't hold the app ports while still building — so the old targeted kill left them
# to pile up across re-ups (26 deep once), thrashing the guest until nothing could
# finish. In a shunt guest every dotnet/aspire process is the app, so a broad kill is
# safe and actually reaps the tree.
# Match executable names instead of full command lines. The script itself is
# passed inline to sh -c, so pkill -f also matches and SIGKILLs this shell.
` + aspireProcessKillScript + `
for hex in ` + aspirePortHex + `; do
  for f in /proc/net/tcp /proc/net/tcp6; do
    ino=$(awk -v h=":$hex" '$2 ~ h"$" {print $10; exit}' "$f" 2>/dev/null)
    [ -n "$ino" ] || continue
    for d in /proc/[0-9]*/fd; do
      ls -l "$d" 2>/dev/null | grep -q "socket:\[$ino\]" && kill -9 "$(echo "$d" | cut -d/ -f3)" 2>/dev/null
    done
  done
done
# kill -9 releases the listen socket asynchronously, so wait until the Aspire
# ports are actually free before returning — otherwise the next StartApp races
# onto a port that's still mid-release and aborts with "address already in use".
for i in $(seq 1 15); do
  busy=
  for hex in ` + aspirePortHex + `; do
    cat /proc/net/tcp /proc/net/tcp6 2>/dev/null | awk '{print $2}' | grep -iq ":$hex" && busy=1
  done
  [ -z "$busy" ] && break
  sleep 1
done
busy=
for hex in ` + aspirePortHex + `; do
  cat /proc/net/tcp /proc/net/tcp6 2>/dev/null | awk '{print $2}' | grep -iq ":$hex" && busy=1
done
[ -z "$busy" ] || { echo "Aspire process tree still owns a managed port after SIGKILL" >&2; exit 1; }
true`
	if _, err := container.Exec(ctx, sd.Container, "sh", "-c", script); err != nil {
		stopErrs = append(stopErrs, fmt.Errorf("force-stop application processes: %w", err))
	}
	return errors.Join(stopErrs...)
}

func nonAspireStartScript(app state.App) string {
	wd := "/workspace"
	if app.Workdir != "" {
		wd += "/" + app.Workdir
	}
	run := fmt.Sprintf("cd %s && exec %s", shellQuote(wd), app.Start)
	return `if [ -s /run/shunt-app.pid ]; then
  old=$(cat /run/shunt-app.pid)
  if /bin/kill -0 -- "-$old" 2>/dev/null; then
    echo "shunt app process group $old is already running" >&2
    exit 1
  fi
  rm -f /run/shunt-app.pid
fi
setsid /bin/sh -lc ` + shellQuote(run) + " > " + shellQuote(appLogPath) + ` 2>&1 &
pid=$!
tmp=$(mktemp /run/.shunt-app.pid.XXXXXX)
printf '%s\n' "$pid" > "$tmp"
mv -f "$tmp" /run/shunt-app.pid`
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func nonAspireStopScript() string {
	return `pid=$(cat /run/shunt-app.pid 2>/dev/null) || exit 0
/bin/kill -TERM -- "-$pid" 2>/dev/null || true
for i in $(seq 1 50); do /bin/kill -0 -- "-$pid" 2>/dev/null || { rm -f /run/shunt-app.pid; exit 0; }; sleep .1; done
/bin/kill -KILL -- "-$pid" 2>/dev/null || true
for i in $(seq 1 50); do /bin/kill -0 -- "-$pid" 2>/dev/null || { rm -f /run/shunt-app.pid; exit 0; }; sleep .1; done
echo "process group $pid is still running after SIGKILL" >&2
exit 1`
}

// LoadWarm imports only cache refs that are absent or changed in this guest. It
// verifies every ref in the selected generation in one Docker operation before
// atomically recording the marker that future preparations compare against.
func LoadWarm(ctx context.Context, app state.App, sd state.Siding) (bool, error) {
	if len(app.PrebakeImages) == 0 && len(app.PrebakeBuilds) == 0 {
		return false, nil
	}
	cachePath := WarmTarPath(app)
	if _, err := os.Stat(cachePath); err != nil {
		return false, err
	}
	marker, err := readGuestImageMarker(ctx, sd)
	if err != nil {
		return false, err
	}
	imageIDs, err := guestImageIDs(ctx, app, sd)
	if err != nil {
		return false, err
	}
	plan, err := planCachedImages(ctx, cachePath, imagecache.GuestState{Marker: marker, ImageIDs: imageIDs})
	if err != nil {
		return false, err
	}
	defer plan.Release()
	for _, image := range plan.Images {
		digest, err := execGuestStdinFileDigest(ctx, sd.Container, image.Path, "docker", "load")
		if err != nil {
			return false, fmt.Errorf("load cached image %q: %w", image.Ref, err)
		}
		if digest != image.Checksum {
			return false, fmt.Errorf("verify cached image %q while loading: checksum %s, want %s", image.Ref, digest, image.Checksum)
		}
	}
	imageIDs, err = guestImageIDs(ctx, app, sd)
	if err != nil {
		return false, fmt.Errorf("verify cached image generation %q: %w", plan.Generation, err)
	}
	configured, err := configuredImageRefs(app)
	if err != nil {
		return false, fmt.Errorf("verify cached image generation %q: %w", plan.Generation, err)
	}
	for _, ref := range configured {
		if strings.TrimSpace(imageIDs[ref]) == "" {
			return false, fmt.Errorf("verify cached image generation %q: image %q is unavailable after load", plan.Generation, ref)
		}
	}
	observedMarker := imagecache.GuestMarker{Generation: plan.Generation, Images: imageIDs, Digests: plan.Marker.Digests}
	if err := writeGuestImageMarker(ctx, sd, observedMarker); err != nil {
		return false, err
	}
	return len(plan.Images) > 0, nil
}

func guestImageIDs(ctx context.Context, app state.App, sd state.Siding) (map[string]string, error) {
	configured, err := configuredImageRefs(app)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(configured))
	if len(configured) == 0 {
		return result, nil
	}
	out, err := execGuest(ctx, sd.Container, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, fmt.Errorf("list guest images: %w", err)
	}
	present := make(map[string]bool, len(configured))
	for _, ref := range strings.Fields(out) {
		parsed, parseErr := name.ParseReference(ref)
		if parseErr == nil {
			present[parsed.Name()] = true
		}
	}
	existing := make([]string, 0, len(configured))
	for _, ref := range configured {
		if present[ref] {
			existing = append(existing, ref)
		}
	}
	if len(existing) == 0 {
		return result, nil
	}
	args := append([]string{"docker", "image", "inspect", "--format", "{{.Id}}"}, existing...)
	out, err = execGuest(ctx, sd.Container, args...)
	if err != nil {
		return nil, fmt.Errorf("inspect guest image IDs: %w", err)
	}
	ids := strings.Fields(out)
	if len(ids) != len(existing) {
		return nil, fmt.Errorf("inspect guest image IDs returned %d values for %d refs", len(ids), len(existing))
	}
	for i, ref := range existing {
		result[ref] = ids[i]
	}
	return result, nil
}

func readGuestImageMarker(ctx context.Context, sd state.Siding) (imagecache.GuestMarker, error) {
	out, err := execGuest(ctx, sd.Container, "sh", "-c", "test ! -f "+guestImageMarkerPath+" || cat "+guestImageMarkerPath)
	if err != nil {
		return imagecache.GuestMarker{}, fmt.Errorf("read guest image-cache marker: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return imagecache.GuestMarker{}, nil
	}
	var marker imagecache.GuestMarker
	if err := json.Unmarshal([]byte(out), &marker); err != nil {
		return imagecache.GuestMarker{}, nil
	}
	return marker, nil
}

func writeGuestImageMarker(ctx context.Context, sd state.Siding, marker imagecache.GuestMarker) error {
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("encode guest image-cache marker: %w", err)
	}
	markerFile, err := os.CreateTemp("", "shunt-image-marker-*.json")
	if err != nil {
		return fmt.Errorf("create guest image-cache marker temp: %w", err)
	}
	markerPath := markerFile.Name()
	defer os.Remove(markerPath)
	if _, err := markerFile.Write(data); err != nil {
		_ = markerFile.Close()
		return fmt.Errorf("write guest image-cache marker temp: %w", err)
	}
	if err := markerFile.Close(); err != nil {
		return fmt.Errorf("close guest image-cache marker temp: %w", err)
	}
	script := "set -e; mkdir -p /var/lib/shunt; tmp=$(mktemp /var/lib/shunt/.image-cache.XXXXXX); cat >\"$tmp\"; chmod 600 \"$tmp\"; mv -f \"$tmp\" " + guestImageMarkerPath
	if err := execGuestStdinFile(ctx, sd.Container, markerPath, "sh", "-c", script); err != nil {
		return fmt.Errorf("record guest image-cache generation: %w", err)
	}
	return nil
}

// PrepareGuest makes the guest's Docker state ready before any application
// runner can start.
func PrepareGuest(ctx context.Context, app state.App, sd state.Siding) error {
	if err := AssureImageCache(ctx, app); err != nil {
		return err
	}
	return prepareGuestFromCache(ctx, app, sd)
}

func prepareGuestFromCache(ctx context.Context, app state.App, sd state.Siding) error {
	if err := EnsureDockerd(ctx, sd); err != nil {
		return err
	}
	if err := CreateBindVolumes(ctx, app, sd); err != nil {
		return err
	}
	if _, err := LoadWarm(ctx, app, sd); err != nil {
		return fmt.Errorf("load dependency image cache: %w", err)
	}
	return EnsureDockerd(ctx, sd)
}

// AssureImageCache fetches only configured tags missing from the project's
// archive. A complete archive is reused without a registry call.
func AssureImageCache(ctx context.Context, app state.App) error {
	if len(app.PrebakeImages) == 0 && len(app.PrebakeBuilds) == 0 {
		return nil
	}
	if _, err := assureImageSources(ctx, WarmTarPath(app), app.PrebakeImages, localBuildSources(app)); err != nil {
		var cleanupErr *imagecache.CommittedCleanupError
		if errors.As(err, &cleanupErr) {
			fmt.Fprintf(cacheWarningWriter, "warning: dependency image cache published, but automatic collection failed at %s: %v\n", WarmTarPath(app), cleanupErr)
			return nil
		}
		return fmt.Errorf("assure dependency image cache: %w", err)
	}
	return nil
}

// WaitStarted blocks until the AppHost log says the app started, the guest exits,
// or the deadline passes. It renders progress in a compact live region that
// collapses to a one-line summary on success and stays visible (last few lines)
// on failure, with the full log available via `shunt logs`.
func WaitStarted(ctx context.Context, guestName string, timeout time.Duration) error {
	tail := ui.NewLiveTail(5)
	logHint := fmt.Sprintf("see `%s logs %s` for the full log", config.Current().BinaryName, guestNameToSiding(guestName))
	deadline := time.Now().Add(timeout)
	start := time.Now()
	shown := 0
	var dashSince time.Time // when the Aspire dashboard first came up
	for time.Now().Before(deadline) {
		st, err := container.State(ctx, guestName)
		if err == nil && st != "running" {
			tail.Freeze()
			return fmt.Errorf("guest exited (state=%s) before the app started — %s", st, logHint)
		}
		out, _ := container.Exec(ctx, guestName, "sh", "-c", "cat "+appLogPath+" 2>/dev/null")
		lines := strings.Split(out, "\n")
		var fresh []string
		if len(lines) > shown {
			fresh = lines[shown:]
			shown = len(lines)
		}
		tail.Update(fmt.Sprintf("⏳ starting Aspire… (%s)", time.Since(start).Round(time.Second)), fresh)

		if strings.Contains(out, startedMarker) {
			tail.Stop(fmt.Sprintf("✓ Aspire started (%s)", time.Since(start).Round(time.Second)))
			return nil
		}
		// Fallback: Aspire's core (the dashboard) is up but not every resource has
		// reported "started" — a flaky/slow resource (a heavy web UI is prone to
		// this). After a grace period, proceed so the front door can still bridge
		// whatever IS up (DB, APIs, dashboard).
		if strings.Contains(out, "Now listening on") && strings.Contains(out, ":18888") {
			if dashSince.IsZero() {
				dashSince = time.Now()
			}
			if time.Since(dashSince) > 90*time.Second {
				tail.Stop(fmt.Sprintf("✓ Aspire up (%s) — dashboard reachable; some resources may still be starting", time.Since(start).Round(time.Second)))
				return nil
			}
		}
		if strings.Contains(out, "Unhandled exception") || strings.Contains(out, "Hosting failed") ||
			strings.Contains(out, "Exited with error code") {
			tail.Freeze()
			return fmt.Errorf("Aspire app failed to start — %s", logHint)
		}
		time.Sleep(2 * time.Second)
	}
	tail.Freeze()
	return fmt.Errorf("timed out after %s waiting for the app to start — %s", timeout, logHint)
}

// guestNameToSiding strips the channel/app prefix from a container name back to
// the siding name, for user-facing hints (best-effort; falls back to the full
// name). Container names are "<prefix>_<app>_<siding>".
func guestNameToSiding(guestName string) string {
	if i := strings.LastIndex(guestName, "_"); i >= 0 && i+1 < len(guestName) {
		return guestName[i+1:]
	}
	return guestName
}

// Activate sets up the loopback->guest-IP bridges and discovers the app's
// endpoints, recording each front-door route's external bridge port on the
// siding. Idempotent re-bridging is avoided by only running once per guest.
// EffRoutes returns the front-door routes to use for a siding: the siding's own
// resolved set (from its worktree .shunt.app.json) when present, else the app-level
// set. The guest runs the siding's worktree, so its contract is authoritative for
// that siding — a route it adds (or drops) applies without an `app add` in the root
// repo. Host-level callers (no siding) keep using app.FrontDoor directly.
func EffRoutes(app state.App, sd state.Siding) []state.Route {
	if len(sd.FrontDoor) > 0 {
		return sd.FrontDoor
	}
	return app.FrontDoor
}

// RouteFromContract builds a front-door state.Route from a contract route and an
// already-resolved host listen port. Shared with `app add` so the field mapping
// (CaddyID, TLS, guestPort) can't drift between the two derivations.
func RouteFromContract(appName string, r contract.FrontDoorRoute, listenPort int) state.Route {
	return state.Route{
		Key:        r.Key,
		Kind:       r.Kind,
		ListenPort: listenPort,
		Resource:   r.Resource,
		Endpoint:   r.Endpoint,
		GuestPort:  r.GuestPort,
		TLS:        r.TLS,
		CaddyID:    caddy.RouteID(appName, r.Kind, r.Key),
	}
}

// resolveSidingFrontDoor reads the siding worktree's .shunt.app.json and derives
// its front-door routes. It reuses the app-level assigned host port for any route
// the app already knows (matched by kind+key) so stable ports never move, and
// honors the contract's declared listenPort for a route only the siding adds.
// Returns nil when the siding has no readable/valid contract, so the caller falls
// back to the app-level set rather than failing an `up`/`switch`.
func resolveSidingFrontDoor(app state.App, sd state.Siding) ([]state.Route, error) {
	src, _, err := Paths(app, sd.Name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(src, contract.FileName)
	if _, err := os.Stat(path); err != nil {
		return nil, nil // no siding contract → the app-level set (not an error)
	}
	ct, err := contract.Load(src)
	if err != nil {
		return nil, fmt.Errorf("load siding contract %s: %w", path, err)
	}
	prev := make(map[string]int, len(app.FrontDoor))
	for _, r := range app.FrontDoor {
		prev[r.Kind+"/"+r.Key] = r.ListenPort
	}
	routes := make([]state.Route, 0, len(ct.FrontDoor))
	for _, r := range ct.FrontDoor {
		lp := r.ListenPort
		if p, ok := prev[r.Kind+"/"+r.Key]; ok {
			lp = p // keep the app-assigned host port; only siding-new routes use the declared one
		}
		routes = append(routes, RouteFromContract(app.Name, r, lp))
	}
	return routes, nil
}

// extraRoutes returns the siding's front-door routes that go beyond the app-level
// set (matched by kind+key) — i.e. routes a siding introduces on its own.
func extraRoutes(app state.App, sd state.Siding) []state.Route {
	known := make(map[string]bool, len(app.FrontDoor))
	for _, r := range app.FrontDoor {
		known[r.Kind+"/"+r.Key] = true
	}
	var extra []state.Route
	for _, r := range EffRoutes(app, sd) {
		if !known[r.Kind+"/"+r.Key] {
			extra = append(extra, r)
		}
	}
	return extra
}

// ensureSidingRoutes creates Caddy front-door servers for any route this siding
// adds beyond the app-level set, so PointCaddy has a server to aim at. It touches
// ONLY the extra routes — the shared app-level servers (from `app add`) are left
// as-is, so a re-up/switch never resets the routes another live siding depends on.
func ensureSidingRoutes(ctx context.Context, admin *caddy.Admin, app state.App, sd state.Siding) error {
	for _, r := range extraRoutes(app, sd) {
		// Skip a route whose server already exists — EnsureFrontDoor does delete-then-put,
		// which would reset a live route's upstream dial to the placeholder (a brief drop,
		// and it clobbers PointCaddy's rollback capture). Only CREATE the missing ones.
		if _, err := admin.GetID(ctx, r.CaddyID); err == nil {
			continue
		}
		ea := app
		ea.FrontDoor = []state.Route{r}
		if err := caddy.EnsureFrontDoor(ctx, admin, ea); err != nil {
			return err
		}
	}
	return nil
}

// routesToRemove returns front-door routes currently bound for the outgoing target
// (the app-level set plus whatever the outgoing siding added) that the incoming
// siding's set no longer includes — so a switch can tear down servers that would
// otherwise keep serving the old siding on their ports (a route the previous siding
// added, or an app-level route the incoming siding's contract drops).
func routesToRemove(app state.App, outgoing, incoming state.Siding) []state.Route {
	want := make(map[string]bool)
	for _, r := range EffRoutes(app, incoming) {
		want[r.Kind+"/"+r.Key] = true
	}
	seen := make(map[string]bool)
	var stale []state.Route
	bound := append(append([]state.Route{}, app.FrontDoor...), EffRoutes(app, outgoing)...)
	for _, r := range bound {
		k := r.Kind + "/" + r.Key
		if want[k] || seen[k] {
			continue
		}
		seen[k] = true
		stale = append(stale, r)
	}
	return stale
}

func Activate(ctx context.Context, app state.App, sd *state.Siding) error {
	ip, err := container.IP(ctx, sd.Container)
	if err != nil {
		return err
	}
	sd.LastIP = ip
	if sd.Bridges == nil {
		sd.Bridges = map[string]int{}
	}

	// Any route with a declared guestPort is bridged eagerly, host==guest: bind
	// the guest IP at the same port and forward to the app's loopback port. socat
	// comes up immediately (before the app even binds), so the front door
	// auto-serves the moment each service starts — no discovery, no waiting on a
	// slow/unhealthy resource, no re-switch.
	var needDiscovery []state.Route
	for _, r := range EffRoutes(app, *sd) {
		if r.GuestPort == 0 {
			needDiscovery = append(needDiscovery, r)
			continue
		}
		if err := container.Bridge(ctx, sd.Container, ip, r.ListenPort, r.GuestPort); err != nil {
			return err
		}
		sd.Bridges[r.Key] = r.ListenPort
	}
	if len(needDiscovery) == 0 {
		return nil // every route declared its port — nothing to discover
	}

	// Discovery fallback only for routes that DON'T declare a guestPort (legacy
	// Aspire apps that let Aspire assign ports): bridge the resource service,
	// resolve each one, bridge whatever has come up.
	if err := container.Bridge(ctx, sd.Container, "", rsExtPort, guestRSPort); err != nil {
		return err
	}
	eps, err := discoverReady(ctx, fmt.Sprintf("%s:%d", ip, rsExtPort), needDiscovery, 90*time.Second)
	if err != nil {
		return fmt.Errorf("discover endpoints: %w", err)
	}
	var pending []string
	for _, r := range needDiscovery {
		ep, ok := aspire.Find(eps, r.Resource, r.Endpoint)
		if !ok {
			pending = append(pending, fmt.Sprintf("%s (%s)", r.Key, r.Resource))
			continue
		}
		// For container-backed resources, target the real docker-published port
		// rather than Aspire's DCP proxy port (which doesn't forward through a
		// bridge). Project/process resources have no container, so keep ep.Port.
		realPort := ep.Port
		if dp := container.DockerPort(ctx, sd.Container, ep.Resource); dp > 0 {
			realPort = dp
		}
		if err := container.Bridge(ctx, sd.Container, ip, r.ListenPort, realPort); err != nil {
			return err
		}
		sd.Bridges[r.Key] = r.ListenPort
	}
	if len(pending) > 0 {
		// These are discovery-only routes (no declared guestPort) that hadn't
		// resolved within the window, so — unlike the eagerly-bridged host==guest
		// routes — they have no bridge yet. Re-run once they're up to bridge them.
		fmt.Printf("  ⚠ %d route(s) didn't resolve in time: %s — run `%s up %s` (or `switch`) again once they start\n",
			len(pending), strings.Join(pending, ", "), config.Current().BinaryName, sd.Name)
	}
	return nil
}

// discoverReady polls the resource service until every front-door route's
// resource has resolved, or the timeout passes (returning the last snapshot so
// the caller can report exactly which route is missing).
func discoverReady(ctx context.Context, addr string, routes []state.Route, timeout time.Duration) ([]aspire.Endpoint, error) {
	deadline := time.Now().Add(timeout)
	var last []aspire.Endpoint
	for {
		eps, err := aspire.Discover(ctx, addr, "")
		if err == nil {
			last = eps
			if allResolved(eps, routes) {
				return eps, nil
			}
		} else if time.Now().After(deadline) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return last, nil
		}
		time.Sleep(2 * time.Second)
	}
}

func allResolved(eps []aspire.Endpoint, routes []state.Route) bool {
	for _, r := range routes {
		if _, ok := aspire.Find(eps, r.Resource, r.Endpoint); !ok {
			return false
		}
	}
	return true
}

// PointCaddy repoints the app's front-door routes at this siding's bridged
// endpoints — the actual switch. It refreshes the guest IP first.
func PointCaddy(ctx context.Context, app state.App, sd *state.Siding) error {
	ip, err := container.IP(ctx, sd.Container)
	if err != nil {
		return err
	}
	sd.LastIP = ip

	admin := caddy.NewAdmin()
	// Switch-as-a-set: repoint every route at this siding, and if any one fails,
	// roll the already-repointed routes back to their previous dials so the front
	// door is never left half on the old siding and half on the new one.
	type patchedRoute struct {
		r    state.Route
		prev string // dial before this switch ("" = unknown, skip on rollback)
	}
	var done []patchedRoute
	rollback := func() {
		for _, p := range done {
			if p.prev == "" {
				continue
			}
			if path, body, err := caddy.DialPatch(p.r, p.prev); err == nil {
				_ = admin.Patch(ctx, path, body)
			}
		}
	}
	for _, r := range EffRoutes(app, *sd) {
		ext, ok := sd.Bridges[r.Key]
		if !ok {
			// Route not up yet (partial activation) — leave it on the placeholder
			// dial; it gets bound when the resource starts and switch re-runs.
			continue
		}
		prev, _ := caddy.CurrentDial(ctx, admin, r) // best-effort capture for rollback
		path, body, err := caddy.DialPatch(r, fmt.Sprintf("%s:%d", ip, ext))
		if err != nil {
			rollback()
			return err
		}
		if err := admin.Patch(ctx, path, body); err != nil {
			rollback()
			return fmt.Errorf("repoint route %q (rolled back %d route(s) to keep the front door coherent): %w", r.Key, len(done), err)
		}
		done = append(done, patchedRoute{r, prev})
	}
	return nil
}

// DashboardURL is the guest's directly-reachable Aspire dashboard. It prefers the
// port the app actually serves it on (the front-door dashboard route's guestPort,
// e.g. 15072) and falls back to the shunt default only when the app doesn't
// declare one.
func DashboardURL(app state.App, sd state.Siding) string {
	if sd.LastIP == "" {
		return ""
	}
	port := dashboardGuestPort(app, sd)
	if port == 0 {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", sd.LastIP, port)
}

func dashboardGuestPort(app state.App, sd state.Siding) int {
	port := 0
	if app.Runner == "" || app.Runner == runner.Aspire {
		port = guestDashboardPort
	}
	for _, r := range EffRoutes(app, sd) {
		if r.Kind == state.KindHTTP && r.GuestPort != 0 &&
			(r.Resource == "aspire-dashboard" || r.Key == "aspire-dashboard" || r.Key == "dashboard") {
			port = r.GuestPort
			break
		}
	}
	return port
}

// expandHome replaces a leading ~ with the user's home dir.
func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~ in %q: %w", p, err)
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
	}
	return p, nil
}

// originForClone prefers the local repo path (fast hardlinked clone) over a
// remote origin when both are known.
func originForClone(app state.App) string {
	if app.RepoPath != "" {
		return app.RepoPath
	}
	return app.RepoOrigin
}

func summarize(eps []aspire.Endpoint) string {
	parts := make([]string, 0, len(eps))
	for _, e := range eps {
		parts = append(parts, fmt.Sprintf("%s/%s=%s:%d", e.Resource, e.Name, e.Host, e.Port))
	}
	return strings.Join(parts, ", ")
}

// WaitReady blocks until the app is ready: Aspire waits for the "started"
// marker, other runners poll until every front-door route's guestPort listens.
func WaitReady(ctx context.Context, app state.App, sd state.Siding, timeout time.Duration) error {
	if app.Runner == "" || app.Runner == runner.Aspire {
		return WaitStarted(ctx, sd.Container, timeout)
	}
	tail := ui.NewLiveTail(5)
	deadline := time.Now().Add(timeout)
	start := time.Now()
	shown := 0
	logHint := fmt.Sprintf("see `%s logs %s`", config.Current().BinaryName, guestNameToSiding(sd.Container))
	for time.Now().Before(deadline) {
		if st, err := container.State(ctx, sd.Container); err == nil && st != "running" {
			tail.Freeze()
			return fmt.Errorf("guest exited (state=%s) before the app started — %s", st, logHint)
		}
		out, _ := container.Exec(ctx, sd.Container, "sh", "-c", "cat "+appLogPath+" 2>/dev/null")
		lines := strings.Split(out, "\n")
		var fresh []string
		if len(lines) > shown {
			fresh = lines[shown:]
			shown = len(lines)
		}
		tail.Update(fmt.Sprintf("⏳ waiting for %s to listen… (%s)", app.Runner, time.Since(start).Round(time.Second)), fresh)
		ready, err := ProbeAppRunning(ctx, app, sd)
		if err != nil {
			tail.Freeze()
			return fmt.Errorf("probe %s readiness: %w", app.Runner, err)
		}
		if ready {
			tail.Stop(fmt.Sprintf("✓ %s up (%s)", app.Runner, time.Since(start).Round(time.Second)))
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	tail.Freeze()
	return fmt.Errorf("timed out waiting for %s to listen — %s", app.Runner, logHint)
}

// allPortsListening reports whether every front-door route's guestPort accepts a
// connection inside the guest (socat is in the base image; sh has no /dev/tcp).
func allPortsListening(ctx context.Context, app state.App, sd state.Siding) bool {
	for _, r := range EffRoutes(app, sd) {
		if r.GuestPort == 0 {
			return false
		}
		out, _ := container.Exec(ctx, sd.Container, "sh", "-c",
			fmt.Sprintf("socat -T1 /dev/null TCP:127.0.0.1:%d 2>/dev/null && echo up", r.GuestPort))
		if !strings.Contains(out, "up") {
			return false
		}
	}
	return true
}

// HealthOK reports whether the app is actually serving, for the dashboard's
// running/idle status. It hits the app's health endpoint from *inside* the guest
// (container exec, not a guest-IP dial), so it works for any siding from the
// launchd dashboard without Local Network access. Unlike AppRunning — which probes
// the Aspire resource-service port to decide whether to (re)start, and reads false
// once the AppHost orchestrator exits even while the services keep serving — this
// asks the real endpoint, so a booted-and-serving app reads as up.
func HealthOK(ctx context.Context, app state.App, sd state.Siding) bool {
	port, path, tls := healthTarget(app)
	if port == 0 {
		return false
	}
	scheme := "http"
	if tls {
		scheme = "https"
	}
	url := fmt.Sprintf("%s://localhost:%d%s", scheme, port, path)
	// Invoke curl directly (NOT via `sh -c`) so a contract-provided health path can't
	// inject shell metacharacters — the URL is passed as a single argv element. -f
	// makes a non-2xx/3xx a non-zero exit, which container.Exec surfaces as an error;
	// -k for the self-signed dev cert; -m 2 keeps the poll snappy.
	_, err := container.Exec(ctx, sd.Container, "curl", "-sfk", "-m", "2", "-o", "/dev/null", url)
	return err == nil
}

// healthyWithin polls the app's health endpoint until it answers or the window
// elapses — used by `up` to tell a still-booting app (goes healthy inside the
// window) from a half-dead one (AppHost up, children crashed → never healthy).
// Note: with the default health target (the Aspire dashboard, up whenever the
// AppHost runs) this only distinguishes a fully-dead AppHost; set healthPort to a
// real app route to catch a half-dead child stack.
func healthyWithin(ctx context.Context, app state.App, sd state.Siding, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if HealthOK(ctx, app, sd) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// healthTarget resolves the health check's guest port, path, and whether it's TLS.
// Explicit contract config (HealthPort/HealthPath) wins; TLS is inferred from the
// front-door route that binds that guest port. Empty HealthPort falls back to the
// Aspire dashboard route's guest port (its home page serves whenever the AppHost is
// up) — same resolution as DashboardURL, over http.
func healthTarget(app state.App) (port int, path string, tls bool) {
	path = "/"
	if app.HealthPath != "" {
		path = app.HealthPath
	}
	// A contract path like "healthz" (no leading slash) would glue onto the port
	// ("localhost:15072healthz"); normalize so the URL is always well-formed.
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if app.HealthPort != 0 {
		for _, r := range app.FrontDoor {
			if r.GuestPort == app.HealthPort {
				tls = r.TLS
				break
			}
		}
		return app.HealthPort, path, tls
	}
	port = guestDashboardPort
	for _, r := range app.FrontDoor {
		if r.Kind == state.KindHTTP && r.GuestPort != 0 &&
			(r.Resource == "aspire-dashboard" || r.Key == "aspire-dashboard" || r.Key == "dashboard") {
			port = r.GuestPort
			break
		}
	}
	return port, path, false
}

// AppRunning reports whether the app is already up in the guest, so a re-run of
// `up` re-activates instead of restarting (and colliding with) a live AppHost —
// some apps never log the "started" marker, so a log check alone re-launches it.
func AppRunning(ctx context.Context, app state.App, sd state.Siding) bool {
	running, _ := ProbeAppRunning(ctx, app, sd)
	return running
}

// ProbeAppRunning reports process state without treating a failed guest probe as
// a stopped application.
func ProbeAppRunning(ctx context.Context, app state.App, sd state.Siding) (bool, error) {
	if app.Runner == "" || app.Runner == runner.Aspire {
		// Probe the dashboard route the AppHost actually exposes. Newer Aspire CLI
		// versions no longer leave the old resource-service port (18890) listening,
		// which made a live AppHost look stopped and caused `up` to restart it.
		port := dashboardGuestPort(app, sd)
		out, err := container.Exec(ctx, sd.Container, "sh", "-c",
			fmt.Sprintf("if socat -T1 /dev/null TCP:127.0.0.1:%d >/dev/null 2>&1; then echo up; else echo down; fi", port))
		if err != nil {
			return false, err
		}
		return strings.Contains(out, "up"), nil
	}
	checks := make([]string, 0, len(EffRoutes(app, sd)))
	for _, route := range EffRoutes(app, sd) {
		if route.GuestPort == 0 {
			return false, nil
		}
		checks = append(checks, fmt.Sprintf("socat -T1 /dev/null TCP:127.0.0.1:%d >/dev/null 2>&1", route.GuestPort))
	}
	script := "if " + strings.Join(checks, " && ") + "; then echo up; else echo down; fi"
	if len(checks) == 0 {
		script = "echo up"
	}
	out, err := container.Exec(ctx, sd.Container, "sh", "-c", script)
	if err != nil {
		return false, err
	}
	return strings.Contains(out, "up"), nil
}

// EnsureVolumeBaselines creates an explicit empty canonical generation when a
// project first declares data volumes. Later generations only come from an
// explicit siding promotion; host Docker is never probed or imported.
func EnsureVolumeBaselines(ctx context.Context, app state.App) error {
	if len(app.Volumes) == 0 {
		return nil
	}
	manager, err := databaseline.New(app.ConfigDir, app.Volumes)
	if err != nil {
		return err
	}
	_, err = manager.InitializeEmpty(ctx)
	return err
}

// CreateBindVolumes points a guest Docker named volume at each siding's cp -c
// clone (bind-mounted at /mnt/dvol/<vol>), so Aspire's WithDataVolume(<vol>)
// mounts the host's copy-on-write data instead of an empty store. Runs after
// dockerd is up and before the app starts; idempotent (skips volumes already
// created, and routes Spin didn't clone because there was no baseline).
func CreateBindVolumes(ctx context.Context, app state.App, sd state.Siding) error {
	for _, vol := range app.Volumes {
		if err := contract.ValidateVolumeName(vol); err != nil {
			return fmt.Errorf("invalid data volume %q: %w", vol, err)
		}
		dev := "/mnt/dvol/" + vol
		probe := `if [ -d "$1" ]; then echo directory; elif [ -e "$1" ]; then echo not-directory; else echo absent; fi`
		out, err := execGuest(ctx, sd.Container, "sh", "-c", probe, "shunt-volume-probe", dev)
		if err != nil {
			return fmt.Errorf("probe host-backed mount for volume %q: %w", vol, err)
		}
		switch strings.TrimSpace(out) {
		case "absent":
			continue
		case "directory":
		case "not-directory":
			return fmt.Errorf("host-backed mount for volume %q exists but is not a directory", vol)
		default:
			return fmt.Errorf("probe host-backed mount for volume %q returned %q", vol, strings.TrimSpace(out))
		}
		out, err = execGuest(ctx, sd.Container, "docker", "volume", "ls", "--quiet", "--filter", "name=^"+vol+"$")
		if err != nil {
			return fmt.Errorf("probe Docker volume %q: %w", vol, err)
		}
		existing := strings.Fields(out)
		if len(existing) == 1 && existing[0] == vol {
			continue
		}
		if len(existing) != 0 {
			return fmt.Errorf("probe Docker volume %q returned unexpected names: %s", vol, strings.Join(existing, ", "))
		}
		if _, err := execGuest(ctx, sd.Container, "docker", "volume", "create",
			"--driver", "local", "--opt", "type=none", "--opt", "o=bind", "--opt", "device="+dev, vol); err != nil {
			return fmt.Errorf("create bind-backed volume %q: %w", vol, err)
		}
		fmt.Printf("  data volume %q backed by host copy-on-write clone\n", vol)
	}
	return nil
}

// Switch points the app's stable front door at a siding. It only repoints; use
// Up or Restart to start or rebuild the application.
func Switch(ctx context.Context, app *state.App, target string) error {
	if app == nil {
		return errors.New("app is required")
	}
	return WithProjectOperation(ctx, app.ConfigDir, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		if err := EnsureNoRemovalInProgress(current, "switch"); err != nil {
			return err
		}
		if err := switchLocked(ctx, &current, target); err != nil {
			return err
		}
		*app = current
		return nil
	})
}

func switchLocked(ctx context.Context, app *state.App, target string) error {
	if target == state.HostTarget {
		return fmt.Errorf("host is no longer a switch target; create or choose a siding")
	}
	if app.LiveSiding == state.HostTarget {
		if err := caddy.RemoveFrontDoor(ctx, caddy.NewAdmin(), *app); err != nil {
			return fmt.Errorf("remove legacy host routes: %w", err)
		}
		app.LiveSiding = ""
	}
	admin := caddy.NewAdmin()

	sd, ok := app.Sidings[target]
	if !ok {
		return fmt.Errorf("no siding %q", target)
	}
	if sd.MaterializationPhase != "" && sd.MaterializationPhase != state.PhaseGuest {
		return fmt.Errorf("siding %q is %s; run `shunt up %s` first", target, sd.MaterializationPhase, target)
	}
	// The guest runs this siding's worktree, so its own .shunt.app.json is
	// authoritative for its front door — resolve it (a route it adds/drops applies
	// without an `app add` in root). A broken contract fails the switch fast rather
	// than silently serving a stale set; nil (no contract) clears any stale set.
	fd, ferr := resolveSidingFrontDoor(*app, sd)
	if ferr != nil {
		return fmt.Errorf("switch to %q: %w", target, ferr)
	}
	sd.FrontDoor = fd
	{
		// Already on a siding: the shared app-level servers are up. First tear down any
		// server the target no longer wants — the previously-live siding's extra routes,
		// or app-level routes this siding's contract drops — so they don't keep serving
		// the old siding on their ports.
		if outgoing, ok := app.Sidings[app.LiveSiding]; ok {
			if stale := routesToRemove(*app, outgoing, sd); len(stale) > 0 {
				so := *app
				so.FrontDoor = stale
				if err := caddy.RemoveFrontDoor(ctx, admin, so); err != nil {
					return err
				}
			}
		}
		// Then create any route THIS siding adds beyond the shared set.
		if err := ensureSidingRoutes(ctx, admin, *app, sd); err != nil {
			return err
		}
	}
	if len(sd.Bridges) == 0 {
		if err := Activate(ctx, *app, &sd); err != nil {
			return err
		}
	}
	if err := PointCaddy(ctx, *app, &sd); err != nil {
		return err
	}
	app.LiveSiding = target
	app.Sidings[target] = sd
	return state.SaveApp(*app)
}

// Restart stops the application, re-assures and loads its configured images,
// starts the runner, and waits for readiness. The guest and data stay in place.
func Restart(ctx context.Context, app state.App, sd state.Siding) error {
	wasLive := false
	recreatedGuest := false
	err := WithSidingOperation(ctx, app.ConfigDir, sd.Name, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		if err := EnsureNoRemovalInProgress(current, "restart"); err != nil {
			return err
		}
		latest, ok := current.Sidings[sd.Name]
		if !ok {
			return fmt.Errorf("no siding %q", sd.Name)
		}
		if err := RequireGuest(latest); err != nil {
			return err
		}
		wasLive = current.LiveSiding == sd.Name
		if err := EnsureGuestLive(ctx, latest); err != nil {
			if ctx.Err() != nil {
				return err
			}
			var recreateRequired *GuestRecreateRequiredError
			if !errors.As(err, &recreateRequired) {
				return err
			}
			recreated, recreateErr := recreate(ctx, current, latest, false)
			if recreateErr != nil {
				return fmt.Errorf("guest for %q needs recreation (%v), but recreation failed: %w", sd.Name, err, recreateErr)
			}
			latest = recreated
			recreatedGuest = true
		}
		if err := restart(ctx, current, latest); err != nil {
			return err
		}
		if recreatedGuest {
			if err := Activate(ctx, current, &latest); err != nil {
				return fmt.Errorf("restore bridges after guest recreation: %w", err)
			}
			if _, err := MergeSidingState(ctx, current.ConfigDir, latest, false); err != nil {
				return fmt.Errorf("save recreated guest bridges: %w", err)
			}
		}
		return nil
	})
	if err == nil && recreatedGuest && wasLive {
		err = restoreLiveRoute(ctx, app.ConfigDir, sd.Name)
	}
	return err
}

func restart(ctx context.Context, app state.App, sd state.Siding) error {
	guestState, err := guestRuntimeState(ctx, sd.Container)
	if err != nil {
		return fmt.Errorf("inspect guest for %q: %w", sd.Name, err)
	}
	if guestState != "running" {
		return fmt.Errorf("the guest for %q isn't running (state=%s)", sd.Name, guestState)
	}
	if err := prepareLifecycle(ctx, app, sd); err != nil {
		return err
	}
	if err := stopLifecycleApp(ctx, app, sd); err != nil {
		return err
	}
	// Clear the old start marker so WaitReady waits for the fresh run.
	_, _ = container.Exec(ctx, sd.Container, "sh", "-c", "> "+appLogPath)
	if err := startLifecycleApp(ctx, app, sd); err != nil {
		return err
	}
	return waitLifecycleReady(ctx, app, sd, 15*time.Minute)
}

// orDefaultStr returns v if non-empty, else def.
func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Recreate rebuilds an idle siding guest with the current app config (memory, cpus,
// mounts, env) — for "reapply config to an existing siding". It keeps the
// worktree, branch, and data (the on-disk src + cp -c volume clones), replacing
// only the container, so guest-creation settings take effect. Run `up` after to
// start the app (the guest's Docker is fresh, so bind volumes + bridges rebuild).
func Recreate(ctx context.Context, app state.App, sd state.Siding, freshData bool) (state.Siding, error) {
	var recreated state.Siding
	err := WithSidingOperation(ctx, app.ConfigDir, sd.Name, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		if err := EnsureNoRemovalInProgress(current, "recreate guest"); err != nil {
			return err
		}
		latest, ok := current.Sidings[sd.Name]
		if !ok {
			return fmt.Errorf("no siding %q", sd.Name)
		}
		if err := RequireGuest(latest); err != nil {
			return err
		}
		recreated, err = recreate(ctx, current, latest, freshData)
		return err
	})
	return recreated, err
}

func recreate(ctx context.Context, app state.App, sd state.Siding, freshData bool) (state.Siding, error) {
	src, volRoot, err := Paths(app, sd.Name)
	if err != nil {
		return sd, err
	}
	if err := AssureImageCache(ctx, app); err != nil {
		return sd, err
	}
	if err := ensureBaseImage(ctx, false); err != nil {
		return sd, fmt.Errorf("ensure native base image before replacing guest: %w", err)
	}
	mounts := []container.Mount{{Host: src, Guest: "/workspace"}}
	// Standing per-siding output dir — recordings/logs land here, outside the
	// worktree so they're never committable. MkdirAll heals sidings created
	// before this directory existed.
	outDir := filepath.Join(filepath.Dir(src), "out")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return sd, err
	}
	mounts = append(mounts, container.Mount{Host: outDir, Guest: "/out"})
	var baseline *databaseline.Manager
	if freshData && len(app.Volumes) > 0 {
		baseline, err = databaseline.New(app.ConfigDir, app.Volumes)
		if err != nil {
			return sd, err
		}
		if _, err := baseline.InitializeEmpty(ctx); err != nil {
			return sd, err
		}
	}
	for _, vol := range app.Volumes {
		host := filepath.Join(volRoot, vol)
		if !freshData {
			if err := os.MkdirAll(host, 0o755); err != nil {
				return sd, err
			}
		}
		mounts = append(mounts, container.Mount{Host: host, Guest: "/mnt/dvol/" + vol})
	}
	for _, m := range app.Mounts {
		host, err := expandHome(m.Host)
		if err != nil {
			return sd, err
		}
		if _, err := os.Stat(host); err != nil {
			return sd, fmt.Errorf("inspect configured mount %q: %w", host, err)
		}
		mounts = append(mounts, container.Mount{Host: host, Guest: m.Guest, ReadOnly: m.ReadOnly})
	}
	if nugetHost, err := expandHome("~/.nuget/packages"); err == nil {
		if _, statErr := os.Stat(nugetHost); statErr == nil {
			mounts = append(mounts, container.Mount{Host: nugetHost, Guest: "/root/.nuget/packages"})
		}
	}
	// Everything that can be checked without disrupting the current guest is now
	// ready. Fresh data is reset only after removal because the guest bind-mounts
	// those directories while it exists.
	if err := removeGuest(ctx, sd.Container); err != nil {
		return sd, err
	}
	sd.Bridges = map[string]int{}
	sd.LastIP = ""
	sd.Stopped = true
	if _, err := mergeSiding(ctx, app.ConfigDir, sd, true); err != nil {
		return sd, fmt.Errorf("guest removed, but replacement state could not be saved: %w", err)
	}
	if baseline != nil {
		if _, err := baseline.ResetVolumeRoot(ctx, volRoot); err != nil {
			return sd, fmt.Errorf("reset siding data after guest removal: %w", err)
		}
	}
	for _, vol := range app.Volumes {
		if err := os.MkdirAll(filepath.Join(volRoot, vol), 0o755); err != nil {
			return sd, err
		}
	}
	if err := runGuest(ctx, container.RunOpts{
		Name:      sd.Container,
		Image:     image.Tag(),
		Init:      true,
		CapAddAll: true,
		Memory:    orDefaultStr(app.Memory, config.GuestMemory()),
		CPUs:      orDefaultStr(app.CPUs, config.GuestCPUs()),
		Rosetta:   true,
		Mounts:    mounts,
		Env:       guestEnv(app),
		Cmd:       []string{"/bin/sh", "-lc", "exec sleep infinity"},
	}); err != nil {
		return sd, err
	}
	sd.Stopped = false
	if _, err := mergeSiding(ctx, app.ConfigDir, sd, true); err != nil {
		if isCommittedStatePublication(err) {
			return sd, fmt.Errorf("replacement guest started and its state is visible but durability is unconfirmed: %w", err)
		}
		stopErr := stopGuest(context.WithoutCancel(ctx), sd.Container)
		return sd, errors.Join(fmt.Errorf("replacement guest started, but its state could not be saved: %w", err), stopErr)
	}
	if err := prepareGuestFromCache(ctx, app, sd); err != nil {
		return sd, err
	}
	return sd, nil
}
