package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

var (
	appSwitchPrepareCaddy = func(ctx context.Context) (*caddy.Admin, error) {
		admin := caddy.NewAdmin()
		if err := admin.Ping(ctx); err != nil {
			return nil, fmt.Errorf("caddy not reachable — run `%s init`: %w", bin(), err)
		}
		return admin, nil
	}
	appSwitchDeleteRoute = func(ctx context.Context, admin *caddy.Admin, path string) error { return admin.Delete(ctx, path) }
	appSwitchPutRoute    = func(ctx context.Context, admin *caddy.Admin, path string, body []byte) error {
		return admin.Put(ctx, path, body)
	}
	// appSwitchDeleteRouteIfExists is the park loop's own delete seam, distinct
	// from appSwitchDeleteRoute: unlike the claim loop, nothing after it replaces
	// the server, so a genuine failure here must be reported rather than
	// swallowed. DeleteIfExists treats "never registered" as success and
	// propagates everything else.
	appSwitchDeleteRouteIfExists = func(ctx context.Context, admin *caddy.Admin, path string) error {
		return admin.DeleteIfExists(ctx, path)
	}
	appSwitchRemoveFrontDoor = caddy.RemoveFrontDoor
	appSwitchSaveApp         = state.SaveApp
	appSwitchSwitchTo        = switchTo
)

func newAppSwitchCmd() *cobra.Command {
	var release bool
	c := &cobra.Command{
		Use:   "switch [app]",
		Short: "Make <app> active on its front-door ports, parking any app that conflicts",
		Long: "For apps that share fixed ports (e.g. several Vite apps on the same port): frees the conflicting " +
			"app's front-door binding without stopping its siding, claims the ports for <app>, and points them at " +
			"<app>'s live siding. Switch back later to reconnect the parked app — its guest stays running the whole time.\n\n" +
			"With --release, it instead drops <app>'s own front-door servers and gives the ports back to the host, " +
			"touching neither its guest nor any siding; a later plain `app switch <app>` reclaims them.",
		Args: func(cmd *cobra.Command, args []string) error {
			// --release can act on the current directory's app, so it alone
			// allows the bare zero-arg form; without it an app name stays
			// required so a bare `app switch` fails loudly instead of doing
			// nothing.
			if release {
				return cobra.MaximumNArgs(1)(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, target, err := resolveAppSwitchTarget(args)
			if err != nil {
				return err
			}
			if release {
				return releaseAppFrontDoor(ctx, app)
			}
			return claimAppFrontDoor(ctx, app, target)
		},
	}
	c.Flags().BoolVar(&release, "release", false,
		"give <app>'s front-door ports back without touching its guest; reclaim later with a plain app switch <app>")
	return c
}

// resolveAppSwitchTarget loads the named app, or — with no argument, valid
// only alongside --release — the app registered for the current directory,
// the way every other bare shunt command resolves its target.
func resolveAppSwitchTarget(args []string) (app state.App, target string, err error) {
	if len(args) == 0 {
		app, _, err = loadCurrentApp()
		return app, app.Name, err
	}
	reg, err := state.LoadRegistry()
	if err != nil {
		return state.App{}, "", err
	}
	canonical, dir, ok := reg.FindProject(args[0])
	if !ok {
		return state.App{}, "", fmt.Errorf("no registered app %q — see `%s ls -a`", args[0], bin())
	}
	app, err = state.LoadApp(dir)
	return app, canonical, err
}

// releaseAppFrontDoor drops every Caddy server app.FrontDoor holds and records
// the choice in state, so status reporting stops calling app's live siding
// "live" once nothing actually answers on its ports. The guest, its sidings,
// and their materialization phases are untouched — only Caddy and the
// FrontDoorReleased flag move. Releasing an already-released app is a no-op.
func releaseAppFrontDoor(ctx context.Context, app state.App) error {
	admin, err := appSwitchPrepareCaddy(ctx)
	if err != nil {
		return err
	}
	return siding.WithProjectOperation(ctx, app.ConfigDir, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		if err := siding.EnsureNoRemovalInProgress(current, "release the front door"); err != nil {
			return err
		}
		if current.FrontDoorReleased {
			fmt.Printf("%s %s front door is already released\n", tick(), current.Name)
			return nil
		}
		if err := appSwitchRemoveFrontDoor(ctx, admin, current); err != nil {
			return fmt.Errorf("release %s front door: %w", current.Name, err)
		}
		current.FrontDoorReleased = true
		if err := appSwitchSaveApp(current); err != nil {
			return err
		}
		fmt.Printf("%s released %s front door\n", tick(), current.Name)
		fmt.Printf("  freed %s\n", freedPortList(current.FrontDoor))
		fmt.Println("  sidings and guests keep running")
		fmt.Printf("  reclaim with `%s app switch %s`\n", bin(), current.Name)
		return nil
	})
}

// freedPortList renders every route's listen port for the release summary, in
// contract order.
func freedPortList(routes []state.Route) string {
	ports := make([]string, len(routes))
	for i, r := range routes {
		ports[i] = fmt.Sprintf(":%d", r.ListenPort)
	}
	return strings.Join(ports, " ")
}

// claimAppFrontDoor parks any other app currently holding a port target needs,
// claims target's own front-door servers, and — if target has a live siding —
// points the claimed ports at it. It clears FrontDoorReleased on target itself
// so status reporting reflects the claim even when target has no live siding
// yet; switchLocked also clears it, but only once a siding is actually pointed
// to.
func claimAppFrontDoor(ctx context.Context, app state.App, target string) error {
	admin, err := appSwitchPrepareCaddy(ctx)
	if err != nil {
		return err
	}

	// Refuse before touching Caddy at all. Checked only alongside the state write
	// below, a removal in progress would abort after other apps had been parked
	// and the target's servers rebuilt, leaving the front door half-moved and
	// disagreeing with the state that was never saved.
	if err := siding.EnsureNoRemovalInProgress(app, "claim the front door"); err != nil {
		return err
	}

	want := map[int]bool{}
	for _, r := range app.FrontDoor {
		want[r.ListenPort] = true
	}

	reg, err := state.LoadRegistry()
	if err != nil {
		return err
	}
	for name, dir := range reg.Projects {
		if name == target {
			continue
		}
		if err := parkConflictingApp(ctx, admin, name, dir, want); err != nil {
			return err
		}
	}

	// Claim the target's front-door servers.
	for _, r := range app.FrontDoor {
		path, body, err := caddy.ServerForRoute(target, r, app.DisableCache)
		if err != nil {
			return err
		}
		_ = appSwitchDeleteRoute(ctx, admin, path)
		if err := appSwitchPutRoute(ctx, admin, path, body); err != nil {
			return fmt.Errorf("claim %s/%s: %w", target, r.Key, err)
		}
	}

	if err := siding.WithProjectOperation(ctx, app.ConfigDir, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		if err := siding.EnsureNoRemovalInProgress(current, "claim the front door"); err != nil {
			return err
		}
		current.FrontDoorReleased = false
		if err := appSwitchSaveApp(current); err != nil {
			return err
		}
		app = current
		return nil
	}); err != nil {
		return err
	}

	// Point the claimed ports at the target's live siding, if it has one.
	if app.LiveSiding != "" {
		if err := appSwitchSwitchTo(ctx, &app, app.LiveSiding); err != nil {
			return err
		}
	} else {
		fmt.Printf("  (no live siding yet — `%s up <siding>` in %s to bring it up)\n", bin(), target)
	}
	fmt.Printf("%s %s is now active on its front-door ports\n", tick(), target)
	return nil
}

// parkConflictingApp frees any of other's front-door ports that target needs,
// without stopping other's guest. It records the release on other's own state
// only when every one of other's routes was parked — a partial park still
// serves the routes it didn't touch, so calling the app "released" would hide
// a live route from `status`'s drift scan; the per-route "parked" lines below
// already make a partial park visible without that. A route's delete failure
// aborts and returns an error: unlike the claim loop, nothing after this
// delete replaces the server, so there's nothing to paper over a real failure
// with — the port would still be bound while state and output both claimed
// otherwise.
func parkConflictingApp(ctx context.Context, admin *caddy.Admin, name, dir string, want map[int]bool) error {
	other, err := state.LoadApp(dir)
	if err != nil {
		// shunt is about to claim ports this app may be holding; the operator
		// should know its state couldn't be read, even though an unrelated
		// switch must not fail over it.
		fmt.Fprintf(os.Stderr, "warning: could not read state for registered app %q, skipping: %v\n", name, err)
		return nil
	}
	parkedAll := true
	anyParked := false
	for _, r := range other.FrontDoor {
		if !want[r.ListenPort] {
			parkedAll = false
			continue
		}
		path, _, err := caddy.ServerForRoute(name, r, false)
		if err != nil {
			parkedAll = false
			continue
		}
		if err := appSwitchDeleteRouteIfExists(ctx, admin, path); err != nil {
			return fmt.Errorf("park %s/%s: %w", name, r.Key, err)
		}
		fmt.Printf("• parked %s/%s (freed :%d — its guest keeps running)\n", name, r.Key, r.ListenPort)
		anyParked = true
	}
	if !anyParked || !parkedAll {
		return nil
	}
	return siding.WithProjectOperation(ctx, other.ConfigDir, func() error {
		latest, err := state.LoadApp(other.ConfigDir)
		if err != nil {
			return err
		}
		latest.FrontDoorReleased = true
		return appSwitchSaveApp(latest)
	})
}
