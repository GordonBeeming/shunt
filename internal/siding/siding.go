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
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/gordonbeeming/shunt/internal/ui"
)

const (
	// Guest-internal ports shunt pins (each guest is isolated, so these are the
	// same across sidings).
	guestDashboardPort = 18888
	guestRSPort        = 18890

	// Guest-external (0.0.0.0) ports the in-guest socat bridges listen on. Reused
	// across guests since each guest has its own IP.
	rsExtPort    = 38890
	routeExtBase = 39001

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
	env := map[string]string{
		"DOTNET_USE_POLLING_FILE_WATCHER":            "1",
		"DOTNET_ASPIRE_CONTAINER_RUNTIME":            "docker",
		"ASPIRE_ALLOW_UNSECURED_TRANSPORT":           "true",
		"ASPIRE_DASHBOARD_UNSECURED_ALLOW_ANONYMOUS": "true",
		// We run the AppHost with --no-launch-profile, which skips the
		// launchSettings that normally set Development. Set it explicitly so
		// .NET loads the dev's user-secrets (where Aspire parameters/secrets like
		// DB passwords and API keys live) — otherwise params are ValueMissing.
		"ASPNETCORE_ENVIRONMENT": "Development",
		"DOTNET_ENVIRONMENT":     "Development",
		// Run apps targeting an older .NET (e.g. net9.0) on the base image's
		// newer runtime instead of requiring every framework version to be present.
		"DOTNET_ROLL_FORWARD":                  "Major",
		"ASPNETCORE_URLS":                      fmt.Sprintf("http://0.0.0.0:%d", guestDashboardPort),
		"ASPIRE_DASHBOARD_OTLP_ENDPOINT_URL":   "http://127.0.0.1:18889",
		"ASPIRE_DASHBOARD_MCP_ENDPOINT_URL":    "http://127.0.0.1:18891",
		"ASPIRE_RESOURCE_SERVICE_ENDPOINT_URL": fmt.Sprintf("http://127.0.0.1:%d", guestRSPort),
	}
	// App-specific env from the contract (Aspire parameters, secrets) wins.
	for k, v := range app.Env {
		env[k] = v
	}
	return env
}

// Spin clones the repo + data volumes and launches the guest. It does not wait
// for the app to be ready (see Activate).
func Spin(ctx context.Context, app state.App, name, branch string) (state.Siding, error) {
	src, volRoot := Paths(app, name)
	wtBranch := "shunt/" + name
	if err := fsclone.AddWorktree(ctx, app.RepoPath, src, wtBranch, branch); err != nil {
		return state.Siding{}, err
	}

	mounts := []container.Mount{{Host: src, Guest: "/workspace"}}
	for _, dv := range app.DataVolumes {
		host := filepath.Join(volRoot, dv.Resource)
		if err := fsclone.CloneVolume(ctx, filepath.Join(app.ConfigDir, "baseline", dv.Resource), host); err != nil {
			return state.Siding{}, err
		}
		mounts = append(mounts, container.Mount{Host: host, Guest: dv.GuestPath})
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
		// Heavy Aspire stacks (SQL Server, Azurite, several projects + the nested
		// Docker daemon) need real headroom; the runtime default of ~1 GB OOMs.
		Memory: "6g",
		CPUs:   "4",
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
// It runs under `dotnet watch` so host edits hot-reload across the VM boundary
// (DOTNET_USE_POLLING_FILE_WATCHER is set at Spin time). Watch is best-effort —
// `shunt restart` (StopApp + StartApp) is the reliable full-rebuild path that
// keeps the guest + dockerd + dependency containers + their data running.
func StartApp(ctx context.Context, app state.App, sd state.Siding) error {
	runCmd := fmt.Sprintf("cd /workspace && dotnet watch --non-interactive --project %s run --no-launch-profile > %s 2>&1",
		app.AppHostPath, appLogPath)
	return container.ExecDetached(ctx, sd.Container, "/bin/sh", "-lc", runCmd)
}

// StopApp kills the AppHost (dotnet watch/run) process inside the guest WITHOUT
// touching the guest, dockerd, or the dependency containers — so a rebuild can
// happen while SQL etc. and their data stay up. The docker-managed dependency
// containers are a separate process tree, unaffected by these kills.
func StopApp(ctx context.Context, sd state.Siding) error {
	// Kill the whole Aspire orchestration — the watch wrapper, the AppHost, the
	// dashboard, and the DCP control plane — so none of them keep holding ports.
	// Dependency containers (SQL, Azurite, etc.) run under dockerd and don't match
	// these patterns, so they stay up.
	_, err := container.Exec(ctx, sd.Container, "sh", "-c",
		"for p in 'dotnet watch' 'AppHost' 'Aspire.Dashboard' '/dcp '; do pkill -f \"$p\" 2>/dev/null; done; true")
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

	// Bridge the resource service so shunt can discover from the host.
	if err := container.Bridge(ctx, sd.Container, rsExtPort, guestRSPort); err != nil {
		return err
	}
	// "Distributed application started" fires before every dependency has
	// published its endpoint URL, so poll until all front-door resources resolve.
	eps, err := discoverReady(ctx, fmt.Sprintf("%s:%d", ip, rsExtPort), app.FrontDoor, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("discover endpoints: %w", err)
	}

	if sd.Bridges == nil {
		sd.Bridges = map[string]int{}
	}
	for i, r := range app.FrontDoor {
		ep, ok := aspire.Find(eps, r.Resource, r.Endpoint)
		if !ok {
			return fmt.Errorf("route %q: no live endpoint for resource %q (endpoint %q) — discovered: %s",
				r.Key, r.Resource, r.Endpoint, summarize(eps))
		}
		// For container-backed resources, target the real docker-published port
		// rather than Aspire's DCP proxy port (which doesn't forward through a
		// bridge). Project/process resources have no container, so keep ep.Port.
		realPort := ep.Port
		if dp := container.DockerPort(ctx, sd.Container, ep.Resource); dp > 0 {
			realPort = dp
		}
		ext := routeExtBase + i
		if err := container.Bridge(ctx, sd.Container, ext, realPort); err != nil {
			return err
		}
		sd.Bridges[r.Key] = ext
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
	for _, r := range app.FrontDoor {
		ext, ok := sd.Bridges[r.Key]
		if !ok {
			return fmt.Errorf("route %q has no bridge on siding %q (activate it first)", r.Key, sd.Name)
		}
		path, body, err := caddy.DialPatch(r, fmt.Sprintf("%s:%d", ip, ext))
		if err != nil {
			return err
		}
		if err := admin.Patch(ctx, path, body); err != nil {
			return fmt.Errorf("repoint route %q: %w", r.Key, err)
		}
	}
	return nil
}

// DashboardURL is the guest's directly-reachable Aspire dashboard.
func DashboardURL(sd state.Siding) string {
	if sd.LastIP == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", sd.LastIP, guestDashboardPort)
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
