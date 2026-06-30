package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/resolve"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

// activeSiding is the machine-readable view of one siding for tooling/skills.
// Live = it's the front-door traffic target; AppRunning = Aspire is started in
// the guest (if false, `shunt up <name>`); GuestRunning = the container is up.
type activeSiding struct {
	Name         string `json:"name"`
	Live         bool   `json:"live"`         // currently the stable-port traffic target
	AppRunning   bool   `json:"appRunning"`   // Aspire started in the guest (else: `shunt up`)
	GuestRunning bool   `json:"guestRunning"` // the container guest is up
	Src          string `json:"src"`          // where to edit code for this siding
	IP           string `json:"ip"`           // cached guest IP ("" if not activated)
	Dashboard    string `json:"dashboard"`    // Aspire dashboard URL ("" if no IP)
}

type activeResult struct {
	Active    bool           `json:"active"`              // is cwd a registered shunt app?
	Project   string         `json:"project"`             // project (repo folder) name
	ConfigDir string         `json:"configDir,omitempty"` // <repos>/.shunt[-ch]/<project>
	RepoPath  string         `json:"repoPath,omitempty"`  // the original repo
	Siding    string         `json:"cwdSiding,omitempty"` // siding name if cwd is inside one
	Sidings   []activeSiding `json:"sidings,omitempty"`
}

func newActiveCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "active",
		Short: "Report whether the current directory is a shunt-managed app, and its sidings",
		Long: "Resolves the project from the current directory. If it's registered with shunt, " +
			"reports its sidings and where to edit each one's code. Designed for scripts/skills: " +
			"`shunt active --json`. Exits non-zero when the directory is not a shunt app.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			loc, err := resolve.FromCwd()
			if err != nil {
				return err
			}
			res := activeResult{Project: loc.Project, Siding: loc.Siding}

			reg, err := state.LoadRegistry()
			if err != nil {
				return err
			}
			dir, registered := reg.Projects[loc.Project]
			if !registered {
				return emitInactive(res, asJSON)
			}
			app, err := state.LoadApp(dir)
			if err != nil {
				return emitInactive(res, asJSON)
			}

			res.Active = true
			res.ConfigDir = app.ConfigDir
			res.RepoPath = app.RepoPath
			for name, s := range app.Sidings {
				src, _ := siding.Paths(app, name)
				guestUp := false
				if st, err := container.State(ctx, s.Container); err == nil {
					guestUp = st == "running"
				}
				appUp := false
				if guestUp {
					out, _ := container.Exec(ctx, s.Container, "sh", "-c", "cat /var/log/apphost.log 2>/dev/null")
					appUp = strings.Contains(out, "Distributed application started")
				}
				res.Sidings = append(res.Sidings, activeSiding{
					Name:         name,
					Live:         app.LiveSiding == name,
					AppRunning:   appUp,
					GuestRunning: guestUp,
					Src:          src,
					IP:           s.LastIP,
					Dashboard:    siding.DashboardURL(s),
				})
			}

			if asJSON {
				return printJSON(res)
			}
			fmt.Printf("✓ %s is a shunt app (%d siding(s)) — config: %s\n", res.Project, len(res.Sidings), res.ConfigDir)
			if res.Siding != "" {
				fmt.Printf("  (cwd is inside siding %q)\n", res.Siding)
			}
			for _, s := range res.Sidings {
				live := " "
				if s.Live {
					live = "*"
				}
				fmt.Printf("  %s %-10s edit: %s\n", live, s.Name, s.Src)
				if s.Dashboard != "" {
					fmt.Printf("              dashboard: %s\n", s.Dashboard)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}

// emitInactive reports a non-app directory. With --json it prints {active:false}
// and exits 0 (so scripts can read it); plain mode exits non-zero.
func emitInactive(res activeResult, asJSON bool) error {
	res.Active = false
	if asJSON {
		return printJSON(res)
	}
	fmt.Printf("%s is not a shunt app (run `shunt app add` to register it)\n", res.Project)
	os.Exit(1)
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
