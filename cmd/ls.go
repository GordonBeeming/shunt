package cmd

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List registered apps and their sidings",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			reg, err := state.LoadRegistry()
			if err != nil {
				return err
			}
			if len(reg.Projects) == 0 {
				fmt.Println("no apps registered yet — run `"+bin()+" app add` in an Aspire repo")
				return nil
			}
			names := make([]string, 0, len(reg.Projects))
			for n := range reg.Projects {
				names = append(names, n)
			}
			sort.Strings(names)

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
}
