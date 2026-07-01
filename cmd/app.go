package cmd

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/contract"
	"github.com/gordonbeeming/shunt/internal/resolve"
	"github.com/gordonbeeming/shunt/internal/runner"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newAppCmd() *cobra.Command {
	c := &cobra.Command{Use: "app", Short: "Manage registered apps"}
	c.AddCommand(newAppAddCmd())
	c.AddCommand(newAppUpdateCmd())
	c.AddCommand(newAppSwitchCmd())
	return c
}

func newAppAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add",
		Short: "Register the app in the current repo (reads .shunt.app.json)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			return applyContract(cmd.Context(), cwd, applyAdd)
		},
	}
}

// applyMode selects what a missing registration means: create it (add) or error
// (update).
type applyMode int

const (
	applyAdd    applyMode = iota // register the app, or update it if already registered
	applyUpdate                  // apply contract edits to an already-registered app
)

// applyContract reads the repo's .shunt.app.json and reconciles the registered
// app + its Caddy front door with it. `app add` and `app update` are thin callers.
func applyContract(ctx context.Context, cwd string, mode applyMode) error {
	loc, err := resolve.From(cwd)
	if err != nil {
		return err
	}
	// Fold a differently-cased cwd onto the registered project (the macOS FS is
	// case-insensitive, so `cd hubX` and `cd HubX` are the same repo): resolve to
	// the registered `HubX` instead of forking a phantom project whose new Caddy
	// servers then collide on the real app's ports.
	if name, dir, ok := state.CanonicalProject(loc.Project); ok {
		loc.Project, loc.ConfigDir = name, dir
	}

	existing, existErr := state.LoadApp(loc.ConfigDir)
	updating := existErr == nil
	if mode == applyUpdate && !updating {
		return fmt.Errorf("no shunt app registered here yet — run `%s app add` first", bin())
	}

	ct, err := contract.Load(cwd)
	if err != nil {
		return err
	}

	// Determine the runner: the contract wins; otherwise detect from the repo; if
	// still unknown, ask for the start command.
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

	// Build the front-door routes from the contract, preserving each route's
	// already-assigned port across re-applies so stable ports stay stable.
	assigned := map[int]bool{}
	for _, r := range ct.FrontDoor {
		// Fixed apps use the exact declared port (Entra/config point at it);
		// otherwise pick a random free host port — preserved across re-applies —
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
		app.FrontDoor = append(app.FrontDoor, state.Route{
			Key:        r.Key,
			Kind:       r.Kind,
			ListenPort: port,
			Resource:   r.Resource,
			Endpoint:   r.Endpoint,
			GuestPort:  r.GuestPort,
			// TLS is config-driven: terminate TLS at the front door only when the
			// route says so (services are https; the dashboard is http).
			TLS:     r.TLS,
			CaddyID: caddy.RouteID(loc.Project, r.Kind, r.Key),
		})
	}

	// Reconcile Caddy. First registration registers every server; an update
	// touches only the routes that changed, so unchanged routes keep their live
	// dial and the front door never blinks.
	var added, removed []state.Route
	if updating {
		added, removed, err = reconcileFrontDoor(ctx, admin, loc.Project, existing.FrontDoor, app.FrontDoor)
		if err != nil {
			return err
		}
	} else if err := caddy.EnsureFrontDoor(ctx, admin, app); err != nil {
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

	// Bring the live siding in line: bridge any newly-added routes (Activate is
	// idempotent — it only adds socat for ports not already listening) and
	// re-point the front door at the guest, so a new route comes up without a
	// full `up`.
	if updating && app.LiveSiding != "" && app.LiveSiding != state.HostTarget {
		if sd, ok := app.Sidings[app.LiveSiding]; ok && len(sd.Bridges) > 0 {
			if err := siding.Activate(ctx, app, &sd); err != nil {
				fmt.Printf("  (couldn't bridge new routes on %q: %v — run `%s up %s`)\n", app.LiveSiding, err, bin(), app.LiveSiding)
			} else if err := siding.PointCaddy(ctx, app, &sd); err != nil {
				fmt.Printf("  (front door not re-pointed at %q: %v — run `%s switch %s`)\n", app.LiveSiding, err, bin(), app.LiveSiding)
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
	for _, r := range added {
		fmt.Printf("  + %s (new/changed)\n", r.Key)
	}
	for _, r := range removed {
		fmt.Printf("  - %s (removed)\n", r.Key)
	}
	for _, r := range app.FrontDoor {
		fmt.Printf("  %-10s %-6s localhost:%d  ->  %s/%s\n", r.Key, r.Kind, r.ListenPort, r.Resource, r.Endpoint)
	}
	if !updating {
		fmt.Println("next: `" + bin() + " new <name>` to create a siding")
	}
	return nil
}

// reconcileFrontDoor applies only the delta between the existing and new front
// door: it (re)registers added or changed routes and deletes removed ones,
// leaving unchanged routes' Caddy servers — and their live upstreams — untouched
// (no front-door blackout). Returns the added/changed and removed routes.
func reconcileFrontDoor(ctx context.Context, admin *caddy.Admin, appName string, existing, next []state.Route) (added, removed []state.Route, err error) {
	oldByKey := map[string]state.Route{}
	for _, r := range existing {
		oldByKey[r.Kind+"/"+r.Key] = r
	}
	newByKey := map[string]state.Route{}
	for _, r := range next {
		newByKey[r.Kind+"/"+r.Key] = r
	}
	for _, r := range next {
		if o, ok := oldByKey[r.Kind+"/"+r.Key]; ok && routesEqual(o, r) {
			continue // unchanged — leave its server + live dial alone
		}
		added = append(added, r)
		path, body, e := caddy.ServerForRoute(appName, r)
		if e != nil {
			return nil, nil, e
		}
		_ = admin.Delete(ctx, path)
		if e := admin.Put(ctx, path, body); e != nil {
			return nil, nil, fmt.Errorf("register route %q in Caddy: %w", r.Key, e)
		}
	}
	for _, r := range existing {
		if _, ok := newByKey[r.Kind+"/"+r.Key]; ok {
			continue
		}
		removed = append(removed, r)
		if path, _, e := caddy.ServerForRoute(appName, r); e == nil {
			_ = admin.Delete(ctx, path)
		}
	}
	return added, removed, nil
}

// routesEqual reports whether two routes match in every field that affects the
// Caddy server or the guest bridge — so an unchanged route can be left alone.
func routesEqual(a, b state.Route) bool {
	return a.Key == b.Key && a.Kind == b.Kind && a.ListenPort == b.ListenPort &&
		a.Resource == b.Resource && a.Endpoint == b.Endpoint &&
		a.GuestPort == b.GuestPort && a.TLS == b.TLS
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
// so re-running keeps stable ports; 0 if not found.
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
