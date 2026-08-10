package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/gordonbeeming/shunt/internal/storage"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newTrimCmd() *cobra.Command {
	var dryRun, yes bool
	command := &cobra.Command{
		Use:   "trim <siding>",
		Short: "Preview or remove only ignored, untracked generated directories",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			name := args[0]
			if _, exists := app.Sidings[name]; !exists {
				return fmt.Errorf("no siding %q", name)
			}
			src, _, err := siding.Paths(app, name)
			if err != nil {
				return err
			}
			candidates, err := storage.ScanTrimCandidates(cmd.Context(), src)
			if err != nil {
				return fmt.Errorf("scan trim candidates for %q: %w", name, err)
			}
			candidateBytes := printTrimPreview(cmd.OutOrStdout(), name, candidates)
			if dryRun || len(candidates) == 0 {
				return nil
			}
			if err := confirmTrim(cmd.InOrStdin(), cmd.OutOrStdout(), yes, len(candidates), candidateBytes); err != nil {
				return err
			}
			var result storage.TrimResult
			result, err = removeConfirmedTrim(cmd.Context(), app.ConfigDir, name, candidates)
			if err != nil {
				return err
			}
			if result.FilesystemObservation == "observed" {
				fmt.Fprintf(cmd.OutOrStdout(), "removed %d generated directories (%s preview logical); filesystem free-space delta: %s\n",
					len(candidates), formatBytes(result.CandidateBytes), signedBytes(result.FilesystemFreeDelta))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "removed %d generated directories (%s preview logical); filesystem free-space observation unavailable (%s)\n",
					len(candidates), formatBytes(result.CandidateBytes), result.FilesystemDetail)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "preview eligible generated directories without deleting them")
	command.Flags().BoolVarP(&yes, "yes", "y", false, "confirm deletion (required outside an interactive terminal)")
	return command
}

type trimLockedDependencies struct {
	withSidingOperation func(context.Context, string, string, func() error) error
	loadApp             func(string) (state.App, error)
	ensureNoRemoval     func(state.App, string) error
	paths               func(state.App, string) (string, string, error)
	scan                func(context.Context, string) ([]storage.TrimCandidate, error)
	remove              func(context.Context, string, []storage.TrimCandidate) (storage.TrimResult, error)
}

func removeConfirmedTrim(ctx context.Context, configDir, name string, preview []storage.TrimCandidate) (storage.TrimResult, error) {
	return removeConfirmedTrimWith(ctx, configDir, name, preview, trimLockedDependencies{
		withSidingOperation: siding.WithSidingOperation,
		loadApp:             state.LoadApp,
		ensureNoRemoval:     siding.EnsureNoRemovalInProgress,
		paths:               siding.Paths,
		scan:                storage.ScanTrimCandidates,
		remove:              storage.RemoveTrimCandidates,
	})
}

func removeConfirmedTrimWith(ctx context.Context, configDir, name string, preview []storage.TrimCandidate, deps trimLockedDependencies) (storage.TrimResult, error) {
	var result storage.TrimResult
	err := deps.withSidingOperation(ctx, configDir, name, func() error {
		app, err := deps.loadApp(configDir)
		if err != nil {
			return fmt.Errorf("reload project state before trim: %w", err)
		}
		if err := deps.ensureNoRemoval(app, "trim"); err != nil {
			return err
		}
		if _, exists := app.Sidings[name]; !exists {
			return fmt.Errorf("no siding %q after acquiring the trim lock", name)
		}
		src, _, err := deps.paths(app, name)
		if err != nil {
			return err
		}
		lockedCandidates, err := deps.scan(ctx, src)
		if err != nil {
			return fmt.Errorf("rescan trim candidates for %q under lock: %w", name, err)
		}
		if !storage.SameTrimCandidateSet(preview, lockedCandidates) {
			return fmt.Errorf("trim candidates changed after confirmation; rerun `%s trim %s` to review and confirm the current preview", bin(), name)
		}
		result, err = deps.remove(ctx, src, lockedCandidates)
		return err
	})
	return result, err
}

func printTrimPreview(out io.Writer, name string, candidates []storage.TrimCandidate) int64 {
	fmt.Fprintf(out, "trim preview for %q:\n", name)
	var total int64
	for _, candidate := range candidates {
		total += candidate.LogicalBytes
		fmt.Fprintf(out, "  %-48s %s logical\n", candidate.RelativePath, formatBytes(candidate.LogicalBytes))
	}
	if len(candidates) == 0 {
		fmt.Fprintln(out, "  no ignored, untracked generated directories found")
	}
	fmt.Fprintf(out, "candidate total: %s logical (not an estimate of physical space reclaimed)\n", formatBytes(total))
	return total
}

func confirmTrim(in io.Reader, out io.Writer, yes bool, count int, logicalBytes int64) error {
	if yes {
		return nil
	}
	file, interactive := in.(*os.File)
	if !interactive || !term.IsTerminal(int(file.Fd())) {
		return errors.New("trim deletion requires --yes outside an interactive terminal; use --dry-run to preview")
	}
	fmt.Fprintf(out, "Remove %d directories (%s logical)? [y/N] ", count, formatBytes(logicalBytes))
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read trim confirmation: %w", err)
	}
	answer = strings.TrimSpace(answer)
	if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
		return errors.New("trim cancelled")
	}
	return nil
}

func signedBytes(value int64) string {
	if value < 0 {
		return "-" + formatBytes(-value)
	}
	return "+" + formatBytes(value)
}
