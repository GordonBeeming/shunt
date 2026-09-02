package container

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/config"
)

// ExecStalledError reports a guest whose exec path did not answer inside the
// probe deadline. It is distinct from an exec that ran and failed: the guest may
// be perfectly healthy, so a caller must not treat it as a reason to restart or
// recreate anything.
type ExecStalledError struct {
	Guest    string
	Timeout  time.Duration
	Attached []HostProcess
}

func (e *ExecStalledError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "exec into guest %q did not answer within %s. The guest itself may still be healthy — "+
		"check by hand with `container exec %s true`.", e.Guest, e.Timeout, e.Guest)
	if len(e.Attached) == 0 {
		b.WriteString(" No host process is currently attached to this guest's exec path.")
	} else {
		b.WriteString(" Host processes attached to this guest's exec path (evidence to look at, not a proven cause):")
		for _, p := range e.Attached {
			fmt.Fprintf(&b, "\n  pid %d: %s", p.PID, p.Command)
		}
	}
	fmt.Fprintf(&b, "\nRecovery order: end the process(es) above first, then run `%s restart %s` — it reaps the "+
		"app's processes without dropping the guest. Only consider recreating the guest if that still doesn't clear it.",
		config.Current().BinaryName, sidingNameHint(e.Guest))
	return b.String()
}

// HostProcess is a host process still attached to a guest's exec path.
type HostProcess struct {
	PID     int
	Command string
}

// attachedExecProbe is the seam over host-process discovery, so ExecBounded's
// stall path can be tested without shelling out to the real `ps`.
var attachedExecProbe = discoverAttachedExecs

// ExecBounded runs a short guest command under a deadline of its own, added to
// whatever the caller's context already carries rather than replacing it: the
// caller can still cancel earlier, but a caller with no deadline can no longer
// wait forever. It reports a stall as ExecStalledError. Only a
// genuinely short, idempotent probe belongs here — Exec stays unbounded for
// slow, legitimate work such as image loads and the app start/stop scripts.
func ExecBounded(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	boundCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := Exec(boundCtx, name, args...)
	if err == nil {
		return out, nil
	}
	// The bound's own deadline tripped, not the caller's — that's a stall the
	// caller doesn't already know how to interpret, as opposed to a normal exec
	// failure (bad command, guest genuinely refusing) or the caller giving up.
	if boundCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return out, &ExecStalledError{Guest: name, Timeout: timeout, Attached: attachedExecProbe(name)}
	}
	return out, err
}

// sidingNameHint best-effort derives the short siding name from a full guest
// container name ("<prefix>_<app>_<siding>"), for a human-actionable recovery
// hint. Falls back to the full name so the hint is never empty.
func sidingNameHint(guest string) string {
	if i := strings.LastIndex(guest, "_"); i >= 0 && i+1 < len(guest) {
		return guest[i+1:]
	}
	return guest
}

// discoverAttachedExecs is a best-effort look at host processes still attached
// to guest's exec path. Discovery failure must never mask the stall it exists
// to explain, so any error here just yields an empty list.
func discoverAttachedExecs(guest string) []HostProcess {
	out, err := exec.Command("ps", "-ax", "-o", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	return parseAttachedExecs(string(out), guest)
}

// parseAttachedExecs extracts host processes from `ps -ax -o pid=,command=`
// output that are `container exec …` sessions targeting guest.
func parseAttachedExecs(psOutput, guest string) []HostProcess {
	var procs []HostProcess
	for _, line := range strings.Split(psOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		command := strings.TrimSpace(fields[1])
		if !execCommandTargetsGuest(command, guest) {
			continue
		}
		procs = append(procs, HostProcess{PID: pid, Command: command})
	}
	return procs
}

// execCommandTargetsGuest reports whether command is a `container exec …` line
// whose target is guest. The guest is matched only in the target position, not
// anywhere on the line: `container exec other printf my-guest` names "other",
// and treating it as a match would blame an unrelated process for a stall and
// advise ending it.
//
// shunt only ever emits `exec <name>` or `exec -i <name>` (see ops.go), so the
// first non-flag token after the subcommand is the target.
func execCommandTargetsGuest(command, guest string) bool {
	fields := strings.Fields(command)
	for i, field := range fields {
		if filepath.Base(field) != Bin {
			continue
		}
		rest := fields[i+1:]
		if len(rest) == 0 || rest[0] != "exec" {
			continue
		}
		for _, arg := range rest[1:] {
			if strings.HasPrefix(arg, "-") {
				continue
			}
			return arg == guest // the target position, and nothing after it
		}
	}
	return false
}
