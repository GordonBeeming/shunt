// Package portcheck names what is listening on a local port.
//
// It exists for one message: a front-door claim that fails with "address
// already in use" tells you which port it wanted and nothing about what has it.
// Finding that out by hand is a detour through lsof at exactly the moment
// someone is trying to get their environment back.
package portcheck

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/proc"
)

const lookupTimeout = 3 * time.Second

// Holder describes the processes listening on port, as "name (pid N)" entries.
//
// It answers "" rather than failing. This is detail added to an error that is
// already being reported, and losing it must never replace that error with one
// about the lookup.
func Holder(ctx context.Context, port int) string {
	if port <= 0 {
		return ""
	}
	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	result, err := proc.Run(lookupCtx, "lsof", "-nP", fmt.Sprintf("-iTCP:%d", port), "-sTCP:LISTEN")
	if err != nil && result.Stdout == "" {
		return ""
	}
	return describe(result.Stdout, port)
}

// describe turns lsof's table into a short list of distinct holders.
//
// Only lines naming the port are read. lsof writes warnings to stdout on a host
// with an unreadable mount, and those lines would otherwise be parsed as
// processes.
func describe(output string, port int) string {
	suffix := fmt.Sprintf(":%d", port)
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 9 || fields[0] == "COMMAND" {
			continue
		}
		named := false
		for _, f := range fields {
			if strings.HasSuffix(f, suffix) {
				named = true
				break
			}
		}
		if !named {
			continue
		}
		seen[fmt.Sprintf("%s (pid %s)", fields[0], fields[1])] = true
	}
	if len(seen) == 0 {
		return ""
	}
	holders := make([]string, 0, len(seen))
	for h := range seen {
		holders = append(holders, h)
	}
	sort.Strings(holders)
	return strings.Join(holders, ", ")
}
