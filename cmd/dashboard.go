package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/dashboard"
	"github.com/gordonbeeming/shunt/internal/launchagent"
	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/spf13/cobra"
)

const (
	dashboardStartupTimeout = 5 * time.Second
	dashboardReadTimeout    = 10 * time.Second
	dashboardWriteTimeout   = 30 * time.Second
	dashboardIdleTimeout    = 2 * time.Minute
)

var dashboardHTTPClient = &http.Client{Timeout: time.Second}

// fileExists reports whether a regular file is present at p.
func fileExists(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func dashboardRuntime() (url, cert, key string, useTLS bool) {
	port := config.Current().DashboardPort
	cert, _ = caddy.DevCertPath()
	key, _ = caddy.DevCertKeyPath()
	useTLS = fileExists(cert) && fileExists(key)
	if useTLS {
		return fmt.Sprintf("https://localhost:%d", port), cert, key, true
	}
	return fmt.Sprintf("http://localhost:%d", port), cert, key, false
}

func dashboardLaunchedByAgent(ppid int, serviceName string) bool {
	return ppid == 1 || serviceName == config.Current().DashboardLaunchAgentID
}

func dashboardResponding(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/", nil)
	if err != nil {
		return err
	}
	resp, err := dashboardHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}
	return nil
}

func waitForDashboard(ctx context.Context, url string, timeout time.Duration) error {
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var lastErr error
	for {
		if lastErr = dashboardResponding(pollCtx, url); lastErr == nil {
			return nil
		}
		select {
		case <-pollCtx.Done():
			if err := ctx.Err(); err != nil {
				return err
			}
			return fmt.Errorf("dashboard health check timed out after %s (last error: %v): %w", timeout, lastErr, pollCtx.Err())
		case <-ticker.C:
		}
	}
}

func openDashboard(ctx context.Context, url string) error {
	if _, err := proc.Run(ctx, "/usr/bin/open", url); err != nil {
		return fmt.Errorf("open dashboard: %w", err)
	}
	return nil
}

func newDashboardHTTPServer() *http.Server {
	return &http.Server{
		Addr:              config.DashboardAddr(),
		Handler:           dashboard.NewServer().Handler(),
		ReadHeaderTimeout: dashboardReadTimeout,
		ReadTimeout:       dashboardReadTimeout,
		WriteTimeout:      dashboardWriteTimeout,
		IdleTimeout:       dashboardIdleTimeout,
	}
}

func serveDashboard(url, cert, key string, useTLS bool) error {
	srv := newDashboardHTTPServer()
	if useTLS {
		fmt.Printf("• shunt dashboard: %s  (%s channel · Ctrl-C to stop)\n", url, config.Current().Channel)
		if err := srv.ListenAndServeTLS(cert, key); err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("dashboard server: %w", err)
		}
		return nil
	}
	fmt.Printf("• shunt dashboard: %s  (no dev cert — `%s cert install` for https · Ctrl-C to stop)\n", url, bin())
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("dashboard server: %w", err)
	}
	return nil
}

// newDashboardCmd health-checks and opens this channel's dashboard. The hidden
// --serve mode is the long-running LaunchAgent process; --install registers it
// so it survives logout/reboot, like Caddy.
func newDashboardCmd() *cobra.Command {
	var install, serve bool
	c := &cobra.Command{
		Use:   "dashboard",
		Short: "Open the hostless dashboard (inspect, start, switch, stop, and park sidings)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			url, cert, key, useTLS := dashboardRuntime()

			// Older installed plists invoked `dashboard` without --serve. Keep those
			// agents working after a binary upgrade, whether launchd is identified by
			// its parent PID or the job label it injects into XPC_SERVICE_NAME.
			if serve || dashboardLaunchedByAgent(os.Getppid(), os.Getenv("XPC_SERVICE_NAME")) {
				return serveDashboard(url, cert, key, useTLS)
			}

			if install {
				if err := installDashboardAgent(ctx, launchagent.InstallDashboard); err != nil {
					return err
				}
				if err := waitForDashboard(ctx, url, dashboardStartupTimeout); err != nil {
					return fmt.Errorf("dashboard LaunchAgent installed but %s did not become healthy: %w", url, err)
				}
				fmt.Printf("%s dashboard LaunchAgent installed — always on at %s\n", tick(), url)
				return openDashboard(ctx, url)
			}

			if err := dashboardResponding(ctx, url); err != nil {
				if !launchagent.Loaded(ctx, config.Current().DashboardLaunchAgentID) {
					return fmt.Errorf("dashboard is not running at %s — run `%s dashboard --install`", url, bin())
				}
				fmt.Printf("• dashboard not responding at %s — restarting LaunchAgent\n", url)
				if err := launchagent.KickstartDashboard(ctx); err != nil {
					return err
				}
				if err := waitForDashboard(ctx, url, dashboardStartupTimeout); err != nil {
					return fmt.Errorf("dashboard did not become healthy at %s: %w", url, err)
				}
			}

			fmt.Printf("%s dashboard running at %s\n", tick(), url)
			return openDashboard(ctx, url)
		},
	}
	c.Flags().BoolVar(&install, "install", false, "install the always-on dashboard LaunchAgent before opening it")
	c.Flags().BoolVar(&serve, "serve", false, "serve the dashboard (used by the LaunchAgent)")
	_ = c.Flags().MarkHidden("serve")
	return c
}
