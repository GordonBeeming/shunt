package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/proc"
)

// Mount is a host->guest bind mount.
type Mount struct {
	Host     string
	Guest    string
	ReadOnly bool
}

// RunOpts describes a guest to launch.
type RunOpts struct {
	Name      string
	Image     string
	Init      bool              // --init (signal forwarding / reaping)
	CapAddAll bool              // --cap-add ALL (dockerd in the guest needs it)
	Mounts    []Mount           // bind mounts
	Env       map[string]string // -e KEY=VALUE
	Memory    string            // -m (e.g. "6g"); empty uses the runtime default
	CPUs      string            // -c (e.g. "4"); empty uses the runtime default
	Rosetta   bool              // --rosetta: x86 translation so amd64 images (e.g. SQL Server) run on arm64
	Cmd       []string          // command + args appended after the image
}

// Run launches a detached guest. Returns an error if the run fails.
func Run(ctx context.Context, o RunOpts) error {
	args := []string{"run", "-d", "--name", o.Name}
	if o.Init {
		args = append(args, "--init")
	}
	if o.CapAddAll {
		args = append(args, "--cap-add", "ALL")
	}
	if o.Memory != "" {
		args = append(args, "-m", o.Memory)
	}
	if o.CPUs != "" {
		args = append(args, "-c", o.CPUs)
	}
	if o.Rosetta {
		args = append(args, "--rosetta")
	}
	for _, m := range o.Mounts {
		v := m.Host + ":" + m.Guest
		if m.ReadOnly {
			v += ":ro"
		}
		args = append(args, "-v", v)
	}
	// Sort env for deterministic command construction (stable logs/tests).
	keys := make([]string, 0, len(o.Env))
	for k := range o.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+o.Env[k])
	}
	args = append(args, o.Image)
	args = append(args, o.Cmd...)

	if _, err := proc.Run(ctx, Bin, args...); err != nil {
		return fmt.Errorf("container run %s: %w", o.Name, err)
	}
	return nil
}

// inspectDoc mirrors the parts of `container inspect` shunt reads.
type inspectDoc struct {
	Status struct {
		State    string `json:"state"`
		Networks []struct {
			IPv4Address string `json:"ipv4Address"`
		} `json:"networks"`
	} `json:"status"`
}

// GuestObservationState is the typed result of inspecting one named guest.
// Callers must only treat GuestAbsent as proof that raw host data is safe to
// use without an in-guest lifecycle.
type GuestObservationState string

const (
	GuestRunning     GuestObservationState = "running"
	GuestStopped     GuestObservationState = "stopped"
	GuestAbsent      GuestObservationState = "absent"
	GuestUnavailable GuestObservationState = "unavailable"
)

type GuestObservation struct {
	State GuestObservationState
}

var (
	observeGuestInspect       = inspect
	postconditionObserveGuest = ObserveGuest
	postconditionAttempts     = 20
	postconditionPoll         = 50 * time.Millisecond
)

func waitGuestPostcondition(ctx context.Context, name, operation string, accepted func(GuestObservationState) bool) error {
	var last GuestObservationState
	for attempt := 0; attempt < postconditionAttempts; attempt++ {
		last = postconditionObserveGuest(ctx, name).State
		if accepted(last) {
			return nil
		}
		if attempt+1 < postconditionAttempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(postconditionPoll):
			}
		}
	}
	return fmt.Errorf("container %s %s did not reach its exact postcondition (state=%s)", operation, name, last)
}

// ObserveGuest distinguishes a proven absent guest from an unavailable or
// ambiguous runtime probe. Error-text interpretation remains contained at the
// container boundary and is never exposed as a caller policy decision.
func ObserveGuest(ctx context.Context, name string) GuestObservation {
	doc, err := observeGuestInspect(ctx, name)
	if err != nil {
		var absent *guestNotFoundError
		if errors.As(err, &absent) && absent.name == name {
			return GuestObservation{State: GuestAbsent}
		}
		return GuestObservation{State: GuestUnavailable}
	}
	if strings.EqualFold(doc.Status.State, "running") {
		return GuestObservation{State: GuestRunning}
	}
	if strings.EqualFold(doc.Status.State, "stopped") {
		return GuestObservation{State: GuestStopped}
	}
	return GuestObservation{State: GuestUnavailable}
}

type guestNotFoundError struct {
	name string
	err  error
}

func (e *guestNotFoundError) Error() string { return fmt.Sprintf("container %s not found", e.name) }
func (e *guestNotFoundError) Unwrap() error { return e.err }

func inspectErrorNamesAbsentGuest(err error, name string) bool {
	if err == nil || strings.TrimSpace(name) == "" {
		return false
	}
	message, target := strings.TrimSpace(err.Error()), strings.TrimSpace(name)
	// Apple container currently reports absence as either
	// `container not found: <name>` or `Error: container not found: <name>`.
	// This form is deliberately parsed as a complete message so a similarly
	// named guest (for example, target-older) cannot prove target absent.
	normalized := message
	if len(normalized) >= len("Error:") && strings.EqualFold(normalized[:len("Error:")], "Error:") {
		normalized = strings.TrimSpace(normalized[len("Error:"):])
	}
	const notFoundPrefix = "container not found:"
	if len(normalized) >= len(notFoundPrefix) && strings.EqualFold(normalized[:len(notFoundPrefix)], notFoundPrefix) {
		return strings.EqualFold(strings.TrimSpace(normalized[len(notFoundPrefix):]), target)
	}

	message, target = strings.ToLower(message), strings.ToLower(target)
	patterns := []string{
		"container " + target + " not found",
		"container \"" + target + "\" not found",
		"no such container: " + target,
		"no such container " + target,
		"container " + target + " does not exist",
		"container \"" + target + "\" does not exist",
	}
	for _, pattern := range patterns {
		if strings.Contains(message, pattern) {
			return true
		}
	}
	return false
}

func inspect(ctx context.Context, name string) (inspectDoc, error) {
	res, err := proc.Run(ctx, Bin, "inspect", name)
	if err != nil {
		if inspectErrorNamesAbsentGuest(err, name) {
			return inspectDoc{}, &guestNotFoundError{name: name, err: err}
		}
		return inspectDoc{}, fmt.Errorf("container inspect %s: %w", name, err)
	}
	var docs []inspectDoc
	if err := json.Unmarshal([]byte(res.Stdout), &docs); err != nil {
		return inspectDoc{}, fmt.Errorf("parse inspect %s: %w", name, err)
	}
	if len(docs) == 0 {
		return inspectDoc{}, &guestNotFoundError{name: name}
	}
	return docs[0], nil
}

// State returns the guest's lifecycle state (running/stopped/…).
func State(ctx context.Context, name string) (string, error) {
	doc, err := inspect(ctx, name)
	if err != nil {
		return "", err
	}
	return doc.Status.State, nil
}

// IP returns the guest's IPv4 address (CIDR stripped). The correct path is
// status.networks[0].ipv4Address — note `.status.networks`, not `.networks`.
func IP(ctx context.Context, name string) (string, error) {
	doc, err := inspect(ctx, name)
	if err != nil {
		return "", err
	}
	if len(doc.Status.Networks) == 0 || doc.Status.Networks[0].IPv4Address == "" {
		return "", fmt.Errorf("container %s has no network address yet", name)
	}
	return strings.SplitN(doc.Status.Networks[0].IPv4Address, "/", 2)[0], nil
}

// Logs returns the guest's combined log output (stdout+stderr of pid 1).
func Logs(ctx context.Context, name string) (string, error) {
	res, err := proc.Run(ctx, Bin, "logs", name)
	if err != nil {
		// `container logs` writes to stdout; return whatever we got.
		return res.Stdout + res.Stderr, err
	}
	return res.Stdout + res.Stderr, nil
}

// Exec runs a command in a running guest and returns its stdout.
func Exec(ctx context.Context, name string, args ...string) (string, error) {
	full := append([]string{"exec", name}, args...)
	res, err := proc.Run(ctx, Bin, full...)
	if err != nil {
		return res.Stdout, err
	}
	return res.Stdout, nil
}

// ExecStdinFile runs a command in a running guest, streaming the host file at
// stdinPath as its stdin (e.g. `docker load` of a warm-cache tar). Uses `exec -i`
// so no bind mount is needed to get the tar into the guest.
func ExecStdinFile(ctx context.Context, name, stdinPath string, args ...string) error {
	_, err := ExecStdinFileDigest(ctx, name, stdinPath, args...)
	return err
}

// ExecStdinFileDigest streams a host file into a guest command and returns the
// digest of exactly the bytes consumed by the guest process.
func ExecStdinFileDigest(ctx context.Context, name, stdinPath string, args ...string) (string, error) {
	full := append([]string{"exec", "-i", name}, args...)
	digest, err := proc.RunStdinDigest(ctx, stdinPath, Bin, full...)
	if err != nil {
		return "", fmt.Errorf("container exec -i %s: %w", name, err)
	}
	return digest, nil
}

// ExecDetached runs a long-lived command in the guest in the background (e.g. a
// socat bridge). It returns once the exec is launched.
func ExecDetached(ctx context.Context, name string, args ...string) error {
	full := append([]string{"exec", "-d", name}, args...)
	if _, err := proc.Run(ctx, Bin, full...); err != nil {
		return fmt.Errorf("container exec -d %s: %w", name, err)
	}
	return nil
}

// DockerPort returns the host-published loopback port for a container running in
// the guest's Docker (via `docker port <name>`). Aspire's resource service
// reports a DCP proxy port that doesn't forward raw TCP through a bridge, so for
// container-backed resources shunt targets the real docker-published port. The
// container name matches the Aspire resource name. Returns 0 if there's no
// container (e.g. a project/process resource).
func DockerPort(ctx context.Context, guest, containerName string) int {
	out, err := Exec(ctx, guest, "docker", "port", containerName)
	if err != nil {
		return 0
	}
	// Lines look like: "6379/tcp -> 127.0.0.1:32768".
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "127.0.0.1:") {
			continue
		}
		if i := strings.LastIndex(line, ":"); i >= 0 {
			if p, err := strconv.Atoi(strings.TrimSpace(line[i+1:])); err == nil {
				return p
			}
		}
	}
	return 0
}

// Bridge starts an in-guest socat that exposes a loopback port (intPort) at the
// guest's external interface (extPort), so the host can reach it at <guest-ip>:<extPort>.
// Aspire binds the resource service and app/dep endpoints to loopback, so this
// is how shunt makes them reachable for discovery and proxying.
//
// bindIP pins socat's listen address. Pass the guest IP so the bridge can reuse
// the app's exact port number (extPort == intPort, host == guest) without
// colliding with the app's own 127.0.0.1:<port> — binding the guest IP and
// loopback are separate addresses. Pass "" to listen on all interfaces (used for
// shunt-internal bridges like the resource service, where extPort != intPort).
//
// On a busy guest (e.g. SQL Server starting under Rosetta) a detached exec can
// fail to take, so this relaunches socat until the port is actually listening.
func Bridge(ctx context.Context, name, bindIP string, extPort, intPort int) error {
	listen := fmt.Sprintf("TCP-LISTEN:%d,fork,reuseaddr", extPort)
	probe := "127.0.0.1"
	if bindIP != "" {
		listen = fmt.Sprintf("TCP-LISTEN:%d,bind=%s,fork,reuseaddr", extPort, bindIP)
		probe = bindIP
	}
	spec := fmt.Sprintf("socat %s TCP:127.0.0.1:%d", listen, intPort)
	// Confirm the bridge by actually connecting to it — not by pgrep, which
	// matches the launching `sh -c 'socat …'` wrapper before socat has bound the
	// port (a false positive that left discovery dialing a dead bridge).
	listening := fmt.Sprintf("socat -T1 /dev/null TCP:%s:%d 2>/dev/null && echo up", probe, extPort)
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		if out, _ := Exec(ctx, name, "sh", "-c", listening); strings.Contains(out, "up") {
			return nil
		}
		lastErr = ExecDetached(ctx, name, "sh", "-c", spec)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if out, _ := Exec(ctx, name, "sh", "-c", listening); strings.Contains(out, "up") {
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("bridge port %d->127.0.0.1:%d never came up: %w", extPort, intPort, lastErr)
	}
	return fmt.Errorf("bridge port %d->127.0.0.1:%d never came up", extPort, intPort)
}

// Stop stops a running guest. Mutation errors are accepted only when an exact
// typed observation proves the named guest is already stopped or absent.
func Stop(ctx context.Context, name string) error {
	result, err := proc.Run(ctx, Bin, "stop", name)
	if err != nil {
		observation := ObserveGuest(ctx, name)
		if observation.State == GuestStopped || observation.State == GuestAbsent {
			return nil
		}
		return &stopCommandError{name: name, diagnostic: result.Stderr, err: err}
	}
	return waitGuestPostcondition(ctx, name, "stop", func(state GuestObservationState) bool {
		return state == GuestStopped || state == GuestAbsent
	})
}

type stopCommandError struct {
	name       string
	diagnostic string
	err        error
}

func (e *stopCommandError) Error() string { return fmt.Sprintf("container stop %s: %v", e.name, e.err) }
func (e *stopCommandError) Unwrap() error { return e.err }

// StopOrForce stops a running guest gracefully, but escapes the one failure a
// graceful stop can't clear: the guest's cgroup.kill wedging with errno 95, where
// `container stop` jams and can never reap the guest. In that case it force-removes
// the guest and reports forced=true — the guest is then *gone* (not merely stopped),
// so callers must recreate it (e.g. `up` self-heals) rather than `start` it. The
// worktree + data clones are host bind mounts, so they survive the force-remove.
//
// A non-cgroup stop failure (timeout, runtime hiccup) is returned as-is with
// forced=false: force is reserved for the wedge, so a transient error never nukes a
// guest the user could have retried.
func StopOrForce(ctx context.Context, name string) (forced bool, err error) {
	serr := Stop(ctx, name)
	if serr == nil {
		return false, nil
	}
	// Match the kernel's cgroup.kill diagnostic specifically, not a bare "cgroup":
	// Stop wraps its error as `container stop <name>: …`, so a bare match would also
	// scan the container name and misfire the force-remove for any siding whose name
	// contains "cgroup". cgroup.kill is the wedge's actual signature.
	var stopErr *stopCommandError
	if !errors.As(serr, &stopErr) || !strings.Contains(strings.ToLower(stopErr.diagnostic), "cgroup.kill") {
		return false, serr
	}
	if _, rerr := proc.Run(ctx, Bin, "rm", "-f", name); rerr != nil {
		if ObserveGuest(ctx, name).State == GuestAbsent {
			return true, nil
		}
		return false, fmt.Errorf("stop %q wedged on cgroup.kill and force-remove failed: %w", name, rerr)
	}
	if err := waitGuestPostcondition(ctx, name, "force-remove", func(state GuestObservationState) bool { return state == GuestAbsent }); err != nil {
		return false, err
	}
	return true, nil
}

// Start boots a stopped guest, re-running its entrypoint (dockerd + dev cert,
// then the idle keep-alive). The guest's own disk and its host bind mounts (the
// worktree + data volumes) persist across stop/start, so nothing is lost.
func Start(ctx context.Context, name string) error {
	if _, err := proc.Run(ctx, Bin, "start", name); err != nil {
		return fmt.Errorf("container start %s: %w", name, err)
	}
	return nil
}

// Remove tears down a guest. It stops first because `container rm` won't remove
// a running guest reliably, then removes (ignoring "not found").
func Remove(ctx context.Context, name string) error {
	if err := Stop(ctx, name); err != nil {
		return err
	}
	if _, err := proc.Run(ctx, Bin, "rm", "-f", name); err != nil {
		if ObserveGuest(ctx, name).State == GuestAbsent {
			return nil
		}
		return fmt.Errorf("container rm %s: %w", name, err)
	}
	return waitGuestPostcondition(ctx, name, "remove", func(state GuestObservationState) bool { return state == GuestAbsent })
}
