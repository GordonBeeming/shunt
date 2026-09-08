package hostreach

import (
	"fmt"
	"net"
	"sort"
)

// loopbackBase is where per-entry guest addresses start. Each entry gets its own
// address in 127/8 rather than sharing one, because a hosts entry maps a name to
// an address and not to a port: a dozen names on 443 would otherwise collide on
// a single address and only one could be served.
//
// 127.0.0.1 is skipped so nothing here can shadow whatever the app already binds
// on plain localhost.
const loopbackBase = 10

// bridgePortBase is the first port allocated on the host bridge. These ports
// exist only on the guest-to-bridge hop, so their values carry no meaning to the
// app: the real port is preserved at both ends of the chain.
const bridgePortBase = 47000

// Relay is one resolved endpoint with both ends of its chain assigned:
//
//	guest: GuestAddress:Port  ->  bridge: BridgeAddress:BridgePort  ->  Address:Port
//
// The guest's hosts file maps Name to GuestAddress, so the app connects to the
// real name on the real port and never sees the hops.
type Relay struct {
	Resolved
	GuestAddress  string
	BridgeAddress string
	BridgePort    int
}

// Plan assigns addresses and ports to resolved entries.
//
// It is deterministic in entry order so a restart reproduces the same layout,
// which keeps a guest's hosts file valid across a stop and start rather than
// silently pointing at an address the relay no longer answers on.
func Plan(bridgeAddress string, resolved []Resolved) ([]Relay, error) {
	if net.ParseIP(bridgeAddress) == nil {
		return nil, fmt.Errorf("invalid bridge address %q", bridgeAddress)
	}
	ordered := append([]Resolved(nil), resolved...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Name != ordered[j].Name {
			return ordered[i].Name < ordered[j].Name
		}
		return ordered[i].Port < ordered[j].Port
	})

	seen := map[string]bool{}
	relays := make([]Relay, 0, len(ordered))
	for i, entry := range ordered {
		key := fmt.Sprintf("%s:%d", entry.Name, entry.Port)
		if seen[key] {
			return nil, fmt.Errorf("duplicate hostReach entry %s", key)
		}
		seen[key] = true

		guestAddress, err := loopbackAddress(i)
		if err != nil {
			return nil, err
		}
		relays = append(relays, Relay{
			Resolved:      entry,
			GuestAddress:  guestAddress,
			BridgeAddress: bridgeAddress,
			BridgePort:    bridgePortBase + i,
		})
	}
	return relays, nil
}

// loopbackAddress numbers entries through 127.0.0.x, then 127.0.1.x and beyond.
// 127/8 is a /8, so the space is effectively unbounded; the error exists to fail
// loudly rather than wrap around and hand two names the same address.
func loopbackAddress(index int) (string, error) {
	if index < 0 || index > 0xFFFF {
		return "", fmt.Errorf("too many hostReach entries (%d)", index)
	}
	third := index / 245
	fourth := loopbackBase + index%245
	return fmt.Sprintf("127.0.%d.%d", third, fourth), nil
}

// BridgeAddressFor derives the host's address on the container bridge from a
// guest's own address: the runtime puts the host at .1 of the guest's /24.
//
// Derived rather than hardcoded so it follows the runtime's subnet instead of
// assuming the 192.168.64.0/24 this happens to use today.
func BridgeAddressFor(guestIP string) (string, error) {
	parsed := net.ParseIP(guestIP)
	if parsed == nil {
		return "", fmt.Errorf("invalid guest address %q", guestIP)
	}
	v4 := parsed.To4()
	if v4 == nil {
		return "", fmt.Errorf("guest address %q is not IPv4", guestIP)
	}
	return fmt.Sprintf("%d.%d.%d.1", v4[0], v4[1], v4[2]), nil
}
