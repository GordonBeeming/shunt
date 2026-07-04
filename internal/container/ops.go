package container

import (
	"context"
	"encoding/json"
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

func inspect(ctx context.Context, name string) (inspectDoc, error) {
	res, err := proc.Run(ctx, Bin, "inspect", name)
	if err != nil {
		return inspectDoc{}, fmt.Errorf("container inspect %s: %w", name, err)
	}
	var docs []inspectDoc
	if err := json.Unmarshal([]byte(res.Stdout), &docs); err != nil {
		return inspectDoc{}, fmt.Errorf("parse inspect %s: %w", name, err)
	}
	if len(docs) == 0 {
		return inspectDoc{}, fmt.Errorf("container %s not found", name)
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
	full := append([]string{"exec", "-i", name}, args...)
	if err := proc.RunStdin(ctx, stdinPath, Bin, full...); err != nil {
		return fmt.Errorf("container exec -i %s: %w", name, err)
	}
	return nil
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

// isAbsentErr reports whether a `container` CLI error just means the guest isn't
// there / isn't running — which Stop and Remove treat as success (the desired end
// state already holds), so Recreate doesn't fail on a first-time guest.
func isAbsentErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not found") ||
		strings.Contains(s, "no such") ||
		strings.Contains(s, "not running") ||
		strings.Contains(s, "not started") ||
		strings.Contains(s, "does not exist")
}

// Stop stops a running guest (ignores "not running" / absent).
func Stop(ctx context.Context, name string) error {
	if _, err := proc.Run(ctx, Bin, "stop", name); err != nil {
		if isAbsentErr(err) {
			return nil
		}
		return fmt.Errorf("container stop %s: %w", name, err)
	}
	return nil
}

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
	if !strings.Contains(serr.Error(), "cgroup") {
		return false, serr
	}
	if _, rerr := proc.Run(ctx, Bin, "rm", "-f", name); rerr != nil {
		if isAbsentErr(rerr) {
			return true, nil
		}
		return false, fmt.Errorf("stop wedged on cgroup and force-remove failed: %w", rerr)
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
	_, _ = proc.Run(ctx, Bin, "stop", name)
	if _, err := proc.Run(ctx, Bin, "rm", "-f", name); err != nil {
		if isAbsentErr(err) {
			return nil
		}
		return fmt.Errorf("container rm %s: %w", name, err)
	}
	return nil
}
