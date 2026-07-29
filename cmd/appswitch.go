package cmd

import (
	"fmt"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newAppSwitchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "switch <app>",
		Short: "Make <app> active on its front-door ports, parking any app that conflicts",
		Long: "For apps that share fixed ports (e.g. several Vite apps on the same port): frees the conflicting " +
			"app's front-door binding without stopping its siding, claims the ports for <app>, and points them at " +
			"<app>'s live siding. Switch back later to reconnect the parked app — its guest stays running the whole time.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target := args[0]

			reg, err := state.LoadRegistry()
			if err != nil {
				return err
			}
			canonical, dir, ok := reg.FindProject(target)
			if !ok {
				return fmt.Errorf("no registered app %q — see `%s ls -a`", target, bin())
			}
			target = canonical // use the registered case for server names etc.
			app, err := state.LoadApp(dir)
			if err != nil {
				return err
			}

			admin := caddy.NewAdmin()
			if err := admin.Ping(ctx); err != nil {
				return fmt.Errorf("caddy not reachable — run `%s init`: %w", bin(), err)
			}

			want := map[int]bool{}
			for _, r := range app.FrontDoor {
				want[r.ListenPort] = true
			}

			// Park any OTHER app currently holding a port the target needs.
			for name, d := range reg.Projects {
				if name == target {
					continue
				}
				other, err := state.LoadApp(d)
				if err != nil {
					continue
				}
				for _, r := range other.FrontDoor {
					if !want[r.ListenPort] {
						continue
					}
					if path, _, e := caddy.ServerForRoute(name, r, false); e == nil {
						_ = admin.Delete(ctx, path)
						fmt.Printf("• parked %s/%s (freed :%d — its guest keeps running)\n", name, r.Key, r.ListenPort)
					}
				}
			}

			// Claim the target's front-door servers.
			for _, r := range app.FrontDoor {
				path, body, err := caddy.ServerForRoute(target, r, app.DisableCache)
				if err != nil {
					return err
				}
				_ = admin.Delete(ctx, path)
				if err := admin.Put(ctx, path, body); err != nil {
					return fmt.Errorf("claim %s/%s: %w", target, r.Key, err)
				}
			}

			// Point the claimed ports at the target's live siding, if it has one.
			if app.LiveSiding != "" {
				if err := switchTo(ctx, &app, app.LiveSiding); err != nil {
					return err
				}
			} else {
				fmt.Printf("  (no live siding yet — `%s up <siding>` in %s to bring it up)\n", bin(), target)
			}
			fmt.Printf("%s %s is now active on its front-door ports\n", tick(), target)
			return nil
		},
	}
}
