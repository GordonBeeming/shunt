// Package config resolves shunt's channel identity and everything derived from
// it. A single build-time Channel value drives the binary name, global dir,
// project-local dir name, Caddy admin port, front-door port offset, LaunchAgent
// label, and container-name prefix — so release/beta/dev builds install and run
// side by side without colliding.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Defaults applied when the user hasn't set a value (and the contract doesn't
// override). BranchPrefix override (e.g. "gb/shunt/") keeps siding branches on
// your convention; Memory/CPUs cap each guest — keep the default modest so many
// sidings coexist, and bump heavy apps via the contract or `shunt config`.
const (
	DefaultBranchPrefix = "shunt/"
	DefaultGuestMemory  = "4g"
	DefaultGuestCPUs    = "4"
)

// UserConfig holds per-user shunt preferences at <GlobalDir>/config.json. Empty
// fields fall back to the defaults above (via the accessors).
type UserConfig struct {
	BranchPrefix string `json:"branchPrefix,omitempty"`
	Memory       string `json:"memory,omitempty"`
	CPUs         string `json:"cpus,omitempty"`
}

// UserConfigPath is <GlobalDir>/config.json for this channel.
func UserConfigPath() (string, error) {
	dir, err := GlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// LoadUserConfig reads the raw user config (unset fields stay empty; accessors
// apply defaults).
func LoadUserConfig() UserConfig {
	var uc UserConfig
	path, err := UserConfigPath()
	if err != nil {
		return uc
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return uc
	}
	_ = json.Unmarshal(data, &uc)
	return uc
}

// SaveUserConfig writes the user config (creating the dir).
func SaveUserConfig(uc UserConfig) error {
	path, err := UserConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(uc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// BranchPrefix is the configured siding-branch prefix (default "shunt/").
func BranchPrefix() string { return orDefault(LoadUserConfig().BranchPrefix, DefaultBranchPrefix) }

// GuestMemory / GuestCPUs are the user-configured per-guest defaults (the contract
// can override per app); they fall back to the package defaults.
func GuestMemory() string { return orDefault(LoadUserConfig().Memory, DefaultGuestMemory) }
func GuestCPUs() string   { return orDefault(LoadUserConfig().CPUs, DefaultGuestCPUs) }

// Channel is set at build time via:
//
//	go build -ldflags "-X github.com/gordonbeeming/shunt/internal/config.Channel=release"
//
// It defaults to "dev" so plain `go run`/`go build` during development never
// touches release state.
var Channel = "dev"

// Identity is the fully-resolved, channel-scoped set of names/ports/paths.
type Identity struct {
	Channel         string // "release" | "beta" | "dev"
	BinaryName      string // shunt | shunt-beta | shunt-dev
	GlobalDirName   string // .shunt | .shunt-beta | .shunt-dev (under $HOME)
	ProjectDirName  string // same name, used as the sibling project-local dir
	AdminPort       int    // Caddy admin API port for this channel
	DashboardPort   int    // shunt dashboard web UI port for this channel
	PortOffset      int    // added to each .shunt.app.json listenPort
	LaunchAgentID   string // launchd label
	ContainerPrefix string // prefix for guest container names
}

// known returns the identity table. Unknown channel values fall back to dev so a
// mis-set ldflag can never masquerade as release.
func known(channel string) Identity {
	switch channel {
	case "release":
		return Identity{
			Channel:         "release",
			BinaryName:      "shunt",
			GlobalDirName:   ".shunt",
			ProjectDirName:  ".shunt",
			AdminPort:       2019,
			DashboardPort:   2020,
			PortOffset:      0,
			LaunchAgentID:   "com.gordonbeeming.shunt.caddy",
			ContainerPrefix: "shunt",
		}
	case "beta":
		return Identity{
			Channel:         "beta",
			BinaryName:      "shunt-beta",
			GlobalDirName:   ".shunt-beta",
			ProjectDirName:  ".shunt-beta",
			AdminPort:       2119,
			DashboardPort:   2120,
			PortOffset:      100,
			LaunchAgentID:   "com.gordonbeeming.shunt-beta.caddy",
			ContainerPrefix: "shuntbeta",
		}
	default:
		return Identity{
			Channel:         "dev",
			BinaryName:      "shunt-dev",
			GlobalDirName:   ".shunt-dev",
			ProjectDirName:  ".shunt-dev",
			AdminPort:       2219,
			DashboardPort:   2220,
			PortOffset:      200,
			LaunchAgentID:   "com.gordonbeeming.shunt-dev.caddy",
			ContainerPrefix: "shuntdev",
		}
	}
}

// Current returns the identity for the channel this binary was built as.
func Current() Identity { return known(Channel) }

// GlobalDir is shunt's per-channel machinery dir under $HOME (caddy binary,
// logs, base image marker, registry.json). It never holds project code.
func GlobalDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, Current().GlobalDirName), nil
}

// AdminAddr is the loopback host:port of this channel's Caddy admin API.
func AdminAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", Current().AdminPort)
}

// DashboardAddr is the loopback host:port of this channel's dashboard web UI.
func DashboardAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", Current().DashboardPort)
}

// AdminBaseURL is the admin API base URL, e.g. http://127.0.0.1:2019.
func AdminBaseURL() string {
	return "http://" + AdminAddr()
}

// RegistryPath is the global project index for this channel.
func RegistryPath() (string, error) {
	dir, err := GlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "registry.json"), nil
}

// CaddyBinaryPath is where the xcaddy-built (with caddy-l4) binary lives.
func CaddyBinaryPath() (string, error) {
	dir, err := GlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "caddy", "shunt"), nil
}

// ProjectConfigDir returns the sibling .shunt[-channel]/<project> dir for a repo
// at repoPath — one directory up from the repo, namespaced by project folder
// name so the same repo can be driven under multiple channels at once.
func ProjectConfigDir(repoPath string) (string, error) {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return "", fmt.Errorf("resolve repo path: %w", err)
	}
	parent := filepath.Dir(abs)
	project := filepath.Base(abs)
	return filepath.Join(parent, Current().ProjectDirName, project), nil
}

// ContainerName builds a channel-prefixed guest name for an app+siding.
func ContainerName(app, siding string) string {
	return fmt.Sprintf("%s_%s_%s", Current().ContainerPrefix, app, siding)
}

// BaseImageTag is the channel-scoped guest base image name, so a dev rebuild
// never clobbers the release image mid-change.
func BaseImageTag() string {
	if Current().Channel == "release" {
		return "shunt-base:latest"
	}
	return "shunt-base-" + Current().Channel + ":latest"
}

// BootstrapConfigPath is where shunt writes Caddy's initial config; the
// LaunchAgent points Caddy at it (with --resume, Caddy prefers its autosave).
func BootstrapConfigPath() (string, error) {
	dir, err := GlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "caddy", "bootstrap.json"), nil
}

// LogDir is the per-channel log directory (Caddy stdout/stderr).
func LogDir() (string, error) {
	dir, err := GlobalDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "logs"), nil
}

// LaunchAgentPlistPath is ~/Library/LaunchAgents/<label>.plist for this channel.
func LaunchAgentPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", Current().LaunchAgentID+".plist"), nil
}
