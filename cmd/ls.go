package cmd

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/resolve"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newLsCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "ls",
		Short: "List sidings for the current project (use -a for every project on the host)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			reg, err := state.LoadRegistry()
			if err != nil {
				return err
			}
			if len(reg.Projects) == 0 {
				fmt.Println("no apps registered yet — run `" + bin() + " app add` in a repo")
				return nil
			}

			var names []string
			if all {
				for n := range reg.Projects {
					names = append(names, n)
				}
				sort.Strings(names)
			} else {
				loc, err := resolve.FromCwd()
				if err != nil {
					return err
				}
				if _, ok := reg.Projects[loc.Project]; !ok {
					fmt.Printf("%q isn't a shunt app here — cd into a registered repo, or `%s ls -a` for all projects\n", loc.Project, bin())
					return nil
				}
				names = []string{loc.Project}
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "APP\tSIDING\tLIVE\tGUEST\tIP\tDASHBOARD")
			for _, n := range names {
				app, err := state.LoadApp(reg.Projects[n])
				if err != nil {
					fmt.Fprintf(w, "%s\t(unreadable)\t\t\t\t\n", n)
					continue
				}
				if len(app.Sidings) == 0 {
					fmt.Fprintf(w, "%s\t-\t\t\t\t\n", app.Name)
					continue
				}
				sidingNames := make([]string, 0, len(app.Sidings))
				for sn := range app.Sidings {
					sidingNames = append(sidingNames, sn)
				}
				sort.Strings(sidingNames)
				for _, sn := range sidingNames {
					s := app.Sidings[sn]
					live := ""
					if app.LiveSiding == sn {
						live = "*"
					}
					guestState, err := container.State(ctx, s.Container)
					if err != nil {
						guestState = "-"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
						app.Name, sn, live, guestState, s.LastIP, siding.DashboardURL(s))
				}
			}
			return w.Flush()
		},
	}
	c.Flags().BoolVarP(&all, "all", "a", false, "list sidings for every project on the host, not just the current one")
	return c
}
