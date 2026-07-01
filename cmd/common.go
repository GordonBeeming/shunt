package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/resolve"
	"github.com/gordonbeeming/shunt/internal/state"
)

// loadCurrentApp resolves the project from cwd and loads its registered app.
func loadCurrentApp() (state.App, resolve.Location, error) {
	loc, err := resolve.FromCwd()
	if err != nil {
		return state.App{}, resolve.Location{}, err
	}
	app, err := state.LoadApp(loc.ConfigDir)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return state.App{}, loc, fmt.Errorf("no shunt app registered for %q — run `"+bin()+" app add` in the repo", loc.Project)
		}
		return state.App{}, loc, err
	}
	return app, loc, nil
}

// bin is this build's command name (shunt / shunt-beta / shunt-dev), used in
// user-facing hints so copy-pasted commands actually exist on PATH.
func bin() string { return config.Current().BinaryName }

// gitOrigin returns the repo's origin URL, or "" if there isn't one.
func gitOrigin(ctx context.Context, repoPath string) string {
	res, err := proc.Run(ctx, "git", "-C", repoPath, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// routeURL formats a front-door route as a browsable localhost address (https
// when the route terminates TLS), so we show localhost rather than guest IPs.
func routeURL(r state.Route) string {
	if r.Kind == state.KindHTTP {
		scheme := "http"
		if r.TLS {
			scheme = "https"
		}
		return fmt.Sprintf("%s://localhost:%d", scheme, r.ListenPort)
	}
	return fmt.Sprintf("localhost:%d (tcp)", r.ListenPort)
}

// printFrontDoor lists an app's front-door routes as localhost URLs to browse to,
// flagging any that aren't bridged yet (partial activation).
func printFrontDoor(app state.App, sd state.Siding) {
	fmt.Println("  front door (browse to):")
	for _, r := range app.FrontDoor {
		note := ""
		if _, ok := sd.Bridges[r.Key]; !ok {
			note = "  (pending — not up yet)"
		}
		fmt.Printf("    %-8s %s%s\n", r.Key, routeURL(r), note)
	}
}

// sidingArg resolves the siding name from the first positional arg, or prompts
// with the interactive picker when none is given (like `switch`). Lets commands
// that act on a siding be run bare and pick from the list.
func sidingArg(app state.App, args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	return pickSiding(app)
}
