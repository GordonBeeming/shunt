package siding

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/hostreach"
	"github.com/gordonbeeming/shunt/internal/state"
)

// probeTimeout bounds the reachability check. Both ends are on this machine, so
// a slow answer means the connection is not being carried rather than that the
// network is far away.
const probeTimeout = 3 * time.Second

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
	probeBridge = probeBridgeReachable
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
		path, body, err := caddy.ServerForHostReach(name, r.BridgeAddress, r.BridgePort, net.JoinHostPort(r.Address, strconv.Itoa(r.Port)))
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

	// Prove the guest can actually reach the host end before writing any names.
	// This hop fails silently: the macOS application firewall drops inbound
	// connections to an unapproved binary on a non-loopback address without
	// refusing them, so the guest gets a hanging connect and the host end never
	// sees the attempt. Writing the hosts entries first would turn that into the
	// worst version of the failure, where every declared name resolves and then
	// hangs, which reads as the dependency being down.
	if len(relays) > 0 {
		if err := probeBridge(ctx, admin, app.Name, sd.Name, sd.Container, bridge); err != nil {
			for _, r := range relays {
				name := caddy.HostReachServerName(app.Name, sd.Name, r.Name, r.Port)
				if e := hostReachDeleteServer(ctx, admin, "/config/apps/layer4/servers/"+name); e != nil {
					// A listener left holding a bridge port to a private endpoint is the
					// exposure this whole path exists to keep narrow, so say so.
					fmt.Fprintf(os.Stdout, "• could not remove the host-reach listener for %s: %v\n", r.Name, e)
				}
			}
			return err
		}
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

// probeBridge proves the guest can carry traffic to the host end on the
// container bridge, by moving a nonce through a listener Caddy holds for the
// length of the check.
//
// The exchange runs in the guest because that is the path the relay uses, and it
// is the only path nothing else exercises. A check made from the host would pass
// on a guest whose own route to the bridge is broken.
//
// A connect is not enough to prove it either way. The macOS application firewall
// blocks inbound connections to an unapproved program on any address except
// loopback, and blocks them after the handshake: the kernel completes the
// connection, the program never accepts it, and the caller sees a dial that
// succeeds and a read that never returns. A check that only dialled would pass
// on exactly the machines where this is broken.
//
// The listener under test belongs to Caddy rather than to shunt, because the
// firewall decides per program and Caddy is what holds the real entries.
func probeBridgeReachable(ctx context.Context, admin *caddy.Admin, app, siding, container, bridgeAddress string) error {
	nonce := fmt.Sprintf("shunt-host-reach-probe-%d", time.Now().UnixNano())
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("start the host-reach probe listener: %w", err)
	}
	defer echo.Close()

	// Reported by the host as well as by the guest. The guest's exit status says
	// it got its nonce back; this says the host end was the one that answered,
	// which is what rules out something else holding the port.
	served := make(chan bool, 1)
	go func() {
		conn, err := echo.Accept()
		if err != nil {
			served <- false
			return
		}
		defer conn.Close()
		buf := make([]byte, len(nonce))
		if _, err := io.ReadFull(conn, buf); err != nil {
			served <- false
			return
		}
		_, _ = conn.Write(buf)
		served <- string(buf) == nonce
	}()

	path, body, err := caddy.ServerForHostReach(caddy.HostReachProbeServerName(app, siding), bridgeAddress, hostreach.ProbePort(app, siding), echo.Addr().String())
	if err != nil {
		return err
	}
	_ = hostReachDeleteServer(ctx, admin, path)
	if err := hostReachPutServer(ctx, admin, path, body); err != nil {
		return fmt.Errorf("publish the host-reach probe listener: %w", err)
	}
	defer func() {
		if err := hostReachDeleteServer(ctx, admin, path); err != nil {
			// Worth saying rather than swallowing: this listener holds a bridge port,
			// and one left behind is the exposure the design is meant to avoid.
			fmt.Fprintf(os.Stdout, "• could not remove the host-reach probe listener %s: %v\n", path, err)
		}
	}()

	target := net.JoinHostPort(bridgeAddress, strconv.Itoa(hostreach.ProbePort(app, siding)))
	checkCtx, cancel := context.WithTimeout(ctx, probeTimeout*2)
	defer cancel()
	_, checkErr := execGuest(checkCtx, container, hostreach.RelayProgram, "--check", target, "--nonce", nonce)
	if checkErr == nil {
		select {
		case ok := <-served:
			if ok {
				return nil
			}
			checkErr = fmt.Errorf("the check reached something other than shunt's own listener")
		case <-time.After(probeTimeout):
			checkErr = fmt.Errorf("the check reported success but shunt's listener was never reached")
		}
	}
	return fmt.Errorf(`the guest cannot carry traffic to shunt on the container bridge at %s, so host-reach would resolve every declared name and then hang.

The macOS application firewall blocks incoming connections to programs it has
not been told to allow, on every address except loopback, and it blocks them
without refusing. shunt's front door is unaffected because it listens on
loopback only. hostReach is the one part of shunt that binds the bridge.

Firewall state:
  /usr/libexec/ApplicationFirewall/socketfilterfw --getglobalstate

Underlying failure: %w`, target, checkErr)
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
