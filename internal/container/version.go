package container

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/proc"
)

// MinimumVersion is the oldest Apple container CLI that accepts
// --read-only-path. Every guest shunt creates sets WritableProcSys, which emits
// that flag so dockerd inside the guest can write /proc/sys, so a CLI below this
// cannot create a guest at all. Older releases reject the whole `container run`
// with exit 64 and a bare "Unknown option", which reads as guest corruption
// rather than as a host tooling floor.
const MinimumVersion = "1.2.0"

// testedVersion is the release these guests are actually exercised against. The
// floor above is the technical requirement; this is the recommendation, and the
// two are deliberately different so a working setup is never refused for being
// merely untested.
const testedVersion = "1.2.2"

const versionProbeTimeout = 5 * time.Second

// versionPattern pulls X.Y.Z out of "container CLI version 1.2.2 (build:
// release, commit: ee848e3)". Only the first three numeric components are read;
// a build or commit suffix is deliberately ignored.
var versionPattern = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// UnsupportedVersionError reports a container CLI too old to create a guest. It
// carries both versions so a caller can render them without re-probing.
type UnsupportedVersionError struct {
	Found    string
	Required string
}

func (e *UnsupportedVersionError) Error() string {
	return fmt.Sprintf(`Apple container %s or newer is required to create a guest; found %s.
shunt passes --read-only-path, which arrived in %s, so dockerd inside the guest
can write /proc/sys.
Upgrade with the signed installer from the apple/container releases, then run
`+"`container system start`"+`. %s is the version these guests are tested on.
`+"`reapply`"+` and `+"`rm`"+` will not help: this is the host CLI, not the siding.`,
		e.Required, e.Found, e.Required, testedVersion)
}

// Version reports the container CLI's semantic version, e.g. "1.2.2". It
// returns ok=false when the CLI is absent or its output carries no recognizable
// version, which callers treat as unknown rather than as too old.
func Version(ctx context.Context) (version string, ok bool) {
	return probeVersion(ctx, proc.Look, proc.Run)
}

func probeVersion(ctx context.Context, look func(string) bool, run systemCommandRunner) (string, bool) {
	if !look(Bin) {
		return "", false
	}
	probeCtx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()
	result, err := run(probeCtx, Bin, "--version")
	if err != nil {
		return "", false
	}
	// The version can land on either stream depending on the release, so read
	// both rather than assuming stdout.
	match := versionPattern.FindString(strings.Join(nonEmptyStrings(result.Stdout, result.Stderr), " "))
	if match == "" {
		return "", false
	}
	return match, true
}

// CheckMinimum fails when the container CLI is definitively older than
// MinimumVersion.
//
// An absent CLI or an unparseable version is NOT a failure. shunt cannot create
// a guest without the runtime anyway, and that path already reports itself
// clearly; refusing every guest because a future release renamed its version
// string would be a worse bug than the one this guards. Unknown means proceed.
func CheckMinimum(ctx context.Context) error {
	return checkMinimum(ctx, proc.Look, proc.Run)
}

func checkMinimum(ctx context.Context, look func(string) bool, run systemCommandRunner) error {
	found, ok := probeVersion(ctx, look, run)
	if !ok {
		return nil
	}
	if compareVersions(found, MinimumVersion) >= 0 {
		return nil
	}
	return &UnsupportedVersionError{Found: found, Required: MinimumVersion}
}

// compareVersions orders two dotted numeric versions, returning -1, 0 or 1. A
// component that will not parse sorts as 0, which keeps a malformed input from
// comparing greater than a real one.
func compareVersions(a, b string) int {
	aParts, bParts := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(aParts) || i < len(bParts); i++ {
		if diff := versionComponent(aParts, i) - versionComponent(bParts, i); diff != 0 {
			if diff < 0 {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionComponent(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, err := strconv.Atoi(parts[i])
	if err != nil {
		return 0
	}
	return n
}
