package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/databaseline"
	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/gitpreservation"
	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newKillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kill [name]",
		Short: "Stop a siding's guest (keeps its worktree, data, and output to restart later)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			app, _, err := commandLoadCurrentApp()
			if err != nil {
				return err
			}
			if err := ensureNoRemovalInProgress(app, "kill"); err != nil {
				return err
			}
			name, err := sidingArg(ctx, app, args)
			if err != nil {
				return err
			}
			if name == state.HostTarget {
				return fmt.Errorf("host execution was removed; choose a siding")
			}
			result, err := siding.Stop(ctx, app, name)
			if err != nil {
				return err
			}
			if result.WasLive {
				gone := "stopped"
				if result.Forced {
					gone = "force-removed"
				}
				fmt.Printf("⚠ %q was live — the front door now points at a %s guest; switch to another siding\n", name, gone)
			}
			if result.Forced {
				fmt.Printf("%s %q was wedged on its cgroup — force-removed it; run `%s up %s` to recreate it (worktree + data are kept)\n", tick(), name, bin(), name)
			} else {
				fmt.Printf("%s stopped %q\n", tick(), name)
			}
			return nil
		},
	}
}

func newParkCmd() *cobra.Command {
	return &cobra.Command{
		Use: "park [name]", Short: "Remove a siding's guest while keeping its worktree, data, and output",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, _, err := loadCurrentApp()
			if err != nil {
				return err
			}
			if err := ensureNoRemovalInProgress(app, "park"); err != nil {
				return err
			}
			name, err := sidingArg(cmd.Context(), app, args)
			if err != nil {
				return err
			}
			if _, err := siding.Park(cmd.Context(), app, name); err != nil {
				return err
			}
			fmt.Printf("%s parked %q — run `%s up %s` to recreate its guest\n", tick(), name, bin(), name)
			return nil
		},
	}
}

func newRmCmd() *cobra.Command {
	var force bool
	var nextBase string
	var promoteData bool
	c := &cobra.Command{
		Use:   "rm [name]",
		Short: "Remove a siding's guest, worktree, data, and output",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			in := bufio.NewReader(os.Stdin)
			app, _, err := commandLoadCurrentApp()
			if err != nil {
				return err
			}
			if app.Removal != nil {
				if len(args) == 1 && args[0] != app.Removal.Siding {
					return ensureNoRemovalInProgress(app, "remove siding "+args[0])
				}
				return commandRemoveSiding(ctx, &app, app.Removal.Siding, force, "")
			}
			name, err := sidingArgWithReader(ctx, app, args, in)
			if err != nil {
				return err
			}
			if name == state.HostTarget {
				return fmt.Errorf("host execution was removed; choose a siding")
			}
			_, ok := app.Sidings[name]
			if !ok {
				return fmt.Errorf("no siding %q", name)
			}
			if app.LiveSiding == name && !force {
				return fmt.Errorf("siding %q is live — switch away first, or pass --force", name)
			}
			var safety *removalSafety
			if !force {
				deletionRefs, err := resolveSelectedRemovalRefs(ctx, app, []string{name})
				if err != nil {
					return err
				}
				snapshot, err := captureRemovalSafety(ctx, app, name, deletionRefs)
				if err != nil {
					return err
				}
				safety = &snapshot
				dirty, reason, err := sidingWorktreeProtection(ctx, app, name, []string{name})
				if err != nil {
					return err
				}
				if dirty {
					confirmed, err := confirmProtectedCleanup([]protectedSiding{{Name: name, Reason: reason}}, in, os.Stdout)
					if err != nil {
						return err
					}
					if !confirmed {
						fmt.Println("removal cancelled")
						return nil
					}
					safety.ExplicitDiscard = true
				}
			}
			successor, err := prepareBaseRemoval(app, []string{name}, nextBase, in)
			if err != nil {
				return err
			}
			return removeSidingWithOptions(ctx, &app, name, force, successor, promoteData, safety)
		},
	}
	c.Flags().BoolVarP(&force, "force", "f", false, "remove even if the siding is live or its worktree has uncommitted changes")
	c.Flags().StringVar(&nextBase, "next-base", "", "successor source base when removing the current base, or `-` to keep the commit and leave no siding as base")
	c.Flags().BoolVar(&promoteData, "promote-data", false, "promote this siding's data as the project baseline before removing it")
	return c
}

type removalSafety struct {
	Removing                []string
	Fingerprint             string
	ObservedBranch          string
	PreservationFingerprint string
	Targets                 []state.RemovalTarget
	ExplicitDiscard         bool
}

func safetyFromJournal(journal *state.RemovalOperation) removalSafety {
	return removalSafety{Removing: append([]string(nil), journal.Removing...), Fingerprint: journal.Safety, ObservedBranch: journal.ObservedWorktreeBranch, PreservationFingerprint: journal.PreservationFingerprint, Targets: append([]state.RemovalTarget(nil), journal.Targets...), ExplicitDiscard: journal.ExplicitDiscard}
}

func removalSafetyEqual(left, right removalSafety) bool {
	return left.Fingerprint == right.Fingerprint && left.PreservationFingerprint == right.PreservationFingerprint && left.ObservedBranch == right.ObservedBranch && left.ExplicitDiscard == right.ExplicitDiscard && reflect.DeepEqual(left.Removing, right.Removing) && reflect.DeepEqual(left.Targets, right.Targets)
}

func hasUnpreservedTargets(targets []state.RemovalTarget) bool {
	for _, target := range targets {
		if !target.Preserved {
			return true
		}
	}
	return false
}

func captureRemovalSafety(ctx context.Context, app state.App, name string, removing []string) (removalSafety, error) {
	return captureRemovalSafetyWithAnalyzer(ctx, app, name, removing, nil)
}

func captureRemovalSafetyWithAnalyzer(ctx context.Context, app state.App, name string, removing []string, analyzer *gitpreservation.Analyzer) (removalSafety, error) {
	src, _, err := siding.Paths(app, name)
	if err != nil {
		return removalSafety{}, err
	}
	return captureRemovalSafetyAtWithAnalyzer(ctx, app, name, removing, src, analyzer)
}

func captureRemovalSafetyAt(ctx context.Context, app state.App, name string, removing []string, src string) (removalSafety, error) {
	return captureRemovalSafetyAtWithAnalyzer(ctx, app, name, removing, src, nil)
}

func captureRemovalSafetyAtWithAnalyzer(ctx context.Context, app state.App, name string, removing []string, src string, analyzer *gitpreservation.Analyzer) (removalSafety, error) {
	if err := ctx.Err(); err != nil {
		return removalSafety{}, err
	}
	sd, ok := app.Sidings[name]
	if !ok {
		return removalSafety{}, fmt.Errorf("no siding %q", name)
	}
	if _, statErr := os.Stat(src); errors.Is(statErr, os.ErrNotExist) {
		return captureMissingRemovalSafety(ctx, app, sd, removing, analyzer)
	} else if statErr != nil {
		return removalSafety{}, fmt.Errorf("inspect worktree for %q: %w", name, statErr)
	}
	dirtySubmodule, err := populatedSubmoduleHasChanges(ctx, src)
	if err != nil {
		return removalSafety{}, fmt.Errorf("inspect populated submodules for %q: %w", name, err)
	}
	if dirtySubmodule {
		return removalSafety{}, fmt.Errorf("siding %q contains dirty populated submodule work; inspect it or rerun with --force only if it may be discarded", name)
	}
	observedBranch, detachedHEAD, err := currentWorktreeBranchState(ctx, src)
	if err != nil {
		return removalSafety{}, fmt.Errorf("inspect checked-out branch for %q: %w", name, err)
	}

	digest := sha256.New()
	writeFingerprintField(digest, "branch", sd.Branch)
	if detachedHEAD {
		writeFingerprintField(digest, "head", "detached")
	} else {
		writeFingerprintField(digest, "head", "branch", observedBranch)
	}
	removingRefs := plannedRemovalRefs(app, removing)
	fingerprintRemovingRefs := make([]string, 0, len(removingRefs))
	for _, ref := range removingRefs {
		if !detachedHEAD && observedBranch != sd.Branch && ref == "refs/heads/"+observedBranch {
			continue
		}
		fingerprintRemovingRefs = append(fingerprintRemovingRefs, ref)
		writeFingerprintField(digest, "removing-ref", ref)
	}
	commands := []struct {
		label string
		args  []string
	}{
		{"status", []string{"status", "--porcelain=v1", "--untracked-files=normal", "--ignore-submodules=none"}},
		{"head", []string{"rev-parse", "--verify", "HEAD^{commit}"}},
	}
	if !detachedHEAD {
		commands = append(commands, struct {
			label string
			args  []string
		}{"symbolic-head", []string{"symbolic-ref", "--quiet", "HEAD"}})
	}
	commands = append(commands, struct {
		label string
		args  []string
	}{"diff", []string{"diff", "--binary", "HEAD", "--"}})
	for _, command := range commands {
		writeFingerprintField(digest, "command", command.label)
		if _, err := proc.RunStreaming(ctx, &contextWriter{ctx: ctx, writer: digest}, io.Discard, "git", append([]string{"-C", src}, command.args...)...); err != nil {
			return removalSafety{}, fmt.Errorf("fingerprint %s for %q: %w", command.label, name, err)
		}
		writeFingerprintField(digest, "command-end", command.label)
	}
	excludedRefs := make(map[string]struct{}, len(fingerprintRemovingRefs))
	for _, ref := range fingerprintRemovingRefs {
		excludedRefs[ref] = struct{}{}
	}
	writeFingerprintField(digest, "command", "refs")
	refs, err := proc.Run(ctx, "git", "-C", src, "for-each-ref", "--sort=refname", "--format=%(refname)", "--contains=HEAD", "refs/heads", "refs/remotes")
	if err != nil {
		return removalSafety{}, fmt.Errorf("fingerprint refs for %q: %w", name, err)
	}
	for _, ref := range strings.Fields(refs.Stdout) {
		if _, excluded := excludedRefs[ref]; !excluded {
			writeFingerprintField(digest, "ref", ref)
		}
	}
	writeFingerprintField(digest, "command-end", "refs")

	paths := &nulRecordWriter{ctx: ctx, handle: func(relative []byte) error {
		return hashUntrackedPath(ctx, digest, src, string(relative))
	}}
	if _, err := proc.RunStreaming(ctx, paths, io.Discard, "git", "-C", src, "ls-files", "--others", "--exclude-standard", "-z"); err != nil {
		return removalSafety{}, fmt.Errorf("list untracked files for %q: %w", name, err)
	}
	if err := paths.finish(); err != nil {
		return removalSafety{}, fmt.Errorf("list untracked files for %q: %w", name, err)
	}
	legacyFingerprint := fmt.Sprintf("%x", digest.Sum(nil))
	observedRef := ""
	if !detachedHEAD {
		observedRef = "refs/heads/" + observedBranch
	}
	if observedRef != "" && !containsName(removingRefs, observedRef) {
		removingRefs = append(removingRefs, observedRef)
		sort.Strings(removingRefs)
	}
	if !detachedHEAD {
		for survivor, survivorSiding := range app.Sidings {
			if survivor != name && !containsName(removing, survivor) && !containsName(removing, "refs/heads/"+survivorSiding.Branch) && survivorSiding.Branch == observedBranch {
				return removalSafety{}, fmt.Errorf("checked-out branch %q is owned by surviving siding %q", observedBranch, survivor)
			}
		}
	}
	preservationDigest := sha256.New()
	targetRefs := []string{"refs/heads/" + sd.Branch}
	if observedRef != "" && observedRef != targetRefs[0] {
		targetRefs = append(targetRefs, observedRef)
	}
	targets := make([]state.RemovalTarget, 0, len(targetRefs))
	if analyzer == nil {
		analyzer = gitpreservation.NewAnalyzer(state.WorktreeOwner(app, sd), gitpreservation.Options{})
	}
	for _, ref := range targetRefs {
		result := analyzer.Analyze(ctx, ref, removingRefs)
		if detachedHEAD {
			result = gitpreservation.Result{Kind: gitpreservation.KindUnproven, Reason: "worktree HEAD is detached; explicit discard confirmation is required"}
		}
		commit, err := gitText(ctx, src, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			commit = ""
		}
		target := state.RemovalTarget{Ref: ref, ExpectedOID: commit, Preserved: result.Preserved, Kind: string(result.Kind), MatchingRef: result.MatchingRef, MatchingCommit: result.MatchingCommit, Reason: result.Reason}
		targets = append(targets, target)
		for _, value := range []string{ref, commit, string(result.Kind), result.MatchingRef, result.MatchingCommit} {
			writeFingerprintField(preservationDigest, "preservation", value)
		}
	}
	return removalSafety{Removing: removingRefs, Fingerprint: legacyFingerprint, ObservedBranch: observedBranch, PreservationFingerprint: fmt.Sprintf("%x", preservationDigest.Sum(nil)), Targets: targets}, nil
}

func captureMissingRemovalSafety(ctx context.Context, app state.App, sd state.Siding, removing []string, analyzer *gitpreservation.Analyzer) (removalSafety, error) {
	owner := state.WorktreeOwner(app, sd)
	removingRefs := plannedRemovalRefs(app, removing)
	digest := sha256.New()
	writeFingerprintField(digest, "branch", sd.Branch)
	writeFingerprintField(digest, "worktree", "missing")
	for _, ref := range removingRefs {
		writeFingerprintField(digest, "removing-ref", ref)
	}
	ref := "refs/heads/" + sd.Branch
	if analyzer == nil {
		analyzer = gitpreservation.NewAnalyzer(owner, gitpreservation.Options{})
	}
	result := analyzer.Analyze(ctx, ref, removingRefs)
	oid := ""
	if value, err := gitText(ctx, owner, "rev-parse", "--verify", ref+"^{commit}"); err == nil {
		oid = value
	}
	target := state.RemovalTarget{Ref: ref, ExpectedOID: oid, Preserved: result.Preserved, Kind: string(result.Kind), MatchingRef: result.MatchingRef, MatchingCommit: result.MatchingCommit, Reason: result.Reason}
	preservation := sha256.New()
	for _, value := range []string{ref, oid, string(result.Kind), result.MatchingRef, result.MatchingCommit} {
		writeFingerprintField(preservation, "preservation", value)
	}
	return removalSafety{Removing: removingRefs, Fingerprint: fmt.Sprintf("%x", digest.Sum(nil)), PreservationFingerprint: fmt.Sprintf("%x", preservation.Sum(nil)), Targets: []state.RemovalTarget{target}}, nil
}

func plannedRemovalRefs(app state.App, removing []string) []string {
	refs := make(map[string]struct{}, len(removing))
	for _, candidate := range removing {
		ref := candidate
		if !strings.HasPrefix(ref, "refs/heads/") {
			sd, exists := app.Sidings[candidate]
			if !exists || sd.Branch == "" {
				continue
			}
			ref = "refs/heads/" + sd.Branch
		}
		refs[ref] = struct{}{}
	}
	result := make([]string, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	sort.Strings(result)
	return result
}

func populatedSubmoduleHasChanges(ctx context.Context, src string) (bool, error) {
	marker := &presenceWriter{}
	var stderr bytes.Buffer
	result, err := proc.RunStreaming(ctx, marker, &stderr, "git", "-C", src, "submodule", "foreach", "--quiet", "--recursive",
		`status=$(git status --porcelain=v1 --untracked-files=normal) || { code=$?; printf 'shunt-submodule-status-error\n' >&2; exit "$code"; }; test -z "$status" || printf x`)
	if err != nil {
		if strings.Contains(stderr.String(), "shunt-submodule-status-error") {
			return false, fmt.Errorf("git status failed inside a populated submodule: %w", err)
		}
		return false, fmt.Errorf("git submodule foreach exited %d: %w", result.ExitCode, err)
	}
	return marker.seen, nil
}

type presenceWriter struct{ seen bool }

func (w *presenceWriter) Write(data []byte) (int, error) {
	w.seen = w.seen || len(data) > 0
	return len(data), nil
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w *contextWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(data)
}

type nulRecordWriter struct {
	ctx    context.Context
	buffer []byte
	handle func([]byte) error
	err    error
}

func (w *nulRecordWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if err := w.ctx.Err(); err != nil {
		w.err = err
		return 0, err
	}
	written := len(data)
	for len(data) > 0 {
		separator := bytes.IndexByte(data, 0)
		if separator < 0 {
			w.buffer = append(w.buffer, data...)
			break
		}
		w.buffer = append(w.buffer, data[:separator]...)
		if len(w.buffer) > 0 {
			if err := w.handle(w.buffer); err != nil {
				w.err = err
				return 0, err
			}
		}
		w.buffer = w.buffer[:0]
		data = data[separator+1:]
	}
	return written, nil
}

func (w *nulRecordWriter) finish() error {
	if w.err != nil {
		return w.err
	}
	if len(w.buffer) != 0 {
		return errors.New("unterminated NUL-delimited path")
	}
	return nil
}

func hashUntrackedPath(ctx context.Context, digest hash.Hash, src, relative string) error {
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return fmt.Errorf("unsafe untracked path %q", relative)
	}
	path := filepath.Join(src, clean)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect untracked file %q: %w", relative, err)
	}
	writeFingerprintField(digest, "untracked", filepath.ToSlash(clean), info.Mode().String(), fmt.Sprintf("%d", info.Size()))
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("read untracked symlink %q: %w", relative, err)
		}
		writeFingerprintField(digest, "symlink", target)
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open untracked file %q: %w", relative, err)
	}
	defer file.Close()
	buffer := make([]byte, 64*1024)
	if _, err := io.CopyBuffer(&contextWriter{ctx: ctx, writer: digest}, file, buffer); err != nil {
		return fmt.Errorf("hash untracked file %q: %w", relative, err)
	}
	writeFingerprintField(digest, "untracked-end", filepath.ToSlash(clean))
	return nil
}

func writeFingerprintField(digest hash.Hash, values ...string) {
	for _, value := range values {
		_, _ = fmt.Fprintf(digest, "%d:", len(value))
		_, _ = io.WriteString(digest, value)
	}
	_, _ = io.WriteString(digest, ";")
}

type removalOperations struct {
	now                func() time.Time
	updateApp          func(context.Context, string, func(*state.App) error) (state.App, error)
	resolveCommit      func(context.Context, string) (string, error)
	ensureControl      func(context.Context, *state.App, string, string) error
	pinBaseCommit      func(context.Context, string, string, string) (string, error)
	promoteBaseline    func(context.Context, state.App, state.Siding, string, string) (databaseline.Result, error)
	observeGuest       func(context.Context, string) container.GuestObservation
	removeGuest        func(context.Context, string) error
	removeWorktree     func(context.Context, string, string, string) error
	quarantineWorktree func(context.Context, string, string, string, string) (fsclone.WorktreeQuarantine, error)
	restoreWorktree    func(fsclone.WorktreeQuarantine) error
	retireWorktree     func(context.Context, fsclone.WorktreeQuarantine) error
	removeFiles        func(state.App, string) error
	forgetOperation    func(context.Context, state.App, string) (databaseline.Result, error)
	afterCheckpoint    func(state.RemovalStage) error
}

func defaultRemovalOperations() removalOperations {
	return removalOperations{
		now:       time.Now,
		updateApp: state.UpdateApp,
		resolveCommit: func(ctx context.Context, source string) (string, error) {
			return gitText(ctx, source, "rev-parse", "--verify", "HEAD^{commit}")
		},
		ensureControl: ensureControlRepository,
		pinBaseCommit: fsclone.PinBaseCommit,
		promoteBaseline: func(ctx context.Context, app state.App, sd state.Siding, operationID, sourceRoot string) (databaseline.Result, error) {
			manager, err := databaseline.New(app.ConfigDir, app.Volumes)
			if err != nil {
				return databaseline.Result{}, err
			}
			if sd.MaterializationPhase == state.PhaseGuest || sd.MaterializationPhase == "" {
				return manager.PromoteWithLifecycleOperation(ctx, operationID, sd.Name, siding.NewDataPromotionLifecycle(app, sd, os.Stdout))
			}
			return manager.PromoteOperation(ctx, operationID, sd.Name, sourceRoot)
		},
		observeGuest:       container.ObserveGuest,
		removeGuest:        container.Remove,
		removeWorktree:     fsclone.RemoveWorktree,
		quarantineWorktree: fsclone.QuarantineWorktree,
		restoreWorktree:    fsclone.RestoreQuarantinedWorktree,
		retireWorktree:     fsclone.RetireQuarantinedWorktree,
		removeFiles:        siding.RemoveFiles,
		forgetOperation: func(ctx context.Context, app state.App, operationID string) (databaseline.Result, error) {
			manager, err := databaseline.New(app.ConfigDir, app.Volumes)
			if err != nil {
				return databaseline.Result{}, err
			}
			return manager.ForgetOperation(ctx, operationID)
		},
	}
}

func removeSiding(ctx context.Context, app *state.App, name string, force bool, successor string, expected ...*removalSafety) error {
	return removeSidingWithOptions(ctx, app, name, force, successor, false, expected...)
}

// removeSidingWithOptions is removeSiding plus the flags that only some callers
// set, kept separate so the common call stays short.
func removeSidingWithOptions(ctx context.Context, app *state.App, name string, force bool, successor string, promoteData bool, expected ...*removalSafety) error {
	return siding.WithProjectSidingOperation(ctx, app.ConfigDir, name, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		*app = current
		if current.Removal != nil && current.Removal.Siding != name {
			return ensureNoRemovalInProgress(current, "remove siding "+name)
		}
		if app.LiveSiding == name && !force && app.Removal == nil {
			return fmt.Errorf("siding %q is live — switch away first, or pass --force", name)
		}
		if app.Removal != nil {
			if force && !app.Removal.Force {
				upgraded, err := state.UpdateApp(ctx, app.ConfigDir, func(latest *state.App) error {
					if latest.Removal == nil || latest.Removal.ID != app.Removal.ID {
						return errors.New("removal journal changed while upgrading force policy")
					}
					latest.Removal.Force = true
					latest.Removal.ExplicitDiscard = true
					return nil
				})
				if err != nil {
					return err
				}
				*app = upgraded
			}
			if !app.Removal.Force {
				sourceRoot, _, err := siding.Paths(*app, name)
				if err != nil {
					return err
				}
				if _, err := os.Lstat(sourceRoot); err == nil {
					if app.Removal.Safety == "" {
						return fmt.Errorf("removal %q has no journaled Git safety evidence; rerun with --force", app.Removal.ID)
					}
					removing := app.Removal.Removing
					if len(removing) == 0 {
						removing = []string{name}
					}
					currentSafety, err := captureRemovalSafety(ctx, *app, name, removing)
					if err != nil {
						return err
					}
					if currentSafety.Fingerprint != app.Removal.Safety {
						return fmt.Errorf("siding %q changed after removal began; inspect it and rerun with --force only if the new work may be discarded", name)
					}
					if app.Removal.PreservationFingerprint == "" {
						upgraded, updateErr := upgradeRemovalPreservation(ctx, *app, currentSafety)
						if updateErr != nil {
							return updateErr
						}
						*app = upgraded
					} else {
						currentSafety.ExplicitDiscard = app.Removal.ExplicitDiscard
						if !removalSafetyEqual(currentSafety, safetyFromJournal(app.Removal)) {
							return fmt.Errorf("siding %q preservation evidence changed after removal began", name)
						}
					}
				} else if errors.Is(err, os.ErrNotExist) && len(app.Removal.Targets) > 0 {
					sd := app.Sidings[name]
					owner := state.WorktreeOwner(*app, sd)
					if len(app.Removal.RecoveryRefs) == 0 {
						if err := fsclone.ValidateRemovalTargets(ctx, owner, app.Removal.Targets); err != nil {
							return err
						}
					} else if err := fsclone.ValidateRecoveryRefs(ctx, owner, app.Removal.RecoveryRefs); err != nil {
						return err
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("inspect worktree during removal resume: %w", err)
				}
			}
		}
		var lockedSafety *removalSafety
		if force && app.Removal == nil {
			forceSafety, err := captureForceRemovalSafety(ctx, current, name)
			if err != nil {
				return err
			}
			lockedSafety = &forceSafety
		}
		if !force && app.Removal == nil {
			removing := []string{name}
			if len(expected) > 0 && expected[0] != nil {
				removing = expected[0].Removing
			}
			currentSafety, err := captureRemovalSafety(ctx, current, name, removing)
			if err != nil {
				return err
			}
			if len(expected) == 0 || expected[0] == nil {
				dirty, err := sidingWorktreeHasChanges(ctx, current, name, removing)
				if err != nil {
					return err
				}
				if dirty {
					return fmt.Errorf("siding %q has uncommitted or uniquely referenced work; confirm again or pass --force", name)
				}
			} else {
				currentSafety.ExplicitDiscard = expected[0].ExplicitDiscard
				if !removalSafetyEqual(currentSafety, *expected[0]) {
					return fmt.Errorf("siding %q changed while waiting for the removal lock; inspect it and confirm removal again", name)
				}
			}
			lockedSafety = &currentSafety
		}
		return removeSidingLocked(ctx, app, name, successor, force, promoteData, lockedSafety, defaultRemovalOperations())
	})
}

func captureForceRemovalSafety(ctx context.Context, app state.App, name string) (removalSafety, error) {
	sd, ok := app.Sidings[name]
	if !ok {
		return removalSafety{}, fmt.Errorf("no siding %q", name)
	}
	refs := []string{"refs/heads/" + sd.Branch}
	observed := ""
	if src, _, err := siding.Paths(app, name); err == nil {
		if branch, branchErr := currentWorktreeBranch(ctx, src); branchErr == nil {
			observed = branch
			if branch != sd.Branch {
				refs = append(refs, "refs/heads/"+branch)
			}
		}
	}
	sort.Strings(refs)
	owner := state.WorktreeOwner(app, sd)
	targets := make([]state.RemovalTarget, 0, len(refs))
	for _, ref := range refs {
		oid, err := gitText(ctx, owner, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			oid = ""
		}
		targets = append(targets, state.RemovalTarget{Ref: ref, ExpectedOID: oid, Kind: string(gitpreservation.KindUnproven), Reason: "force removal explicitly authorized"})
	}
	return removalSafety{Removing: refs, ObservedBranch: observed, Targets: targets, ExplicitDiscard: true}, nil
}

func upgradeRemovalPreservation(ctx context.Context, app state.App, safety removalSafety) (state.App, error) {
	journal := app.Removal
	if journal == nil {
		return state.App{}, errors.New("removal journal disappeared while upgrading preservation evidence")
	}
	return state.UpdateApp(ctx, app.ConfigDir, func(latest *state.App) error {
		if latest.Removal == nil || latest.Removal.ID != journal.ID || latest.Removal.Safety != safety.Fingerprint {
			return errors.New("removal journal changed while upgrading preservation evidence")
		}
		latest.Removal.Removing = append([]string(nil), safety.Removing...)
		latest.Removal.ObservedWorktreeBranch = safety.ObservedBranch
		latest.Removal.PreservationFingerprint = safety.PreservationFingerprint
		latest.Removal.Targets = append([]state.RemovalTarget(nil), safety.Targets...)
		if hasUnpreservedTargets(safety.Targets) && !safety.ExplicitDiscard {
			return errors.New("legacy removal evidence is no longer preserved; rerun with --force or restart removal for fresh confirmation")
		}
		latest.Removal.ExplicitDiscard = safety.ExplicitDiscard
		return nil
	})
}

func removeSidingLocked(ctx context.Context, app *state.App, name, successor string, force, promoteData bool, safety *removalSafety, operations removalOperations) error {
	if err := prepareRemovalStage(ctx, app, name, successor, force, promoteData, safety, operations); err != nil {
		return err
	}
	if err := ensureRemovalRecoveryRefs(ctx, app, operations); err != nil {
		return err
	}
	if err := promoteRemovalBaselineStage(ctx, app, operations); err != nil {
		return err
	}
	if err := removeGuestStage(ctx, app, operations); err != nil {
		return err
	}
	if err := removeWorktreeStage(ctx, app, operations); err != nil {
		return err
	}
	if err := removeFilesStage(ctx, app, operations); err != nil {
		return err
	}
	if err := forgetRemovalOperationStage(ctx, app, operations); err != nil {
		return err
	}
	if err := clearRemovalJournal(ctx, app, operations); err != nil {
		return err
	}
	fmt.Printf("%s removed %q\n", tick(), name)
	return nil
}

func ensureRemovalRecoveryRefs(ctx context.Context, app *state.App, operations removalOperations) error {
	journal := app.Removal
	if journal == nil || len(journal.RecoveryRefs) > 0 || len(journal.Targets) == 0 {
		return nil
	}
	sd, ok := app.Sidings[journal.Siding]
	if !ok {
		return fmt.Errorf("removal %q lost siding state before recovery refs", journal.ID)
	}
	owner := state.WorktreeOwner(*app, sd)
	if err := fsclone.ValidateRemovalTargets(ctx, owner, journal.Targets); err != nil {
		return err
	}
	refs, err := fsclone.EnsureRemovalRecoveryRefs(ctx, owner, journal.ID, journal.Targets)
	if err != nil {
		return err
	}
	archiveRef, archiveOID, err := fsclone.EnsureRemovalWitnessArchive(ctx, owner, journal.Targets)
	if err != nil {
		_ = fsclone.RemoveRecoveryRefs(context.WithoutCancel(ctx), owner, refs)
		return err
	}
	updated, err := operations.updateApp(ctx, app.ConfigDir, func(current *state.App) error {
		if current.Removal == nil || current.Removal.ID != journal.ID {
			return errors.New("removal journal changed while publishing recovery refs")
		}
		current.Removal.RecoveryRefs = append([]string(nil), refs...)
		current.Removal.RecoveryRepo = owner
		current.Removal.ArchiveRef = archiveRef
		current.Removal.ArchiveOID = archiveOID
		return nil
	})
	if err != nil {
		var committed *state.CommittedDurabilityError
		if errors.As(err, &committed) {
			*app = updated
			return err
		}
		cleanupErr := fsclone.RemoveRecoveryRefs(context.WithoutCancel(ctx), owner, refs)
		return errors.Join(err, cleanupErr)
	}
	*app = updated
	return nil
}

func prepareRemovalStage(ctx context.Context, app *state.App, name, successor string, force, promoteData bool, safety *removalSafety, operations removalOperations) error {
	if app == nil {
		return errors.New("app is required")
	}
	if !force && safety != nil && hasUnpreservedTargets(safety.Targets) && !safety.ExplicitDiscard {
		return fmt.Errorf("siding %q has unpreserved work without explicit discard confirmation", name)
	}
	if app.Removal != nil {
		if app.Removal.Siding != name {
			return ensureNoRemovalInProgress(*app, "remove siding "+name)
		}
		if app.Removal.Stage != state.RemovalStarted {
			return validateRemovalPostcondition(*app, app.Removal.ID, state.RemovalBasePinned)
		}
	}
	if _, ok := app.Sidings[name]; !ok {
		return fmt.Errorf("no siding %q", name)
	}
	started := operations.now().UTC()
	operationID := "remove-" + name + "-" + started.Format("20060102T150405.000000000Z")
	startedAt := started.Format(time.RFC3339Nano)
	if app.Removal != nil {
		operationID = app.Removal.ID
		startedAt = app.Removal.StartedAt
	}
	baseSiding := app.BaseSiding
	baseCommit := app.BaseCommit
	if baseSiding == name {
		excluded := map[string]bool{name: true}
		survivors := sortedSidingNames(*app, excluded)
		sourceName := name
		if len(survivors) > 0 {
			switch {
			case successor == "":
				return fmt.Errorf("removing base %q requires a successor siding", name)
			case successor == detachedBaseChoice:
				// Detaching keeps this siding's commit as the seed and leaves no
				// siding as base, so nothing has to stay alive merely to carry it.
				// sourceName stays as the siding being removed: its HEAD is what
				// gets pinned below, exactly as in the no-survivors case.
				baseSiding = ""
			case successor == name:
				return fmt.Errorf("successor base %q is being removed", successor)
			default:
				if _, exists := app.Sidings[successor]; !exists {
					return fmt.Errorf("no successor siding %q", successor)
				}
				sourceName = successor
				baseSiding = successor
			}
		}
		sourceSiding := app.Sidings[sourceName]
		sourceRoot, _, err := siding.Paths(*app, sourceName)
		if err != nil {
			return err
		}
		if sourceName != name {
			branch, err := gitText(ctx, sourceRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
			if err != nil || branch != sourceSiding.Branch {
				return fmt.Errorf("successor base %q is not on its recorded branch %q", sourceName, sourceSiding.Branch)
			}
		}
		commit, err := operations.resolveCommit(ctx, sourceRoot)
		if err != nil && safety != nil {
			for _, target := range safety.Targets {
				if target.Ref == "refs/heads/"+sourceSiding.Branch && target.ExpectedOID != "" {
					commit, err = target.ExpectedOID, nil
					break
				}
			}
		}
		if err != nil {
			return fmt.Errorf("resolve source commit for %q: %w", sourceName, err)
		}
		prepared := *app
		owner := state.WorktreeOwner(*app, sourceSiding)
		if err := operations.ensureControl(ctx, &prepared, owner, commit); err != nil {
			return err
		}
		pinned, err := operations.pinBaseCommit(ctx, prepared.ControlRepoPath, owner, commit)
		if err != nil {
			return err
		}
		baseCommit = pinned
	}
	updated, err := operations.updateApp(ctx, app.ConfigDir, func(current *state.App) error {
		if current.Removal != nil && (current.Removal.ID != operationID || current.Removal.Siding != name || current.Removal.Stage != state.RemovalStarted) {
			return ensureNoRemovalInProgress(*current, "remove siding "+name)
		}
		if _, exists := current.Sidings[name]; !exists {
			return fmt.Errorf("no siding %q", name)
		}
		current.BaseSiding = baseSiding
		current.BaseCommit = baseCommit
		journalForce := force
		journalSafety := ""
		journalRemoving := []string(nil)
		if safety != nil {
			journalSafety = safety.Fingerprint
			journalRemoving = append(journalRemoving, safety.Removing...)
		}
		journalPromoteData := promoteData
		if current.Removal != nil {
			// A resumed removal keeps the request it started with, so a crash
			// between stages cannot silently drop the promotion and take the data.
			journalPromoteData = current.Removal.PromoteData || promoteData
			journalForce = current.Removal.Force || force
			if current.Removal.Safety != "" {
				journalSafety = current.Removal.Safety
				journalRemoving = append([]string(nil), current.Removal.Removing...)
			}
		}
		observedBranch, preservationFingerprint := "", ""
		if safety != nil {
			observedBranch, preservationFingerprint = safety.ObservedBranch, safety.PreservationFingerprint
		}
		if current.Removal != nil {
			observedBranch = current.Removal.ObservedWorktreeBranch
			preservationFingerprint = current.Removal.PreservationFingerprint
		}
		targets := []state.RemovalTarget(nil)
		explicitDiscard := force
		if safety != nil {
			targets = append(targets, safety.Targets...)
			explicitDiscard = explicitDiscard || safety.ExplicitDiscard
		}
		if current.Removal != nil {
			targets = append([]state.RemovalTarget(nil), current.Removal.Targets...)
			explicitDiscard = current.Removal.ExplicitDiscard || explicitDiscard
		}
		current.Removal = &state.RemovalOperation{ID: operationID, Siding: name, Stage: state.RemovalBasePinned, StartedAt: startedAt, Force: journalForce, Safety: journalSafety, Removing: journalRemoving, ObservedWorktreeBranch: observedBranch, PreservationFingerprint: preservationFingerprint, Targets: targets, ExplicitDiscard: explicitDiscard, PromoteData: journalPromoteData}
		return nil
	})
	return finishRemovalCheckpoint(app, updated, err, operations, operationID, state.RemovalBasePinned)
}

func promoteRemovalBaselineStage(ctx context.Context, app *state.App, operations removalOperations) error {
	journal, err := requireRemovalStage(*app, state.RemovalBasePinned)
	if err != nil {
		return err
	}
	if removalAtLeast(journal.Stage, state.RemovalBaselinePromoted) {
		return validateRemovalPostcondition(*app, journal.ID, state.RemovalBaselinePromoted)
	}
	sd, ok := app.Sidings[journal.Siding]
	if !ok {
		return fmt.Errorf("removal %q lost siding state before baseline promotion", journal.ID)
	}
	_, volumeRoot, err := siding.Paths(*app, journal.Siding)
	if err != nil {
		return err
	}
	generationID := ""
	// The final base siding promotes automatically, because its data would
	// otherwise be the last copy and vanish with it. Any other siding promotes
	// only when the caller asked, which is what --promote-data records.
	if journal.PromoteData || (len(app.Sidings) == 1 && app.BaseSiding == journal.Siding) {
		promote, err := validateFinalVolumeSet(sd, volumeRoot, app.Volumes)
		if err != nil {
			return err
		}
		if promote {
			observation := operations.observeGuest(ctx, sd.Container)
			switch observation.State {
			case container.GuestAbsent:
				// Raw host-volume promotion is safe only after the exact guest is
				// proven absent, regardless of the persisted materialization phase.
				sd.MaterializationPhase = state.PhaseData
			case container.GuestRunning, container.GuestStopped:
				// A present guest must use the lifecycle-aware promotion path even
				// when a crash left its siding recorded in an earlier phase.
				sd.MaterializationPhase = state.PhaseGuest
			case container.GuestUnavailable:
				return fmt.Errorf("inspect guest %q before final baseline promotion: runtime unavailable; refusing raw host-volume promotion", sd.Container)
			default:
				return fmt.Errorf("inspect guest %q before final baseline promotion: ambiguous state %q; refusing raw host-volume promotion", sd.Container, observation.State)
			}
			result, err := operations.promoteBaseline(ctx, *app, sd, journal.ID, volumeRoot)
			if err != nil {
				return err
			}
			if !result.Committed || result.GenerationID == "" || result.OperationID != journal.ID {
				return fmt.Errorf("baseline promotion for removal %q did not publish its operation", journal.ID)
			}
			generationID = result.GenerationID
		}
	}
	return checkpointRemoval(ctx, app, operations, state.RemovalBasePinned, state.RemovalBaselinePromoted, func(_ *state.App, current *state.RemovalOperation) {
		current.GenerationID = generationID
	})
}

func removeGuestStage(ctx context.Context, app *state.App, operations removalOperations) error {
	journal, err := requireRemovalStage(*app, state.RemovalBaselinePromoted)
	if err != nil {
		return err
	}
	if removalAtLeast(journal.Stage, state.RemovalGuestRemoved) {
		if err := validateRemovalPostcondition(*app, journal.ID, state.RemovalGuestRemoved); err != nil {
			return err
		}
		if removalAtLeast(journal.Stage, state.RemovalFilesRemoved) {
			return nil
		}
		return ensureRemovalGuestAbsent(ctx, *app, journal, operations)
	}
	if sd, ok := app.Sidings[journal.Siding]; ok && len(journal.Targets) > 0 {
		if err := fsclone.ValidateRemovalTargets(ctx, state.WorktreeOwner(*app, sd), journal.Targets); err != nil {
			return err
		}
	}
	if err := ensureRemovalGuestAbsent(ctx, *app, journal, operations); err != nil {
		return err
	}
	return checkpointRemoval(ctx, app, operations, state.RemovalBaselinePromoted, state.RemovalGuestRemoved, nil)
}

func ensureRemovalGuestAbsent(ctx context.Context, app state.App, journal *state.RemovalOperation, operations removalOperations) error {
	sd, ok := app.Sidings[journal.Siding]
	if !ok {
		return fmt.Errorf("removal %q lost siding state while proving guest absence", journal.ID)
	}
	observation := operations.observeGuest(ctx, sd.Container)
	switch observation.State {
	case container.GuestAbsent:
		return nil
	case container.GuestRunning, container.GuestStopped:
		fmt.Printf("• removing guest %q…\n", sd.Container)
		if err := operations.removeGuest(ctx, sd.Container); err != nil {
			return fmt.Errorf("remove guest %q: %w", sd.Container, err)
		}
		confirmed := operations.observeGuest(ctx, sd.Container)
		if confirmed.State != container.GuestAbsent {
			return fmt.Errorf("guest %q still has state %q after removal", sd.Container, confirmed.State)
		}
		return nil
	case container.GuestUnavailable:
		return fmt.Errorf("inspect guest %q before removal: runtime unavailable", sd.Container)
	default:
		return fmt.Errorf("inspect guest %q before removal: ambiguous state %q", sd.Container, observation.State)
	}
}

func removeWorktreeStage(ctx context.Context, app *state.App, operations removalOperations) error {
	journal, err := requireRemovalStage(*app, state.RemovalGuestRemoved)
	if err != nil {
		return err
	}
	if removalAtLeast(journal.Stage, state.RemovalWorktreeRemoved) {
		return validateRemovalPostcondition(*app, journal.ID, state.RemovalWorktreeRemoved)
	}
	sd, ok := app.Sidings[journal.Siding]
	if !ok {
		return fmt.Errorf("removal %q lost siding state before worktree removal", journal.ID)
	}
	sourceRoot, _, err := siding.Paths(*app, journal.Siding)
	if err != nil {
		return err
	}
	fmt.Println("• removing the worktree…")
	owner := state.WorktreeOwner(*app, sd)
	targetsAlreadyAbsent := false
	if len(journal.Targets) > 0 {
		allAbsent, absentErr := fsclone.RemovalTargetsAbsent(ctx, owner, journal.Targets)
		if absentErr != nil {
			return absentErr
		}
		if allAbsent {
			if err := fsclone.ValidateRecoveryRefs(ctx, owner, journal.RecoveryRefs); err != nil {
				return err
			}
			if _, err := os.Lstat(sourceRoot); errors.Is(err, os.ErrNotExist) {
				return checkpointRemoval(ctx, app, operations, state.RemovalGuestRemoved, state.RemovalWorktreeRemoved, nil)
			}
			targetsAlreadyAbsent = true
		}
		if !targetsAlreadyAbsent {
			if err := fsclone.ValidateRemovalTargets(ctx, owner, journal.Targets); err != nil {
				return err
			}
		}
		if err := fsclone.ValidateRecoveryRefs(ctx, owner, journal.RecoveryRefs); err != nil {
			return err
		}
	}
	worktreeBranch := sd.Branch
	if journal.ObservedWorktreeBranch != "" {
		worktreeBranch = journal.ObservedWorktreeBranch
	}
	quarantine := fsclone.WorktreeQuarantineFor(owner, sourceRoot, worktreeBranch, journal.ID)
	quarantine.RetainBranch = true
	if journal.Force {
		if _, recoveryErr := os.Lstat(quarantine.RecoveryPath); recoveryErr == nil {
			// A previous non-force retirement can fail after repairing Git's exact
			// registration to the deterministic recovery path. Force explicitly
			// authorizes discarding those bytes, so retire that named recovery
			// without applying the superseded non-force fingerprint policy.
			if err := operations.retireWorktree(ctx, quarantine); err != nil {
				return err
			}
		} else if !errors.Is(recoveryErr, os.ErrNotExist) {
			return fmt.Errorf("inspect worktree recovery path %q: %w", quarantine.RecoveryPath, recoveryErr)
		} else if err := operations.removeWorktree(ctx, owner, sourceRoot, ""); err != nil {
			return err
		}
	} else {
		if journal.Safety == "" {
			return fmt.Errorf("removal %q has no journaled Git safety evidence; rerun with --force", journal.ID)
		}
		if _, err := os.Lstat(sourceRoot); errors.Is(err, os.ErrNotExist) {
			if _, recoveryErr := os.Lstat(quarantine.RecoveryPath); recoveryErr == nil {
				if err := validateAndRetireRemovalQuarantine(ctx, *app, journal, quarantine, operations); err != nil {
					return err
				}
			} else if !errors.Is(recoveryErr, os.ErrNotExist) {
				return fmt.Errorf("inspect worktree recovery path %q: %w", quarantine.RecoveryPath, recoveryErr)
			} else if err := operations.removeWorktree(ctx, owner, sourceRoot, ""); err != nil {
				return err
			}
		} else if err != nil {
			return fmt.Errorf("inspect worktree before quarantine: %w", err)
		} else {
			quarantine, err = operations.quarantineWorktree(ctx, owner, sourceRoot, worktreeBranch, journal.ID)
			if err != nil {
				return err
			}
			quarantine.RetainBranch = true
			if err := validateAndRetireRemovalQuarantine(ctx, *app, journal, quarantine, operations); err != nil {
				return err
			}
		}
	}
	if len(journal.Targets) > 0 && !targetsAlreadyAbsent {
		if err := fsclone.RetireRemovalTargetRefs(ctx, owner, journal.Targets, journal.RecoveryRefs); err != nil {
			return err
		}
	}
	return checkpointRemoval(ctx, app, operations, state.RemovalGuestRemoved, state.RemovalWorktreeRemoved, nil)
}

func validateAndRetireRemovalQuarantine(ctx context.Context, app state.App, journal *state.RemovalOperation, quarantine fsclone.WorktreeQuarantine, operations removalOperations) error {
	currentSafety, safetyErr := captureRemovalSafetyAt(ctx, app, journal.Siding, removalJournalRefs(journal), quarantine.RecoveryPath)
	if safetyErr != nil {
		return restoreRemovalQuarantine(quarantine, safetyErr, operations)
	}
	currentSafety.ExplicitDiscard = journal.ExplicitDiscard
	if !removalSafetyEqual(currentSafety, safetyFromJournal(journal)) {
		return restoreRemovalQuarantine(quarantine, fmt.Errorf("siding %q changed after removal began; inspect it and rerun with --force only if the new work may be discarded", journal.Siding), operations)
	}
	if err := ctx.Err(); err != nil {
		return restoreRemovalQuarantine(quarantine, err, operations)
	}
	return operations.retireWorktree(ctx, quarantine)
}

func removalJournalRefs(journal *state.RemovalOperation) []string {
	if len(journal.Removing) == 0 {
		return []string{journal.Siding}
	}
	return append([]string(nil), journal.Removing...)
}

func restoreRemovalQuarantine(quarantine fsclone.WorktreeQuarantine, cause error, operations removalOperations) error {
	if restoreErr := operations.restoreWorktree(quarantine); restoreErr != nil {
		return errors.Join(cause, fmt.Errorf("restore quarantined worktree; recovery remains at %q: %w", quarantine.RecoveryPath, restoreErr))
	}
	return cause
}

func removeFilesStage(ctx context.Context, app *state.App, operations removalOperations) error {
	journal, err := requireRemovalStage(*app, state.RemovalWorktreeRemoved)
	if err != nil {
		return err
	}
	if removalAtLeast(journal.Stage, state.RemovalFilesRemoved) {
		return validateRemovalPostcondition(*app, journal.ID, state.RemovalFilesRemoved)
	}
	// The guest can reappear after its earlier checkpoint (or the journal can
	// resume after the worktree disappeared), so prove exact absence again at
	// the final boundary before deleting host data and output.
	if err := ensureRemovalGuestAbsent(ctx, *app, journal, operations); err != nil {
		return err
	}
	fmt.Println("• deleting siding data (a large data volume can take a while)…")
	if err := operations.removeFiles(*app, journal.Siding); err != nil {
		return err
	}
	return checkpointRemoval(ctx, app, operations, state.RemovalWorktreeRemoved, state.RemovalFilesRemoved, func(current *state.App, _ *state.RemovalOperation) {
		delete(current.Sidings, journal.Siding)
		if current.LiveSiding == journal.Siding || current.LiveSiding == state.HostTarget {
			current.LiveSiding = ""
		}
		if current.BaseSiding == journal.Siding {
			current.BaseSiding = ""
		}
	})
}

func forgetRemovalOperationStage(ctx context.Context, app *state.App, operations removalOperations) error {
	journal, err := requireRemovalStage(*app, state.RemovalFilesRemoved)
	if err != nil {
		return err
	}
	if removalAtLeast(journal.Stage, state.RemovalOperationForgotten) {
		return validateRemovalPostcondition(*app, journal.ID, state.RemovalOperationForgotten)
	}
	if journal.GenerationID != "" {
		result, err := operations.forgetOperation(context.WithoutCancel(ctx), *app, journal.ID)
		if err != nil {
			return err
		}
		if !result.Committed || result.OperationID != journal.ID {
			return fmt.Errorf("baseline operation %q was not durably forgotten", journal.ID)
		}
	}
	return checkpointRemoval(ctx, app, operations, state.RemovalFilesRemoved, state.RemovalOperationForgotten, nil)
}

func clearRemovalJournal(ctx context.Context, app *state.App, operations removalOperations) error {
	journal, err := requireRemovalStage(*app, state.RemovalOperationForgotten)
	if err != nil {
		return err
	}
	if err := fsclone.CompleteRemovalRecoveryHandoff(ctx, journal.RecoveryRepo, journal.ArchiveRef, journal.ArchiveOID, journal.Targets, journal.RecoveryRefs); err != nil {
		return err
	}
	updated, err := operations.updateApp(ctx, app.ConfigDir, func(current *state.App) error {
		if current.Removal == nil || current.Removal.ID != journal.ID || current.Removal.Stage != state.RemovalOperationForgotten {
			return fmt.Errorf("removal journal changed before completion")
		}
		current.Removal = nil
		return nil
	})
	if err == nil {
		*app = updated
		return nil
	}
	var committed *state.CommittedDurabilityError
	if errors.As(err, &committed) {
		*app = updated
		return err
	}
	return err
}

func checkpointRemoval(ctx context.Context, app *state.App, operations removalOperations, from, to state.RemovalStage, mutate func(*state.App, *state.RemovalOperation)) error {
	journal, err := requireRemovalStage(*app, from)
	if err != nil {
		return err
	}
	if journal.Stage != from {
		return fmt.Errorf("removal %q is at stage %q, expected %q", journal.ID, journal.Stage, from)
	}
	updated, err := operations.updateApp(ctx, app.ConfigDir, func(current *state.App) error {
		if current.Removal == nil || current.Removal.ID != journal.ID || current.Removal.Siding != journal.Siding || current.Removal.Stage != from {
			return fmt.Errorf("removal journal changed before checkpoint %q", to)
		}
		if mutate != nil {
			mutate(current, current.Removal)
		}
		current.Removal.Stage = to
		return nil
	})
	return finishRemovalCheckpoint(app, updated, err, operations, journal.ID, to)
}

func finishRemovalCheckpoint(app *state.App, updated state.App, err error, operations removalOperations, operationID string, stage state.RemovalStage) error {
	if err == nil {
		*app = updated
		if err := validateRemovalPostcondition(*app, operationID, stage); err != nil {
			return err
		}
		if operations.afterCheckpoint != nil {
			return operations.afterCheckpoint(stage)
		}
		return nil
	}
	var committed *state.CommittedDurabilityError
	if errors.As(err, &committed) {
		*app = updated
	}
	return err
}

func validateTerminalPreservation(ctx context.Context, journal *state.RemovalOperation) error {
	if journal.ExplicitDiscard {
		return nil
	}
	analyzer := gitpreservation.NewAnalyzer(journal.RecoveryRepo, gitpreservation.Options{})
	deletions := append(append([]string(nil), journal.Removing...), journal.RecoveryRefs...)
	for _, target := range journal.Targets {
		if !target.Preserved {
			return fmt.Errorf("unpreserved target %q lacks explicit discard authorization", target.Ref)
		}
		var recovery string
		for _, ref := range journal.RecoveryRefs {
			oid, err := gitText(ctx, journal.RecoveryRepo, "rev-parse", "--verify", ref+"^{commit}")
			if err == nil && oid == target.ExpectedOID {
				recovery = ref
				break
			}
		}
		if recovery == "" {
			return fmt.Errorf("no recovery ref retains target %q at %s", target.Ref, target.ExpectedOID)
		}
		if result := analyzer.Analyze(ctx, recovery, deletions); !result.Preserved {
			return fmt.Errorf("target %q is no longer preserved: %s", target.Ref, result.Reason)
		}
	}
	return nil
}

func requireRemovalStage(app state.App, minimum state.RemovalStage) (*state.RemovalOperation, error) {
	if app.Removal == nil {
		return nil, errors.New("removal journal is missing")
	}
	if !validRemovalStage(app.Removal.Stage) {
		return nil, fmt.Errorf("removal %q has unsupported stage %q", app.Removal.ID, app.Removal.Stage)
	}
	if !removalAtLeast(app.Removal.Stage, minimum) {
		return nil, fmt.Errorf("removal %q is at stage %q, expected at least %q", app.Removal.ID, app.Removal.Stage, minimum)
	}
	return app.Removal, nil
}

func validateRemovalPostcondition(app state.App, operationID string, minimum state.RemovalStage) error {
	journal, err := requireRemovalStage(app, minimum)
	if err != nil {
		return err
	}
	if journal.ID != operationID || journal.Siding == "" || journal.StartedAt == "" {
		return fmt.Errorf("removal journal postcondition failed for %q", operationID)
	}
	if removalAtLeast(journal.Stage, state.RemovalFilesRemoved) {
		if _, exists := app.Sidings[journal.Siding]; exists {
			return fmt.Errorf("removal %q retained siding state after file removal", operationID)
		}
	} else if _, exists := app.Sidings[journal.Siding]; !exists {
		return fmt.Errorf("removal %q lost siding state at stage %q", operationID, journal.Stage)
	}
	return nil
}

func validRemovalStage(stage state.RemovalStage) bool {
	return removalStageRank(stage) > 0
}

func removalAtLeast(got, want state.RemovalStage) bool {
	return removalStageRank(got) >= removalStageRank(want)
}

func removalStageRank(stage state.RemovalStage) int {
	switch stage {
	case state.RemovalStarted:
		return 1
	case state.RemovalBasePinned:
		return 2
	case state.RemovalBaselinePromoted:
		return 3
	case state.RemovalGuestRemoved:
		return 4
	case state.RemovalWorktreeRemoved:
		return 5
	case state.RemovalFilesRemoved:
		return 6
	case state.RemovalOperationForgotten:
		return 7
	default:
		return 0
	}
}

func ensureNoRemovalInProgress(app state.App, operation string) error {
	if app.Removal == nil {
		return nil
	}
	return fmt.Errorf("%s is blocked while siding %q removal is at stage %q; resume it with `%s rm %s`", operation, app.Removal.Siding, app.Removal.Stage, bin(), app.Removal.Siding)
}

func validateFinalVolumeSet(sd state.Siding, volRoot string, volumes []string) (bool, error) {
	if sd.MaterializationPhase == state.PhaseWorktree {
		if _, err := os.Lstat(volRoot); err == nil {
			return false, fmt.Errorf("worktree-only final siding %q unexpectedly has data at %s; refusing removal", sd.Name, volRoot)
		} else if !os.IsNotExist(err) {
			return false, err
		}
		return false, nil
	}
	if len(volumes) == 0 {
		return false, nil
	}
	if sd.MaterializationPhase != state.PhaseGuest && sd.MaterializationPhase != state.PhaseParked && sd.MaterializationPhase != state.PhaseData && sd.MaterializationPhase != "" {
		return false, fmt.Errorf("final siding %q has unsupported materialization phase %q", sd.Name, sd.MaterializationPhase)
	}
	for _, volume := range volumes {
		info, err := os.Lstat(filepath.Join(volRoot, volume))
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("final siding volume %q is incomplete; refusing baseline promotion", volume)
		}
	}
	return true, nil
}

// detachedBaseChoice asks for a base that is a pinned commit rather than a
// siding. It is a reserved --next-base value so a script can choose it without
// a terminal, and "-" cannot collide with a siding name because ValidateName
// rejects it.
// baseRemovalIsInteractive is a seam so a test can pin which path it exercises.
// Reading os.Stdin directly made the choice depend on how `go test` was invoked,
// which is exactly the kind of environment dependence that turns into a flake
// nobody can reproduce.
var baseRemovalIsInteractive = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

const detachedBaseChoice = "-"

func prepareBaseRemoval(app state.App, removing []string, requested string, in *bufio.Reader) (string, error) {
	if app.Removal != nil || app.BaseSiding == "" || !containsName(removing, app.BaseSiding) {
		return "", nil
	}
	excluded := map[string]bool{}
	for _, name := range removing {
		excluded[name] = true
	}
	survivors := sortedSidingNames(app, excluded)
	if len(survivors) == 0 {
		return "", nil
	}
	choice := requested
	if choice == "" {
		if !baseRemovalIsInteractive() {
			return "", fmt.Errorf("removing base %q requires --next-base <siding>, or --next-base %s to keep the commit and leave no siding as base", app.BaseSiding, detachedBaseChoice)
		}
		fmt.Println("Choose the successor source base:")
		for i, name := range survivors {
			fmt.Printf("  %d) %s\n", i+1, name)
		}
		// Detaching is the option that stops a siding being kept alive purely to
		// carry the seed. It is listed last so the numbering of the survivors
		// above is unchanged.
		fmt.Printf("  %d) none — keep the commit as the base and hold no siding open for it\n", len(survivors)+1)
		var index int
		if _, err := fmt.Fscan(in, &index); err != nil || index < 1 || index > len(survivors)+1 {
			return "", fmt.Errorf("invalid successor base selection")
		}
		if index == len(survivors)+1 {
			return detachedBaseChoice, nil
		}
		choice = survivors[index-1]
	}
	if choice == detachedBaseChoice {
		return detachedBaseChoice, nil
	}
	if excluded[choice] {
		return "", fmt.Errorf("successor base %q is also being removed", choice)
	}
	if _, ok := app.Sidings[choice]; !ok {
		return "", fmt.Errorf("no successor siding %q", choice)
	}
	return choice, nil
}

func orderBaseLast(selected []string, base string) []string {
	ordered := append([]string(nil), selected...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[j] == base && ordered[i] != base })
	return ordered
}
