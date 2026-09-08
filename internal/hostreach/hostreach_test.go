package hostreach

import (
	"context"
	"errors"
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
	relays, err := Plan("192.168.64.1", entries)
	if err != nil {
		t.Fatal(err)
	}
	guestAddresses, bridgePorts := map[string]bool{}, map[int]bool{}
	for _, r := range relays {
		if guestAddresses[r.GuestAddress] {
			t.Errorf("guest address %s reused; entries on the same port would collide", r.GuestAddress)
		}
		if bridgePorts[r.BridgePort] {
			t.Errorf("bridge port %d reused", r.BridgePort)
		}
		guestAddresses[r.GuestAddress], bridgePorts[r.BridgePort] = true, true
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
	first, err := Plan("192.168.64.1", entries)
	if err != nil {
		t.Fatal(err)
	}
	shuffled := []Resolved{entries[1], entries[0]}
	second, err := Plan("192.168.64.1", shuffled)
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
	_, err := Plan("192.168.64.1", []Resolved{
		{Name: "same", Port: 443, Address: "10.0.0.1"},
		{Name: "same", Port: 443, Address: "10.0.0.2"},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want a duplicate refusal", err)
	}
}

func TestBridgeAddressIsDerivedFromTheGuestSubnet(t *testing.T) {
	// Derived rather than hardcoded, so it follows the runtime's subnet instead
	// of assuming the one this machine happens to use.
	got, err := BridgeAddressFor("192.168.64.8")
	if err != nil || got != "192.168.64.1" {
		t.Fatalf("BridgeAddressFor() = %q, %v", got, err)
	}
	if other, err := BridgeAddressFor("10.42.7.3"); err != nil || other != "10.42.7.1" {
		t.Fatalf("BridgeAddressFor() on another subnet = %q, %v", other, err)
	}
	if _, err := BridgeAddressFor("not-an-ip"); err == nil {
		t.Error("BridgeAddressFor() accepted a non-address")
	}
}

func TestMergeHostsReplacesShuntsBlockAndKeepsTheRest(t *testing.T) {
	relays, err := Plan("192.168.64.1", []Resolved{{Name: "vm-sql-dev", Port: 1433, Address: "10.0.0.4"}})
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
	relays, err := Plan("192.168.64.1", []Resolved{{Name: "vm-sql-dev", Port: 1433, Address: "10.0.0.4"}})
	if err != nil {
		t.Fatal(err)
	}
	config, err := RelayConfig(relays)
	if err != nil {
		t.Fatal(err)
	}
	// The guest has no use for the private address and should not carry a copy of
	// the host's network layout. It dials the bridge; the host end knows the rest.
	if strings.Contains(string(config), "10.0.0.4") {
		t.Errorf("relay config leaked the private address into the guest:\n%s", config)
	}
	if !strings.Contains(string(config), "192.168.64.1") {
		t.Errorf("relay config is missing its bridge target:\n%s", config)
	}
}
