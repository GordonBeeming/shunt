package dockerdpolicy

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEntrypointPolicyEnvironment(t *testing.T) {
	asset := entrypointAsset(t)
	out, err := exec.Command("/bin/sh", asset, "--print-policy-env").CombinedOutput()
	if err != nil {
		t.Fatalf("print policy environment: %v\n%s", err, out)
	}

	got := strings.Split(strings.TrimSpace(string(out)), "\n")
	want := []string{
		"HTTP_PROXY=http://127.0.0.1:1",
		"HTTPS_PROXY=http://127.0.0.1:1",
		"http_proxy=http://127.0.0.1:1",
		"https_proxy=http://127.0.0.1:1",
		"NO_PROXY=",
		"no_proxy=",
		"SHUNT_DOCKERD_OFFLINE_POLICY=" + PolicyVersion,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("policy environment mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestEntrypointShellSyntax(t *testing.T) {
	asset := entrypointAsset(t)
	if out, err := exec.Command("/bin/sh", "-n", asset).CombinedOutput(); err != nil {
		t.Fatalf("entrypoint shell syntax: %v\n%s", err, out)
	}
}

func TestEntrypointPolicyContract(t *testing.T) {
	asset := entrypointAsset(t)
	out, err := exec.Command("/bin/sh", asset, "--print-policy-contract").CombinedOutput()
	if err != nil {
		t.Fatalf("print policy contract: %v\n%s", err, out)
	}
	want := "version=" + PolicyVersion + "\nmarker=" + ReadyMarker + "\n"
	if string(out) != want {
		t.Fatalf("policy contract mismatch: got %q, want %q", out, want)
	}
}

func TestStableGuestContract(t *testing.T) {
	if EnsureCommand != "/usr/local/bin/shunt-dockerd-offline" {
		t.Fatalf("unexpected ensure command: %q", EnsureCommand)
	}
	if ReadyMarker != "/run/shunt/dockerd-offline.ready" {
		t.Fatalf("unexpected ready marker: %q", ReadyMarker)
	}
	if AdmissionSocket != "/var/run/docker.sock" || BackendSocket != "/run/shunt/dockerd/docker.sock" {
		t.Fatalf("unexpected admission sockets: public=%q backend=%q", AdmissionSocket, BackendSocket)
	}
}

func entrypointAsset(t *testing.T) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve current test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(current), "..", "image", "assets", "shunt-entrypoint.sh"))
}
