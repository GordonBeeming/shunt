package cmd

import (
	"fmt"
	"os"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/contract"
	"github.com/gordonbeeming/shunt/internal/resolve"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newAppCmd() *cobra.Command {
	c := &cobra.Command{Use: "app", Short: "Manage registered apps"}
	c.AddCommand(newAppAddCmd())
	return c
}

func newAppAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add",
		Short: "Register the Aspire app in the current repo (reads .shunt.app.json)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			loc, err := resolve.From(cwd)
			if err != nil {
				return err
			}
			if _, err := state.LoadApp(loc.ConfigDir); err == nil {
				return fmt.Errorf("app %q is already registered (config: %s)", loc.Project, loc.ConfigDir)
			}
			ct, err := contract.Load(cwd)
			if err != nil {
				return err
			}

			offset := config.Current().PortOffset
			app := state.App{
				Name:        loc.Project,
				RepoPath:    cwd,
				RepoOrigin:  gitOrigin(ctx, cwd),
				AppHostPath: ct.AppHost,
				ConfigDir:   loc.ConfigDir,
				DataVolumes: ct.DataVolumes,
				Env:         ct.Env,
				Mounts:      ct.Mounts,
				Sidings:     map[string]state.Siding{},
			}

			admin := caddy.NewAdmin()
			if err := admin.Ping(ctx); err != nil {
				return fmt.Errorf("caddy admin API not reachable — run `"+bin()+" init` first: %w", err)
			}
			for _, r := range ct.FrontDoor {
				route := state.Route{
					Key:        r.Key,
					Kind:       r.Kind,
					ListenPort: r.ListenPort + offset,
					Resource:   r.Resource,
					Endpoint:   r.Endpoint,
					TLS:        r.TLS,
					CaddyID:    caddy.RouteID(loc.Project, r.Kind, r.Key),
				}
				app.FrontDoor = append(app.FrontDoor, route)
				path, body, err := caddy.ServerForRoute(loc.Project, route)
				if err != nil {
					return err
				}
				if err := admin.Put(ctx, path, body); err != nil {
					return fmt.Errorf("register route %q in Caddy: %w", r.Key, err)
				}
			}

			if err := state.SaveApp(app); err != nil {
				return err
			}
			reg, err := state.LoadRegistry()
			if err != nil {
				return err
			}
			reg.Projects[app.Name] = app.ConfigDir
			if err := state.SaveRegistry(reg); err != nil {
				return err
			}

			fmt.Printf("✓ registered %s (%d front-door routes, channel offset +%d)\n", app.Name, len(app.FrontDoor), offset)
			for _, r := range app.FrontDoor {
				fmt.Printf("  %-10s %-6s localhost:%d  ->  %s/%s\n", r.Key, r.Kind, r.ListenPort, r.Resource, r.Endpoint)
			}
			fmt.Printf("next: `"+bin()+" new <name>` to create a siding\n")
			return nil
		},
	}
}
