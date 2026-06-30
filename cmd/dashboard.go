package cmd

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/dashboard"
	"github.com/gordonbeeming/shunt/internal/launchagent"
	"github.com/spf13/cobra"
)

// fileExists reports whether a regular file is present at p.
func fileExists(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

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
			// Graceful: if it's already being served (usually the always-on
			// LaunchAgent), don't crash with "address in use" — just say so.
			if conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
				_ = conn.Close()
				fmt.Printf("• dashboard already running at http://%s (LaunchAgent) — `%s dashboard --install` to (re)install it\n", addr, bin())
				return nil
			}
			srv := &http.Server{Addr: addr, Handler: dashboard.NewServer().Handler()}
			port := config.Current().DashboardPort
			// Serve over TLS with the shared dotnet dev cert when it's installed
			// (CN=localhost, host-trusted), so it's https://localhost:<port>;
			// otherwise plain http. Run `shunt cert install` to get the cert.
			cert, _ := caddy.DevCertPath()
			key, _ := caddy.DevCertKeyPath()
			if fileExists(cert) && fileExists(key) {
				fmt.Printf("• shunt dashboard: https://localhost:%d  (%s channel · Ctrl-C to stop)\n", port, config.Current().Channel)
				if err := srv.ListenAndServeTLS(cert, key); err != nil && err != http.ErrServerClosed {
					return fmt.Errorf("dashboard server: %w", err)
				}
				return nil
			}
			fmt.Printf("• shunt dashboard: http://localhost:%d  (no dev cert — `%s cert install` for https · Ctrl-C to stop)\n", port, bin())
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("dashboard server: %w", err)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&install, "install", false, "install the always-on dashboard LaunchAgent instead of running in the foreground")
	return c
}
