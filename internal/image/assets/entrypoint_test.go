package main

import (
	"os"
	"strings"
	"testing"
)

func TestEntrypointPreservesDockerdRepairEvidence(t *testing.T) {
	contents, err := os.ReadFile("shunt-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	for _, fragment := range []string{
		"reason=$repair_reason prior_pid=${prior_pid:-none}",
		"mv \"$dockerd_log\" \"$rotated\"",
		"failed to TERM dockerd PID",
		"failed to KILL dockerd PID",
		"failed to TERM admission PID",
		"failed to KILL admission PID",
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("entrypoint is missing repair breadcrumb %q", fragment)
		}
	}
}

func TestEntrypointSerializesDockerdRepairs(t *testing.T) {
	contents, err := os.ReadFile("shunt-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	lock := `exec 9>"$dockerd_lock_file"`
	acquire := "flock -w 120 9"
	repair := "ensure_offline_dockerd"
	unlock := "flock -u 9"
	closeLock := "exec 9>&-"
	for _, fragment := range []string{
		"dockerd_lock_file=/run/shunt/dockerd-offline.lock",
		lock,
		acquire,
		unlock,
		closeLock,
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("entrypoint is missing serialized repair fragment %q", fragment)
		}
	}
	lockAt := strings.LastIndex(text, lock)
	acquireAt := strings.LastIndex(text, acquire)
	repairAt := strings.LastIndex(text, repair)
	unlockAt := strings.LastIndex(text, unlock)
	closeAt := strings.LastIndex(text, closeLock)
	if !(lockAt < acquireAt && acquireAt < repairAt && repairAt < unlockAt && unlockAt < closeAt) {
		t.Fatalf("repair lock order is invalid: lock=%d acquire=%d repair=%d unlock=%d close=%d", lockAt, acquireAt, repairAt, unlockAt, closeAt)
	}
}

func TestEntrypointHealthHelpersDoNotClobberDockerdPID(t *testing.T) {
	contents, err := os.ReadFile("shunt-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(contents)
	for _, fragment := range []string{
		"process_pid=$1",
		"daemon_pid=$1",
		"marker_pid=$1",
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("entrypoint is missing PID-isolation fragment %q", fragment)
		}
	}
	if strings.Contains(text, "process_is_running() {\n    pid=$1") {
		t.Fatal("process_is_running still clobbers the caller's dockerd PID")
	}
}
