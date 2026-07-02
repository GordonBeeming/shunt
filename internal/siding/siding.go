// Package siding orchestrates a single experiment guest end to end: clone the
// repo, launch the Aspire app inside an Apple container, bridge its loopback
// endpoints to the guest IP, discover them, and point the host Caddy at them.
package siding

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/aspire"
	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/hostdocker"
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

	startedMarker = "Distributed application started"
)

// Paths returns the host src and vol-root paths for a siding under the app's
// config dir.
func Paths(app state.App, name string) (src, volRoot string) {
	base := filepath.Join(app.ConfigDir, name)
	return filepath.Join(base, "src"), filepath.Join(base, "vol")
}

// guestWarmTar is where Spin mounts the project warm image cache, read-only.
const guestWarmTar = "/mnt/base/images.tar"

// WarmTarPath is the per-project warm image cache (a `docker save` tar produced
// by `shunt warm`). Its presence means `up` should `docker load` it into a new
// siding instead of pulling/rebuilding dependency images from scratch.
func WarmTarPath(app state.App) string {
	return filepath.Join(app.ConfigDir, "base", "images.tar")
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

// Spin clones the repo + data volumes and launches the guest. It does not wait
// for the app to be ready (see Activate).
func Spin(ctx context.Context, app state.App, name, branch, fromBranch string) (state.Siding, error) {
	src, volRoot := Paths(app, name)
	wtBranch := config.BranchPrefix() + name
	if fromBranch != "" {
		// Pick up an existing (remote) branch and stay ON it, so commits continue
		// that branch and push back to it — rather than forking a new prefixed
		// siding branch off a start point.
		wtBranch = fromBranch
		if err := fsclone.AddWorktreeTracking(ctx, app.RepoPath, src, fromBranch); err != nil {
			return state.Siding{}, err
		}
	} else if err := fsclone.AddWorktree(ctx, app.RepoPath, src, wtBranch, branch); err != nil {
		return state.Siding{}, err
	}

	mounts := []container.Mount{{Host: src, Guest: "/workspace"}}
	// Copy-on-write each declared data volume from the project baseline (cp -c,
	// instant + shares blocks) and bind-mount it at /mnt/dvol/<vol>; `up` then
	// points a guest Docker volume at it so Aspire mounts the host's test data.
	for _, vol := range app.Volumes {
		base := baselineDir(app, vol)
		if _, err := os.Stat(base); err != nil {
			continue // no baseline (host lacked it) — this siding starts empty for it
		}
		host := filepath.Join(volRoot, vol)
		// cp -c needs the dest's parent to exist (the worktree clone creates src/,
		// not vol/).
		if err := os.MkdirAll(volRoot, 0o755); err != nil {
			return state.Siding{}, err
		}
		if err := fsclone.CloneVolume(ctx, base, host); err != nil {
			return state.Siding{}, err
		}
		mounts = append(mounts, container.Mount{Host: host, Guest: "/mnt/dvol/" + vol})
	}
	// Explicit per-project mounts from the contract (e.g. user-secrets) — shunt
	// honors these verbatim, no app-specific magic.
	for _, m := range app.Mounts {
		host, err := expandHome(m.Host)
		if err != nil {
			return state.Siding{}, err
		}
		mounts = append(mounts, container.Mount{Host: host, Guest: m.Guest, ReadOnly: m.ReadOnly})
	}
	// Reuse the host's NuGet package cache so `dotnet restore` doesn't re-download
	// every package for every siding — the single biggest first-run speedup.
	if nugetHost, err := expandHome("~/.nuget/packages"); err == nil {
		if _, statErr := os.Stat(nugetHost); statErr == nil {
			mounts = append(mounts, container.Mount{Host: nugetHost, Guest: "/root/.nuget/packages"})
		}
	}
	// Project warm image cache (from `shunt warm`): mount read-only so `up` can
	// `docker load` the pre-built/pulled dependency images instead of rebuilding.
	if IsWarmed(app) {
		mounts = append(mounts, container.Mount{Host: WarmTarPath(app), Guest: guestWarmTar, ReadOnly: true})
	}

	guestName := config.ContainerName(app.Name, name)
	if err := container.Run(ctx, container.RunOpts{
		Name:      guestName,
		Image:     config.BaseImageTag(),
		Init:      true,
		CapAddAll: true,
		// Per-guest caps: the contract wins (heavy stacks like SQL + several
		// projects + nested Docker need headroom), else the user-config default,
		// else shunt's default. The runtime's own ~1 GB default OOMs Aspire.
		Memory: orDefaultStr(app.Memory, config.GuestMemory()),
		CPUs:   orDefaultStr(app.CPUs, config.GuestCPUs()),
		// Rosetta lets amd64-only images (SQL Server) run on the arm64 guest —
		// the same x86 translation Docker Desktop uses; qemu segfaults SQL Server.
		Rosetta: true,
		Mounts:  mounts,
		Env:     guestEnv(app),
		// Idle keep-alive: the entrypoint starts dockerd + the dev cert, then this
		// holds the guest open WITHOUT running Aspire. Run the app later with `up`,
		// so `new` is fast and you can edit code first.
		Cmd: []string{"/bin/sh", "-lc", "exec sleep infinity"},
	}); err != nil {
		return state.Siding{}, err
	}

	return state.Siding{
		Name:      name,
		Branch:    wtBranch,
		Container: guestName,
		RSPort:    guestRSPort,
		Bridges:   map[string]int{},
	}, nil
}

// Up brings a siding online: it makes sure the guest is live, starts the app if
// it isn't already running, and (when bridge is true) bridges its routes to the
// host. Progress lines are written to progress (os.Stdout for the CLI, io.Discard
// for the dashboard, which runs it async and polls status). Returns the updated
// siding for the caller to persist; it does not switch the front door.
func Up(ctx context.Context, app state.App, sd state.Siding, bridge bool, progress io.Writer) (state.Siding, error) {
	fmt.Fprintln(progress, "• checking the guest is up…")
	if err := EnsureGuestLive(ctx, sd); err != nil {
		return sd, err
	}
	// The guest is live again — clear any `stopped` marker `kill` left behind.
	sd.Stopped = false

	// Idempotent: only (re)launch the app if it isn't already running, so re-upping
	// a live app re-activates instead of starting a second AppHost (port clash).
	if !AppRunning(ctx, app, sd) {
		_ = StopApp(ctx, app, sd)
		_, _ = container.Exec(ctx, sd.Container, "sh", "-c", "> "+appLogPath)

		fmt.Fprintln(progress, "• checking the in-guest Docker daemon…")
		if e := EnsureDockerd(ctx, sd); e != nil {
			return sd, e
		}
		if e := CreateBindVolumes(ctx, app, sd); e != nil {
			fmt.Fprintf(progress, "  (data volume bind failed: %v — continuing with empty volumes)\n", e)
		}
		// Keep the host as the canonical cache: warm the project tar from the host
		// once, then load it into the guest so the siding never pulls from the net.
		tar := WarmTarPath(app)
		if len(app.PrebakeImages) > 0 && hostdocker.Available(ctx) {
			if _, statErr := os.Stat(tar); statErr != nil {
				fmt.Fprintln(progress, "• warming the host image cache (one-time)…")
				if _, e := hostdocker.Ensure(ctx, app.PrebakeImages); e != nil {
					return sd, fmt.Errorf("warm host cache: %w", e)
				}
				if e := os.MkdirAll(filepath.Dir(tar), 0o755); e != nil {
					return sd, e
				}
				if e := hostdocker.Save(ctx, app.PrebakeImages, tar); e != nil {
					return sd, e
				}
			}
		}
		if loaded, e := LoadWarm(ctx, app, sd); e != nil {
			fmt.Fprintf(progress, "  (warm load failed: %v)\n", e)
		} else if loaded {
			fmt.Fprintln(progress, "• loaded dependency images from cache (no pull)")
		} else {
			fmt.Fprintln(progress, "• no warm cache — declare prebakeImages + run `warm`, or it'll build/pull cold")
		}

		fmt.Fprintf(progress, "• starting the app in %q…\n", sd.Name)
		if err := StartApp(ctx, app, sd); err != nil {
			return sd, err
		}
		// Non-blocking: the app keeps building in the background; the eager bridges
		// serve each route as it comes up.
		fmt.Fprintln(progress, "• starting the app (it keeps building in the background)…")
		_ = WaitReady(ctx, app, sd, 45*time.Second)
	} else {
		fmt.Fprintf(progress, "• the app is already running in %q\n", sd.Name)
	}

	if !bridge {
		if ip, e := container.IP(ctx, sd.Container); e == nil {
			sd.LastIP = ip
		}
		return sd, nil
	}
	fmt.Fprintln(progress, "• discovering endpoints + bridging to the host…")
	if err := Activate(ctx, app, &sd); err != nil {
		return sd, err
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
	if _, err := container.Exec(ctx, sd.Container, "true"); err == nil {
		return nil // truly alive
	}
	_ = container.Stop(ctx, sd.Container) // clear the zombie/stopped state
	if err := container.Start(ctx, sd.Container); err != nil {
		return fmt.Errorf("guest for %q wouldn't restart: %w", sd.Name, err)
	}
	for i := 0; i < 20; i++ {
		if _, err := container.Exec(ctx, sd.Container, "true"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("guest for %q didn't become reachable after a restart", sd.Name)
}

// EnsureDockerd makes sure the in-guest Docker daemon is healthy before Aspire
// needs it. A guest that was stopped and started again often fails to re-start
// dockerd from its entrypoint (stale pid/socket state), leaving it dead even
// though the guest is "running" — Aspire then reports the runtime unhealthy. This
// clears the stale state and (re)starts dockerd, waiting for it to answer.
func EnsureDockerd(ctx context.Context, sd state.Siding) error {
	if out, _ := container.Exec(ctx, sd.Container, "sh", "-c", "docker info >/dev/null 2>&1 && echo ok"); strings.Contains(out, "ok") {
		return nil
	}
	_, _ = container.Exec(ctx, sd.Container, "sh", "-c",
		"pkill dockerd 2>/dev/null; pkill containerd 2>/dev/null; rm -f /var/run/docker.pid /var/run/docker/containerd/containerd.pid 2>/dev/null; true")
	if err := container.ExecDetached(ctx, sd.Container, "/bin/sh", "-lc", "dockerd > /var/log/dockerd.log 2>&1"); err != nil {
		return err
	}
	for i := 0; i < 20; i++ {
		if out, _ := container.Exec(ctx, sd.Container, "sh", "-c", "docker info >/dev/null 2>&1 && echo ok"); strings.Contains(out, "ok") {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("in-guest Docker daemon didn't become healthy (see `dockerd` log in the guest)")
}

// StartApp runs the Aspire AppHost inside an already-running siding guest (build
// + dependency image pulls happen here). It's detached and its output goes to
// appLogPath. The guest's env (Development, Aspire endpoints) was set at Spin
// time and is inherited by the exec.
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
		wd := "/workspace"
		if app.Workdir != "" {
			wd = "/workspace/" + app.Workdir
		}
		runCmd = fmt.Sprintf("cd %s && %s > %s 2>&1", wd, app.Start, appLogPath)
	}
	return container.ExecDetached(ctx, sd.Container, "/bin/sh", "-lc", runCmd)
}

// aspirePortHex are the Aspire host ports (dashboard 18888, OTLP 18889, resource
// service 18890, MCP 18891) as the hex used in /proc/net/tcp local addresses.
const aspirePortHex = "49C8 49C9 49CA 49CB"

// StopApp stops the app inside the guest WITHOUT touching the guest, dockerd, or
// the dependency containers (which run under dockerd on other ports), so a
// rebuild keeps SQL etc. + their data up.
//
// First it runs the app's configured clean-stop command if any (e.g. `aspire
// stop`). Then — always, as a fallback, because `dotnet run`/`watch` RESPAWN the
// compiled AppHost binary when killed — it SIGKILLs the run/watch wrappers so
// nothing respawns and frees the Aspire host ports by finding whatever holds
// them via the socket inode in /proc/net/tcp (name-agnostic; catches the
// compiled AppHost binary, dashboard, and DCP).
func StopApp(ctx context.Context, app state.App, sd state.Siding) error {
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
		_, _ = container.Exec(ctx, sd.Container, "/bin/sh", "-lc", fmt.Sprintf("cd %s && %s", wd, app.Stop))
	}
	script := `
# Reap the whole managed app tree. ` + "`aspire start`" + ` detaches the AppHost plus its
# dotnet build / MSBuild worker nodes / VBCSCompiler, which aren't ` + "`dotnet run`/`watch`" + ` and
# don't hold the app ports while still building — so the old targeted kill left them
# to pile up across re-ups (26 deep once), thrashing the guest until nothing could
# finish. In a shunt guest every dotnet/aspire process is the app, so a broad kill is
# safe and actually reaps the tree.
pkill -9 -f dotnet 2>/dev/null
pkill -9 -f aspire 2>/dev/null
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
true`
	_, err := container.Exec(ctx, sd.Container, "sh", "-c", script)
	return err
}

// LoadWarm streams the project's warm-cache tar from the host into the guest's
// Docker store (via `docker load`, no bind mount), so Aspire reuses the images
// instead of pulling/rebuilding. No-op if the project isn't warmed.
func LoadWarm(ctx context.Context, app state.App, sd state.Siding) (bool, error) {
	tar := WarmTarPath(app)
	if _, err := os.Stat(tar); err != nil {
		return false, nil
	}
	if err := container.ExecStdinFile(ctx, sd.Container, tar, "docker", "load"); err != nil {
		return false, err
	}
	return true, nil
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
	for _, r := range app.FrontDoor {
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
	for _, r := range app.FrontDoor {
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
	port := guestDashboardPort
	for _, r := range app.FrontDoor {
		if r.Kind == state.KindHTTP && r.GuestPort != 0 &&
			(r.Resource == "aspire-dashboard" || r.Key == "aspire-dashboard" || r.Key == "dashboard") {
			port = r.GuestPort
			break
		}
	}
	return fmt.Sprintf("http://%s:%d", sd.LastIP, port)
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
		if allPortsListening(ctx, app, sd) {
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
	for _, r := range app.FrontDoor {
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
	if app.Runner == "" || app.Runner == runner.Aspire {
		// The resource-service port (18890 = hex 49CA) being bound means the
		// AppHost is running.
		out, _ := container.Exec(ctx, sd.Container, "sh", "-c",
			"cat /proc/net/tcp 2>/dev/null | awk '{print $2}' | grep -iq ':49CA' && echo up")
		return strings.Contains(out, "up")
	}
	return allPortsListening(ctx, app, sd)
}

// baselineDir is where a project's one-time host-volume extraction lives — the
// APFS source each siding cp -c clones from.
func baselineDir(app state.App, vol string) string {
	return filepath.Join(app.ConfigDir, "baseline", vol)
}

// EnsureVolumeBaselines extracts each declared host Docker named volume to an
// APFS baseline dir under the project's config (one-time, the only expensive
// step — e.g. a 50 GB SQL volume copied once), so `new` can cp -c (copy-on-write)
// an instant per-siding clone from it. Skips volumes already extracted or absent
// on the host. Requires host Docker (online once for the tiny alpine helper).
func EnsureVolumeBaselines(ctx context.Context, app state.App) error {
	if len(app.Volumes) == 0 {
		return nil
	}
	if !hostdocker.Available(ctx) {
		fmt.Println("• host Docker unavailable — sidings start with empty data volumes")
		return nil
	}
	for _, vol := range app.Volumes {
		base := baselineDir(app, vol)
		if _, err := os.Stat(base); err == nil {
			continue // already extracted
		}
		if !hostdocker.HasVolume(ctx, vol) {
			fmt.Printf("  (skip data volume %q — not on host Docker)\n", vol)
			continue
		}
		fmt.Printf("• extracting data volume %q from host (one-time baseline)…\n", vol)
		if err := os.MkdirAll(base, 0o755); err != nil {
			return err
		}
		if err := hostdocker.ExtractVolumeToDir(ctx, vol, base); err != nil {
			_ = os.RemoveAll(base) // don't leave a half-extracted baseline
			return fmt.Errorf("extract host volume %q: %w", vol, err)
		}
	}
	return nil
}

// CreateBindVolumes points a guest Docker named volume at each siding's cp -c
// clone (bind-mounted at /mnt/dvol/<vol>), so Aspire's WithDataVolume(<vol>)
// mounts the host's copy-on-write data instead of an empty store. Runs after
// dockerd is up and before the app starts; idempotent (skips volumes already
// created, and routes Spin didn't clone because there was no baseline).
func CreateBindVolumes(ctx context.Context, app state.App, sd state.Siding) error {
	for _, vol := range app.Volumes {
		dev := "/mnt/dvol/" + vol
		if out, _ := container.Exec(ctx, sd.Container, "sh", "-c", "test -d "+dev+" && echo yes"); !strings.Contains(out, "yes") {
			continue // no cp -c clone mounted for this volume
		}
		if out, _ := container.Exec(ctx, sd.Container, "docker", "volume", "inspect", vol); strings.Contains(out, vol) {
			continue // already created
		}
		if _, err := container.Exec(ctx, sd.Container, "docker", "volume", "create",
			"--driver", "local", "--opt", "type=none", "--opt", "o=bind", "--opt", "device="+dev, vol); err != nil {
			return fmt.Errorf("create bind-backed volume %q: %w", vol, err)
		}
		fmt.Printf("  data volume %q backed by host copy-on-write clone\n", vol)
	}
	return nil
}

// Switch points the app's stable front door at a target: either a siding or the
// special `host` target (the local app running natively from the main repo).
// Switch only repoints — it never starts or builds the app. Bringing the app up
// is `up`/`restart` (or `restart host` for the native app); keeping the two
// independent lets you, say, switch to the host to run just the DB yourself.
func Switch(ctx context.Context, app *state.App, target string) error {
	admin := caddy.NewAdmin()
	wasHost := app.LiveSiding == state.HostTarget

	if target == state.HostTarget {
		// Step the front door aside so the native app can bind the real ports
		// directly. We deliberately don't start it — that's `restart host` (or you,
		// e.g. running only the database). Ports stay dead until something binds them.
		// Only tear it down when coming FROM a siding: if the host is already live the
		// front door is already aside, and deleting absent servers would error. When
		// it does run, a failure (e.g. Caddy admin down) propagates so we don't mark
		// the host live while Caddy still holds the ports.
		if !wasHost {
			if err := caddy.RemoveFrontDoor(ctx, admin, *app); err != nil {
				return err
			}
		}
		app.LiveSiding = state.HostTarget
		return state.SaveApp(*app)
	}

	sd, ok := app.Sidings[target]
	if !ok {
		return fmt.Errorf("no siding %q", target)
	}
	// Coming back from the host, the local app may still hold the real ports and
	// the front-door servers were removed — stop it (best-effort) so it releases
	// them, then re-create the servers so Caddy re-binds before pointing at the guest.
	if wasHost {
		HostStop(ctx, *app)
		if err := caddy.EnsureFrontDoor(ctx, admin, *app); err != nil {
			return err
		}
		// If the switch fails after we've re-bound the front door, step it aside
		// again — otherwise state still says host while Caddy holds the ports with
		// nothing behind them. On success LiveSiding is the target, so this is a no-op.
		defer func() {
			if app.LiveSiding == state.HostTarget {
				_ = caddy.RemoveFrontDoor(ctx, admin, *app)
			}
		}()
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

// hostShellCmd runs a command on the HOST (not a guest) in the app's main repo,
// via a login shell so the user's PATH (incl. the aspire global tool) is loaded.
func hostShellCmd(ctx context.Context, app state.App, command string) error {
	// Set the working directory via cmd.Dir rather than injecting `cd %q` — in a
	// login shell, double quotes don't stop `$`/backtick expansion, so a repo path
	// with those characters would be mis-evaluated.
	_, err := proc.RunInDir(ctx, app.RepoPath, "sh", "-lc", command)
	return err
}

// HostStart runs the app natively on the host from the main repo, so the local
// copy serves the front-door ports directly. aspire → `aspire start` (managed,
// background); other runners → the contract's start command.
func HostStart(ctx context.Context, app state.App) error {
	cmd := app.Start
	if app.Runner == "" || app.Runner == runner.Aspire {
		// Raise the CLI start timeout past its 120s default for a heavy cold start.
		cmd = fmt.Sprintf("ASPIRE_CLI_START_TIMEOUT=900 aspire start --apphost \"%s\" --non-interactive", app.AppHostPath)
	}
	if cmd == "" {
		return fmt.Errorf("no host start command for runner %q — set `start` in the contract", app.Runner)
	}
	if err := hostShellCmd(ctx, app, cmd); err != nil {
		return fmt.Errorf("host start: %w", err)
	}
	return nil
}

// HostStop stops the natively-running app (best-effort — fine if nothing's
// running), freeing the front-door ports. aspire → `aspire stop`; else the
// contract's stop command. It also stops any host Docker container still
// publishing a front-door port: `aspire stop` leaves persistent-lifetime deps
// (e.g. the SQL container behind the layer4 route) running, and those hold the
// ports a siding switch needs to re-bind.
func HostStop(ctx context.Context, app state.App) {
	cmd := app.Stop
	if app.Runner == "" || app.Runner == runner.Aspire {
		cmd = "aspire stop --non-interactive"
	}
	if cmd != "" {
		_ = hostShellCmd(ctx, app, cmd)
	}
	for _, r := range app.FrontDoor {
		out, err := proc.Run(ctx, "docker", "ps", "--filter", fmt.Sprintf("publish=%d", r.ListenPort), "--format", "{{.ID}}")
		if err != nil {
			continue // no host docker / not reachable — nothing to free
		}
		for _, id := range strings.Fields(out.Stdout) {
			_, _ = proc.Run(ctx, "docker", "stop", id) // stop (keeps data); host-restart re-creates it
		}
	}
}

// Restart stops the app in the guest and starts it again (the configured stop +
// start), keeping the guest, dockerd, deps, and data up, then waits for it to be
// ready. This is the "bring a down route back up" path. Shared by `shunt restart`
// and the dashboard.
func Restart(ctx context.Context, app state.App, sd state.Siding) error {
	if err := StopApp(ctx, app, sd); err != nil {
		return err
	}
	// Clear the old start marker so WaitReady waits for the fresh run.
	_, _ = container.Exec(ctx, sd.Container, "sh", "-c", "> "+appLogPath)
	if err := StartApp(ctx, app, sd); err != nil {
		return err
	}
	return WaitReady(ctx, app, sd, 15*time.Minute)
}

// orDefaultStr returns v if non-empty, else def.
func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Recreate rebuilds a siding's guest with the current app config (memory, cpus,
// mounts, env) — for "reapply config to an existing siding". It keeps the
// worktree, branch, and data (the on-disk src + cp -c volume clones), replacing
// only the container, so guest-creation settings take effect. Run `up` after to
// start the app (the guest's Docker is fresh, so bind volumes + bridges rebuild).
func Recreate(ctx context.Context, app state.App, sd state.Siding) (state.Siding, error) {
	src, volRoot := Paths(app, sd.Name)
	mounts := []container.Mount{{Host: src, Guest: "/workspace"}}
	for _, vol := range app.Volumes {
		host := filepath.Join(volRoot, vol)
		if _, err := os.Stat(host); err == nil {
			mounts = append(mounts, container.Mount{Host: host, Guest: "/mnt/dvol/" + vol})
		}
	}
	for _, m := range app.Mounts {
		host, err := expandHome(m.Host)
		if err != nil {
			return sd, err
		}
		mounts = append(mounts, container.Mount{Host: host, Guest: m.Guest, ReadOnly: m.ReadOnly})
	}
	if nugetHost, err := expandHome("~/.nuget/packages"); err == nil {
		if _, statErr := os.Stat(nugetHost); statErr == nil {
			mounts = append(mounts, container.Mount{Host: nugetHost, Guest: "/root/.nuget/packages"})
		}
	}
	if IsWarmed(app) {
		mounts = append(mounts, container.Mount{Host: WarmTarPath(app), Guest: guestWarmTar, ReadOnly: true})
	}
	if err := container.Remove(ctx, sd.Container); err != nil {
		return sd, err
	}
	if err := container.Run(ctx, container.RunOpts{
		Name:      sd.Container,
		Image:     config.BaseImageTag(),
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
	sd.Bridges = map[string]int{} // fresh guest Docker — rebuild bind volumes + bridges on next up
	return sd, nil
}
