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
	AppHost     string             `json:"apphost"`   // rel path to the AppHost project dir or csproj
	FrontDoor   []FrontDoorRoute   `json:"frontDoor"` // stable front-door routes
	DataVolumes []state.DataVolume `json:"dataVolumes"`
	Env         map[string]string  `json:"env"`    // extra guest env (Aspire parameters, secrets)
	Mounts      []state.MountSpec  `json:"mounts"` // explicit extra host->guest mounts (e.g. user-secrets)
	// Dependency container images Aspire brings up (SQL, Azurite, etc.). shunt
	// keeps these in the host Docker cache and copies them into each siding, so
	// siding guests never pull from the network. `shunt warm` uses this list.
	PrebakeImages []string `json:"prebakeImages"`
}

// FrontDoorRoute is one stable port mapping in the contract.
type FrontDoorRoute struct {
	Key        string `json:"key"`        // logical name: frontend | api | db
	Kind       string `json:"kind"`       // http | layer4
	ListenPort int    `json:"listenPort"` // stable host port (before channel offset)
	Resource   string `json:"resource"`   // Aspire resource name to proxy
	Endpoint   string `json:"endpoint"`   // endpoint name within that resource (optional)
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
	if c.AppHost == "" {
		return errors.New("apphost is required")
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
