package hostreach

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// RelayProgram is the in-guest relay built into the base image.
const RelayProgram = "/usr/local/bin/shunt-host-reach-relay"

// ConfigPath is where shunt writes the relay's configuration inside the guest.
// It lives under /run so it never survives a boot: the addresses are resolved
// per start, and a stale file would point the relay at where an endpoint used
// to be.
const ConfigPath = "/run/shunt/host-reach.json"

// hostsMarker fences shunt's entries inside the guest's /etc/hosts so a rewrite
// replaces exactly what shunt wrote and leaves everything else alone.
const (
	hostsBegin = "# >>> shunt host-reach (managed, do not edit) >>>"
	hostsEnd   = "# <<< shunt host-reach <<<"
)

// guestEntry is the relay's on-disk shape. It is deliberately not state.HostReach:
// the relay has no use for the real address, and not sending it keeps the guest
// from carrying a copy of the host's private-network layout.
type guestEntry struct {
	Name         string `json:"name"`
	GuestAddress string `json:"guestAddress"`
	Port         int    `json:"port"`
	BridgeTarget string `json:"bridgeTarget"`
}

// RelayConfig renders the configuration for the in-guest relay.
func RelayConfig(relays []Relay) ([]byte, error) {
	entries := make([]guestEntry, 0, len(relays))
	for _, r := range relays {
		entries = append(entries, guestEntry{
			Name:         r.Name,
			GuestAddress: r.GuestAddress,
			Port:         r.Port,
			BridgeTarget: net.JoinHostPort(r.BridgeAddress, strconv.Itoa(r.BridgePort)),
		})
	}
	return json.Marshal(entries)
}

// HostsBlock renders the fenced section for the guest's /etc/hosts.
func HostsBlock(relays []Relay) string {
	var b strings.Builder
	b.WriteString(hostsBegin + "\n")
	for _, r := range relays {
		b.WriteString(fmt.Sprintf("%s\t%s\n", r.GuestAddress, r.Name))
	}
	b.WriteString(hostsEnd + "\n")
	return b.String()
}

// MergeHosts replaces shunt's fenced block in an existing /etc/hosts, or appends
// one when none is present.
//
// Rewriting the whole file would drop whatever else put entries there, and
// appending unconditionally would grow the file on every start until duplicate
// names made resolution depend on ordering.
func MergeHosts(existing string, relays []Relay) string {
	block := HostsBlock(relays)
	begin := strings.Index(existing, hostsBegin)
	if begin < 0 {
		if existing != "" && !strings.HasSuffix(existing, "\n") {
			existing += "\n"
		}
		return existing + block
	}
	end := strings.Index(existing[begin:], hostsEnd)
	if end < 0 {
		// A truncated block, most likely from a write interrupted mid-flight.
		// Everything from the marker on is shunt's, so replacing it is safe and
		// leaves the file valid rather than carrying a half-written fence forever.
		return existing[:begin] + block
	}
	tail := existing[begin+end+len(hostsEnd):]
	return existing[:begin] + block + strings.TrimPrefix(tail, "\n")
}

// GuestScript is the shell shunt runs inside the guest to apply a relay plan: it
// writes the config, merges the hosts block, and restarts the relay.
//
// One script rather than several execs because the steps are not independently
// useful. A hosts file naming addresses no relay answers on is worse than not
// having written it, so they land together or not at all.
func GuestScript(config, hosts string) string {
	return strings.Join([]string{
		"set -eu",
		"mkdir -p /run/shunt",
		// A previous relay holds the loopback addresses this one needs, so it has
		// to be gone before the new one binds rather than merely signalled.
		"if [ -f /run/shunt/host-reach.pid ]; then kill \"$(cat /run/shunt/host-reach.pid)\" 2>/dev/null || true; fi",
		"sleep 0.2",
		fmt.Sprintf("cat > %s <<'SHUNT_RELAY_CONFIG'\n%s\nSHUNT_RELAY_CONFIG", ConfigPath, config),
		// Write beside /etc/hosts and rename over it. A copy that is interrupted
		// leaves the file half-written, and the half that goes missing is whatever
		// else put entries there. Rename is atomic, so the file is either the old
		// one or the new one and never part of both. The temp file has to be on
		// the same filesystem for that, which is why it is not under /run.
		"cat > /etc/hosts.shunt.tmp <<'SHUNT_HOSTS'\n" + hosts + "\nSHUNT_HOSTS",
		"mv /etc/hosts.shunt.tmp /etc/hosts",
		fmt.Sprintf("%s --config %s >>/var/log/shunt-host-reach.log 2>&1 &", RelayProgram, ConfigPath),
		"echo $! > /run/shunt/host-reach.pid",
	}, "\n")
}
