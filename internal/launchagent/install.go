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
