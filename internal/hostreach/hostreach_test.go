package hostreach

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

func stubLookup(answers map[string][]string) lookupFunc {
	return func(_ context.Context, host string) ([]string, error) {
		addrs, ok := answers[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		return addrs, nil
	}
}

func noInterface(context.Context, string) string { return "" }

func TestResolveUsesTheHostsCurrentAnswerRatherThanADeclaredAddress(t *testing.T) {
	// The whole reason an address is not normally declared: the host's answer
	// moves when a private endpoint is recreated, and shunt has to follow it.
	got, err := resolve(context.Background(),
		[]state.HostReach{{Name: "vm-sql-dev", Port: 1433}},
		stubLookup(map[string][]string{"vm-sql-dev": {"10.0.9.9"}}), noInterface)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Address != "10.0.9.9" {
		t.Fatalf("resolved = %+v, want the host's current answer", got)
	}
}

func TestResolveKeepsAnExplicitAddressWithoutLookup(t *testing.T) {
	// The escape hatch for a name the host has no record for at all. It must not
	// consult the resolver, or it would fail for exactly the case it exists for.
	got, err := resolve(context.Background(),
		[]state.HostReach{{Name: "no-dns-record", Port: 443, Address: "10.1.2.3"}},
		func(context.Context, string) ([]string, error) {
			t.Error("lookup called for an entry that declared its address")
			return nil, errors.New("must not be called")
		}, noInterface)
	if err != nil || len(got) != 1 || got[0].Address != "10.1.2.3" {
		t.Fatalf("resolved = %+v, err = %v", got, err)
	}
}

func TestResolveReportsEveryUnresolvableNameAtOnce(t *testing.T) {
	// Reporting one failure per run turns a broken hosts file into a sequence of
	// guest starts. Naming all of them makes it one fix.
	_, err := resolve(context.Background(), []state.HostReach{
		{Name: "gone-one", Port: 443},
		{Name: "present", Port: 443},
		{Name: "gone-two", Port: 1433},
	}, stubLookup(map[string][]string{"present": {"10.0.0.5"}}), noInterface)
	if err == nil {
		t.Fatal("resolve() = nil error, want a failure naming the unresolvable entries")
	}
	for _, want := range []string{"gone-one", "gone-two"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%s", want, err)
		}
	}
	if !strings.Contains(err.Error(), "the host cannot resolve") {
		t.Errorf("error should point at the host's state, not the contract:\n%s", err)
	}
}

func TestPlanGivesEveryEntryItsOwnGuestAddress(t *testing.T) {
	// The case that killed the single shared address: many names on one port.
	// They must land on distinct guest addresses, because a hosts entry maps a
	// name to an address and not to a port.
	var entries []Resolved
	for _, name := range []string{"kv-dev", "appconfig-dev", "acr-dev", "ai-dev"} {
		entries = append(entries, Resolved{Name: name, Port: 443, Address: "10.0.0.9"})
	}
	relays, err := Plan(entries)
	if err != nil {
		t.Fatal(err)
	}
	guestAddresses := map[string]bool{}
	for _, r := range relays {
		if guestAddresses[r.GuestAddress] {
			t.Errorf("guest address %s reused; entries on the same port would collide", r.GuestAddress)
		}
		guestAddresses[r.GuestAddress] = true
		if r.Port != 443 {
			t.Errorf("%s: port = %d, want the real port preserved", r.Name, r.Port)
		}
		if r.GuestAddress == "127.0.0.1" {
			t.Errorf("%s: allocated plain localhost, which would shadow the app's own binds", r.Name)
		}
	}
}

func TestPlanIsStableAcrossRuns(t *testing.T) {
	// A guest's hosts file outlives a stop. If a restart renumbered the entries,
	// those names would point at addresses the relay no longer answers on.
	entries := []Resolved{
		{Name: "b", Port: 443, Address: "10.0.0.2"},
		{Name: "a", Port: 1433, Address: "10.0.0.1"},
	}
	first, err := Plan(entries)
	if err != nil {
		t.Fatal(err)
	}
	shuffled := []Resolved{entries[1], entries[0]}
	second, err := Plan(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("plan changed with input order:\n %+v\n %+v", first[i], second[i])
		}
	}
}

func TestPlanRejectsADuplicateEntry(t *testing.T) {
	_, err := Plan([]Resolved{
		{Name: "same", Port: 443, Address: "10.0.0.1"},
		{Name: "same", Port: 443, Address: "10.0.0.2"},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want a duplicate refusal", err)
	}
}

func TestGuestControlAddressUsesTheGuestsOwnAddress(t *testing.T) {
	// The host dials the guest, so the address it needs is the guest's, not the
	// bridge's. A malformed one has to fail here rather than at dial time.
	got, err := GuestControlAddress("192.168.64.8")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("192.168.64.8:%d", ControlPort); got != want {
		t.Errorf("GuestControlAddress() = %s, want %s", got, want)
	}
	if _, err := GuestControlAddress("not-an-address"); err == nil {
		t.Error("GuestControlAddress() accepted something that is not an address")
	}
}

func TestMergeHostsReplacesShuntsBlockAndKeepsTheRest(t *testing.T) {
	relays, err := Plan([]Resolved{{Name: "vm-sql-dev", Port: 1433, Address: "10.0.0.4"}})
	if err != nil {
		t.Fatal(err)
	}
	base := "127.0.0.1\tlocalhost\n::1\tip6-localhost\n"

	first := MergeHosts(base, relays)
	if !strings.Contains(first, "localhost") {
		t.Fatal("merge dropped entries shunt did not write")
	}
	if !strings.Contains(first, "vm-sql-dev") {
		t.Fatal("merge did not add the relayed name")
	}

	// Restarting must replace the block, not append a second one: duplicate names
	// would make resolution depend on file order.
	second := MergeHosts(first, relays)
	if strings.Count(second, "vm-sql-dev") != 1 {
		t.Fatalf("re-merge duplicated the entry:\n%s", second)
	}
	if strings.Count(second, hostsBegin) != 1 {
		t.Fatalf("re-merge duplicated the fence:\n%s", second)
	}

	// A write interrupted mid-block leaves an opening fence with no close. The
	// merge has to recover rather than carry it forever.
	truncated := base + hostsBegin + "\n127.0.0.10\thalf-written\n"
	repaired := MergeHosts(truncated, relays)
	if strings.Count(repaired, hostsBegin) != 1 || strings.Contains(repaired, "half-written") {
		t.Fatalf("truncated block not repaired:\n%s", repaired)
	}
	if !strings.Contains(repaired, "localhost") {
		t.Fatal("repair dropped entries shunt did not write")
	}
}

func TestRelayConfigOmitsTheRealAddress(t *testing.T) {
	relays, err := Plan([]Resolved{{Name: "vm-sql-dev", Port: 1433, Address: "10.0.0.4"}})
	if err != nil {
		t.Fatal(err)
	}
	config, err := RelayConfig(relays, "a-token")
	if err != nil {
		t.Fatal(err)
	}
	// The guest has no use for the private address and should not carry a copy of
	// the host's network layout. It names the endpoint; the host end knows where
	// that is.
	if strings.Contains(string(config), "10.0.0.4") {
		t.Errorf("relay config leaked the private address into the guest:\n%s", config)
	}
	if !strings.Contains(string(config), "vm-sql-dev") {
		t.Errorf("relay config is missing the endpoint name:\n%s", config)
	}
}

func TestRelayConfigCarriesTheProbeButTheHostsBlockDoesNot(t *testing.T) {
	relays, err := Plan([]Resolved{{Name: "vm-sql-dev", Port: 1433, Address: "10.0.0.4"}})
	if err != nil {
		t.Fatal(err)
	}
	config, err := RelayConfig(relays, "a-token")
	if err != nil {
		t.Fatal(err)
	}
	// The probe needs a listener so a check travels the same path an application
	// does. It must not be resolvable, or an app could reach an endpoint that
	// only ever echoes.
	if !strings.Contains(string(config), ProbeName) {
		t.Errorf("relay config has no probe listener:\n%s", config)
	}
	if strings.Contains(HostsBlock(relays), ProbeName) {
		t.Errorf("the probe name is resolvable in the guest:\n%s", HostsBlock(relays))
	}
	if strings.Contains(HostsBlock(relays), ProbeGuestAddress) {
		t.Errorf("the probe address is in the hosts block:\n%s", HostsBlock(relays))
	}
}

func TestRelayConfigCarriesTheToken(t *testing.T) {
	// Without it the guest would pool any connection from any guest on the
	// bridge, and hand one of them an application's traffic.
	config, err := RelayConfig(nil, "the-secret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "the-secret") {
		t.Errorf("relay config has no token:\n%s", config)
	}
}

func TestNewTokenIsUnpredictable(t *testing.T) {
	first, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("two tokens matched; the guest's listener would be open to any siding")
	}
	if len(first) < 32 {
		t.Errorf("token is %d characters, too short to be worth checking", len(first))
	}
}

func TestResolveRejectsUnusableEntriesAlongsideResolutionFailures(t *testing.T) {
	// Port 0 is the one worth naming: it binds an ephemeral port rather than
	// failing, so the relay would come up listening where nothing connects.
	_, err := resolve(context.Background(), []state.HostReach{
		{Name: "", Port: 443},
		{Name: "bad-port", Port: 0},
		{Name: "huge-port", Port: 70000},
		{Name: "unresolvable", Port: 443},
	}, stubLookup(nil), noInterface)
	if err == nil {
		t.Fatal("resolve() = nil error, want every unusable entry reported")
	}
	for _, want := range []string{"no name", "bad-port", "huge-port", "unresolvable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%s", want, err)
		}
	}
}

func TestPlanRefusesRatherThanBuildingAnInvalidAddress(t *testing.T) {
	// The arithmetic used to run the third octet past 255, producing something
	// like 127.0.267.4 — which reads as an address and only fails at bind time.
	if _, err := loopbackAddress(maxEntries); err == nil {
		t.Error("loopbackAddress() accepted an index past the usable space")
	}
	last, err := loopbackAddress(maxEntries - 1)
	if err != nil {
		t.Fatalf("last usable index rejected: %v", err)
	}
	if net.ParseIP(last) == nil {
		t.Errorf("last usable address %q is not a valid IP", last)
	}
	for _, i := range []int{0, 1, perThirdOctet - 1, perThirdOctet, maxEntries - 1} {
		addr, err := loopbackAddress(i)
		if err != nil {
			t.Fatalf("index %d: %v", i, err)
		}
		if net.ParseIP(addr) == nil {
			t.Errorf("index %d produced %q, which is not a valid IP", i, addr)
		}
	}
}
