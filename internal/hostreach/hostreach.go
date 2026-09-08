// Package hostreach resolves the endpoints only the host can reach and reports
// which interface carries each one.
//
// A siding's guest cannot reach an address behind the host's VPN: macOS does not
// forward the container bridge into the tunnel interface, so a connect from the
// guest times out while the identical connect from the host succeeds. shunt
// relays instead of routing, and this package is the host end's view of what to
// relay to.
package hostreach

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/state"
)

const resolveTimeout = 5 * time.Second

// Resolved is one declared endpoint with the address the host currently
// believes, plus the interface holding its route.
//
// Interface is diagnostic only and may be empty. It exists because a second VPN
// client can take a prefix from the first while the first still reports itself
// connected: the failure then looks like shunt being broken, and naming the
// interface is what points at the real cause.
type Resolved struct {
	Name      string
	Port      int
	Address   string
	Interface string
}

func (r Resolved) String() string {
	if r.Interface == "" {
		return fmt.Sprintf("%s:%d -> %s", r.Name, r.Port, r.Address)
	}
	return fmt.Sprintf("%s:%d -> %s via %s", r.Name, r.Port, r.Address, r.Interface)
}

// Resolve turns declared entries into addresses, using the host's own resolver
// so it inherits whatever the host currently believes, including a hosts file
// regenerated since the guest was last started.
//
// It resolves every entry before returning an error, so a run with several bad
// names reports all of them rather than one per attempt.
func Resolve(ctx context.Context, entries []state.HostReach) ([]Resolved, error) {
	return resolve(ctx, entries, net.DefaultResolver.LookupHost, routeInterface)
}

type lookupFunc func(ctx context.Context, host string) ([]string, error)
type interfaceFunc func(ctx context.Context, address string) string

func resolve(ctx context.Context, entries []state.HostReach, lookup lookupFunc, iface interfaceFunc) ([]Resolved, error) {
	resolved := make([]Resolved, 0, len(entries))
	var failures []string
	for _, entry := range entries {
		// Validate before resolving, and collect rather than return: a contract
		// with several bad entries should report them together, like the
		// resolution failures below.
		if entry.Name == "" {
			failures = append(failures, "an entry has no name")
			continue
		}
		if entry.Port < 1 || entry.Port > 65535 {
			// Port 0 is the trap worth naming: it binds an ephemeral port in the
			// guest rather than failing, so the relay would come up listening
			// somewhere nothing connects to.
			failures = append(failures, fmt.Sprintf("%s: port %d is not a usable port", entry.Name, entry.Port))
			continue
		}
		address := entry.Address
		if address == "" {
			lookupCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
			addrs, err := lookup(lookupCtx, entry.Name)
			cancel()
			if err != nil || len(addrs) == 0 {
				// The host's state is what is wrong here, not the app's contract,
				// so the message points at the host rather than at the declaration.
				failures = append(failures, fmt.Sprintf("%s: the host cannot resolve it (%v)", entry.Name, err))
				continue
			}
			address = addrs[0]
		}
		if net.ParseIP(address) == nil {
			failures = append(failures, fmt.Sprintf("%s: %q is not an address", entry.Name, address))
			continue
		}
		resolved = append(resolved, Resolved{
			Name:      entry.Name,
			Port:      entry.Port,
			Address:   address,
			Interface: iface(ctx, address),
		})
	}
	if len(failures) > 0 {
		return nil, fmt.Errorf("cannot reach declared hosts:\n  %s", strings.Join(failures, "\n  "))
	}
	return resolved, nil
}

// routeInterface names the interface carrying traffic to address. It answers ""
// rather than failing: this is diagnostic detail, and losing it must never stop
// a relay that would otherwise work.
func routeInterface(ctx context.Context, address string) string {
	probeCtx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	result, err := proc.Run(probeCtx, "route", "-n", "get", address)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		name, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if found && strings.TrimSpace(name) == "interface" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
