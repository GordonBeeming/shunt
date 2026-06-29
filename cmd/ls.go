package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List registered apps and their sidings",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := state.LoadRegistry()
			if err != nil {
				return err
			}
			if len(reg.Projects) == 0 {
				fmt.Println("no apps registered yet — run `shunt app add` in an Aspire repo")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "APP\tSIDING\tLIVE\tBRANCH\tIP\tCLONE")
			for name, dir := range reg.Projects {
				app, err := state.LoadApp(dir)
				if err != nil {
					fmt.Fprintf(w, "%s\t(unreadable: %v)\t\t\t\t%s\n", name, err, dir)
					continue
				}
				if len(app.Sidings) == 0 {
					fmt.Fprintf(w, "%s\t-\t\t\t\t%s\n", app.Name, dir)
					continue
				}
				for sName, s := range app.Sidings {
					live := ""
					if app.LiveSiding == sName {
						live = "*"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
						app.Name, sName, live, s.Branch, s.LastIP, dir)
				}
			}
			return w.Flush()
		},
	}
}
