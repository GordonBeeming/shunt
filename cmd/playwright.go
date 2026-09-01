package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var runPlaywrightCommand = proc.RunPassthrough

const (
	// pwOutputDirEnv is the env var @playwright/cli@0.1.9's bundled playwright-core reads
	// (envToString(e.PLAYWRIGHT_MCP_OUTPUT_DIR)) to redirect auto-named output — snapshots,
	// traces, and video saved without an explicit --filename — under a directory of our
	// choosing. A command given its own explicit filename still resolves it relative to the
	// guest's cwd, not this dir, so callers wanting everything under /out should pass an
	// explicit /out/... path too.
	pwOutputDirEnv = "PLAYWRIGHT_MCP_OUTPUT_DIR"

	// pwOutputDir is where auto-named playwright-cli output lands in the guest. /out is the
	// standing per-siding host bind mount, so it survives past the exec'd process and is
	// reachable from the host at <siding>/out/playwright.
	pwOutputDir = "/out/playwright"

	// pwSandboxEnv, set false, is playwright-core's env-based route to `--no-sandbox` on the
	// launched Chromium (envToBoolean(e.PLAYWRIGHT_MCP_SANDBOX) → chromiumSandbox). Guests run
	// as root, where Chromium's own sandbox can't set up its usual namespaces, so this is
	// required for headless launch to succeed at all — it's not merely a hardening nicety.
	// `--disable-dev-shm-usage` needs no equivalent: playwright-core bakes it into every
	// Chromium launch unconditionally.
	pwSandboxEnv = "PLAYWRIGHT_MCP_SANDBOX"
)

// newPlaywrightCmd proxies playwright-cli commands into a siding's guest, so a browser
// session drives the app on its own in-guest localhost ports. That keeps auth redirects,
// cookies, and CORS self-consistent with what the app itself thinks its origin is — a host
// port forwarded in from outside the guest wouldn't match.
func newPlaywrightCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "playwright [siding] [args...]",
		Short: "Run playwright-cli inside a siding's guest",
		Long: "Execs `playwright-cli <args>` in a siding's running guest, from /workspace (or\n" +
			"the contract's `workdir`), with stdio passed through.\n\n" +
			"The guest's app URLs are its own `https://localhost:<port>`s — the same ones\n" +
			"the app serves inside that siding, not a host-forwarded port. Auto-named\n" +
			"output (snapshots, traces, video saved without --filename) lands under\n" +
			"/out/playwright in the guest, which is `<siding>/out/playwright` on the host.\n\n" +
			"Which siding: the one your cwd is inside (a siding's `src`), else a leading\n" +
			"argument that names a siding, else the live siding. Examples:\n" +
			"  shunt playwright open https://localhost:8080   # in the live (or cwd) siding\n" +
			"  shunt playwright exp1 snapshot                 # explicitly in siding exp1\n" +
			"  shunt playwright close",
		// Pass everything after the siding straight to playwright-cli (incl. its own
		// flags like `--json`), so don't let cobra parse them.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			ctx := cmd.Context()
			app, loc, err := loadCurrentApp()
			if err != nil {
				return err
			}
			if err := ensureNoRemovalInProgress(app, "run playwright"); err != nil {
				return err
			}
			// Resolve the siding the way the cwd implies: if you're inside a siding's
			// worktree, that's the one (every arg is a playwright-cli arg); otherwise a
			// leading arg that names a siding wins; otherwise fall back to the live siding.
			var name string
			rest := args
			explicit := false
			switch {
			case loc.Siding != "":
				name = loc.Siding
			case len(args) > 0 && isSiding(app, args[0]):
				name, rest, explicit = args[0], args[1:], true
			case app.LiveSiding != "" && app.LiveSiding != state.HostTarget:
				name = app.LiveSiding
			default:
				if name, err = pickSiding(ctx, app); err != nil {
					return err
				}
			}
			return runPlaywrightInSiding(ctx, app.ConfigDir, name, rest, !explicit && loc.Siding == "")
		},
	}
}

func runPlaywrightInSiding(ctx context.Context, configDir, name string, args []string, announce bool) error {
	return withLatestSiding(ctx, configDir, name, "run playwright", func(app state.App, sd state.Siding) error {
		if err := siding.RequireGuest(sd); err != nil {
			return err
		}
		// A guest can list as running while its exec path refuses every command,
		// and it stays that way until something restarts it. EnsureGuestLive is
		// the same recovery `up` performs, so an ad-hoc command in the guest
		// heals it instead of reporting the runtime's raw refusal.
		if err := siding.EnsureGuestLive(ctx, sd); err != nil {
			return err
		}
		if announce {
			fmt.Fprintf(os.Stderr, "• in siding %q\n", name)
		}
		wd := "/workspace"
		if app.Workdir != "" {
			wd = "/workspace/" + app.Workdir
		}
		execArgs := []string{"exec", "-i"}
		if term.IsTerminal(int(os.Stdin.Fd())) {
			execArgs = append(execArgs, "-t")
		}
		script := fmt.Sprintf(`cd "$1" && shift && exec env %s=%s %s=false playwright-cli "$@"`, pwOutputDirEnv, pwOutputDir, pwSandboxEnv)
		execArgs = append(execArgs, sd.Container, "sh", "-c", script, "sh", wd)
		execArgs = append(execArgs, args...)
		return runPlaywrightCommand(ctx, container.Bin, execArgs...)
	})
}
