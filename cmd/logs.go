package cmd

import (
	"fmt"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/spf13/cobra"
)

func newLogsCmd() *cobra.Command {
	var lines int
	c := &cobra.Command{
		Use:   "logs <name>",
		Short: "Print a siding's Aspire AppHost log (build + startup output)",
		Long: "Dumps the AppHost log captured inside the guest (/var/log/apphost.log) — the build, " +
			"dependency startup, and any crash output. Useful for diagnosing a failed `up`, by hand or by an agent.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			sd, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q — `%s ls`", name, bin())
			}
			read := "cat /var/log/apphost.log 2>/dev/null"
			if lines > 0 {
				read = fmt.Sprintf("tail -n %d /var/log/apphost.log 2>/dev/null", lines)
			}
			out, err := container.Exec(ctx, sd.Container, "sh", "-c", read)
			if err != nil {
				return fmt.Errorf("read log (is the guest running?): %w", err)
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
