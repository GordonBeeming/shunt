package siding

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/image"
	"github.com/gordonbeeming/shunt/internal/state"
)

func TestAppleContainerDockerdRecreateProcSysPolicy(t *testing.T) {
	if os.Getenv("SHUNT_CONTAINER_INTEGRATION") != "1" {
		t.Skip("set SHUNT_CONTAINER_INTEGRATION=1 to exercise Apple Container 1.2.2 proc/sys policy")
	}
	if err := exec.Command("container", "system", "status").Run(); err != nil {
		t.Skipf("Apple container runtime unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if err := image.EnsureBuilt(ctx, false); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("shunt-procsys-test-%d", time.Now().UnixNano())
	if err := container.Run(ctx, container.RunOpts{Name: name, Image: image.Tag(), Init: true, CapAddAll: true, WritableProcSys: true, Cmd: []string{"/bin/sh", "-lc", "exec sleep infinity"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = container.Remove(context.Background(), name) })
	sd := state.Siding{Name: "proc-sys", Container: name, MaterializationPhase: state.PhaseGuest}
	if err := EnsureDockerd(ctx, sd); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"sh", "-c", "test -w /proc/sys/net/ipv4/ip_forward"}, {"sh", "-c", "test ! -w /proc/sysrq-trigger"}, {"docker", "info"}} {
		if _, err := container.Exec(ctx, name, command...); err != nil {
			t.Fatalf("%v: %v", command, err)
		}
	}
}
