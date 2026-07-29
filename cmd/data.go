package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gordonbeeming/shunt/internal/databaseline"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// newDataCmd is registered by the command integration layer once the data
// lifecycle adapter is available for every supported runner.
func newDataCmd() *cobra.Command {
	c := &cobra.Command{Use: "data", Short: "Manage the promotable data baseline"}
	c.AddCommand(newDataPromoteCmd(), newDataRollbackCmd())
	return c
}

func newDataPromoteCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "promote [siding]",
		Short: "Promote a siding's complete data set as the project baseline",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			name, err := sidingArg(cmd.Context(), app, args)
			if err != nil {
				return err
			}
			if name == state.HostTarget {
				return fmt.Errorf("%q cannot be promoted as a data source", state.HostTarget)
			}
			if _, exists := app.Sidings[name]; !exists {
				return fmt.Errorf("no siding %q", name)
			}
			if err := confirmDataChange(force, "Promote this siding's data as the new baseline?", bufio.NewReader(os.Stdin), os.Stdout); err != nil {
				return err
			}
			manager, err := databaseline.New(app.ConfigDir, app.Volumes)
			if err != nil {
				return err
			}
			_, sourceRoot := siding.Paths(app, name)
			if _, err := manager.Promote(cmd.Context(), name, sourceRoot); err != nil {
				return err
			}
			fmt.Printf("%s data baseline promoted from %q\n", tick(), name)
			return nil
		},
	}
	c.Flags().BoolVarP(&force, "force", "f", false, "do not ask for confirmation (required outside an interactive terminal)")
	return c
}

func newDataRollbackCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "rollback",
		Short: "Restore the immediately preceding data baseline",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			if err := confirmDataChange(force, "Rollback the data baseline to its previous generation?", bufio.NewReader(os.Stdin), os.Stdout); err != nil {
				return err
			}
			manager, err := databaseline.New(app.ConfigDir, app.Volumes)
			if err != nil {
				return err
			}
			if _, err := manager.Rollback(); err != nil {
				return err
			}
			fmt.Printf("%s data baseline rolled back\n", tick())
			return nil
		},
	}
	c.Flags().BoolVarP(&force, "force", "f", false, "do not ask for confirmation (required outside an interactive terminal)")
	return c
}

func confirmDataChange(force bool, prompt string, in *bufio.Reader, out io.Writer) error {
	if force {
		return nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("data changes require --force outside an interactive terminal")
	}
	fmt.Fprintf(out, "%s [y/N] ", prompt)
	answer, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read confirmation: %w", err)
	}
	answer = strings.TrimSpace(answer)
	if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
		return errors.New("data change cancelled")
	}
	return nil
}
