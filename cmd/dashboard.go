package cmd

import (
	"fmt"
	"net/http"

	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/dashboard"
	"github.com/spf13/cobra"
)

// newDashboardCmd runs the shunt web dashboard: one local page (this channel's
// own port) to browse every app's front-door ports with live up/down status and
// switch/restart sidings with a click.
func newDashboardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dashboard",
		Short: "Serve the shunt web dashboard (browse ports, switch/restart sidings)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			addr := config.DashboardAddr()
			srv := &http.Server{Addr: addr, Handler: dashboard.NewServer().Handler()}
			fmt.Printf("• shunt dashboard: http://%s  (%s channel · Ctrl-C to stop)\n", addr, config.Current().Channel)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("dashboard server: %w", err)
			}
			return nil
		},
	}
}
