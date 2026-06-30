package cmd

import (
	"fmt"
	"strings"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/spf13/cobra"
)

// newRunCmd runs an arbitrary command inside a siding's guest, from the app's
// workdir, with stdio passed through (interactive). The guest-side complement to
// `shunt git` (which runs on the host worktree): use this for migrations, one-off
// dotnet/npm commands, or a shell.
func newRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run <siding> [command...]",
		Short: "Run a command inside a siding's guest (from the app's workdir)",
		Long: "Executes a command in the siding's running guest, from /workspace (or the\n" +
			"contract's `workdir`), with stdio passed through. No command drops you into a\n" +
			"shell. Examples:\n" +
			"  shunt run exp1 dotnet ef migrations add Init -p src/Db\n" +
			"  shunt run exp1                # interactive shell in the guest",
		// Pass everything after the siding straight to the guest command (incl. its
		// own flags like `--version`), so don't let cobra parse them.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
				return cmd.Help()
			}
			ctx := cmd.Context()
			name := args[0]
			rest := args[1:]
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			sd, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q — `%s ls`", name, bin())
			}
			wd := "/workspace"
			if app.Workdir != "" {
				wd = "/workspace/" + app.Workdir
			}
			script := "cd " + wd
			if len(rest) > 0 {
				script += " && " + strings.Join(rest, " ")
			} else {
				script += " && exec ${SHELL:-bash}" // bare run → interactive shell
			}
			// `exec -i` so stdin (and the command's own flags) pass through.
			return proc.RunPassthrough(ctx, container.Bin, "exec", "-i", sd.Container, "sh", "-lc", script)
		},
	}
}
