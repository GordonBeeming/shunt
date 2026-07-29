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

type lsSiding struct {
	Name      string `json:"name"`
	Live      bool   `json:"live"`
	Status    string `json:"status"` // live / up / idle / stopped (see sidingStatus)
	Guest     string `json:"guest"`
	IP        string `json:"ip,omitempty"`
	Dashboard string `json:"dashboard,omitempty"`
	Src       string `json:"src,omitempty"`
}

type lsApp struct {
	Name    string     `json:"name"`
	Sidings []lsSiding `json:"sidings"`
}

func newLsCmd() *cobra.Command {
	var all, asJSON bool
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
				if asJSON {
					return printJSON([]lsApp{})
				}
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
				canonical, _, ok := reg.FindProject(loc.Project)
				if !ok {
					if asJSON {
						return printJSON([]lsApp{})
					}
					fmt.Printf("%q isn't a shunt app here — cd into a registered repo, or `%s ls -a` for all projects\n", loc.Project, bin())
					return nil
				}
				names = []string{canonical}
			}

			apps := make([]lsApp, 0, len(names))
			for _, n := range names {
				app, err := state.LoadApp(reg.Projects[n])
				if err != nil {
					apps = append(apps, lsApp{Name: n})
					continue
				}
				la := lsApp{Name: app.Name, Sidings: []lsSiding{}}
				// The host (your local copy) is a switch target too — list it first.
				hostLive := app.LiveSiding == state.HostTarget
				hostStatus := "-"
				if hostLive {
					hostStatus = "live"
				}
				la.Sidings = append(la.Sidings, lsSiding{
					Name:   state.HostTarget,
					Live:   hostLive,
					Status: hostStatus,
					Guest:  "local",
				})
				sidingNames := make([]string, 0, len(app.Sidings))
				for sn := range app.Sidings {
					sidingNames = append(sidingNames, sn)
				}
				sort.Strings(sidingNames)
				for _, sn := range sidingNames {
					s := app.Sidings[sn]
					guestState, err := container.State(ctx, s.Container)
					if err != nil {
						guestState = "-"
					}
					src, _, err := siding.Paths(app, sn)
					if err != nil {
						return err
					}
					la.Sidings = append(la.Sidings, lsSiding{
						Name:      sn,
						Live:      app.LiveSiding == sn,
						Status:    sidingStatus(app, sn, s, guestState),
						Guest:     guestState,
						IP:        s.LastIP,
						Dashboard: siding.DashboardURL(app, s),
						Src:       src,
					})
				}
				apps = append(apps, la)
			}

			if asJSON {
				return printJSON(apps)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "APP\tSIDING\tSTATUS\tGUEST\tIP\tDASHBOARD")
			for _, a := range apps {
				if len(a.Sidings) == 0 {
					fmt.Fprintf(w, "%s\t-\t\t\t\t\n", a.Name)
					continue
				}
				for _, s := range a.Sidings {
					// Plain text (no colour): tabwriter counts ANSI bytes as width
					// and would misalign the columns.
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
						a.Name, s.Name, s.Status, s.Guest, s.IP, s.Dashboard)
				}
			}
			return w.Flush()
		},
	}
	c.Flags().BoolVarP(&all, "all", "a", false, "list sidings for every project on the host, not just the current one")
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}
