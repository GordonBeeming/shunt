package siding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gordonbeeming/shunt/internal/hostreach"
	"github.com/gordonbeeming/shunt/internal/state"
)

// probeTimeout bounds the reachability check. Every hop is on this machine, so a
// slow answer means the chain is not carrying traffic rather than that the
// network is far away.
const probeTimeout = 20 * time.Second

// probeAttemptTimeout bounds one attempt, and probeRetryPause spaces them. One
// attempt is short so a broken chain is not waited out at full length, and the
// budget above is what covers a host end still connecting.
const (
	probeAttemptTimeout = 5 * time.Second
	probeRetryPause     = 500 * time.Millisecond
)

// Seams so the relay can be tested without a guest or a host process.
var (
	resolveHostReach     = hostreach.Resolve
	newHostReachToken    = hostreach.NewToken
	startHostReachServer = startHostReachProcess
	stopHostReachServer  = stopHostReachProcess
	probeHostReach       = probeHostReachChain

	// hostReachBinary is what runs the host end. It is the running shunt binary
	// in normal use, and a seam because a test binary is not shunt: os.Executable
	// under `go test` names the test runner, which has no host-reach command.
	hostReachBinary = os.Executable
)

// hostReachPaths returns the per-siding files the host end needs: its config and
// the pid of the process serving it.
func hostReachPaths(app state.App, siding string) (configPath, pidPath string, err error) {
	base, err := SidingBase(app, siding)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(base, "host-reach.json"), filepath.Join(base, "host-reach.pid"), nil
}

// applyHostReach carries the endpoints only the host can reach into a running
// guest: the guest's relay listens on a loopback address per entry, and a host
// process dials the guest and does the outbound connecting on its behalf.
//
// The host dials rather than listens. The macOS application firewall blocks
// incoming connections to programs it has not been told to allow, on every
// address except loopback, and blocks them without refusing, so a listener on
// the host completes the handshake and then carries nothing.
//
// It runs on every start rather than at guest creation, because the addresses
// are resolved fresh each time. An endpoint recreated since the last start
// moves, and a guest carrying the previous address would fail in the one way
// this feature exists to avoid: confidently, at the wrong place.
func applyHostReach(ctx context.Context, app state.App, sd state.Siding) error {
	if len(app.HostReach) == 0 {
		return nil
	}
	if sd.LastIP == "" {
		return errors.New("host-reach needs the guest address; start the siding first")
	}

	// Resolve before touching anything. A name the host cannot answer for is the
	// host's state being wrong, and failing here leaves no listener and no hosts
	// entry behind to half-work.
	resolved, err := resolveHostReach(ctx, app.HostReach)
	if err != nil {
		return err
	}
	relays, err := hostreach.Plan(resolved)
	if err != nil {
		return err
	}
	token, err := newHostReachToken()
	if err != nil {
		return err
	}

	// The guest is started first: it holds the listener, so the host process has
	// something to connect to rather than backing off against a closed port.
	config, err := hostreach.RelayConfig(relays, token)
	if err != nil {
		return err
	}
	existing, err := execGuest(ctx, sd.Container, "cat", "/etc/hosts")
	if err != nil {
		return fmt.Errorf("read guest hosts file: %w", err)
	}
	script := hostreach.GuestScript(string(config), hostreach.MergeHosts(existing, relays))
	if _, err := execGuest(ctx, sd.Container, "sh", "-c", script); err != nil {
		return fmt.Errorf("start the guest host-reach relay: %w", err)
	}

	if err := startHostReachServer(ctx, app, sd, token, relays); err != nil {
		return err
	}
	for _, r := range relays {
		fmt.Fprintf(os.Stdout, "• reaching %s\n", r)
	}

	// Prove the chain carries traffic before trusting it. Everything above can
	// succeed while the chain still carries nothing, which is the failure this
	// feature has already shipped once.
	if err := probeHostReach(ctx, sd); err != nil {
		stopHostReachServer(app, sd)
		return err
	}
	return nil
}

// startHostReachProcess launches the host end for one siding and records its pid.
//
// Detached into its own process group so it outlives the CLI invocation that
// started it. The guest's relay is started the same way, and the symmetry is
// deliberate: both ends live for as long as the siding, not for as long as a
// command.
func startHostReachProcess(ctx context.Context, app state.App, sd state.Siding, token string, relays []hostreach.Relay) error {
	configPath, pidPath, err := hostReachPaths(app, sd.Name)
	if err != nil {
		return err
	}
	// Replace any process still serving a previous start of this siding, so the
	// guest is not fed by two hosts with different tokens. This happens before the
	// config is written, because stopping also removes the config it used, and
	// doing it the other way round deletes the file the new process needs.
	stopHostReachProcess(app, sd)

	body, err := hostreach.HostConfigFor(sd.LastIP, token, relays)
	if err != nil {
		return err
	}
	// 0600: the file holds the siding's token and the private addresses the guest
	// is deliberately never told.
	if err := os.WriteFile(configPath, body, 0o600); err != nil {
		return fmt.Errorf("write the host-reach config: %w", err)
	}

	self, err := hostReachBinary()
	if err != nil {
		return fmt.Errorf("locate the shunt binary for host-reach: %w", err)
	}
	logPath := filepath.Join(filepath.Dir(configPath), "host-reach.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open the host-reach log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(self, "host-reach", "serve", "--config", configPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the host-reach process: %w", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("record the host-reach pid: %w", err)
	}
	// Released rather than waited on: this process is meant to outlive the
	// command, and not reaping it here is what lets that happen.
	return cmd.Process.Release()
}

// stopHostReachProcess ends the host end for one siding.
//
// Best effort by design: it runs on teardown paths that have to complete, and a
// process that has already gone is the normal case rather than an error.
func stopHostReachProcess(app state.App, sd state.Siding) {
	configPath, pidPath, err := hostReachPaths(app, sd.Name)
	if err != nil {
		return
	}
	if raw, err := os.ReadFile(pidPath); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	_ = os.Remove(pidPath)
	// The config carries the token and the private addresses, so it goes with the
	// process that used it rather than sitting on disk until the next start.
	_ = os.Remove(configPath)
}

// probeHostReachChain proves the whole chain carries traffic, by sending a value
// from the guest and requiring the same value back.
//
// It goes through the guest's own relay to a name only the host end answers, so
// one pass covers the loopback listener, the pool, the token and the host
// process. Nothing here touches a declared dependency, so a slow or broken
// endpoint cannot make this fail and a working one cannot make it pass.
//
// A dial would prove nothing: an unattended listener still completes the
// handshake in the kernel, so a connect succeeds and only the read hangs.
func probeHostReachChain(ctx context.Context, sd state.Siding) error {
	target := fmt.Sprintf("%s:%d", hostreach.ProbeGuestAddress, hostreach.ProbeGuestPort)
	deadline := time.Now().Add(probeTimeout)
	var last error
	for attempt := 1; ; attempt++ {
		nonce := fmt.Sprintf("shunt-host-reach-probe-%d", time.Now().UnixNano())
		attemptCtx, cancel := context.WithTimeout(ctx, probeAttemptTimeout)
		_, last = execGuest(attemptCtx, sd.Container, hostreach.RelayProgram, "--check", target, "--nonce", nonce)
		cancel()
		if last == nil {
			return nil
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			break
		}
		// The host end has just been started and may not have finished connecting.
		// Retrying is the difference between reporting a broken chain and waiting
		// out the first second of a working one.
		select {
		case <-time.After(probeRetryPause):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf(`the host-reach chain is not carrying traffic for siding %q.

The guest's relay is running and shunt's host process is started, but a value
sent from the guest did not come back. Check both ends:

  guest   /var/log/shunt-host-reach.log inside the guest
  host    host-reach.log beside this siding's config

Underlying failure: %w`, sd.Name, last)
}

// removeHostReach ends the host end a siding started. A process that outlived
// its guest would keep a token and a route to a private endpoint alive with
// nothing using them.
func removeHostReach(_ context.Context, app state.App, sd state.Siding) {
	if len(app.HostReach) == 0 {
		return
	}
	stopHostReachServer(app, sd)
}
