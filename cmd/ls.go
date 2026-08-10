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
	Name          string `json:"name"`
	Live          bool   `json:"live"`
	Status        string `json:"status"` // Deprecated compatibility: live / up / idle / stopped.
	Guest         string `json:"guest"`  // Deprecated compatibility: running / stopped.
	Phase         string `json:"phase"`
	Runtime       string `json:"runtime"`
	RuntimeDetail string `json:"runtimeDetail,omitempty"`
	IP            string `json:"ip,omitempty"`
	Dashboard     string `json:"dashboard,omitempty"`
	Src           string `json:"src,omitempty"`
}

type lsApp struct {
	SchemaVersion int        `json:"schemaVersion"`
	Name          string     `json:"name"`
	Sidings       []lsSiding `json:"sidings"`
}

const lsSchemaVersion = 2

func newLsCmd() *cobra.Command {
	var all, asJSON bool
	c := &cobra.Command{
		Use:   "ls",
		Short: "List sidings for the current project (use -a for every registered project)",
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
			var runtimeObservation *container.RuntimeObservation
			for _, n := range names {
				app, err := state.LoadApp(reg.Projects[n])
				if err != nil {
					apps = append(apps, lsApp{SchemaVersion: lsSchemaVersion, Name: n})
					continue
				}
				la := lsApp{SchemaVersion: lsSchemaVersion, Name: app.Name, Sidings: []lsSiding{}}
				if app.LiveSiding == state.HostTarget {
					app.LiveSiding = ""
				}
				sidingNames := make([]string, 0, len(app.Sidings))
				for sn := range app.Sidings {
					sidingNames = append(sidingNames, sn)
				}
				sort.Strings(sidingNames)
				for _, sn := range sidingNames {
					s := app.Sidings[sn]
					phase := effectiveLsPhase(s)
					system := container.RuntimeObservation{State: container.RuntimeRunning}
					guest := container.GuestObservation{State: container.GuestAbsent}
					if phase == state.PhaseGuest {
						if runtimeObservation == nil {
							observed := container.ObserveSystem(ctx)
							runtimeObservation = &observed
						}
						system = *runtimeObservation
						if system.State == container.RuntimeRunning {
							guest = container.ObserveGuest(ctx, s.Container)
						}
					}
					runtime, runtimeDetail, guestState := classifyLsRuntime(phase, system, guest)
					src, _, err := siding.Paths(app, sn)
					if err != nil {
						return err
					}
					la.Sidings = append(la.Sidings, lsSiding{
						Name:          sn,
						Live:          app.LiveSiding == sn,
						Status:        compatibilityLsStatus(app, sn, s, guestState),
						Guest:         guestState,
						Phase:         string(phase),
						Runtime:       runtime,
						RuntimeDetail: runtimeDetail,
						IP:            s.LastIP,
						Dashboard:     siding.DashboardURL(app, s),
						Src:           src,
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
	c.Flags().BoolVarP(&all, "all", "a", false, "list sidings for every registered project, not just the current one")
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}

func classifyLsRuntime(phase state.MaterializationPhase, system container.RuntimeObservation, guest container.GuestObservation) (runtime, detail, legacyGuest string) {
	if phase != state.PhaseGuest {
		return "missing", "no guest is materialized for this phase", "stopped"
	}
	switch system.State {
	case container.RuntimeStopped:
		return "stopped", system.Detail, "stopped"
	case container.RuntimeUnavailable:
		return "runtime-unavailable", system.Detail, "stopped"
	}
	switch guest.State {
	case container.GuestRunning:
		return "running", "", "running"
	case container.GuestStopped:
		return "stopped", "", "stopped"
	case container.GuestAbsent:
		return "missing", "guest is not materialized", "stopped"
	default:
		return "runtime-unavailable", "guest inspection unavailable", "stopped"
	}
}

func effectiveLsPhase(s state.Siding) state.MaterializationPhase {
	if s.MaterializationPhase == "" {
		return state.PhaseGuest
	}
	return s.MaterializationPhase
}

func compatibilityLsStatus(app state.App, name string, sd state.Siding, guest string) string {
	if app.LiveSiding == name {
		return "live"
	}
	if sd.Stopped {
		return "stopped"
	}
	if guest == "running" && len(sd.Bridges) > 0 {
		return "up"
	}
	return "idle"
}
