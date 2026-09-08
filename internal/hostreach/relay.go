package hostreach

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"sort"
	"strconv"
)

// loopbackBase is where per-entry guest addresses start. Each entry gets its own
// address in 127/8 rather than sharing one, because a hosts entry maps a name to
// an address and not to a port: a dozen names on 443 would otherwise collide on
// a single address and only one could be served.
//
// 127.0.0.1 is skipped so nothing here can shadow whatever the app already binds
// on plain localhost.
const loopbackBase = 10

// ControlPort is where a guest accepts the host's connections. It is the same in
// every guest because each one has its own address on the bridge, so nothing has
// to be allocated or remembered.
const ControlPort = 47999

// ProbeName is the endpoint the host end answers itself, with an echo, instead
// of dialling out. It exercises the loopback listener, the pool, the token and
// the host process in one pass, which is the whole chain except the last dial.
//
// The .invalid TLD is reserved and never resolves, so this can never collide
// with a declared entry.
const ProbeName = "shunt-probe.invalid"

// The probe's own listener in the guest. It sits below loopbackBase, so the
// entry numbering can never reach it, and it is deliberately not in the guest's
// hosts file: nothing should be able to resolve this name.
const (
	ProbeGuestAddress = "127.0.0.2"
	ProbeGuestPort    = 46999
)

// Relay is one resolved endpoint with its guest address assigned:
//
//	guest: GuestAddress:Port  ->  host end  ->  Address:Port
//
// The guest's hosts file maps Name to GuestAddress, so the app connects to the
// real name on the real port and never sees the hops.
type Relay struct {
	Resolved
	GuestAddress string
}

// Plan assigns a guest address to each resolved entry.
//
// It is deterministic in entry order so a restart reproduces the same layout,
// which keeps a guest's hosts file valid across a stop and start rather than
// silently pointing at an address the relay no longer answers on.
func Plan(resolved []Resolved) ([]Relay, error) {
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
		relays = append(relays, Relay{Resolved: entry, GuestAddress: guestAddress})
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

// GuestControlAddress is where the host end dials to reach a guest's relay.
func GuestControlAddress(guestIP string) (string, error) {
	if net.ParseIP(guestIP) == nil {
		return "", fmt.Errorf("invalid guest address %q", guestIP)
	}
	return net.JoinHostPort(guestIP, strconv.Itoa(ControlPort)), nil
}

// NewToken generates a siding's shared secret.
//
// Every guest on the bridge can reach every other guest's control port, so an
// unauthenticated pool would let one siding's guest join another's and be handed
// its traffic, credentials included. The token is what makes the guest's
// listener safe to have at all.
func NewToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate the host-reach token: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
