package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gordonbeeming/shunt/internal/databaseline"
	"github.com/gordonbeeming/shunt/internal/resolve"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

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
			app, loc, err := loadCurrentApp()
			if err != nil {
				return err
			}
			name, err := resolveDataPromoteSource(cmd.Context(), app, loc, args, func(ctx context.Context, app state.App) (string, error) {
				return sidingArg(ctx, app, nil)
			})
			if err != nil {
				return err
			}
			if name == state.HostTarget {
				return fmt.Errorf("%q cannot be promoted as a data source", state.HostTarget)
			}
			if _, exists := app.Sidings[name]; !exists {
				return fmt.Errorf("no siding %q", name)
			}
			if len(app.Volumes) == 0 {
				return errors.New("no dataVolumes are declared for this app")
			}
			prompt := dataPromotePrompt(name, app.Volumes)
			if err := confirmDataChange(force, prompt, bufio.NewReader(os.Stdin), os.Stdout); err != nil {
				return err
			}
			result, err := siding.PromoteData(cmd.Context(), app, name, os.Stdout)
			if err != nil {
				if warning, ok := committedDataWarning("data baseline", result, err); ok {
					fmt.Fprintln(cmd.ErrOrStderr(), warning)
				} else {
					return formatDataPromoteError(result, err)
				}
			}
			fmt.Printf("%s data baseline promoted from %q\n", tick(), name)
			return nil
		},
	}
	c.Flags().BoolVarP(&force, "force", "f", false, "do not ask for confirmation (required outside an interactive terminal)")
	return c
}

func formatDataPromoteError(result databaseline.Result, err error) error {
	var restoreErr *databaseline.RestoreError
	var cleanupErr *databaseline.CommittedCleanupError
	var durabilityErr *databaseline.CommittedDurabilityError
	switch {
	case errors.As(err, &durabilityErr):
		return fmt.Errorf("data baseline is visible but durability is unconfirmed; do not retry: %w", err)
	case errors.As(err, &restoreErr) && restoreErr.Committed:
		return fmt.Errorf("data baseline committed, but restore failed (details=%v, recovery=%v): %w", result.Restore.Details, result.RecoveryPaths, err)
	case errors.As(err, &restoreErr):
		return fmt.Errorf("data baseline was not committed and restore failed (details=%v, recovery=%v): %w", result.Restore.Details, result.RecoveryPaths, err)
	case result.Committed && errors.As(err, &cleanupErr):
		return fmt.Errorf("data baseline committed with a cleanup warning (recovery=%v): %w", result.RecoveryPaths, err)
	case result.Committed:
		return fmt.Errorf("data baseline committed with a follow-up failure (recovery=%v): %w", result.RecoveryPaths, err)
	default:
		return err
	}
}

func dataPromotePrompt(name string, volumes []string) string {
	return fmt.Sprintf("Promote the complete data set from %q (%s)? This replaces the canonical baseline for future new sidings and reapply --fresh-data, keeps one rollback generation, leaves existing siding copies unchanged, and briefly pauses the application and volume consumers.", name, strings.Join(volumes, ", "))
}

type dataSourcePicker func(context.Context, state.App) (string, error)

func resolveDataPromoteSource(ctx context.Context, app state.App, loc resolve.Location, args []string, pick dataSourcePicker) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	if loc.Siding != "" {
		return loc.Siding, nil
	}
	if app.LiveSiding != "" && app.LiveSiding != state.HostTarget {
		if _, ok := app.Sidings[app.LiveSiding]; ok {
			return app.LiveSiding, nil
		}
	}
	return pick(ctx, app)
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
			if len(app.Volumes) == 0 {
				return errors.New("no dataVolumes are declared for this app")
			}
			if err := confirmDataChange(force, "Rollback the data baseline to its previous generation?", bufio.NewReader(os.Stdin), os.Stdout); err != nil {
				return err
			}
			result, err := siding.RollbackData(cmd.Context(), app)
			if err != nil {
				if warning, ok := committedDataWarning("data baseline rollback", result, err); ok {
					fmt.Fprintln(cmd.ErrOrStderr(), warning)
				} else {
					return err
				}
			}
			fmt.Printf("%s data baseline rolled back\n", tick())
			return nil
		},
	}
	c.Flags().BoolVarP(&force, "force", "f", false, "do not ask for confirmation (required outside an interactive terminal)")
	return c
}

func committedDataWarning(operation string, result databaseline.Result, err error) (string, bool) {
	var durabilityErr *databaseline.CommittedDurabilityError
	if result.Committed && errors.As(err, &durabilityErr) {
		return fmt.Sprintf("warning: %s committed but durability is unconfirmed; do not retry: %v", operation, durabilityErr), true
	}
	var cleanupErr *databaseline.CommittedCleanupError
	if !result.Committed || !errors.As(err, &cleanupErr) {
		return "", false
	}
	return fmt.Sprintf("warning: %s committed with follow-up cleanup work (recovery=%v): %v", operation, result.RecoveryPaths, cleanupErr), true
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
