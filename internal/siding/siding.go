// Package siding orchestrates a single experiment guest end to end: clone the
// repo, launch the Aspire app inside an Apple container, bridge its loopback
// endpoints to the guest IP, discover them, and point the host Caddy at them.
package siding

import (
	"context"
	"fmt"
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
func Spin(ctx context.Context, app state.App, name, branch string) (state.Siding, error) {
	src, volRoot := Paths(app, name)
	wtBranch := config.BranchPrefix() + name
	if err := fsclone.AddWorktree(ctx, app.RepoPath, src, wtBranch, branch); err != nil {
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

// appLogPath is where StartApp redirects the AppHost output inside the guest, so
// WaitStarted can read it (the AppHost runs as a detached exec, not pid 1, so it
// doesn't show up in `container logs`).
const appLogPath = "/var/log/apphost.log"

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
		// Run WITH the launch profile so each project binds the fixed ports from
		// its launchSettings (7011, 5001, …) — the ports the contract front-doors
		// host==guest — instead of Aspire-assigned ones. shunt's pinned dashboard/
		// resource-service ports come from explicit ASPIRE_* env (not launchSettings),
		// so discovery still connects on 18888/18890.
		runCmd = fmt.Sprintf("cd /workspace && dotnet run --project %s > %s 2>&1",
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
	if app.Stop != "" {
		wd := "/workspace"
		if app.Workdir != "" {
			wd = "/workspace/" + app.Workdir
		}
		_, _ = container.Exec(ctx, sd.Container, "/bin/sh", "-lc", fmt.Sprintf("cd %s && %s", wd, app.Stop))
	}
	script := `
for p in 'dotnet watch' 'dotnet-watch' 'dotnet run'; do pkill -9 -f "$p" 2>/dev/null; done
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
		fmt.Printf("  ⚠ %d route(s) not up yet: %s — they'll serve automatically once they start\n",
			len(pending), strings.Join(pending, ", "))
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

// Switch points the front door at a siding: it activates it (bridges) if not yet
// done, repoints Caddy, marks it live, and persists. This is the fast path — a
// Caddy rebind, no app restart — and assumes the siding is running on its ports.
// Shared by `shunt switch` and the dashboard.
func Switch(ctx context.Context, app *state.App, name string) error {
	sd, ok := app.Sidings[name]
	if !ok {
		return fmt.Errorf("no siding %q", name)
	}
	if len(sd.Bridges) == 0 {
		if err := Activate(ctx, *app, &sd); err != nil {
			return err
		}
	}
	if err := PointCaddy(ctx, *app, &sd); err != nil {
		return err
	}
	app.LiveSiding = name
	app.Sidings[name] = sd
	return state.SaveApp(*app)
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
