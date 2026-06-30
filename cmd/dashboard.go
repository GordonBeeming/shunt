package cmd

import (
	"fmt"
	"net/http"
	"os"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/dashboard"
	"github.com/gordonbeeming/shunt/internal/launchagent"
	"github.com/spf13/cobra"
)

// newDashboardCmd runs the shunt web dashboard: one local page (this channel's
// own port) to browse every app's front-door ports with live up/down status and
// switch/restart sidings with a click. `--install` registers it as an always-on
// LaunchAgent instead (so it survives logout/reboot, like Caddy).
func newDashboardCmd() *cobra.Command {
	var install bool
	c := &cobra.Command{
		Use:   "dashboard",
		Short: "Serve the shunt web dashboard (browse ports, switch/restart sidings)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if install {
				exe, err := os.Executable()
				if err != nil {
					return err
				}
				if err := launchagent.InstallDashboard(cmd.Context(), exe); err != nil {
					return err
				}
				fmt.Printf("✓ dashboard LaunchAgent installed — always on at http://%s\n", config.DashboardAddr())
				return nil
			}
			addr := config.DashboardAddr()
			srv := &http.Server{Addr: addr, Handler: dashboard.NewServer().Handler()}
			fmt.Printf("• shunt dashboard: http://%s  (%s channel · Ctrl-C to stop)\n", addr, config.Current().Channel)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("dashboard server: %w", err)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&install, "install", false, "install the always-on dashboard LaunchAgent instead of running in the foreground")
	return c
}
