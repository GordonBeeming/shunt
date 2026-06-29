package container

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

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
func Bridge(ctx context.Context, name string, extPort, intPort int) error {
	spec := fmt.Sprintf("socat TCP-LISTEN:%d,fork,reuseaddr TCP:127.0.0.1:%d", extPort, intPort)
	return ExecDetached(ctx, name, "sh", "-c", spec)
}

// Stop stops a running guest (ignores "not running").
func Stop(ctx context.Context, name string) error {
	if _, err := proc.Run(ctx, Bin, "stop", name); err != nil {
		return fmt.Errorf("container stop %s: %w", name, err)
	}
	return nil
}

// Remove tears down a guest. It stops first because `container rm` won't remove
// a running guest reliably, then removes (ignoring "not found").
func Remove(ctx context.Context, name string) error {
	_, _ = proc.Run(ctx, Bin, "stop", name)
	if _, err := proc.Run(ctx, Bin, "rm", "-f", name); err != nil {
		return fmt.Errorf("container rm %s: %w", name, err)
	}
	return nil
}
