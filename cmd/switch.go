package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
)

func newSwitchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "switch [name]",
		Short: "Point the stable front door at a siding (live, no restart)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			if len(app.Sidings) == 0 {
				return fmt.Errorf("no sidings yet — run `%s new <name>` first", bin())
			}
			var name string
			if len(args) == 1 {
				name = args[0]
			} else if name, err = pickSiding(app); err != nil {
				return err
			}
			return switchTo(ctx, &app, name)
		},
	}
}

// switchTo activates the siding if needed, repoints Caddy, and persists.
func switchTo(ctx context.Context, app *state.App, name string) error {
	sd, ok := app.Sidings[name]
	if !ok {
		return fmt.Errorf("no siding %q", name)
	}
	if len(sd.Bridges) == 0 {
		fmt.Println("• siding not activated yet — discovering + bridging…")
		if err := siding.Activate(ctx, *app, &sd); err != nil {
			return err
		}
	}
	if err := siding.PointCaddy(ctx, *app, &sd); err != nil {
		return err
	}
	app.LiveSiding = name
	app.Sidings[name] = sd
	if err := state.SaveApp(*app); err != nil {
		return err
	}
	fmt.Printf("✓ switched to %q\n", name)
	for _, r := range app.FrontDoor {
		fmt.Printf("  localhost:%d  ->  %s:%d  (%s)\n", r.ListenPort, sd.LastIP, sd.Bridges[r.Key], r.Key)
	}
	return nil
}

// pickSiding presents a numbered list and reads a choice from stdin.
func pickSiding(app state.App) (string, error) {
	names := make([]string, 0, len(app.Sidings))
	for n := range app.Sidings {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 1 {
		return names[0], nil
	}
	fmt.Println("Select a siding:")
	for i, n := range names {
		marker := " "
		if app.LiveSiding == n {
			marker = "*"
		}
		fmt.Printf("  %d) %s %s\n", i+1, marker, n)
	}
	fmt.Print("> ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read selection: %w", err)
	}
	idx, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || idx < 1 || idx > len(names) {
		return "", fmt.Errorf("invalid selection %q", strings.TrimSpace(line))
	}
	return names[idx-1], nil
}
