package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newUpCmd() *cobra.Command {
	var noSwitch bool
	c := &cobra.Command{
		Use:   "up <name>",
		Short: "Build + start Aspire in a siding's guest, then point the front door at it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			sd, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q — create it with `"+bin()+" new %s`", name, name)
			}
			st, _ := container.State(ctx, sd.Container)
			if st != "running" {
				return fmt.Errorf("the guest for %q isn't running (state=%s); recreate it with `"+bin()+" new`", name, st)
			}

			// Idempotent: only launch Aspire if it isn't already up in the guest.
			out, _ := container.Exec(ctx, sd.Container, "sh", "-c", "cat /var/log/apphost.log 2>/dev/null")
			if !strings.Contains(out, "Distributed application started") {
				// Stop any stale AppHost first so we don't start a second one (port
				// clash), and clear its log so WaitStarted waits for the fresh run.
				_ = siding.StopApp(ctx, sd)
				_, _ = container.Exec(ctx, sd.Container, "sh", "-c", "> /var/log/apphost.log")

				// Load the project warm cache if it's mounted in this guest, so Aspire
				// reuses pre-built/pulled images instead of rebuilding/pulling.
				if siding.IsWarmed(app) {
					out, _ := container.Exec(ctx, sd.Container, "sh", "-c",
						"if [ -f /mnt/base/images.tar ]; then docker load -i /mnt/base/images.tar && echo LOADED; fi")
					if strings.Contains(out, "LOADED") {
						fmt.Println("• loaded pre-warmed dependency images (no pull)")
					} else {
						fmt.Printf("• no warm cache in this guest — recreate it (`%s rm %s && %s new %s`) to use the cache\n", bin(), name, bin(), name)
					}
				}
				fmt.Printf("• starting Aspire in %q (first run builds + pulls anything not warmed)…\n", name)
				if err := siding.StartApp(ctx, app, sd); err != nil {
					return err
				}
				fmt.Println("• waiting for the app to start…")
				if err := siding.WaitStarted(ctx, sd.Container, 25*time.Minute); err != nil {
					return err
				}
			} else {
				fmt.Printf("• Aspire is already running in %q\n", name)
			}

			fmt.Println("• discovering endpoints + bridging to the host…")
			if err := siding.Activate(ctx, app, &sd); err != nil {
				return err
			}
			app.Sidings[name] = sd
			if err := state.SaveApp(app); err != nil {
				return err
			}
			fmt.Printf("✓ %q is up — dashboard %s\n", name, siding.DashboardURL(sd))

			if noSwitch {
				fmt.Printf("  run `"+bin()+" switch %s` to point the stable ports at it\n", name)
				return nil
			}
			return switchTo(ctx, &app, name)
		},
	}
	c.Flags().BoolVar(&noSwitch, "no-switch", false, "start it but don't point the front door at it")
	return c
}
