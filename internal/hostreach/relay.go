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

// ProbePort carries the reachability check. It sits below the entry ports so a
// plan can never allocate it, and it is only bound for the length of one check.
const ProbePort = bridgePortBase - 1

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
		bridgePort := bridgePortBase + i
		if bridgePort > 65535 {
			return nil, fmt.Errorf("too many hostReach entries: bridge port %d is above the maximum", bridgePort)
		}
		relays = append(relays, Relay{
			Resolved:      entry,
			GuestAddress:  guestAddress,
			BridgeAddress: bridgeAddress,
			BridgePort:    bridgePort,
		})
	}
	return relays, nil
}

// perThirdOctet is how many addresses each 127.0.N.x block contributes: the
// range from loopbackBase to 254, leaving .255 alone as the broadcast-shaped
// address.
const perThirdOctet = 255 - loopbackBase

// maxEntries is the whole usable space, 127.0.0.10 through 127.0.255.254.
const maxEntries = 256 * perThirdOctet

// loopbackAddress numbers entries through 127.0.0.x, then 127.0.1.x and beyond.
// Both octets are bounded so an out-of-range index fails here with a clear
// message rather than producing something like 127.0.267.4, which reads as an
// address and only fails much later at bind time.
func loopbackAddress(index int) (string, error) {
	if index < 0 || index >= maxEntries {
		return "", fmt.Errorf("too many hostReach entries: %d exceeds the %d addresses available in 127.0.0.0/16", index+1, maxEntries)
	}
	return fmt.Sprintf("127.0.%d.%d", index/perThirdOctet, loopbackBase+index%perThirdOctet), nil
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
