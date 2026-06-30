// Package state holds shunt's persisted model: a thin global registry mapping
// projects to their config dirs, and the per-project runtime state (the app and
// its sidings) stored project-locally in <repos>/.shunt[-channel]/<project>/state.json.
package state

import (
	"encoding/json"
	"path"
	"strings"
)

const (
	// RegistryVersion / StateVersion let us migrate on-disk formats later.
	RegistryVersion = 1
	StateVersion    = 1

	// KindHTTP / KindLayer4 are the two front-door route kinds.
	KindHTTP   = "http"
	KindLayer4 = "layer4"

	// PlaceholderDial is the upstream a route points at before any siding is
	// live. A refused connection here is an honest "nothing live yet" signal.
	PlaceholderDial = "127.0.0.1:1"
)

// Registry is the global index (~/.shunt[-channel]/registry.json): just enough
// for the single Caddy server and cross-project `shunt ls` to find each
// project's config dir.
type Registry struct {
	Version  int               `json:"version"`
	Projects map[string]string `json:"projects"` // project name -> config dir
}

// App is the per-project runtime state, derived from the committed in-repo
// .shunt.app.json contract and persisted to <configDir>/state.json.
type App struct {
	Name        string            `json:"name"`
	RepoOrigin  string            `json:"repoOrigin"`
	RepoPath    string            `json:"repoPath"`          // the original repo on disk
	Runner      string            `json:"runner"`            // aspire | dotnet | node | custom
	Start       string            `json:"start,omitempty"`   // start command (non-aspire)
	Stop        string            `json:"stop,omitempty"`    // optional clean-stop command; force-kill is the fallback
	Workdir     string            `json:"workdir,omitempty"` // dir to run Start/Stop in (non-aspire)
	AppHostPath string            `json:"appHostPath"`       // aspire: rel path to the AppHost project
	ConfigDir   string            `json:"configDir"`         // <repos>/.shunt[-ch]/<project>
	FrontDoor   []Route           `json:"frontDoor"`
	DataVolumes []DataVolume      `json:"dataVolumes"`
	Env         map[string]string `json:"env"`    // extra guest env (Aspire parameters, secrets)
	Mounts      []MountSpec       `json:"mounts"` // explicit extra host->guest mounts
	// Dependency images kept in the host Docker cache and copied into sidings so
	// guests never pull from the network (see `shunt warm`).
	PrebakeImages []string          `json:"prebakeImages,omitempty"`
	// Docker named volumes cloned from the host's Docker into each siding's guest
	// Docker on `new`, so sidings start with the host's test data.
	Volumes []string `json:"volumes,omitempty"`
	// Per-guest resource caps (Apple `container`); empty uses shunt's defaults.
	Memory  string            `json:"memory,omitempty"`
	CPUs    string            `json:"cpus,omitempty"`
	Sidings map[string]Siding `json:"sidings"`
	LiveSiding    string            `json:"liveSiding"` // "" = nothing live
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
	Name      string `json:"name"`
	Branch    string `json:"branch"`
	Container string `json:"container"` // channel-prefixed guest name
	CreatedAt string `json:"createdAt"` // RFC3339, stamped by the caller
	RSPort    int    `json:"rsPort"`    // pinned Aspire resource-service port in the guest
	RSKey     string `json:"rsKey"`     // resource-service API key shunt set on launch
	LastIP    string `json:"lastIp"`    // cached guest IP, refreshed on switch
	Stopped   bool   `json:"stopped"`   // kill keeps the clone/volume but stops the guest
	// Bridges maps each front-door route key to the guest-external port shunt's
	// in-guest socat exposes (Aspire binds the real endpoint to loopback).
	Bridges map[string]int `json:"bridges"`
}
