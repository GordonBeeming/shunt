package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type cleanupCandidate struct {
	Name   string
	Status string
	Dirty  bool
}

func newCleanupCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "cleanup",
		Short: "Select and permanently remove one or more sidings",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			in := bufio.NewReader(os.Stdin)
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			if len(app.Sidings) == 0 {
				fmt.Println("no sidings to clean up")
				return nil
			}

			candidates, err := buildCleanupCandidates(ctx, app, !force)
			if err != nil {
				return err
			}
			selected, err := pickCleanupCandidates(candidates, in)
			if err != nil {
				return err
			}
			if len(selected) == 0 {
				fmt.Println("no sidings selected")
				return nil
			}

			if app.LiveSiding != "" && containsName(selected, app.LiveSiding) && !force {
				return fmt.Errorf("siding %q is live — switch away first, or pass --force", app.LiveSiding)
			}
			if !force {
				dirty, err := dirtySelectedSidings(ctx, app, selected)
				if err != nil {
					return err
				}
				if len(dirty) > 0 {
					confirmed, err := confirmDirtyCleanup(dirty, in, os.Stdout)
					if err != nil {
						return err
					}
					if !confirmed {
						fmt.Println("cleanup cancelled")
						return nil
					}
				}
			}

			for _, name := range selected {
				if err := removeSiding(ctx, &app, name); err != nil {
					return fmt.Errorf("clean up siding %q: %w", name, err)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&force, "force", "f", false, "skip live-siding and uncommitted-change safety checks")
	return c
}

func buildCleanupCandidates(ctx context.Context, app state.App, checkDirty bool) ([]cleanupCandidate, error) {
	names := make([]string, 0, len(app.Sidings))
	for name := range app.Sidings {
		names = append(names, name)
	}
	sort.Strings(names)
	statuses := sidingStatuses(ctx, app, names)
	candidates := make([]cleanupCandidate, 0, len(names))
	for _, name := range names {
		candidate := cleanupCandidate{Name: name, Status: statuses[name]}
		if checkDirty {
			dirty, err := sidingWorktreeHasChanges(ctx, app, name, []string{name})
			if err != nil {
				return nil, err
			}
			candidate.Dirty = dirty
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func sidingWorktreeHasChanges(ctx context.Context, app state.App, name string, removing []string) (bool, error) {
	src, _ := siding.Paths(app, name)
	branches := make([]string, 0, len(removing))
	for _, candidate := range removing {
		if siding, ok := app.Sidings[candidate]; ok {
			branches = append(branches, siding.Branch)
		}
	}
	return worktreeHasChanges(ctx, src, branches)
}

func worktreeHasChanges(ctx context.Context, src string, removing []string) (bool, error) {
	info, err := os.Stat(src)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil // fail closed: removing the branch may still lose committed work
	}
	if err != nil {
		return false, fmt.Errorf("inspect siding worktree %s: %w", src, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("siding worktree %s is not a directory", src)
	}
	out, err := proc.Run(ctx, "git", "-C", src, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return false, fmt.Errorf("check siding worktree %s for uncommitted changes: %w", src, err)
	}
	if strings.TrimSpace(out.Stdout) != "" {
		return true, nil
	}

	branch, err := proc.Run(ctx, "git", "-C", src, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		return false, fmt.Errorf("identify siding branch %s: %w", src, err)
	}
	refs, err := proc.Run(ctx, "git", "-C", src, "for-each-ref", "--format=%(refname)", "--contains=HEAD", "refs/heads", "refs/remotes")
	if err != nil {
		return false, fmt.Errorf("check whether siding commits are reachable: %w", err)
	}
	removed := map[string]bool{strings.TrimSpace(branch.Stdout): true}
	for _, branch := range removing {
		removed["refs/heads/"+branch] = true
	}
	for _, ref := range strings.Fields(refs.Stdout) {
		if !removed[ref] {
			return false, nil
		}
	}
	// A clean worktree is still unsafe to remove when its checked-out branch is
	// the only ref that reaches HEAD: deleting it would lose committed work.
	return true, nil
}

func pickCleanupCandidates(candidates []cleanupCandidate, in *bufio.Reader) ([]string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return pickCleanupByNumber(candidates, in, os.Stdout)
	}
	return pickCleanupInteractive(candidates, fd, in)
}

func pickCleanupInteractive(candidates []cleanupCandidate, fd int, in *bufio.Reader) ([]string, error) {
	old, err := term.MakeRaw(fd)
	if err != nil {
		return pickCleanupByNumber(candidates, in, os.Stdout)
	}
	defer term.Restore(fd, old)

	selected := make([]bool, len(candidates))
	cursor := 0
	draw := func(first bool) {
		if !first {
			fmt.Fprintf(os.Stdout, "\x1b[%dA", len(candidates)+1)
		}
		fmt.Fprint(os.Stdout, "\rSelect sidings to clean up (↑/↓ move, Space toggle, a all, Enter continue, q quits):\x1b[K\r\n")
		for i, candidate := range candidates {
			check := " "
			if selected[i] {
				check = "x"
			}
			row := fmt.Sprintf("[%s] %s  (%s)", check, candidate.Name, cleanupStatus(candidate))
			if i == cursor {
				fmt.Fprintf(os.Stdout, "\r\x1b[7m> %s \x1b[0m\x1b[K\r\n", row)
			} else {
				fmt.Fprintf(os.Stdout, "\r  %s\x1b[K\r\n", row)
			}
		}
	}
	draw(true)
	for {
		b, err := in.ReadByte()
		if err != nil {
			fmt.Fprint(os.Stdout, "\r\n")
			return nil, err
		}
		switch {
		case b == '\r' || b == '\n':
			fmt.Fprint(os.Stdout, "\r\n")
			return selectedCleanupNames(candidates, selected), nil
		case b == 3 || b == 'q':
			fmt.Fprint(os.Stdout, "\r\n")
			return nil, fmt.Errorf("cancelled")
		case b == ' ':
			selected[cursor] = !selected[cursor]
			draw(false)
		case b == 'a':
			allSelected := true
			for _, value := range selected {
				allSelected = allSelected && value
			}
			for i := range selected {
				selected[i] = !allSelected
			}
			draw(false)
		case b == 0x1b:
			b2, _ := in.ReadByte()
			b3, _ := in.ReadByte()
			if b2 == '[' || b2 == 'O' {
				if b3 == 'A' && cursor > 0 {
					cursor--
				} else if b3 == 'B' && cursor < len(candidates)-1 {
					cursor++
				}
			}
			draw(false)
		}
	}
}

func pickCleanupByNumber(candidates []cleanupCandidate, in *bufio.Reader, out io.Writer) ([]string, error) {
	fmt.Fprintln(out, "Select sidings to clean up (comma-separated numbers, 'all', or 'q'):")
	for i, candidate := range candidates {
		fmt.Fprintf(out, "  %d) %s  (%s)\n", i+1, candidate.Name, cleanupStatus(candidate))
	}
	fmt.Fprint(out, "> ")
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read selection: %w", err)
	}
	return parseCleanupSelection(line, candidates)
}

func parseCleanupSelection(line string, candidates []cleanupCandidate) ([]string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, nil
	}
	if line == "q" || strings.EqualFold(line, "quit") {
		return nil, fmt.Errorf("cancelled")
	}
	if strings.EqualFold(line, "all") {
		out := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			out = append(out, candidate.Name)
		}
		return out, nil
	}

	var out []string
	seen := make(map[int]bool, len(candidates))
	for _, token := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' }) {
		index, err := strconv.Atoi(strings.TrimSpace(token))
		if err != nil || index < 1 || index > len(candidates) {
			return nil, fmt.Errorf("invalid selection %q", token)
		}
		if !seen[index] {
			seen[index] = true
			out = append(out, candidates[index-1].Name)
		}
	}
	return out, nil
}

func cleanupStatus(candidate cleanupCandidate) string {
	if candidate.Dirty {
		return candidate.Status + ", work not safely saved"
	}
	return candidate.Status
}

func selectedCleanupNames(candidates []cleanupCandidate, selected []bool) []string {
	var names []string
	for i, candidate := range candidates {
		if selected[i] {
			names = append(names, candidate.Name)
		}
	}
	return names
}

func dirtySelectedSidings(ctx context.Context, app state.App, selected []string) ([]string, error) {
	var dirty []string
	for _, name := range selected {
		hasChanges, err := sidingWorktreeHasChanges(ctx, app, name, selected)
		if err != nil {
			return nil, err
		}
		if hasChanges {
			dirty = append(dirty, name)
		}
	}
	return dirty, nil
}

func containsName(names []string, target string) bool {
	for _, name := range names {
		if name == target {
			return true
		}
	}
	return false
}

func confirmDirtyCleanup(names []string, in *bufio.Reader, out io.Writer) (bool, error) {
	fmt.Fprintln(out, "The following sidings have work that is not safely saved:")
	for _, name := range names {
		fmt.Fprintf(out, "  - %s\n", name)
	}
	fmt.Fprint(out, "Permanently remove them and discard those changes? [y/N] ")
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.TrimSpace(line)
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes"), nil
}
