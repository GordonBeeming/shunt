package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/config"
)

func TestExecBoundedReturnsStalledErrorWithAttachedProcessesOnDeadline(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, Bin), []byte("#!/bin/sh\nsleep 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	original := attachedExecProbe
	defer func() { attachedExecProbe = original }()
	attachedExecProbe = func(guest string) []HostProcess {
		return []HostProcess{{PID: 4321, Command: "container exec " + guest + " true"}}
	}

	_, err := ExecBounded(context.Background(), 50*time.Millisecond, "shuntdev_app_alpha", "true")
	var stalled *ExecStalledError
	if !errors.As(err, &stalled) {
		t.Fatalf("ExecBounded() error = %v, want *ExecStalledError", err)
	}
	if stalled.Guest != "shuntdev_app_alpha" || stalled.Timeout != 50*time.Millisecond || len(stalled.Attached) != 1 {
		t.Fatalf("stalled error = %#v", stalled)
	}
	message := err.Error()
	for _, want := range []string{
		"shuntdev_app_alpha", "4321", "container exec shuntdev_app_alpha true",
		config.Current().BinaryName + " restart alpha",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("stalled error text = %q, missing %q", message, want)
		}
	}
}

func TestExecBoundedReturnsNormalErrorWhenCommandFailsFast(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, Bin), []byte("#!/bin/sh\necho boom >&2\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := ExecBounded(context.Background(), 5*time.Second, "guest", "true")
	var stalled *ExecStalledError
	if errors.As(err, &stalled) {
		t.Fatalf("fast failure misreported as a stall: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("ExecBounded() error = %v, want the underlying failure", err)
	}
}

func TestExecBoundedTreatsCallerCancellationAsOrdinaryNotAStall(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, Bin), []byte("#!/bin/sh\nsleep 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ExecBounded(ctx, 5*time.Second, "guest", "true")
	var stalled *ExecStalledError
	if errors.As(err, &stalled) {
		t.Fatalf("caller cancellation misreported as a guest stall: %v", err)
	}
	if err == nil {
		t.Fatal("ExecBounded() unexpectedly succeeded against a cancelled context")
	}
}

func TestParseAttachedExecsExtractsRightGuestOnly(t *testing.T) {
	psOutput := strings.Join([]string{
		"  501 /usr/bin/vim file.txt",
		"  601 /opt/homebrew/bin/container exec guest-a sh -c true",
		"  602 /opt/homebrew/bin/container exec guest-b sh -c true",
		"  603 /opt/homebrew/bin/container run -d --name guest-a-decoy",
		"",
	}, "\n")
	got := parseAttachedExecs(psOutput, "guest-a")
	if len(got) != 1 {
		t.Fatalf("parseAttachedExecs() = %#v, want exactly one match", got)
	}
	if got[0].PID != 601 || !strings.Contains(got[0].Command, "container exec guest-a") {
		t.Fatalf("matched process = %#v", got[0])
	}
}

func TestSidingNameHintDerivesShortNameFromPrefixedGuest(t *testing.T) {
	if got := sidingNameHint("shuntdev_Alpha_one"); got != "one" {
		t.Fatalf("sidingNameHint() = %q", got)
	}
	if got := sidingNameHint("no-underscore"); got != "no-underscore" {
		t.Fatalf("sidingNameHint() fallback = %q", got)
	}
}
