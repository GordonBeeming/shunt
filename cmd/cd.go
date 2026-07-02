package cmd

import (
	"fmt"

	"github.com/gordonbeeming/shunt/internal/siding"
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
			"Siding is taken from the name arg, else the one your cwd is inside, else the live one.",
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
				return fmt.Errorf("which siding? pass a name or make one live (`%s ls`)", bin())
			}
			if _, ok := app.Sidings[name]; !ok {
				return fmt.Errorf("no siding %q in %q", name, app.Name)
			}
			src, _ := siding.Paths(app, name)
			fmt.Println(src)
			return nil
		},
	}
}
