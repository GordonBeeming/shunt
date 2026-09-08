package siding

import (
	"context"
	"fmt"
	"os"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/hostreach"
	"github.com/gordonbeeming/shunt/internal/state"
)

// Seams so the relay can be tested without a guest or a running Caddy.
var (
	resolveHostReach   = hostreach.Resolve
	hostReachPrepare   = func(ctx context.Context) (*caddy.Admin, error) { a := caddy.NewAdmin(); return a, a.Ping(ctx) }
	hostReachPutServer = func(ctx context.Context, a *caddy.Admin, path string, body []byte) error {
		return a.Put(ctx, path, body)
	}
	hostReachDeleteServer = func(ctx context.Context, a *caddy.Admin, path string) error {
		return a.DeleteIfExists(ctx, path)
	}
)

// applyHostReach relays the endpoints only the host can reach into a running
// guest: a listener per entry on the container bridge, plus the guest's hosts
// entries and relay process.
//
// It runs on every start rather than at guest creation, because the addresses
// are resolved fresh each time. An endpoint recreated since the last start moves,
// and a guest carrying the previous address would fail in the one way this
// feature exists to avoid: confidently, at the wrong place.
func applyHostReach(ctx context.Context, app state.App, sd state.Siding) error {
	if len(app.HostReach) == 0 {
		return nil
	}
	if sd.LastIP == "" {
		return fmt.Errorf("host-reach needs the guest address; activate the siding first")
	}
	bridge, err := hostreach.BridgeAddressFor(sd.LastIP)
	if err != nil {
		return err
	}

	// Resolve before touching anything. A name the host cannot answer for is the
	// host's state being wrong, and failing here leaves no listener and no hosts
	// entry behind to half-work.
	resolved, err := resolveHostReach(ctx, app.HostReach)
	if err != nil {
		return err
	}
	relays, err := hostreach.Plan(bridge, resolved)
	if err != nil {
		return err
	}

	admin, err := hostReachPrepare(ctx)
	if err != nil {
		return fmt.Errorf("caddy admin API not reachable for host-reach: %w", err)
	}
	for _, r := range relays {
		name := caddy.HostReachServerName(app.Name, sd.Name, r.Name, r.Port)
		path, body, err := caddy.ServerForHostReach(name, r.BridgeAddress, r.BridgePort, fmt.Sprintf("%s:%d", r.Address, r.Port))
		if err != nil {
			return err
		}
		// Delete first: a restart re-plans deterministically onto the same ports,
		// and Caddy rejects a PUT onto a name that already exists.
		_ = hostReachDeleteServer(ctx, admin, path)
		if err := hostReachPutServer(ctx, admin, path, body); err != nil {
			return fmt.Errorf("publish host-reach listener for %s: %w", r.Name, err)
		}
		fmt.Fprintf(os.Stdout, "• reaching %s\n", r)
	}

	config, err := hostreach.RelayConfig(relays)
	if err != nil {
		return err
	}
	existing, err := execGuest(ctx, sd.Container, "cat", "/etc/hosts")
	if err != nil {
		return fmt.Errorf("read guest hosts file: %w", err)
	}
	script := hostreach.GuestScript(string(config), hostreach.MergeHosts(existing, relays))
	if _, err := execGuest(ctx, sd.Container, "sh", "-c", script); err != nil {
		return fmt.Errorf("start the guest host-reach relay: %w", err)
	}
	return nil
}

// removeHostReach drops the bridge listeners a siding created. A listener that
// outlived its guest would keep a path to a private endpoint open on the bridge
// with nothing using it.
//
// It is best effort by design: it runs on teardown paths that must complete, and
// a stale listener is a smaller problem than a stop that refuses to finish.
func removeHostReach(ctx context.Context, app state.App, sd state.Siding) {
	if len(app.HostReach) == 0 {
		return
	}
	admin, err := hostReachPrepare(ctx)
	if err != nil {
		return
	}
	for _, entry := range app.HostReach {
		name := caddy.HostReachServerName(app.Name, sd.Name, entry.Name, entry.Port)
		_ = hostReachDeleteServer(ctx, admin, "/config/apps/layer4/servers/"+name)
	}
}
