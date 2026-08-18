// Package state holds shunt's persisted model: a thin global registry mapping
// projects to their config dirs, and the per-project runtime state (the app and
// its sidings) stored project-locally in <repos>/.shunt[-channel]/<project>/state-v2.json.
package state

import (
	"encoding/json"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// RegistryVersion / StateVersion let us migrate on-disk formats later.
	RegistryVersion = 1
	StateVersion    = 2

	// KindHTTP / KindLayer4 are the two front-door route kinds.
	KindHTTP   = "http"
	KindLayer4 = "layer4"

	// PlaceholderDial is the upstream a route points at before any siding is
	// live. A refused connection here is an honest "nothing live yet" signal.
	PlaceholderDial = "127.0.0.1:1"

	// HostTarget is retained only to recognize and migrate legacy state. It remains
	// reserved so an old live-target value can never be confused with a siding.
	HostTarget = "host"
)

// MaterializationPhase records how far a siding has grown beyond its Git
// worktree. The empty value is reserved for legacy state and is projected on
// load without rewriting the state file.
type MaterializationPhase string

const (
	PhaseWorktree MaterializationPhase = "worktree"
	PhaseData     MaterializationPhase = "data"
	PhaseGuest    MaterializationPhase = "guest"
	PhaseParked   MaterializationPhase = "parked"
)

// RemovalStage is a durable checkpoint in the project-exclusive siding
// removal workflow. Commands can resume from the last published stage after a
// process crash without repeating a baseline promotion.
type RemovalStage string

const (
	RemovalStarted            RemovalStage = "started"
	RemovalBasePinned         RemovalStage = "base-pinned"
	RemovalBaselinePromoted   RemovalStage = "baseline-promoted"
	RemovalGuestRemoved       RemovalStage = "guest-removed"
	RemovalWorktreeRemoved    RemovalStage = "worktree-removed"
	RemovalFilesRemoved       RemovalStage = "files-removed"
	RemovalOperationForgotten RemovalStage = "operation-forgotten"
)

// RemovalOperation journals the one project-exclusive destructive operation
// that may be resumed. Force authorizes lifecycle overrides; ExplicitDiscard
// separately records permission to lose unpreserved Git work. Removing may hold
// legacy siding names, while Targets/RecoveryRefs are canonical exact refs and
// immutable once destructive stages begin. Recovery refs are short-lived crash
// guards; WitnessRefs are deduplicated durable proof for preserved targets.
type RemovalOperation struct {
	ID                      string          `json:"id"`
	Siding                  string          `json:"siding"`
	Stage                   RemovalStage    `json:"stage"`
	GenerationID            string          `json:"generationId,omitempty"`
	StartedAt               string          `json:"startedAt"`
	Force                   bool            `json:"force,omitempty"`
	Safety                  string          `json:"safetyFingerprint,omitempty"`
	Removing                []string        `json:"removing,omitempty"`
	ObservedWorktreeBranch  string          `json:"observedWorktreeBranch,omitempty"`
	PreservationFingerprint string          `json:"preservationFingerprint,omitempty"`
	Targets                 []RemovalTarget `json:"targets,omitempty"`
	RecoveryRefs            []string        `json:"recoveryRefs,omitempty"`
	RecoveryRepo            string          `json:"recoveryRepo,omitempty"`
	WitnessRefs             []string        `json:"witnessRefs,omitempty"`
	ExplicitDiscard         bool            `json:"explicitDiscard,omitempty"`
}

// RemovalTarget is an immutable local-ref witness captured before removal.
// ExpectedOID is empty only when the ref was explicitly absent at confirmation.
type RemovalTarget struct {
	Ref            string `json:"ref"`
	ExpectedOID    string `json:"expectedOid,omitempty"`
	Preserved      bool   `json:"preserved"`
	Kind           string `json:"kind"`
	MatchingRef    string `json:"matchingRef,omitempty"`
	MatchingCommit string `json:"matchingCommit,omitempty"`
	Reason         string `json:"reason"`
}

// Registry is the global index (~/.shunt[-channel]/registry.json): just enough
// for the single Caddy server and cross-project `shunt ls` to find each
// project's config dir.
type Registry struct {
	Version  int               `json:"version"`
	Projects map[string]string `json:"projects"` // project name -> config dir
}

// FindProject looks up a project by name — exact first, then case-insensitively.
// macOS paths are case-insensitive, so a cwd basename or arg like "hubX" must still
// resolve the registered "HubX". Returns the canonical registered name, its config
// dir, and whether it was found.
func (r Registry) FindProject(name string) (canonical, dir string, ok bool) {
	if d, found := r.Projects[name]; found {
		return name, d, true
	}
	for k, d := range r.Projects {
		if strings.EqualFold(k, name) {
			return k, d, true
		}
	}
	return "", "", false
}

// App is the per-project runtime state, derived from the committed in-repo
// .shunt.app.json contract and persisted to <configDir>/state-v2.json.
type App struct {
	Version    int    `json:"version"`
	Name       string `json:"name"`
	RepoOrigin string `json:"repoOrigin"`
	RepoPath   string `json:"repoPath"` // legacy checkout reference; never an execution target
	// ControlRepoPath is Shunt's independent bare repository. It owns managed
	// worktrees and preserves the pinned source seed when no siding remains.
	ControlRepoPath string            `json:"controlRepoPath,omitempty"`
	BaseSiding      string            `json:"baseSiding,omitempty"`
	BaseCommit      string            `json:"baseCommit,omitempty"`
	Runner          string            `json:"runner"`            // aspire | dotnet | node | custom
	Start           string            `json:"start,omitempty"`   // start command (non-aspire)
	Stop            string            `json:"stop,omitempty"`    // optional clean-stop command; force-kill is the fallback
	Workdir         string            `json:"workdir,omitempty"` // dir to run Start/Stop in (non-aspire)
	AppHostPath     string            `json:"appHostPath"`       // aspire: rel path to the AppHost project
	ConfigDir       string            `json:"configDir"`         // <repos>/.shunt[-ch]/<project>
	FrontDoor       []Route           `json:"frontDoor"`
	DataVolumes     []DataVolume      `json:"dataVolumes"`
	Env             map[string]string `json:"env"`    // extra guest env (Aspire parameters, secrets)
	Mounts          []MountSpec       `json:"mounts"` // explicit extra host->guest mounts
	// Registry dependency images kept in shunt's daemon-free host cache and
	// loaded into sidings so guests never pull from the network (see `shunt warm`).
	PrebakeImages []string `json:"prebakeImages,omitempty"`
	// Local dependency images built on the host before sidings start. Paths are
	// absolute after the repository contract is loaded.
	PrebakeBuilds []PrebakeBuild `json:"prebakeBuilds,omitempty"`
	// Docker named volumes backed by per-siding host directories. New/fresh-data
	// sidings clone the project's selected canonical baseline generation.
	Volumes []string `json:"volumes,omitempty"`
	// Per-guest resource caps (Apple `container`); empty uses shunt's defaults.
	Memory string `json:"memory,omitempty"`
	CPUs   string `json:"cpus,omitempty"`
	// HealthPort/HealthPath are the endpoint the dashboard hits (from inside the
	// guest) to decide whether a siding is actually "running" vs merely booted.
	// Empty HealthPort defaults to the Aspire dashboard's home page. Path defaults
	// to "/".
	HealthPort int    `json:"healthPort,omitempty"`
	HealthPath string `json:"healthPath,omitempty"`
	// DisableCache makes the front door send `Cache-Control: no-store` on every
	// HTTP response — for Blazor/SPA apps that serve stale assets across a siding
	// switch on the shared port.
	DisableCache bool              `json:"disableCache,omitempty"`
	Sidings      map[string]Siding `json:"sidings"`
	LiveSiding   string            `json:"liveSiding"` // "" = nothing live
	Removal      *RemovalOperation `json:"removal,omitempty"`
}

// PrebakeBuild declares one local image build that feeds shunt's shared,
// daemon-free host cache.
type PrebakeBuild struct {
	Image      string            `json:"image"`
	Context    string            `json:"context"`
	Dockerfile string            `json:"dockerfile"`
	Platform   string            `json:"platform,omitempty"`
	BuildArgs  map[string]string `json:"buildArgs,omitempty"`
}

// Route is a stable front-door entry. The upstream target is NOT stored — it's
// discovered live from the running Aspire app on each switch.
type Route struct {
	Key        string `json:"key"`                 // logical name: frontend | api | db
	Kind       string `json:"kind"`                // KindHTTP | KindLayer4
	ListenPort int    `json:"listenPort"`          // stable host port Caddy listens on (offset applied)
	Resource   string `json:"resource"`            // aspire: resource name to proxy
	Endpoint   string `json:"endpoint"`            // aspire: endpoint name within that resource
	GuestPort  int    `json:"guestPort,omitempty"` // non-aspire: in-guest port the app binds
	TLS        bool   `json:"tls"`                 // terminate TLS at the front door (https redirect)
	CaddyID    string `json:"caddyId"`             // @id, e.g. app_myapp_http_frontend
}

// DataVolume is a reusable data dir cloned per siding and bind-mounted into the
// guest at GuestPath (where the app's Aspire config mounts its DB data).
type DataVolume struct {
	Resource  string `json:"resource"`
	GuestPath string `json:"guestPath"`
}

// MountSpec is an explicit extra host->guest bind mount declared per project
// (e.g. the developer's ~/.microsoft/usersecrets so Aspire parameters resolve).
// shunt honors these verbatim — it never auto-mounts anything app-specific.
type MountSpec struct {
	Host     string `json:"host"`     // host path, ~ expanded
	Guest    string `json:"guest"`    // path inside the guest
	ReadOnly bool   `json:"readOnly"` // mount read-only
}

// UnmarshalJSON accepts either the full object {host, guest, readOnly} or a plain
// path string. A string auto-maps both sides from one path: `~/X` mounts the host
// home's X to the guest's home (/root/X, since the guest runs as root); an
// absolute `/X` mounts to the same path. String form defaults to read-only (it's
// for config/secrets the app reads) — use the object form for read-write.
func (m *MountSpec) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		m.Host = s
		m.Guest = guestPathFor(s)
		m.ReadOnly = true
		return nil
	}
	type raw MountSpec // avoid recursing into this method
	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	*m = MountSpec(r)
	return nil
}

// guestPathFor maps a mount path to its in-guest location: `~/X` -> /root/X (the
// guest's home), absolute paths unchanged.
func guestPathFor(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return path.Join("/root", strings.TrimPrefix(p, "~"))
	}
	return p
}

// Siding is one isolated experiment of an app.
type Siding struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	// WorktreeRepoPath records the repository that owns this linked worktree.
	// Legacy sidings project to App.RepoPath; managed sidings use ControlRepoPath.
	WorktreeRepoPath     string               `json:"worktreeRepoPath,omitempty"`
	MaterializationPhase MaterializationPhase `json:"materializationPhase,omitempty"`
	Container            string               `json:"container"` // channel-prefixed guest name
	CreatedAt            string               `json:"createdAt"` // RFC3339, stamped by the caller
	RSPort               int                  `json:"rsPort"`    // pinned Aspire resource-service port in the guest
	RSKey                string               `json:"rsKey"`     // resource-service API key shunt set on launch
	LastIP               string               `json:"lastIp"`    // cached guest IP, refreshed on switch
	Stopped              bool                 `json:"stopped"`   // kill keeps the clone/volume but stops the guest
	// Bridges maps each front-door route key to the guest-external port shunt's
	// in-guest socat exposes (Aspire binds the real endpoint to loopback).
	Bridges map[string]int `json:"bridges"`
	// FrontDoor is this siding's own resolved front-door route set, read from the
	// siding worktree's .shunt.app.json (the guest runs that code). Empty falls back
	// to the app-level set — so a siding that adds/drops a route applies it on
	// `up`/`switch` without an `app add` in the root repo. Set by `up`/`switch`.
	FrontDoor []Route `json:"frontDoor,omitempty"`
}

// EnsureV2 applies the deterministic state-v2 projection and marks the app for
// v2 publication. It returns whether any in-memory field changed. It performs
// no filesystem or Git work.
func EnsureV2(app *App) bool {
	if app == nil {
		return false
	}
	changed := projectCompatibility(app)
	if app.Version < StateVersion {
		app.Version = StateVersion
		changed = true
	}
	return changed
}

// NeedsBaseSelection reports the only migration decision state cannot infer:
// multiple existing sidings with no designated source base.
func NeedsBaseSelection(app App) bool {
	if len(app.Sidings) == 0 {
		return false
	}
	if app.BaseSiding == "" {
		return len(app.Sidings) > 1
	}
	_, exists := app.Sidings[app.BaseSiding]
	return !exists
}

// WorktreeOwner returns the repository that owns a siding's linked worktree.
// The original repository is the compatibility owner for legacy state.
func WorktreeOwner(app App, siding Siding) string {
	if siding.WorktreeRepoPath != "" {
		return siding.WorktreeRepoPath
	}
	return app.RepoPath
}

// projectCompatibility supplies safe read-time defaults without changing the
// persisted version. LoadApp calls it only on the returned in-memory value.
func projectCompatibility(app *App) bool {
	changed := false
	legacy := app.Version < StateVersion
	if app.ControlRepoPath == "" && app.ConfigDir != "" {
		app.ControlRepoPath = filepath.Join(app.ConfigDir, ".control.git")
		changed = true
	}
	if app.Sidings == nil {
		app.Sidings = map[string]Siding{}
		changed = true
	}
	if app.BaseSiding == "" && len(app.Sidings) == 1 {
		for name := range app.Sidings {
			app.BaseSiding = name
		}
		changed = true
	}
	// Stable ordering is not required for the map update itself, but makes this
	// projection deterministic if callers observe copied values while debugging.
	names := make([]string, 0, len(app.Sidings))
	for name := range app.Sidings {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		siding := app.Sidings[name]
		if siding.WorktreeRepoPath == "" {
			siding.WorktreeRepoPath = app.RepoPath
			if !legacy && app.ControlRepoPath != "" {
				siding.WorktreeRepoPath = app.ControlRepoPath
			}
			changed = true
		}
		if siding.MaterializationPhase == "" {
			if legacy {
				siding.MaterializationPhase = PhaseGuest
			} else {
				siding.MaterializationPhase = PhaseWorktree
			}
			changed = true
		}
		app.Sidings[name] = siding
	}
	return changed
}
