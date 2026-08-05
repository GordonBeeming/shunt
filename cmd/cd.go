package cmd

import (
	"fmt"
	"os"

	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

// newCdCmd prints a siding's host worktree src directory so you can jump into it.
// A child process can't change its parent shell's directory, so this prints the
// path rather than cd-ing itself — pair it with the shell: `cd "$(shunt cd <name>)"`.
func newCdCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cd [name]",
		Short: "Print a siding's worktree src path (use `cd \"$(" + bin() + " cd <name>)\"`)",
		Long: "Resolves a siding and prints its host worktree src directory to stdout, so you can\n" +
			"jump into it. A process can't cd its parent shell, so this prints the path:\n\n" +
			"  cd \"$(" + bin() + " cd <name>)\"\n\n" +
			"Handy as a shell function: `scd() { cd \"$(" + bin() + " cd \"$@\")\"; }`.\n" +
			"Siding is taken from the name arg, else the one your cwd is inside, else the live\n" +
			"target. `host` (passed explicitly, or when it's the live target) prints the original\n" +
			"repo checkout rather than a siding.",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, loc, err := loadCurrentApp()
			if err != nil {
				return err
			}
			name := ""
			switch {
			case len(args) == 1:
				name = args[0]
			case loc.Siding != "":
				name = loc.Siding
			default:
				name = app.LiveSiding
			}
			if name == "" {
				return fmt.Errorf("which siding? pass a name, or make one live with `%s switch`", bin())
			}
			// "host" isn't a siding — it's the original repo checkout.
			dir := app.RepoPath
			if name != state.HostTarget {
				if _, ok := app.Sidings[name]; !ok {
					return fmt.Errorf("no siding %q in %q", name, app.Name)
				}
				var pathErr error
				dir, _, pathErr = siding.Paths(app, name)
				if pathErr != nil {
					return pathErr
				}
			}
			// Guard a missing worktree (siding metadata without its checkout) so
			// `cd "$(shunt cd …)"` never lands in a nonexistent path.
			if _, err := os.Stat(dir); err != nil {
				return fmt.Errorf("path not found at %s: %w", dir, err)
			}
			fmt.Println(dir)
			return nil
		},
	}
}
