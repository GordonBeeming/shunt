package cmd

import (
	"fmt"
	"net"
	"sort"

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
	Apps             []appHealth `json:"apps,omitempty"`
}

type appHealth struct {
	Name       string      `json:"name"`
	LiveSiding string      `json:"liveSiding"`
	CurrentIP  string      `json:"currentIp,omitempty"` // the live siding's guest IP right now
	Drift      []driftView `json:"drift,omitempty"`
	Fixed      bool        `json:"fixed,omitempty"` // --fix re-pointed it this run
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
			h := healthView{
				Channel:          config.Current().Channel,
				CaddyAdmin:       admin.Ping(ctx) == nil,
				CaddyAgent:       launchagent.Loaded(ctx, config.Current().LaunchAgentID),
				DashboardAgent:   launchagent.Loaded(ctx, config.Current().DashboardLaunchAgentID),
				ContainerRuntime: container.SystemRunning(ctx),
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
				ah := appHealth{Name: app.Name, LiveSiding: app.LiveSiding}
				if app.LiveSiding != "" && h.CaddyAdmin {
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

func printStatus(h healthView) error {
	fmt.Printf("shunt status — %s channel\n", h.Channel)
	check := func(label string, ok bool) {
		mark := "ok"
		if !ok {
			mark = "DOWN"
		}
		fmt.Printf("  %-18s %s\n", label, mark)
	}
	check("caddy admin", h.CaddyAdmin)
	check("caddy agent", h.CaddyAgent)
	check("dashboard agent", h.DashboardAgent)
	check("container runtime", h.ContainerRuntime)

	if len(h.Apps) == 0 {
		fmt.Println("  (no apps registered)")
		return nil
	}
	fmt.Println("  apps:")
	for _, a := range h.Apps {
		live := a.LiveSiding
		if live == "" {
			live = "(nothing live)"
		}
		fmt.Printf("    %-16s live: %s\n", a.Name, live)
		if a.Fixed {
			fmt.Printf("      ✓ drift fixed — re-pointed at %s\n", a.CurrentIP)
		}
		for _, d := range a.Drift {
			fmt.Printf("      ⚠ drift: route %q dials %s but the live guest is now %s — `%s status --fix`\n",
				d.Route, d.DialedIP, d.WantIP, bin())
		}
	}
	return nil
}

// hostOf returns the host part of a "host:port" dial (the whole string if it has
// no port).
func hostOf(dial string) string {
	if h, _, err := net.SplitHostPort(dial); err == nil {
		return h
	}
	return dial
}
