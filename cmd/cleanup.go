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
	"unicode"

	"github.com/gordonbeeming/shunt/internal/gitpreservation"
	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"golang.org/x/text/width"
)

type cleanupCandidate struct {
	Name             string
	Status           string
	Dirty            bool
	ProtectionReason string
}

type protectedSiding struct{ Name, Reason string }

var (
	commandLoadCurrentApp = loadCurrentApp
	commandRemoveSiding   = removeSiding
	newCommandAnalyzer    = func(repo string) *gitpreservation.Analyzer {
		return gitpreservation.NewAnalyzer(repo, gitpreservation.Options{})
	}
)

func newCleanupCmd() *cobra.Command {
	var force bool
	var nextBase string
	c := &cobra.Command{
		Use:   "cleanup",
		Short: "Select and permanently remove one or more sidings",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			in := bufio.NewReader(os.Stdin)
			app, _, err := commandLoadCurrentApp()
			if err != nil {
				return err
			}
			if app.Removal != nil {
				return commandRemoveSiding(ctx, &app, app.Removal.Siding, force, "")
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
			var safety map[string]*removalSafety
			if !force {
				deletionRefs, err := resolveSelectedRemovalRefs(ctx, app, selected)
				if err != nil {
					return err
				}
				safety, err = captureSelectedRemovalSafety(ctx, app, selected, deletionRefs)
				if err != nil {
					return err
				}
				dirty, err := protectedSelectedSidings(ctx, app, selected, safety)
				if err != nil {
					return err
				}
				if len(dirty) > 0 {
					confirmed, err := confirmProtectedCleanup(dirty, in, os.Stdout)
					if err != nil {
						return err
					}
					if !confirmed {
						fmt.Println("cleanup cancelled")
						return nil
					}
					for _, item := range dirty {
						if snapshot := safety[item.Name]; snapshot != nil {
							snapshot.ExplicitDiscard = true
						}
					}
				}
			}
			removedBase := app.BaseSiding
			successor, err := prepareBaseRemoval(app, selected, nextBase, in)
			if err != nil {
				return err
			}
			selected = orderBaseLast(selected, removedBase)
			for _, name := range selected {
				next := ""
				if name == removedBase {
					next = successor
				}
				var expected *removalSafety
				if safety != nil {
					expected = safety[name]
				}
				if err := commandRemoveSiding(ctx, &app, name, force, next, expected); err != nil {
					return fmt.Errorf("clean up siding %q: %w", name, err)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&force, "force", "f", false, "skip live-siding and uncommitted-change safety checks")
	c.Flags().StringVar(&nextBase, "next-base", "", "successor source base when the current base is selected, or `-` to keep the commit and leave no siding as base")
	return c
}

func resolveSelectedRemovalRefs(ctx context.Context, app state.App, selected []string) ([]string, error) {
	refs := map[string]bool{}
	selectedSet := map[string]bool{}
	for _, name := range selected {
		selectedSet[name] = true
		if sd, ok := app.Sidings[name]; ok && sd.Branch != "" {
			refs["refs/heads/"+sd.Branch] = true
		}
	}
	for _, name := range selected {
		src, _, err := siding.Paths(app, name)
		if err != nil {
			return nil, err
		}
		branch, err := currentWorktreeBranch(ctx, src)
		if err != nil {
			continue
		}
		// A branch owned by any OTHER siding is a collision, whether that siding
		// survives or is selected too. Checking only survivors let a selected pair
		// through: removing the first deletes the ref the second's worktree is
		// still on, and the second then cannot resolve HEAD, so the batch aborts
		// half-done with some sidings already gone.
		for owner, sd := range app.Sidings {
			if owner == name || sd.Branch != branch {
				continue
			}
			if selectedSet[owner] {
				return nil, fmt.Errorf("checked-out branch %q for %q is also owned by selected siding %q; remove them one at a time so the ref is not deleted while the other worktree is still on it", branch, name, owner)
			}
			return nil, fmt.Errorf("checked-out branch %q for %q is owned by surviving siding %q", branch, name, owner)
		}
		refs["refs/heads/"+branch] = true
	}
	result := make([]string, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	sort.Strings(result)
	return result, nil
}

func captureSelectedRemovalSafety(ctx context.Context, app state.App, selected, deletionRefs []string) (map[string]*removalSafety, error) {
	result := make(map[string]*removalSafety, len(selected))
	analyzers := map[string]*gitpreservation.Analyzer{}
	for _, name := range selected {
		owner := state.WorktreeOwner(app, app.Sidings[name])
		analyzer := analyzers[owner]
		if analyzer == nil {
			analyzer = newCommandAnalyzer(owner)
			analyzers[owner] = analyzer
		}
		snapshot, err := captureRemovalSafetyWithAnalyzer(ctx, app, name, deletionRefs, analyzer)
		if err != nil {
			return nil, err
		}
		result[name] = &snapshot
	}
	return result, nil
}

func buildCleanupCandidates(ctx context.Context, app state.App, checkDirty bool) ([]cleanupCandidate, error) {
	names := make([]string, 0, len(app.Sidings))
	for name := range app.Sidings {
		names = append(names, name)
	}
	sort.Strings(names)
	statuses := sidingStatuses(ctx, app, names)
	analyzers := map[string]*gitpreservation.Analyzer{}
	candidates := make([]cleanupCandidate, 0, len(names))
	for _, name := range names {
		candidate := cleanupCandidate{Name: name, Status: statuses[name]}
		if checkDirty {
			owner := state.WorktreeOwner(app, app.Sidings[name])
			if owner == "" {
				if src, _, pathErr := siding.Paths(app, name); pathErr == nil {
					owner = src
				}
			}
			analyzer := analyzers[owner]
			if analyzer == nil {
				analyzer = newCommandAnalyzer(owner)
				analyzers[owner] = analyzer
			}
			dirty, reason, err := sidingWorktreeProtectionWithAnalyzer(ctx, app, name, []string{name}, analyzer)
			if err != nil {
				return nil, err
			}
			candidate.Dirty = dirty
			candidate.ProtectionReason = reason
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func sidingWorktreeHasChanges(ctx context.Context, app state.App, name string, removing []string) (bool, error) {
	protected, _, err := sidingWorktreeProtection(ctx, app, name, removing)
	return protected, err
}

func sidingWorktreeProtection(ctx context.Context, app state.App, name string, removing []string) (bool, string, error) {
	return sidingWorktreeProtectionWithAnalyzer(ctx, app, name, removing, nil)
}

func sidingWorktreeProtectionWithAnalyzer(ctx context.Context, app state.App, name string, removing []string, analyzer *gitpreservation.Analyzer) (bool, string, error) {
	src, _, err := siding.Paths(app, name)
	if err != nil {
		return false, "", err
	}
	if _, statErr := os.Stat(src); errors.Is(statErr, os.ErrNotExist) {
		return true, "worktree is missing; committed and uncommitted state cannot be inspected", nil
	} else if statErr != nil {
		return false, "", fmt.Errorf("inspect siding worktree %s: %w", src, statErr)
	}
	status, err := proc.Run(ctx, "git", "-C", src, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return false, "", fmt.Errorf("check siding worktree %s: %w", src, err)
	}
	if output := strings.TrimSpace(status.Stdout); output != "" {
		if strings.Contains(output, "?? ") {
			return true, "dirty worktree with untracked files", nil
		}
		return true, "dirty worktree with uncommitted changes", nil
	}
	branches := make([]string, 0, len(removing))
	for _, candidate := range removing {
		if siding, ok := app.Sidings[candidate]; ok {
			branches = append(branches, siding.Branch)
		}
	}
	checkedOut, err := currentWorktreeBranch(ctx, src)
	if err != nil {
		return true, "worktree branch is detached or unavailable", nil
	}
	for survivor, siding := range app.Sidings {
		if !containsName(removing, survivor) && siding.Branch == checkedOut {
			return true, fmt.Sprintf("checked-out branch %q is owned by surviving siding %q", checkedOut, survivor), nil
		}
	}
	if checkedOut != app.Sidings[name].Branch {
		branches = append(branches, checkedOut)
	}
	deletions := plannedBranchRefs(branches)
	if analyzer == nil {
		owner := state.WorktreeOwner(app, app.Sidings[name])
		if owner == "" {
			owner = src
		}
		analyzer = newCommandAnalyzer(owner)
	}
	observedResult := analyzer.Analyze(ctx, "refs/heads/"+checkedOut, deletions)
	var recordedResult gitpreservation.Result
	if checkedOut != app.Sidings[name].Branch {
		recordedResult = analyzer.Analyze(ctx, "refs/heads/"+app.Sidings[name].Branch, deletions)
	} else {
		recordedResult = observedResult
	}
	if !observedResult.Preserved {
		return true, fmt.Sprintf("branch %q is not proven preserved: %s", checkedOut, observedResult.Reason), nil
	}
	if !recordedResult.Preserved {
		return true, fmt.Sprintf("recorded branch %q is not proven preserved: %s", app.Sidings[name].Branch, recordedResult.Reason), nil
	}
	return false, "", nil
}

func plannedBranchRefs(branches []string) []string {
	refs := make([]string, 0, len(branches))
	for _, branch := range branches {
		refs = append(refs, "refs/heads/"+branch)
	}
	return refs
}

func worktreeHasChanges(ctx context.Context, src, branch string, removing []string) (bool, error) {
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

	removed := map[string]bool{"refs/heads/" + branch: true}
	for _, branch := range removing {
		removed["refs/heads/"+branch] = true
	}
	deletions := make([]string, 0, len(removed))
	for ref := range removed {
		deletions = append(deletions, ref)
	}
	sort.Strings(deletions)
	result := gitpreservation.Analyze(ctx, gitpreservation.Request{Repo: src, TargetRef: "refs/heads/" + branch, DeletionRefs: deletions})
	return !result.Preserved, nil
}

func pickCleanupCandidates(candidates []cleanupCandidate, in *bufio.Reader) ([]string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return pickCleanupByNumber(candidates, in, os.Stdout)
	}
	return pickCleanupInteractive(candidates, fd, in)
}

func pickCleanupInteractive(candidates []cleanupCandidate, fd int, in *bufio.Reader) ([]string, error) {
	if width, _, err := term.GetSize(fd); err != nil || width <= 0 {
		return pickCleanupByNumber(candidates, in, os.Stdout)
	}
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
			width, _, sizeErr := term.GetSize(fd)
			if sizeErr != nil || width <= 0 {
				width = 1
			}
			available := width - 4
			if available < 1 {
				available = 1
			}
			row = truncateTerminalRow(row, available)
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

func truncateTerminalRow(value string, width int) string {
	if width <= 1 {
		return "…"
	}
	used := 0
	runes := []rune(value)
	cut := len(runes)
	for index, r := range runes {
		cells := terminalRuneWidth(r)
		if used+cells > width {
			cut = index
			break
		}
		used += cells
	}
	if cut == len(runes) {
		return value
	}
	limit := width - 1
	used = 0
	cut = 0
	for index, r := range runes {
		cells := terminalRuneWidth(r)
		if used+cells > limit {
			break
		}
		used += cells
		cut = index + 1
	}
	return string(runes[:cut]) + "…"
}

func terminalRuneWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return 0
	}
	kind := width.LookupRune(r).Kind()
	if kind == width.EastAsianWide || kind == width.EastAsianFullwidth || r >= 0x1f300 && r <= 0x1faff {
		return 2
	}
	return 1
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
		if candidate.ProtectionReason == "" {
			return candidate.Status + ", work not safely saved"
		}
		return candidate.Status + ", protected: " + candidate.ProtectionReason
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

func protectedSelectedSidings(ctx context.Context, app state.App, selected []string, snapshots map[string]*removalSafety) ([]protectedSiding, error) {
	var dirty []protectedSiding
	for _, name := range selected {
		src, _, err := siding.Paths(app, name)
		if err != nil {
			return nil, err
		}
		if _, statErr := os.Stat(src); errors.Is(statErr, os.ErrNotExist) {
			dirty = append(dirty, protectedSiding{Name: name, Reason: "worktree is missing; state cannot be inspected"})
			continue
		} else if statErr != nil {
			return nil, statErr
		}
		status, err := proc.Run(ctx, "git", "-C", src, "status", "--porcelain=v1", "--untracked-files=normal")
		if err != nil {
			return nil, err
		}
		if output := strings.TrimSpace(status.Stdout); output != "" {
			reason := "dirty worktree with uncommitted changes"
			if strings.Contains(output, "?? ") {
				reason = "dirty worktree with untracked files"
			}
			dirty = append(dirty, protectedSiding{Name: name, Reason: reason})
			continue
		}
		if snapshot := snapshots[name]; snapshot != nil {
			for _, target := range snapshot.Targets {
				if !target.Preserved {
					dirty = append(dirty, protectedSiding{Name: name, Reason: fmt.Sprintf("branch %q is not proven preserved: %s", strings.TrimPrefix(target.Ref, "refs/heads/"), target.Reason)})
					break
				}
			}
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
	protected := make([]protectedSiding, 0, len(names))
	for _, name := range names {
		protected = append(protected, protectedSiding{Name: name, Reason: "work not safely saved"})
	}
	return confirmProtectedCleanup(protected, in, out)
}

func confirmProtectedCleanup(names []protectedSiding, in *bufio.Reader, out io.Writer) (bool, error) {
	fmt.Fprintln(out, "The following sidings have work that is not safely saved:")
	for _, item := range names {
		fmt.Fprintf(out, "  - %s: %s\n", item.Name, item.Reason)
	}
	fmt.Fprint(out, "Permanently remove them and discard those changes? [y/N] ")
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.TrimSpace(line)
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes"), nil
}
