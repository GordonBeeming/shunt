// Package contract reads the committed in-repo .shunt.app.json — the developer's
// declaration of an app's stable front-door ports and runner settings. shunt
// derives its runtime state from this.
package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/gordonbeeming/shunt/internal/state"
)

// FileName is the per-repo contract file shunt looks for.
const FileName = ".shunt.app.json"

// Contract is the parsed .shunt.app.json.
type Contract struct {
	// Runner selects how the app starts (aspire | dotnet | node | custom).
	// Empty = auto-detect at `app add`. Aspire runs the AppHost through the Aspire
	// CLI; the others use Start. Every runner declares a guestPort per route.
	Runner  string `json:"runner"`
	Start   string `json:"start"`   // start command for dotnet/node/custom (run in Workdir)
	Stop    string `json:"stop"`    // optional clean-stop command (e.g. `aspire stop`); shunt's force-kill is the fallback
	Workdir string `json:"workdir"` // dir to run Start/Stop in, relative to the repo (default repo root)

	AppHost   string            `json:"apphost"`   // aspire only: rel path to the AppHost project/csproj
	FrontDoor []FrontDoorRoute  `json:"frontDoor"` // stable front-door routes
	Env       map[string]string `json:"env"`       // extra guest env (Aspire parameters, secrets)
	Mounts    []state.MountSpec `json:"mounts"`    // explicit extra host->guest mounts (e.g. user-secrets)
	// Registry dependency-image tags incorporated into an immutable,
	// content-addressed cache generation. Per-image Docker archives are derived
	// exports loaded into each guest; `shunt warm` refreshes every tag.
	PrebakeImages []string `json:"prebakeImages"`
	// Local dependency images built on the host and stored in the same cache as
	// registry images. Relative paths are resolved from the app repository.
	PrebakeBuilds []state.PrebakeBuild `json:"prebakeBuilds"`
	// Volumes lists the Docker named volumes shunt stores in host-backed siding
	// directories. A new siding clones the current baseline, or starts empty when
	// no baseline exists. Omit this list when the app has no persistent test data.
	Volumes []string `json:"dataVolumes"`
	// FixedPorts pins the front door to the exact listenPort values (no channel
	// offset). Use when the app's config + Entra redirect URIs point at specific
	// ports. Only one channel can run the app at a time on those ports.
	FixedPorts bool `json:"fixedPorts"`
	// Memory caps each siding guest's RAM (Apple `container -m`, e.g. "16g").
	// Heavy stacks (SQL + several services) need headroom; empty uses shunt's
	// default. CPUs similarly caps cores (e.g. "4"); empty uses the default.
	Memory string `json:"memory"`
	CPUs   string `json:"cpus"`
	// HealthPort/HealthPath define the endpoint the dashboard hits (from inside the
	// guest) to decide whether a siding is actually "running" vs merely booted. Empty
	// HealthPort defaults to the Aspire dashboard's home page, which serves whenever
	// the AppHost is up. For non-Aspire apps, point it at a real health route (e.g.
	// port 8080, path "/healthz"). HealthPath defaults to "/".
	HealthPort int    `json:"healthPort"`
	HealthPath string `json:"healthPath"`
	// DisableCache, when true, makes the front door send `Cache-Control: no-store`
	// on every HTTP response for this app. Use it for Blazor/SPA apps that serve
	// stale assets when you switch sidings on the shared front-door port — the whole
	// environment then goes through uncached.
	DisableCache bool `json:"disableCache"`
}

// FrontDoorRoute is one stable port mapping in the contract.
type FrontDoorRoute struct {
	Key        string `json:"key"`        // logical name: frontend | api | db
	Kind       string `json:"kind"`       // http | layer4
	ListenPort int    `json:"listenPort"` // stable host port (before channel offset)
	Resource   string `json:"resource"`   // aspire: resource name to proxy
	Endpoint   string `json:"endpoint"`   // aspire: endpoint name within that resource (optional)
	GuestPort  int    `json:"guestPort"`  // required: the in-guest port the app binds
	TLS        bool   `json:"tls"`        // terminate TLS at the front door
	// Optional excludes the route from the readiness check. It is still bridged
	// and still served the moment it comes up; it just does not hold the app back,
	// which suits a slow dev server next to an API and a database that must be up.
	Optional bool `json:"optional"`
}

// Load reads and validates the contract from a repo directory.
func Load(repoPath string) (Contract, error) {
	path := filepath.Join(repoPath, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Contract{}, fmt.Errorf("%s not found in %s — add one to register this app", FileName, repoPath)
		}
		return Contract{}, fmt.Errorf("read %s: %w", path, err)
	}
	var c Contract
	if err := json.Unmarshal(data, &c); err != nil {
		return Contract{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.validate(repoPath); err != nil {
		return Contract{}, fmt.Errorf("%s: %w", FileName, err)
	}
	return c, nil
}

var dockerVolumeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ValidateVolumeName matches the Docker engine's named-volume grammar.
func ValidateVolumeName(volume string) error {
	if !dockerVolumeNamePattern.MatchString(volume) {
		return fmt.Errorf("volume name must match %s", dockerVolumeNamePattern.String())
	}
	return nil
}

func (c *Contract) validate(repoPath string) error {
	// Every route states the port its resource binds inside the guest. shunt does
	// not work it out: a missing port used to fall through to an Aspire-specific
	// discovery path that could not serve any other runner, so it is config now,
	// and a missing one fails here rather than at `up`.
	for i, r := range c.FrontDoor {
		if r.GuestPort == 0 {
			key := r.Key
			if key == "" {
				key = fmt.Sprintf("#%d", i)
			}
			return fmt.Errorf("frontDoor route %q: guestPort is required — set it to the port this route's resource listens on inside the guest (for a project resource, its launch-profile port; for a container, the port the app pins)", key)
		}
	}
	seenImage := make(map[string]struct{}, len(c.PrebakeImages))
	for i, ref := range c.PrebakeImages {
		parsed, err := name.ParseReference(ref)
		if err != nil {
			return fmt.Errorf("prebakeImages[%d] %q: invalid image reference: %w", i, ref, err)
		}
		if _, ok := parsed.(name.Digest); ok {
			return fmt.Errorf("prebakeImages[%d] %q: digest-pinned images are not supported because Docker save/load cannot recreate a runnable repo@digest alias; use a tag", i, ref)
		}
		if _, exists := seenImage[parsed.Name()]; exists {
			return fmt.Errorf("prebakeImages: duplicate image reference %q", ref)
		}
		seenImage[parsed.Name()] = struct{}{}
	}
	for i := range c.PrebakeBuilds {
		build := &c.PrebakeBuilds[i]
		parsed, err := name.ParseReference(build.Image)
		if err != nil {
			return fmt.Errorf("prebakeBuilds[%d].image %q: invalid image reference: %w", i, build.Image, err)
		}
		if _, ok := parsed.(name.Digest); ok {
			return fmt.Errorf("prebakeBuilds[%d].image %q: output image must use a tag", i, build.Image)
		}
		if _, exists := seenImage[parsed.Name()]; exists {
			return fmt.Errorf("duplicate prebake image output %q", build.Image)
		}
		seenImage[parsed.Name()] = struct{}{}
		if build.Context == "" {
			return fmt.Errorf("prebakeBuilds[%d].context is required", i)
		}
		if build.Dockerfile == "" {
			return fmt.Errorf("prebakeBuilds[%d].dockerfile is required", i)
		}
		build.Context, err = resolveBuildPath(repoPath, build.Context)
		if err != nil {
			return fmt.Errorf("prebakeBuilds[%d].context: %w", i, err)
		}
		info, err := os.Stat(build.Context)
		if err != nil {
			return fmt.Errorf("prebakeBuilds[%d].context %q: %w", i, build.Context, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("prebakeBuilds[%d].context %q is not a directory", i, build.Context)
		}
		build.Dockerfile, err = resolveBuildPath(repoPath, build.Dockerfile)
		if err != nil {
			return fmt.Errorf("prebakeBuilds[%d].dockerfile: %w", i, err)
		}
		info, err = os.Stat(build.Dockerfile)
		if err != nil {
			return fmt.Errorf("prebakeBuilds[%d].dockerfile %q: %w", i, build.Dockerfile, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("prebakeBuilds[%d].dockerfile %q is not a regular file", i, build.Dockerfile)
		}
		for key, value := range build.BuildArgs {
			if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "=\x00\r\n") {
				return fmt.Errorf("prebakeBuilds[%d].buildArgs contains invalid key %q", i, key)
			}
			if strings.ContainsRune(value, 0) {
				return fmt.Errorf("prebakeBuilds[%d].buildArgs[%q] contains a NUL byte", i, key)
			}
		}
	}
	seenVolume := make(map[string]struct{}, len(c.Volumes))
	for i, volume := range c.Volumes {
		if err := ValidateVolumeName(volume); err != nil {
			return fmt.Errorf("dataVolumes[%d] %q: %w", i, volume, err)
		}
		if _, exists := seenVolume[volume]; exists {
			return fmt.Errorf("dataVolumes: duplicate volume name %q", volume)
		}
		seenVolume[volume] = struct{}{}
	}
	// apphost is Aspire-only (StartApp reads it just for the aspire runner). A
	// dotnet/node/custom app declares `runner` + `start` instead, so only require
	// it for the aspire runner (empty runner defaults to aspire).
	if (c.Runner == "" || c.Runner == "aspire") && c.AppHost == "" {
		return errors.New("apphost is required for the aspire runner")
	}
	if len(c.FrontDoor) == 0 {
		return errors.New("at least one frontDoor route is required")
	}
	seenKey := map[string]bool{}
	seenPort := map[int]bool{}
	for i, r := range c.FrontDoor {
		switch r.Kind {
		case state.KindHTTP, state.KindLayer4:
		default:
			return fmt.Errorf("frontDoor[%d] (%s): kind must be %q or %q", i, r.Key, state.KindHTTP, state.KindLayer4)
		}
		if r.Key == "" {
			return fmt.Errorf("frontDoor[%d]: key is required", i)
		}
		if seenKey[r.Key] {
			return fmt.Errorf("frontDoor: duplicate key %q", r.Key)
		}
		seenKey[r.Key] = true
		if r.ListenPort <= 0 || r.ListenPort > 65535 {
			return fmt.Errorf("frontDoor[%d] (%s): listenPort %d out of range", i, r.Key, r.ListenPort)
		}
		if seenPort[r.ListenPort] {
			return fmt.Errorf("frontDoor: duplicate listenPort %d", r.ListenPort)
		}
		seenPort[r.ListenPort] = true
		if r.Resource == "" {
			return fmt.Errorf("frontDoor[%d] (%s): resource is required", i, r.Key)
		}
	}
	return nil
}

func resolveBuildPath(repoPath, configured string) (string, error) {
	repoRoot, err := filepath.Abs(filepath.Clean(repoPath))
	if err != nil {
		return "", err
	}
	repoRoot, err = filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", fmt.Errorf("resolve app repository: %w", err)
	}
	path := configured
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoRoot, path)
	}
	path, err = filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(repoRoot, path)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q is outside app repository %q", configured, repoRoot)
	}
	return path, nil
}
