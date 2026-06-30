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
