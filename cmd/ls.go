package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
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
	// Reclaimable is nil unless --reclaimable asked for it. A pointer rather than
	// a bool because "we did not check" and "checked, not reclaimable" must not
	// look alike: absent means unknown, and only false is a claim that the siding
	// still holds work.
	Reclaimable *bool `json:"reclaimable,omitempty"`
}

type lsApp struct {
	SchemaVersion int    `json:"schemaVersion"`
	Name          string `json:"name"`
	// FrontDoorReleased mirrors state.App.FrontDoorReleased: the app's fixed
	// ports are deliberately free even though a siding still shows as live
	// below. Additive to schemaVersion 2 — existing consumers ignore it.
	FrontDoorReleased bool       `json:"frontDoorReleased,omitempty"`
	Sidings           []lsSiding `json:"sidings"`
}

const lsSchemaVersion = 2

func newLsCmd() *cobra.Command {
	var all, asJSON, reclaimable bool
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
				la := lsApp{SchemaVersion: lsSchemaVersion, Name: app.Name, FrontDoorReleased: app.FrontDoorReleased, Sidings: []lsSiding{}}
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
					var reclaimableFlag *bool
					if reclaimable {
						reclaimableFlag = sidingReclaimable(ctx, app, sn, guestState)
					}
					la.Sidings = append(la.Sidings, lsSiding{
						Reclaimable:   reclaimableFlag,
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
						a.Name, s.Name, lsTableStatus(a, s), s.Guest, s.IP, s.Dashboard)
				}
			}
			if err := w.Flush(); err != nil {
				return err
			}
			printReclaimableFooter(os.Stdout, reclaimable, apps)
			return nil
		},
	}
	c.Flags().BoolVarP(&all, "all", "a", false, "list sidings for every registered project, not just the current one")
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	c.Flags().BoolVar(&reclaimable, "reclaimable", false,
		"also report which sidings hold no unpreserved work; off by default because it runs Git per siding")
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

// sidingReclaimable reports whether a siding holds no work that removing it
// would lose. It is the inverse of the protection cleanup already computes, so
// the two can never disagree about what is safe.
//
// This is a hint for a person deciding what to tidy, never an authority: rm and
// cleanup re-run their own analysis before deleting anything, and must keep
// doing so. A live or base siding is never offered, because removing either
// needs a deliberate switch or a successor first.
//
// Returns nil when the answer could not be established, so an error reads as
// unknown rather than as "safe to delete".
// printReclaimableFooter names the sidings that hold no unpreserved work, after
// the table rather than as a column: it is only ever populated under
// --reclaimable, and an extra column that is blank on every normal run costs
// width for nothing.
func printReclaimableFooter(out io.Writer, asked bool, apps []lsApp) {
	if !asked {
		return
	}
	var free []string
	for _, a := range apps {
		for _, s := range a.Sidings {
			if s.Reclaimable != nil && *s.Reclaimable {
				free = append(free, a.Name+"/"+s.Name)
			}
		}
	}
	if len(free) == 0 {
		fmt.Fprintln(out, "\nno siding is reclaimable — every one holds work, is live, or is the base")
		return
	}
	fmt.Fprintf(out, "\nreclaimable (no unpreserved work): %s\n", strings.Join(free, ", "))
	fmt.Fprintf(out, "  `%s cleanup` re-checks each one before removing it\n", bin())
}

func sidingReclaimable(ctx context.Context, app state.App, name string, guest string) *bool {
	// A running guest is work in progress even when Git has nothing to lose: a
	// test run or a debugger inside it leaves no trace in the worktree, so the
	// preservation analysis below would happily call it free.
	if app.LiveSiding == name || app.BaseSiding == name || guest == "running" {
		no := false
		return &no
	}
	protected, _, err := sidingWorktreeProtectionWithAnalyzer(ctx, app, name, []string{name}, nil)
	if err != nil {
		return nil
	}
	free := !protected
	return &free
}

func effectiveLsPhase(s state.Siding) state.MaterializationPhase {
	if s.MaterializationPhase == "" {
		return state.PhaseGuest
	}
	return s.MaterializationPhase
}

// lsTableStatus is the plain-table STATUS column. Unlike the deprecated-
// compatible JSON status field, it surfaces a released front door instead of
// claiming the siding is still live — a released app still points LiveSiding
// there, but nothing answers on its ports.
func lsTableStatus(a lsApp, s lsSiding) string {
	if a.FrontDoorReleased && s.Live {
		return "released"
	}
	return s.Status
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
