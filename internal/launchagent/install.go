package launchagent

import (
	"context"
	"fmt"
	"os"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/proc"
)

// Install writes the channel's Caddy plist and (re)loads it into the user's
// launchd domain, leaving Caddy running. caddyBin and bootstrapPath are baked
// into the agent's launch command.
func Install(ctx context.Context, caddyBin, bootstrapPath string) error {
	plistPath, err := writePlist(caddyBin, bootstrapPath)
	if err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	label := config.Current().LaunchAgentID

	// Replace any prior instance: bootout is allowed to fail (not loaded yet).
	_, _ = proc.Run(ctx, "launchctl", "bootout", domain, plistPath)
	if _, err := proc.Run(ctx, "launchctl", "bootstrap", domain, plistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	// enable + kickstart are best-effort hardening so the agent is running now.
	_, _ = proc.Run(ctx, "launchctl", "enable", domain+"/"+label)
	if _, err := proc.Run(ctx, "launchctl", "kickstart", "-k", domain+"/"+label); err != nil {
		return fmt.Errorf("launchctl kickstart: %w", err)
	}
	return nil
}

// InstallDashboard writes + loads the per-channel dashboard LaunchAgent so the
// web UI stays up (KeepAlive) like Caddy. binPath is the shunt binary to run
// `dashboard` from (usually os.Executable()).
func InstallDashboard(ctx context.Context, binPath string) error {
	plistPath, err := config.DashboardPlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepathDir(plistPath), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	content, err := renderDashboard(binPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(plistPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write dashboard plist: %w", err)
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	label := config.Current().DashboardLaunchAgentID
	_, _ = proc.Run(ctx, "launchctl", "bootout", domain, plistPath)
	if _, err := proc.Run(ctx, "launchctl", "bootstrap", domain, plistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap dashboard: %w", err)
	}
	_, _ = proc.Run(ctx, "launchctl", "enable", domain+"/"+label)
	_, _ = proc.Run(ctx, "launchctl", "kickstart", "-k", domain+"/"+label)
	return nil
}

func filepathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}
