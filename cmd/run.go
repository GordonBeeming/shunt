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

var runGuestCommand = proc.RunPassthrough

// isSiding reports whether name is one of the app's sidings (so `run` can tell a
// leading siding name from the first word of a command).
func isSiding(app state.App, name string) bool {
	_, ok := app.Sidings[name]
	return ok
}

// newRunCmd runs an arbitrary command inside a siding's guest, from the app's
// workdir, with stdio passed through (interactive). The guest-side complement to
// `shunt git` (which runs on the host worktree): use this for migrations, one-off
// dotnet/npm commands, or a shell.
func newRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run [siding] [command...]",
		Short: "Run a command inside a siding's guest (from the app's workdir)",
		Long: "Executes a command in a siding's running guest, from /workspace (or the\n" +
			"contract's `workdir`), with stdio passed through. No command drops you into a\n" +
			"shell.\n\n" +
			"Which siding: the one your cwd is inside (a siding's `src`), else a leading\n" +
			"argument that names a siding, else the live siding. Examples:\n" +
			"  shunt run dotnet ef migrations add Init    # in the live (or cwd) siding\n" +
			"  shunt run exp1 dotnet --version            # explicitly in siding exp1\n" +
			"  shunt run                                  # interactive shell in the guest",
		// Pass everything after the siding straight to the guest command (incl. its
		// own flags like `--version`), so don't let cobra parse them.
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
			if err := ensureNoRemovalInProgress(app, "run"); err != nil {
				return err
			}
			// Resolve the siding the way the cwd implies: if you're inside a siding's
			// worktree, that's the one (every arg is the command); otherwise a leading
			// arg that names a siding wins; otherwise fall back to the live siding.
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
			return runInSiding(ctx, app.ConfigDir, name, rest, !explicit && loc.Siding == "")
		},
	}
}

func runInSiding(ctx context.Context, configDir, name string, command []string, announce bool) error {
	return withLatestSiding(ctx, configDir, name, "run", func(app state.App, sd state.Siding) error {
		if err := siding.RequireGuest(sd); err != nil {
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
		execArgs = append(execArgs, sd.Container, "sh", "-c")
		if len(command) > 0 {
			execArgs = append(execArgs, `cd "$1" && shift && exec "$@"`, "sh", wd)
			execArgs = append(execArgs, command...)
		} else {
			execArgs = append(execArgs, `cd "$1" && exec "${SHELL:-bash}"`, "sh", wd)
		}
		return runGuestCommand(ctx, container.Bin, execArgs...)
	})
}
