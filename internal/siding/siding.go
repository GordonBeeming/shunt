// Package siding orchestrates a single experiment guest end to end: clone the
// repo, launch the Aspire app inside an Apple container, bridge its loopback
// endpoints to the guest IP, discover them, and point the host Caddy at them.
package siding

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/aspire"
	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/state"
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

// guestEnv is the proven Aspire-13 launch env: unsecured anonymous dashboard (so
// shunt can read the resource service without an API key), all endpoints pinned,
// http transport, in-guest Docker runtime.
func guestEnv() map[string]string {
	return map[string]string{
		"DOTNET_USE_POLLING_FILE_WATCHER":            "1",
		"DOTNET_ASPIRE_CONTAINER_RUNTIME":            "docker",
		"ASPIRE_ALLOW_UNSECURED_TRANSPORT":           "true",
		"ASPIRE_DASHBOARD_UNSECURED_ALLOW_ANONYMOUS": "true",
		"ASPNETCORE_URLS":                            fmt.Sprintf("http://0.0.0.0:%d", guestDashboardPort),
		"ASPIRE_DASHBOARD_OTLP_ENDPOINT_URL":         "http://127.0.0.1:18889",
		"ASPIRE_DASHBOARD_MCP_ENDPOINT_URL":          "http://127.0.0.1:18891",
		"ASPIRE_RESOURCE_SERVICE_ENDPOINT_URL":       fmt.Sprintf("http://127.0.0.1:%d", guestRSPort),
	}
}

// Spin clones the repo + data volumes and launches the guest. It does not wait
// for the app to be ready (see Activate).
func Spin(ctx context.Context, app state.App, name, branch string) (state.Siding, error) {
	src, volRoot := Paths(app, name)
	if err := fsclone.CloneRepo(ctx, originForClone(app), src, branch); err != nil {
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

	guestName := config.ContainerName(app.Name, name)
	runCmd := fmt.Sprintf("cd /workspace && dotnet run --no-launch-profile --project %s", app.AppHostPath)
	if err := container.Run(ctx, container.RunOpts{
		Name:      guestName,
		Image:     config.BaseImageTag(),
		Init:      true,
		CapAddAll: true,
		Mounts:    mounts,
		Env:       guestEnv(),
		Cmd:       []string{"/bin/sh", "-lc", runCmd},
	}); err != nil {
		return state.Siding{}, err
	}

	return state.Siding{
		Name:      name,
		Branch:    branch,
		Container: guestName,
		RSPort:    guestRSPort,
		Bridges:   map[string]int{},
	}, nil
}

// WaitStarted blocks until the guest logs that the Aspire app started, the guest
// exits, or the deadline passes.
func WaitStarted(ctx context.Context, guestName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := container.State(ctx, guestName)
		if err == nil && st != "running" {
			return fmt.Errorf("guest %s exited (state=%s) before the app started", guestName, st)
		}
		out, _ := container.Logs(ctx, guestName)
		if strings.Contains(out, startedMarker) {
			return nil
		}
		if strings.Contains(out, "Unhandled exception") || strings.Contains(out, "Hosting failed") {
			return fmt.Errorf("guest %s: Aspire app failed to start (see `container logs %s`)", guestName, guestName)
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("timed out waiting for %s to start", guestName)
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
	eps, err := aspire.Discover(ctx, fmt.Sprintf("%s:%d", ip, rsExtPort), "")
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
		ext := routeExtBase + i
		if err := container.Bridge(ctx, sd.Container, ext, ep.Port); err != nil {
			return err
		}
		sd.Bridges[r.Key] = ext
	}
	return nil
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
