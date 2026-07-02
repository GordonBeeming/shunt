// Package contract reads the committed in-repo .shunt.app.json — the developer's
// declaration of an app's stable front-door ports and how they map onto the
// app's Aspire resources. shunt derives its runtime state from this.
package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gordonbeeming/shunt/internal/state"
)

// FileName is the per-repo contract file shunt looks for.
const FileName = ".shunt.app.json"

// Contract is the parsed .shunt.app.json.
type Contract struct {
	// Runner selects how the app starts (aspire | dotnet | node | custom).
	// Empty = auto-detect at `app add`. Aspire keeps gRPC discovery; the others
	// use Start + a declared per-route guestPort.
	Runner  string `json:"runner"`
	Start   string `json:"start"`   // start command for dotnet/node/custom (run in Workdir)
	Stop    string `json:"stop"`    // optional clean-stop command (e.g. `aspire stop`); shunt's force-kill is the fallback
	Workdir string `json:"workdir"` // dir to run Start/Stop in, relative to the repo (default repo root)

	AppHost     string             `json:"apphost"`   // aspire only: rel path to the AppHost project/csproj
	FrontDoor   []FrontDoorRoute   `json:"frontDoor"` // stable front-door routes
	Env         map[string]string  `json:"env"`    // extra guest env (Aspire parameters, secrets)
	Mounts      []state.MountSpec  `json:"mounts"` // explicit extra host->guest mounts (e.g. user-secrets)
	// Dependency container images Aspire brings up (SQL, Azurite, etc.). shunt
	// keeps these in the host Docker cache and copies them into each siding, so
	// siding guests never pull from the network. `shunt warm` uses this list.
	PrebakeImages []string `json:"prebakeImages"`
	// Volumes lists Docker named volumes (as named in the AppHost's
	// WithDataVolume) whose data shunt clones from the host's Docker into each
	// siding's guest Docker on `new`, so sidings start with the host's test data
	// instead of an empty DB. Omit for a clean per-siding database.
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
	GuestPort  int    `json:"guestPort"`  // non-aspire: the in-guest port the app binds (no discovery)
	TLS        bool   `json:"tls"`        // terminate TLS at the front door
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
	if err := c.validate(); err != nil {
		return Contract{}, fmt.Errorf("%s: %w", FileName, err)
	}
	return c, nil
}

func (c Contract) validate() error {
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
