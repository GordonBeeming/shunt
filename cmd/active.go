package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/resolve"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

// activeSiding is the machine-readable view of one siding for tooling/skills.
// Live = it's the front-door traffic target; AppRunning = Aspire is started in
// the guest (if false, `"+bin()+" up <name>`); GuestRunning = the container is up.
type activeSiding struct {
	Name         string `json:"name"`
	Live         bool   `json:"live"`         // currently the stable-port traffic target
	AppRunning   bool   `json:"appRunning"`   // app started in the guest (else: `"+bin()+" up`)
	GuestRunning bool   `json:"guestRunning"` // the container guest is up
	Src          string `json:"src"`          // where to edit code for this siding
	IP           string `json:"ip"`           // cached guest IP ("" if not activated)
	Dashboard    string `json:"dashboard"`    // dashboard URL ("" if no IP)
}

type activeResult struct {
	Active     bool           `json:"active"`              // is cwd a registered shunt app? Kept for existing consumers.
	Managed    bool           `json:"managed"`             // does cwd belong to a project with durable Shunt state?
	Registered bool           `json:"registered"`          // has app add published runtime state to the registry?
	Project    string         `json:"project"`             // project (repo folder) name
	ConfigDir  string         `json:"configDir,omitempty"` // <repos>/.shunt[-ch]/<project>
	RepoPath   string         `json:"repoPath,omitempty"`  // the original repo
	Siding     string         `json:"cwdSiding,omitempty"` // siding name if cwd is inside one
	Sidings    []activeSiding `json:"sidings,omitempty"`
}

func newActiveCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "active",
		Short: "Report whether the current directory is a shunt-managed project, and its sidings",
		Long: "Resolves the project from the current Git repository or siding. If Shunt state exists, " +
			"reports its registration status, sidings, and where to edit each one's code. Designed for scripts/skills: " +
			"`shunt active --json`. Plain mode exits non-zero unless the project is registered; JSON mode exits successfully " +
			"with managed/active/registered flags whenever discovery succeeds.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("get cwd: %w", err)
			}
			res, err := activeResultForDir(cmd.Context(), cwd)
			if err != nil {
				return err
			}
			if !res.Managed {
				return emitInactive(res, asJSON)
			}

			if asJSON {
				return printJSON(res)
			}
			kind := "shunt app"
			if !res.Registered {
				kind = "worktree-only shunt project"
			}
			fmt.Printf("%s %s is a %s (%d siding(s)) — config: %s\n", tick(), res.Project, kind, len(res.Sidings), res.ConfigDir)
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
			// Preserve the original command-level contract for existing shell
			// consumers: plain `active` succeeds only for a registered app. The
			// worktree-only details above are still useful to an interactive user;
			// new machine consumers use --json and branch on managed/registered.
			if !res.Registered {
				os.Exit(1)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}

func activeResultForDir(ctx context.Context, cwd string) (activeResult, error) {
	loc, candidate, err := activeProjectForDir(ctx, cwd)
	if err != nil {
		return activeResult{}, err
	}
	res := activeResult{Project: loc.Project, Siding: loc.Siding}

	reg, err := state.LoadRegistry()
	if err != nil {
		return activeResult{}, err
	}
	_, registeredDir, registered := reg.FindProject(loc.Project)
	registered = registered && filepath.Clean(registeredDir) == filepath.Clean(loc.ConfigDir)
	res.Registered = registered

	var app state.App
	switch {
	case registered:
		app, err = state.LoadApp(registeredDir)
	case candidate != nil:
		app = *candidate
	default:
		app, err = state.LoadApp(loc.ConfigDir)
	}
	if err != nil {
		if !registered && errors.Is(err, state.ErrNotFound) {
			return res, nil
		}
		return activeResult{}, fmt.Errorf("load Shunt state for %s: %w", loc.ConfigDir, err)
	}

	res.Managed = true
	res.Active = registered
	res.ConfigDir = app.ConfigDir
	res.RepoPath = app.RepoPath
	for name, s := range app.Sidings {
		src, _, err := siding.Paths(app, name)
		if err != nil {
			return activeResult{}, err
		}
		guestUp := false
		if s.MaterializationPhase == state.PhaseGuest || s.MaterializationPhase == "" {
			if st, err := container.State(ctx, s.Container); err == nil {
				guestUp = st == "running"
			}
		}
		// Reliable across runners: the log marker is Aspire-only (and even
		// some Aspire apps never emit it), so reuse the shared helper.
		appUp := false
		if guestUp {
			appUp = siding.AppRunning(ctx, app, s)
		}
		res.Sidings = append(res.Sidings, activeSiding{
			Name:         name,
			Live:         app.LiveSiding == name,
			AppRunning:   appUp,
			GuestRunning: guestUp,
			Src:          src,
			IP:           s.LastIP,
			Dashboard:    siding.DashboardURL(app, s),
		})
	}
	return res, nil
}

func activeProjectForDir(ctx context.Context, cwd string) (resolve.Location, *state.App, error) {
	repoRoot, err := gitText(ctx, cwd, "rev-parse", "--show-toplevel")
	if err == nil {
		return resolveGitRootProject(filepath.Clean(repoRoot))
	}
	loc, resolveErr := resolve.From(cwd)
	return loc, nil, resolveErr
}

// emitInactive reports a directory with no Shunt state. With --json it prints
// {active:false,managed:false} and exits 0 (so scripts can read it); plain mode
// exits non-zero.
func emitInactive(res activeResult, asJSON bool) error {
	res.Active = false
	res.Managed = false
	if asJSON {
		return printJSON(res)
	}
	fmt.Printf("%s has no Shunt state (run `"+bin()+" new <name>` in a Git repo, or `"+bin()+" app add` to register an app)\n", res.Project)
	os.Exit(1)
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
