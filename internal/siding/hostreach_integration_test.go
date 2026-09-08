//go:build integration

package siding

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gordonbeeming/shunt/internal/state"
)

// TestHostReachCarriesTrafficEndToEnd drives the real chain against a real
// guest: applyHostReach starts both ends, and an application inside the guest
// then reaches a host-only endpoint by the name the contract declared.
//
// The endpoint is a listener on the host's loopback, which no guest can reach on
// its own. That is the whole property under test, and it needs no VPN and no
// shared dependency to demonstrate.
func TestHostReachCarriesTrafficEndToEnd(t *testing.T) {
	container := requireGuest(t)
	useRealShuntBinary(t)

	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 64)
				n, _ := conn.Read(buf)
				_, _ = conn.Write(append([]byte("HOST:"), buf[:n]...))
			}(conn)
		}
	}()
	_, portText, _ := net.SplitHostPort(echo.Addr().String())
	port := 0
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil {
		t.Fatal(err)
	}

	app, sd := sidingForGuest(t, container, state.HostReach{
		// An explicit address, because this endpoint exists only for the test and
		// the host has no record of the name. Declared entries normally carry no
		// address at all.
		Name: "echo-endpoint.invalid", Port: port, Address: "127.0.0.1",
	})

	if err := applyHostReach(context.Background(), app, sd); err != nil {
		reportEnds(t, app, sd)
		t.Fatalf("applyHostReach: %v", err)
	}
	t.Cleanup(func() { removeHostReach(context.Background(), app, sd) })

	out, err := guestConnect(t, container, "echo-endpoint.invalid", port, "ping")
	if err != nil {
		t.Fatalf("the guest could not reach the endpoint: %v (%s)", err, out)
	}
	if !strings.Contains(out, "HOST:ping") {
		t.Errorf("guest got %q, want the host's answer", out)
	}
}

// TestHostReachProbeFailsWhenTheHostEndIsGone proves the check can fail. A check
// that cannot fail is how this feature shipped broken twice.
func TestHostReachProbeFailsWhenTheHostEndIsGone(t *testing.T) {
	container := requireGuest(t)
	useRealShuntBinary(t)
	app, sd := sidingForGuest(t, container, state.HostReach{
		Name: "echo-endpoint.invalid", Port: 9, Address: "127.0.0.1",
	})

	if err := applyHostReach(context.Background(), app, sd); err != nil {
		reportEnds(t, app, sd)
		t.Fatalf("applyHostReach: %v", err)
	}
	t.Cleanup(func() { removeHostReach(context.Background(), app, sd) })

	// With the host end stopped, the guest's relay still listens and still
	// accepts, so only a byte round trip can tell the difference.
	stopHostReachProcess(app, sd)
	if err := probeHostReachChain(context.Background(), sd); err == nil {
		t.Fatal("the probe passed with no host end running")
	}
}

// useRealShuntBinary points the host end at the installed shunt binary. Under
// `go test`, os.Executable names the test runner, which has no host-reach
// command, so the process would start and immediately fail.
func useRealShuntBinary(t *testing.T) {
	t.Helper()
	path := os.Getenv("SHUNT_BINARY")
	if path == "" {
		t.Skip("set SHUNT_BINARY to the shunt binary that runs the host end")
	}
	orig := hostReachBinary
	t.Cleanup(func() { hostReachBinary = orig })
	hostReachBinary = func() (string, error) { return path, nil }
}

// reportEnds prints both ends' logs. A failure here is a failure of a chain, and
// which end broke is the first thing worth knowing.
func reportEnds(t *testing.T, app state.App, sd state.Siding) {
	t.Helper()
	if configPath, _, err := hostReachPaths(app, sd.Name); err == nil {
		if body, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), "host-reach.log")); err == nil {
			t.Logf("host end log:\n%s", body)
		} else {
			t.Logf("no host end log: %v", err)
		}
	}
	if out, err := execGuest(context.Background(), sd.Container, "tail", "-20", "/var/log/shunt-host-reach.log"); err == nil {
		t.Logf("guest relay log:\n%s", out)
	}
}

func requireGuest(t *testing.T) string {
	t.Helper()
	if os.Getenv("SHUNT_CONTAINER_INTEGRATION") == "" {
		t.Skip("needs a guest; set SHUNT_CONTAINER_INTEGRATION=1")
	}
	container := os.Getenv("SHUNT_PROBE_GUEST")
	if container == "" {
		t.Skip("set SHUNT_PROBE_GUEST to a running siding guest")
	}
	return container
}

func sidingForGuest(t *testing.T, container string, entries ...state.HostReach) (state.App, state.Siding) {
	t.Helper()
	configDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configDir, "one"), 0o755); err != nil {
		t.Fatal(err)
	}
	ip, err := guestAddress(t, container)
	if err != nil {
		t.Fatal(err)
	}
	return state.App{Name: "itest", ConfigDir: configDir, HostReach: entries},
		state.Siding{Name: "one", Container: container, LastIP: ip}
}

func guestAddress(t *testing.T, container string) (string, error) {
	t.Helper()
	// hostname -i rather than `ip`: iproute2 is not in the base image.
	out, err := execGuest(context.Background(), container, "sh", "-c", "hostname -i | awk '{print $1}'")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// guestConnect runs a connection from inside the guest to a declared name, which
// is what an application in the siding does.
func guestConnect(t *testing.T, container, name string, port int, payload string) (string, error) {
	t.Helper()
	script := fmt.Sprintf(`python3 - <<'PY'
import socket
s = socket.socket(); s.settimeout(10)
s.connect((%q, %d))
s.sendall(%q.encode())
print(s.recv(128).decode())
PY`, name, port, payload)
	return execGuest(context.Background(), container, "sh", "-c", script)
}
