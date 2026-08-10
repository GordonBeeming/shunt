package cmd

import (
	"context"
	"fmt"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

var readGuestLog = container.Exec

func newLogsCmd() *cobra.Command {
	var lines int
	c := &cobra.Command{
		Use:   "logs [name]",
		Short: "Print a siding's app log (build + startup output)",
		Long: "Dumps the app's log captured inside the guest (/var/log/apphost.log) — the build, " +
			"startup, and any crash output. Useful for diagnosing a failed `up`, by hand or by an agent.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			name, err := sidingArg(ctx, app, args)
			if err != nil {
				return err
			}
			out, err := sidingLog(ctx, app.ConfigDir, name, lines)
			if err != nil {
				return err
			}
			if out == "" {
				fmt.Printf("(no app log yet — has `%s up %s` run?)\n", bin(), name)
				return nil
			}
			fmt.Print(out)
			return nil
		},
	}
	c.Flags().IntVarP(&lines, "tail", "n", 0, "show only the last N lines (default: all)")
	return c
}

func sidingLog(ctx context.Context, configDir, name string, lines int) (string, error) {
	var output string
	err := withLatestSiding(ctx, configDir, name, "read logs", func(_ state.App, sd state.Siding) error {
		if err := siding.RequireGuest(sd); err != nil {
			return err
		}
		read := "cat /var/log/apphost.log 2>/dev/null"
		if lines > 0 {
			read = fmt.Sprintf("tail -n %d /var/log/apphost.log 2>/dev/null", lines)
		}
		var err error
		output, err = readGuestLog(ctx, sd.Container, "sh", "-c", read)
		if err != nil {
			return fmt.Errorf("read log (is the guest running?): %w", err)
		}
		return nil
	})
	return output, err
}
