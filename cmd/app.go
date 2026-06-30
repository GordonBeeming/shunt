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
			// Re-running app add updates the registration from the contract
			// (e.g. new prebakeImages or front-door routes), preserving sidings.
			existing, existErr := state.LoadApp(loc.ConfigDir)
			updating := existErr == nil
			ct, err := contract.Load(cwd)
			if err != nil {
				return err
			}

			// Fixed-port apps use the exact declared ports (Entra/config point at
			// them); otherwise apply the channel offset so channels coexist.
			offset := config.Current().PortOffset
			if ct.FixedPorts {
				offset = 0
			}
			app := state.App{
				Name:          loc.Project,
				RepoPath:      cwd,
				RepoOrigin:    gitOrigin(ctx, cwd),
				AppHostPath:   ct.AppHost,
				ConfigDir:     loc.ConfigDir,
				DataVolumes:   ct.DataVolumes,
				Env:           ct.Env,
				Mounts:        ct.Mounts,
				PrebakeImages: ct.PrebakeImages,
				Sidings:       map[string]state.Siding{},
			}
			if updating {
				app.Sidings = existing.Sidings
				app.LiveSiding = existing.LiveSiding
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
					// HTTP front-door routes serve HTTPS by default (Caddy terminates
					// TLS with its internal CA); layer4/TCP routes stay raw.
					TLS:     r.TLS || r.Kind == state.KindHTTP,
					CaddyID: caddy.RouteID(loc.Project, r.Kind, r.Key),
				}
				app.FrontDoor = append(app.FrontDoor, route)
				path, body, err := caddy.ServerForRoute(loc.Project, route)
				if err != nil {
					return err
				}
				// Delete-then-put so re-running app add applies route config changes
				// (e.g. switching a route to HTTPS) instead of 409-ing on the existing one.
				_ = admin.Delete(ctx, path)
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

			verb := "registered"
			if updating {
				verb = "updated"
			}
			fmt.Printf("✓ %s %s (%d front-door routes, channel offset +%d)\n", verb, app.Name, len(app.FrontDoor), offset)
			for _, r := range app.FrontDoor {
				fmt.Printf("  %-10s %-6s localhost:%d  ->  %s/%s\n", r.Key, r.Kind, r.ListenPort, r.Resource, r.Endpoint)
			}
			fmt.Printf("next: `" + bin() + " new <name>` to create a siding\n")
			return nil
		},
	}
}
