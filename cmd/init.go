package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gordonbeeming/shunt/internal/caddy"
	"github.com/gordonbeeming/shunt/internal/config"
	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/image"
	"github.com/gordonbeeming/shunt/internal/launchagent"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "init",
		Short: "Bootstrap shunt: build Caddy + base image, start the proxy and container runtime",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			if err := ensureDirs(); err != nil {
				return err
			}

			fmt.Println("• building Caddy (xcaddy + caddy-l4)…")
			caddyBin, err := caddy.Build(ctx, force)
			if err != nil {
				return err
			}

			fmt.Println("• building base guest image…")
			if err := image.EnsureBuilt(ctx, force); err != nil {
				return err
			}

			fmt.Println("• exporting the dotnet dev certificate for Caddy…")
			if err := caddy.ExportDevCert(ctx); err != nil {
				return err
			}

			fmt.Println("• writing bootstrap config…")
			if err := writeBootstrap(); err != nil {
				return err
			}
			// Make sure the global registry exists so `ls` and `app add` work.
			if _, err := state.LoadRegistry(); err != nil {
				return err
			}

			fmt.Println("• installing Caddy LaunchAgent…")
			bootstrapPath, err := config.BootstrapConfigPath()
			if err != nil {
				return err
			}
			if err := launchagent.Install(ctx, caddyBin, bootstrapPath); err != nil {
				return err
			}

			fmt.Println("• starting container runtime…")
			if err := container.EnsureSystemStarted(ctx); err != nil {
				return err
			}

			fmt.Println("• waiting for Caddy admin API…")
			if err := waitForAdmin(ctx, 30*time.Second); err != nil {
				return fmt.Errorf("caddy admin API never came up: %w", err)
			}

			fmt.Printf("✓ shunt ready (channel=%s, admin=%s)\n", config.Channel, config.AdminBaseURL())
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "rebuild Caddy and the base image even if present")
	return c
}

// ensureDirs creates the per-channel machinery dirs (never project code).
func ensureDirs() error {
	gdir, err := config.GlobalDir()
	if err != nil {
		return err
	}
	logDir, err := config.LogDir()
	if err != nil {
		return err
	}
	for _, d := range []string{
		gdir,
		filepath.Join(gdir, "caddy"),
		filepath.Join(gdir, "caddy", "xdg"),
		logDir,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}

func writeBootstrap() error {
	path, err := config.BootstrapConfigPath()
	if err != nil {
		return err
	}
	doc, err := caddy.Bootstrap()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, doc, 0o644); err != nil {
		return fmt.Errorf("write bootstrap config: %w", err)
	}
	return nil
}

// waitForAdmin polls the Caddy admin API until it answers or the deadline hits.
func waitForAdmin(ctx context.Context, timeout time.Duration) error {
	admin := caddy.NewAdmin()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = admin.Ping(ctx); lastErr == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}
