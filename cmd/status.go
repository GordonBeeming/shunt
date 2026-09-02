package cmd

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/launchagent"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

// healthView is the machine-readable result of `shunt status`: the channel's
// machinery health plus, per app, whether its live front-door routes still point
// at the live siding's current guest IP (drift = a guest that restarted and got a
// new IP, so the front door dials a dead address until the next switch).
type healthView struct {
	Channel          string      `json:"channel"`
	CaddyAdmin       bool        `json:"caddyAdmin"`
	CaddyAgent       bool        `json:"caddyAgent"`
	DashboardAgent   bool        `json:"dashboardAgent"`
	ContainerRuntime bool        `json:"containerRuntime"`
	Runtime          runtimeView `json:"runtime"`
	Apps             []appHealth `json:"apps,omitempty"`
}

type runtimeView struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

type removalHealth struct {
	ID         string `json:"id"`
	Siding     string `json:"siding"`
	Stage      string `json:"stage"`
	Generation string `json:"generation,omitempty"`
	StartedAt  string `json:"startedAt"`
	Age        string `json:"age"`
	Resume     string `json:"resume"`
}

type appHealth struct {
	Name       string      `json:"name"`
	LiveSiding string      `json:"liveSiding"`
	CurrentIP  string      `json:"currentIp,omitempty"` // the live siding's guest IP right now
	Drift      []driftView `json:"drift,omitempty"`
	Fixed      bool        `json:"fixed,omitempty"` // --fix re-pointed it this run
	// FrontDoorReleased mirrors state.App.FrontDoorReleased: LiveSiding still
	// names where a later claim would point, but there is no Caddy binding to
	// check for drift, so the scan below and --fix both skip this app.
	FrontDoorReleased bool           `json:"frontDoorReleased,omitempty"`
	Removal           *removalHealth `json:"removal,omitempty"`
}

type driftView struct {
	Route    string `json:"route"`
	DialedIP string `json:"dialedIp"` // what Caddy currently points the route at
	WantIP   string `json:"wantIp"`   // the live siding's current guest IP
}

func newStatusCmd() *cobra.Command {
	var asJSON, fix bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Health of the shunt machinery (Caddy, agents, runtime) + front-door drift",
		Long: "Reports whether this channel's Caddy admin, LaunchAgents, and container " +
			"runtime are up, and whether each app's live front-door routes still point at " +
			"the live siding's current guest IP. `--fix` re-points any drifted app.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			admin := caddy.NewAdmin()
			runtime := container.ObserveSystem(ctx)
			h := healthView{
				Channel:          config.Current().Channel,
				CaddyAdmin:       admin.Ping(ctx) == nil,
				CaddyAgent:       launchagent.Loaded(ctx, config.Current().LaunchAgentID),
				DashboardAgent:   launchagent.Loaded(ctx, config.Current().DashboardLaunchAgentID),
				ContainerRuntime: runtime.State == container.RuntimeRunning,
				Runtime:          runtimeView{State: string(runtime.State), Detail: runtime.Detail},
			}

			reg, err := state.LoadRegistry()
			if err != nil {
				return err
			}
			names := make([]string, 0, len(reg.Projects))
			for n := range reg.Projects {
				names = append(names, n)
			}
			sort.Strings(names)
			placeholderIP := hostOf(state.PlaceholderDial)
			for _, name := range names {
				app, err := state.LoadApp(reg.Projects[name])
				if err != nil {
					continue // unreadable project state — skip, don't fail the whole report
				}
				if app.LiveSiding == state.HostTarget {
					app.LiveSiding = ""
				}
				if fix {
					if err := ensureNoRemovalInProgress(app, "fix route drift"); err != nil {
						return err
					}
				}
				ah := appHealth{Name: app.Name, LiveSiding: app.LiveSiding, FrontDoorReleased: app.FrontDoorReleased, Removal: makeRemovalHealth(app.Removal)}
				if appFrontDoorBound(app, h.CaddyAdmin) {
					if sd, ok := app.Sidings[app.LiveSiding]; ok {
						ip, _ := container.IP(ctx, sd.Container)
						ah.CurrentIP = ip
						for _, r := range app.FrontDoor {
							dial, derr := caddy.CurrentDial(ctx, admin, r)
							if derr != nil || dial == "" {
								continue
							}
							dialIP := hostOf(dial)
							// Only real divergence counts: skip the placeholder (route not
							// bound yet) and the no-IP case (guest down — a different problem).
							if ip != "" && dialIP != "" && dialIP != ip && dialIP != placeholderIP {
								ah.Drift = append(ah.Drift, driftView{Route: r.Key, DialedIP: dialIP, WantIP: ip})
							}
						}
						if fix && len(ah.Drift) > 0 {
							if e := siding.Switch(ctx, &app, app.LiveSiding); e != nil {
								fmt.Printf("  (fix %s failed: %v)\n", app.Name, e)
							} else {
								ah.Drift = nil
								ah.Fixed = true
							}
						}
					}
				}
				h.Apps = append(h.Apps, ah)
			}

			if asJSON {
				return printJSON(h)
			}
			return printStatus(h)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	c.Flags().BoolVar(&fix, "fix", false, "re-point any drifted live routes at the live siding's current guest IP")
	return c
}

// appFrontDoorBound reports whether an app currently has a Caddy binding worth
// drift-checking, and worth re-pointing under --fix. A released front door
// has deliberately given its ports back, so there is no binding: every route
// would read as drifted, and --fix would try to re-point servers that no
// longer exist.
func appFrontDoorBound(app state.App, caddyAdminUp bool) bool {
	return app.LiveSiding != "" && caddyAdminUp && !app.FrontDoorReleased
}

func printStatus(h healthView) error {
	fmt.Print(statusText(h))
	return nil
}

func statusText(h healthView) string {
	var out strings.Builder
	fmt.Fprintf(&out, "shunt status — %s channel\n", h.Channel)
	check := func(label string, ok bool) {
		mark := "ok"
		if !ok {
			mark = "DOWN"
		}
		fmt.Fprintf(&out, "  %-18s %s\n", label, mark)
	}
	check("caddy admin", h.CaddyAdmin)
	check("caddy agent", h.CaddyAgent)
	check("dashboard agent", h.DashboardAgent)
	check("container runtime", h.ContainerRuntime)
	if h.Runtime.Detail != "" {
		fmt.Fprintf(&out, "    %s\n", h.Runtime.Detail)
	}

	if len(h.Apps) == 0 {
		fmt.Fprintln(&out, "  (no apps registered)")
		return out.String()
	}
	fmt.Fprintln(&out, "  apps:")
	for _, a := range h.Apps {
		live := a.LiveSiding
		if live == "" {
			live = "(nothing live)"
		}
		fmt.Fprintf(&out, "    %-16s live: %s\n", a.Name, live)
		if a.FrontDoorReleased {
			fmt.Fprintln(&out, "      front door released — ports are free, no drift check")
		}
		if a.Removal != nil {
			fmt.Fprintf(&out, "      removal in progress: %s (siding %s, stage %s, generation %s, started %s ago)\n", a.Removal.ID, a.Removal.Siding, a.Removal.Stage, a.Removal.Generation, a.Removal.Age)
			fmt.Fprintf(&out, "        resume: `%s`\n", a.Removal.Resume)
		}
		if a.Fixed {
			fmt.Fprintf(&out, "      ✓ drift fixed — re-pointed at %s\n", a.CurrentIP)
		}
		for _, d := range a.Drift {
			fmt.Fprintf(&out, "      ⚠ drift: route %q dials %s but the live guest is now %s — `%s status --fix`\n",
				d.Route, d.DialedIP, d.WantIP, bin())
		}
	}
	return out.String()
}

func makeRemovalHealth(removal *state.RemovalOperation) *removalHealth {
	if removal == nil {
		return nil
	}
	age := "unknown"
	if started, err := time.Parse(time.RFC3339Nano, removal.StartedAt); err == nil {
		age = time.Since(started).Round(time.Second).String()
	}
	return &removalHealth{ID: removal.ID, Siding: removal.Siding, Stage: string(removal.Stage), Generation: removal.GenerationID, StartedAt: removal.StartedAt, Age: age, Resume: bin() + " rm " + removal.Siding}
}

// hostOf returns the host part of a "host:port" dial (the whole string if it has
// no port).
func hostOf(dial string) string {
	if h, _, err := net.SplitHostPort(dial); err == nil {
		return h
	}
	return dial
}
