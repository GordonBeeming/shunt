// Package launchagent installs the per-channel Caddy LaunchAgent so the proxy
// runs in the background and survives logout/reboot.
package launchagent

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gordonbeeming/shunt/internal/config"
)

// plistTemplate runs Caddy with --resume so it reloads its autosaved config on
// restart, falling back to the bootstrap config on first run. XDG_CONFIG_HOME is
// pinned per channel so each channel's Caddy autosaves to its own dir (no
// cross-channel collision).
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
    <string>--resume</string>
    <string>--config</string><string>%s</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>XDG_CONFIG_HOME</key><string>%s</string>
    <key>XDG_DATA_HOME</key><string>%s</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
  <key>WorkingDirectory</key><string>%s</string>
</dict>
</plist>
`

// render produces the plist XML for this channel's Caddy agent.
func render(caddyBin, bootstrapPath string) (string, error) {
	id := config.Current()
	globalDir, err := config.GlobalDir()
	if err != nil {
		return "", err
	}
	logDir, err := config.LogDir()
	if err != nil {
		return "", err
	}
	xdg := filepath.Join(globalDir, "caddy", "xdg")
	return fmt.Sprintf(plistTemplate,
		id.LaunchAgentID,
		caddyBin,
		bootstrapPath,
		xdg, xdg,
		filepath.Join(logDir, "caddy.out.log"),
		filepath.Join(logDir, "caddy.err.log"),
		globalDir,
	), nil
}

// writePlist renders and writes the plist, returning its path.
func writePlist(caddyBin, bootstrapPath string) (string, error) {
	plistPath, err := config.LaunchAgentPlistPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return "", fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	content, err := render(caddyBin, bootstrapPath)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(plistPath, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write plist: %w", err)
	}
	return plistPath, nil
}
