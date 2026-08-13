package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

var runSidingUp = func(ctx context.Context, app state.App, sd state.Siding, bridge bool, progress io.Writer) (state.Siding, error) {
	return siding.Up(ctx, app, sd, bridge, progress)
}

func newUpCmd() *cobra.Command {
	var doSwitch bool
	var noBridge bool
	c := &cobra.Command{
		Use:   "up [name]",
		Short: "Build + start the app in a siding's guest and bridge it (use `switch` to go live)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			if app.Runner == "" {
				return fmt.Errorf("this project currently has worktree-only Shunt state; add a .shunt.app.json contract, then run `%s app add` before `%s up`", bin(), bin())
			}
			name, err := sidingArg(ctx, app, args)
			if err != nil {
				return err
			}
			sd, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q — create it with `"+bin()+" new %s`", name, name)
			}
			// The whole guest-liveness + app-start (+ bridge, unless --no-bridge) flow
			// lives in siding.Up so the dashboard's Start button shares it exactly.
			sd, err = runSidingUp(ctx, app, sd, !noBridge, os.Stdout)
			if err != nil {
				b := bin()
				// reapply recreates just the guest from saved settings (keeps the worktree,
				// branch, and data) — the non-destructive recovery when the container is
				// missing/wedged. rm+new is the last resort because it destroys the worktree.
				return fmt.Errorf("%w — if it persists, recreate the guest with `%s reapply %q` then `%s up %q` (keeps your worktree, branch, and data); only if that still fails, `%s rm %q && %s new %q` (this destroys the worktree + data)",
					err, b, name, b, name, b, name, b, name)
			}
			app, err = state.LoadApp(app.ConfigDir)
			if err != nil {
				return err
			}
			app.Sidings[name] = sd

			if noBridge {
				fmt.Printf("%s %q started in the guest — not bridged to the host (front door untouched)\n", tick(), name)
				if u := siding.DashboardURL(app, sd); u != "" {
					fmt.Printf("  dashboard (guest): %s\n", u)
				}
				fmt.Printf("  when it looks good: `%s up %s` bridges it, then `%s switch %s` (or `%s up %s --switch`) goes live\n", bin(), name, bin(), name, bin(), name)
				return nil
			}
			if name == app.LiveSiding {
				fmt.Printf("%s %q is up and live\n", tick(), name)
				printFrontDoor(app, sd)
				if u := siding.DashboardURL(app, sd); u != "" {
					fmt.Printf("  dashboard (guest): %s\n", u)
				}
				return nil
			}
			// By default `up` brings the siding online (bridged) but leaves the
			// front door where it is, so it can't yank the shared ports away from
			// whatever's currently live. Going live is a deliberate `switch`.
			if !doSwitch {
				fmt.Printf("%s %q is up and bridged — not live yet\n", tick(), name)
				fmt.Printf("  run `%s switch %s` to point the front door at it (or `%s up %s --switch` to go live now)\n", bin(), name, bin(), name)
				if u := siding.DashboardURL(app, sd); u != "" {
					fmt.Printf("  dashboard (guest): %s\n", u)
				}
				return nil
			}
			fmt.Printf("%s %q is up\n", tick(), name)
			if err := switchTo(ctx, &app, name); err != nil {
				return err
			}
			printFrontDoor(app, app.Sidings[name])
			if dashboard := siding.DashboardURL(app, sd); dashboard != "" {
				fmt.Printf("  dashboard (guest): %s\n", dashboard)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&doSwitch, "switch", false, "also point the front door at it once it's up (go live in one shot)")
	c.Flags().BoolVar(&noBridge, "no-bridge", false, "start it in the guest only — no host bridges, no front door (verify before going live)")
	return c
}
