package cmd

import (
	"fmt"
	"os"

	"bufio"
	"strings"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/contract"
	"github.com/gordonbeeming/shunt/internal/resolve"
	"github.com/gordonbeeming/shunt/internal/runner"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
	"net"
)

func newAppCmd() *cobra.Command {
	c := &cobra.Command{Use: "app", Short: "Manage registered apps"}
	c.AddCommand(newAppAddCmd())
	c.AddCommand(newAppSwitchCmd())
	return c
}

func newAppAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add",
		Short: "Register the app in the current repo (reads .shunt.app.json)",
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

			// Determine the runner: the contract wins; otherwise detect from the
			// repo; if still unknown, ask for the start command.
			runnerKind, startCmd, workdir := ct.Runner, ct.Start, ct.Workdir
			if runnerKind == "" {
				det := runner.Detect(cwd)
				runnerKind = det.Kind
				if startCmd == "" {
					startCmd = det.Start
				}
				if workdir == "" {
					workdir = det.Workdir
				}
			}
			if runnerKind != runner.Aspire && startCmd == "" {
				startCmd, err = promptStartCommand(loc.Project)
				if err != nil {
					return err
				}
				runnerKind = runner.Custom
			}

			app := state.App{
				Name:          loc.Project,
				RepoPath:      cwd,
				RepoOrigin:    gitOrigin(ctx, cwd),
				Runner:        runnerKind,
				Start:         startCmd,
				Stop:          ct.Stop,
				Workdir:       workdir,
				AppHostPath:   ct.AppHost,
				ConfigDir:     loc.ConfigDir,
				Env:           ct.Env,
				Mounts:        ct.Mounts,
				PrebakeImages: ct.PrebakeImages,
				Volumes:       ct.Volumes,
				Memory:        ct.Memory,
				CPUs:          ct.CPUs,
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
			// Drop any existing Caddy server whose route was renamed or removed in
			// the contract. Without this the old server keeps squatting its listen
			// port, and Caddy 500s when the new (renamed) server tries to claim the
			// same port. The live siding's upstreams are restored by re-activating
			// after the re-register.
			if updating {
				keep := map[string]bool{}
				for _, r := range ct.FrontDoor {
					keep[r.Kind+"/"+r.Key] = true
				}
				for _, r := range existing.FrontDoor {
					if keep[r.Kind+"/"+r.Key] {
						continue
					}
					if p, _, perr := caddy.ServerForRoute(loc.Project, r); perr == nil {
						_ = admin.Delete(ctx, p)
					}
				}
			}
			assigned := map[int]bool{}
			for _, r := range ct.FrontDoor {
				// Fixed apps use the exact declared port (Entra/config point at it);
				// otherwise pick a random free host port — preserved across re-adds —
				// so different apps and channels never collide.
				port := r.ListenPort
				if !ct.FixedPorts {
					port = 0
					if updating {
						port = existingRoutePort(existing, r.Key, r.Kind)
					}
					if port == 0 {
						port, err = freePort(assigned)
						if err != nil {
							return err
						}
					}
				}
				assigned[port] = true
				route := state.Route{
					Key:        r.Key,
					Kind:       r.Kind,
					ListenPort: port,
					Resource:   r.Resource,
					Endpoint:   r.Endpoint,
					GuestPort:  r.GuestPort,
					// TLS is config-driven: terminate TLS at the front door only when
					// the route says so (services are https; the dashboard is http).
					TLS:     r.TLS,
					CaddyID: caddy.RouteID(loc.Project, r.Kind, r.Key),
				}
				app.FrontDoor = append(app.FrontDoor, route)
			}
			// Register (delete-then-put) every front-door server in one place, shared
			// with the switch-back-from-host path (caddy.EnsureFrontDoor).
			if err := caddy.EnsureFrontDoor(ctx, admin, app); err != nil {
				return err
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

			// The delete-then-put above reset every route to the placeholder dial,
			// so a re-`app add` of a live app would drop its front door. Re-point the
			// live siding back at itself (best effort — its socat bridges are still
			// up, so this just re-patches Caddy).
			if updating && app.LiveSiding != "" {
				if sd, ok := app.Sidings[app.LiveSiding]; ok && len(sd.Bridges) > 0 {
					if err := siding.PointCaddy(ctx, app, &sd); err != nil {
						fmt.Printf("  (front door not re-pointed at %q: %v — run `%s switch %s`)\n",
							app.LiveSiding, err, bin(), app.LiveSiding)
					} else {
						app.Sidings[app.LiveSiding] = sd
						_ = state.SaveApp(app)
					}
				}
			}

			verb := "registered"
			if updating {
				verb = "updated"
			}
			ports := "random free ports"
			if ct.FixedPorts {
				ports = "fixed ports"
			}
			fmt.Printf("✓ %s %s (runner: %s, %d front-door routes, %s)\n", verb, app.Name, app.Runner, len(app.FrontDoor), ports)
			for _, r := range app.FrontDoor {
				fmt.Printf("  %-10s %-6s localhost:%d  ->  %s/%s\n", r.Key, r.Kind, r.ListenPort, r.Resource, r.Endpoint)
			}
			fmt.Println("next: `" + bin() + " new <name>` to create a siding")
			return nil
		},
	}
}

// freePort returns a free TCP port on the host not already in used.
func freePort(used map[int]bool) (int, error) {
	for i := 0; i < 200; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		if !used[port] {
			return port, nil
		}
	}
	return 0, fmt.Errorf("could not find a free port")
}

// existingRoutePort returns the port already assigned to a route (by key+kind)
// so re-running app add keeps stable ports; 0 if not found.
func existingRoutePort(app state.App, key, kind string) int {
	for _, r := range app.FrontDoor {
		if r.Key == key && r.Kind == kind {
			return r.ListenPort
		}
	}
	return 0
}

// promptStartCommand asks (interactively) how to start an app shunt couldn't
// classify; errors in non-interactive mode so CI declares it in the contract.
func promptStartCommand(project string) (string, error) {
	if fi, _ := os.Stdout.Stat(); fi == nil || fi.Mode()&os.ModeCharDevice == 0 {
		return "", fmt.Errorf("could not detect how to start %q — set `runner` + `start` in .shunt.app.json", project)
	}
	fmt.Printf("shunt couldn't detect how to start %q.\nWhat command starts it (run from the repo root)?\n> ", project)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", err
	}
	cmd := strings.TrimSpace(line)
	if cmd == "" {
		return "", fmt.Errorf("no start command given — set `runner` + `start` in .shunt.app.json")
	}
	return cmd, nil
}
