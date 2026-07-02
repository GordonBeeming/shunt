package cmd

import (
	"fmt"
	"os"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

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
				if name, err = pickSiding(ctx, app, false); err != nil { // run needs a guest, not the host
					return err
				}
			}
			sd, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q — `%s ls`", name, bin())
			}
			if !explicit && loc.Siding == "" {
				fmt.Fprintf(os.Stderr, "• in siding %q\n", name) // say where it ran, since it wasn't named
			}
			wd := "/workspace"
			if app.Workdir != "" {
				wd = "/workspace/" + app.Workdir
			}
			// Pass the workdir + command as positional params to sh so each argument
			// keeps its exact boundaries/quoting (flattening with strings.Join would
			// mangle spaces, quotes, and flags). `exec -i` passes stdin through.
			// Allocate a pseudo-TTY when stdin is a real terminal, so interactive
			// guest commands work — a shell's line editing, and CLIs that show a
			// selection prompt (e.g. `aspire stop` with several AppHosts) which
			// otherwise fail with "the current terminal isn't interactive".
			execArgs := []string{"exec", "-i"}
			if term.IsTerminal(int(os.Stdin.Fd())) {
				execArgs = append(execArgs, "-t")
			}
			execArgs = append(execArgs, sd.Container, "sh", "-c")
			if len(rest) > 0 {
				execArgs = append(execArgs, `cd "$1" && shift && exec "$@"`, "sh", wd)
				execArgs = append(execArgs, rest...)
			} else {
				execArgs = append(execArgs, `cd "$1" && exec "${SHELL:-bash}"`, "sh", wd) // bare run → interactive shell
			}
			return proc.RunPassthrough(ctx, container.Bin, execArgs...)
		},
	}
}
