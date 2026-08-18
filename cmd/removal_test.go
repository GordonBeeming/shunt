package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/databaseline"
	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
)

type removalFixture struct {
	app           state.App
	control       string
	sourceCommit  string
	baselineState string
}

func TestFinalRemovalRetriesAfterEveryJournalCheckpoint(t *testing.T) {
	stages := []state.RemovalStage{
		state.RemovalBasePinned,
		state.RemovalBaselinePromoted,
		state.RemovalGuestRemoved,
		state.RemovalWorktreeRemoved,
		state.RemovalFilesRemoved,
		state.RemovalOperationForgotten,
	}

	for _, stage := range stages {
		t.Run(string(stage), func(t *testing.T) {
			fixture := newRemovalFixture(t, state.PhaseData, []string{"db"}, true)
			command := exec.Command(os.Args[0], "-test.run=^TestRemovalCrashHelper$")
			command.Env = append(os.Environ(),
				"SHUNT_REMOVAL_CRASH_HELPER=1",
				"SHUNT_REMOVAL_CONFIG="+fixture.app.ConfigDir,
				"SHUNT_REMOVAL_STAGE="+string(stage),
			)
			err := command.Run()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 74 {
				t.Fatalf("removal crash at %s = %v, want exit 74", stage, err)
			}
			persisted, err := state.LoadApp(fixture.app.ConfigDir)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Removal == nil || persisted.Removal.Stage != stage {
				t.Fatalf("persisted removal = %#v, want stage %q", persisted.Removal, stage)
			}

			operationsMap, manifestExists := readRemovalBaselineOperations(t, fixture.baselineState)
			if removalAtLeast(stage, state.RemovalBaselinePromoted) && !removalAtLeast(stage, state.RemovalOperationForgotten) {
				if !manifestExists || operationsMap[persisted.Removal.ID] == "" {
					t.Fatalf("operation %q was not protected at stage %q: %v", persisted.Removal.ID, stage, operationsMap)
				}
			}
			if stage == state.RemovalOperationForgotten && operationsMap[persisted.Removal.ID] != "" {
				t.Fatalf("operation %q remains protected at terminal stage: %v", persisted.Removal.ID, operationsMap)
			}

			if err := removeSiding(context.Background(), &persisted, "one", true, ""); err != nil {
				t.Fatalf("retry after %s: %v", stage, err)
			}
			completed, err := state.LoadApp(fixture.app.ConfigDir)
			if err != nil {
				t.Fatal(err)
			}
			if completed.Removal != nil || len(completed.Sidings) != 0 || completed.BaseCommit == "" {
				t.Fatalf("completed state = %#v", completed)
			}
			assertRemovalSourceCommit(t, fixture.control, completed.BaseCommit)
			if completed.BaseCommit != fixture.sourceCommit {
				t.Fatalf("preserved source = %s, want %s", completed.BaseCommit, fixture.sourceCommit)
			}
			if got := removalGenerationCount(t, filepath.Dir(fixture.baselineState)); got != 1 {
				t.Fatalf("baseline generation count = %d, want one promotion", got)
			}
		})
	}
}

func TestRemovalCrashHelper(t *testing.T) {
	if os.Getenv("SHUNT_REMOVAL_CRASH_HELPER") == "" {
		return
	}
	app, err := state.LoadApp(os.Getenv("SHUNT_REMOVAL_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	operations := removalTestOperations()
	operations.afterCheckpoint = func(stage state.RemovalStage) error {
		if string(stage) == os.Getenv("SHUNT_REMOVAL_STAGE") {
			os.Exit(74)
		}
		return nil
	}
	if err := runRemovalWithOperations(context.Background(), &app, "one", "", operations); err != nil {
		t.Fatal(err)
	}
	t.Fatalf("removal stage %q did not terminate the helper", os.Getenv("SHUNT_REMOVAL_STAGE"))
}

// These cases fail before the checkpoint's state callback runs. The subprocess
// crash cases above exit only after the checkpoint is already visible.
func TestRemovalReusesPromotionWhenCheckpointPublicationFails(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseData, []string{"db"}, true)
	operations := removalTestOperations()
	failure := errors.New("injected baseline checkpoint publication failure")
	restore := failRemovalPublicationBeforeUpdate(t, &operations, state.RemovalBasePinned, failure)
	app := fixture.app
	if err := runRemovalWithOperations(context.Background(), &app, "one", "", operations); !errors.Is(err, failure) {
		t.Fatalf("remove = %v, want publication failure", err)
	}
	restore()
	persisted, err := state.LoadApp(app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Removal == nil || persisted.Removal.Stage != state.RemovalBasePinned {
		t.Fatalf("persisted removal = %#v, want base-pinned", persisted.Removal)
	}
	operationID := persisted.Removal.ID
	operationsMap, exists := readRemovalBaselineOperations(t, fixture.baselineState)
	if !exists || operationsMap[operationID] == "" {
		t.Fatalf("baseline operation %q was not published: %v", operationID, operationsMap)
	}
	if got := removalGenerationCount(t, filepath.Dir(fixture.baselineState)); got != 1 {
		t.Fatalf("generation count before retry = %d, want one", got)
	}
	if err := runRemovalWithOperations(context.Background(), &persisted, "one", "", operations); err != nil {
		t.Fatalf("retry removal: %v", err)
	}
	if got := removalGenerationCount(t, filepath.Dir(fixture.baselineState)); got != 1 {
		t.Fatalf("generation count after retry = %d, want one", got)
	}
}

func TestCaptureRemovalSafetyJournalsUnprovenCommittedWork(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	sourceRoot, _, err := siding.Paths(fixture.app, "one")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "unmatched.txt"), []byte("unmatched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, sourceRoot, "add", "unmatched.txt")
	runGit(t, sourceRoot, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "unmatched")
	safety, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatalf("capture unproven work: %v", err)
	}
	if safety.Fingerprint == "" || safety.PreservationFingerprint == "" || safety.ObservedBranch == "" {
		t.Fatalf("incomplete safety = %#v", safety)
	}
}

func TestConfirmedUnprovenRemovalJournalsExactEvidence(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	sourceRoot, _, err := siding.Paths(fixture.app, "one")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "unmatched.txt"), []byte("unmatched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, sourceRoot, "add", "unmatched.txt")
	runGit(t, sourceRoot, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "unmatched")
	safety, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	safety.ExplicitDiscard = true
	stop := errors.New("stop after journal")
	operations := removalTestOperations()
	operations.afterCheckpoint = func(stage state.RemovalStage) error {
		if stage == state.RemovalBasePinned {
			return stop
		}
		return nil
	}
	app := fixture.app
	if err := runRemovalWithPolicy(context.Background(), &app, "one", "", false, &safety, operations); !errors.Is(err, stop) {
		t.Fatalf("remove = %v", err)
	}
	if app.Removal == nil || app.Removal.ObservedWorktreeBranch != safety.ObservedBranch || app.Removal.PreservationFingerprint != safety.PreservationFingerprint || len(app.Removal.Removing) == 0 {
		t.Fatalf("journaled evidence = %#v; safety = %#v", app.Removal, safety)
	}
}

func TestLegacyRemovalResumeUpgradesMismatchAndRetiresExactBranches(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	sourceRoot, _, err := siding.Paths(fixture.app, "one")
	if err != nil {
		t.Fatal(err)
	}
	recorded := fixture.app.Sidings["one"].Branch
	observed := "shunt/observed-one"
	runGit(t, sourceRoot, "checkout", "-b", observed)
	legacySafety, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"refs/heads/" + recorded})
	if err != nil {
		t.Fatal(err)
	}
	fixture.app.Removal = &state.RemovalOperation{
		ID: "remove-one-legacy", Siding: "one", Stage: state.RemovalBaselinePromoted,
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Safety: legacySafety.Fingerprint,
		Removing: []string{"refs/heads/" + recorded},
	}
	if err := state.SaveApp(fixture.app); err != nil {
		t.Fatal(err)
	}

	reproduced, err := captureRemovalSafety(context.Background(), fixture.app, "one", fixture.app.Removal.Removing)
	if err != nil {
		t.Fatal(err)
	}
	if reproduced.Fingerprint != fixture.app.Removal.Safety {
		t.Fatal("legacy safety fingerprint did not reproduce")
	}
	upgraded, err := upgradeRemovalPreservation(context.Background(), fixture.app, reproduced)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Removal == nil || upgraded.Removal.ObservedWorktreeBranch != observed || upgraded.Removal.PreservationFingerprint == "" || !containsName(upgraded.Removal.Removing, "refs/heads/"+recorded) || !containsName(upgraded.Removal.Removing, "refs/heads/"+observed) {
		t.Fatalf("upgraded journal = %#v", upgraded.Removal)
	}
	if err := runRemovalWithPolicy(context.Background(), &upgraded, "one", "", false, nil, removalTestOperations()); err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{recorded, observed} {
		command := exec.Command("git", "-C", fixture.control, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
		if err := command.Run(); err == nil {
			t.Fatalf("branch %q remains after legacy resume", branch)
		}
	}
}

func TestTerminalPreservationFailureKeepsRecoveryAuthorizationRequired(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	sourceRoot, _, pathErr := siding.Paths(fixture.app, "one")
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "terminal.txt"), []byte("unique\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, sourceRoot, "add", "terminal.txt")
	runGit(t, sourceRoot, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "terminal unique")
	oid := removalGitOutput(t, fixture.control, "rev-parse", "refs/heads/shunt/one")
	targets := []state.RemovalTarget{{Ref: "refs/heads/shunt/one", ExpectedOID: oid, Preserved: true, Kind: "reachable", Reason: "was preserved"}}
	refs, err := fsclone.EnsureRemovalRecoveryRefs(context.Background(), fixture.control, "terminal-check", targets)
	if err != nil {
		t.Fatal(err)
	}
	journal := &state.RemovalOperation{RecoveryRepo: fixture.control, RecoveryRefs: refs, Removing: []string{"refs/heads/shunt/one"}, Targets: targets}
	if err := validateTerminalPreservation(context.Background(), journal); err == nil {
		t.Fatal("lost preservation witness was accepted")
	}
	if err := fsclone.ValidateRecoveryRefs(context.Background(), fixture.control, refs); err != nil {
		t.Fatalf("recovery refs were not retained: %v", err)
	}
	journal.ExplicitDiscard = true
	if err := validateTerminalPreservation(context.Background(), journal); err != nil {
		t.Fatalf("explicit discard did not permit terminal clear: %v", err)
	}
}

func TestSuccessfulNonForceRemovalClearsTargetsRecoveriesAndArchivesWitness(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	safety, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	matching := ""
	for _, target := range safety.Targets {
		if target.Preserved {
			matching = target.MatchingCommit
			break
		}
	}
	if matching == "" {
		t.Fatal("fixture target was not preserved")
	}
	app := fixture.app
	if err := runRemovalWithPolicy(context.Background(), &app, "one", "", false, &safety, removalTestOperations()); err != nil {
		t.Fatal(err)
	}
	if app.Removal != nil {
		t.Fatalf("journal remains: %#v", app.Removal)
	}
	assertRefAbsent(t, fixture.control, "refs/heads/shunt/one")
	if refs := removalRefsWithPrefix(t, fixture.control, "refs/shunt/recovery"); len(refs) != 0 {
		t.Fatalf("recoveries remain: %v", refs)
	}
	archive := removalGitOutput(t, fixture.control, "rev-parse", "refs/shunt/witness/archive^{commit}")
	runGit(t, fixture.control, "merge-base", "--is-ancestor", matching, archive)
}

func TestSuccessfulForceRemovalClearsWithoutArchiveParent(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	before := removalOptionalRef(t, fixture.control, "refs/shunt/witness/archive")
	app := fixture.app
	if err := removeSiding(context.Background(), &app, "one", true, ""); err != nil {
		t.Fatal(err)
	}
	if app.Removal != nil {
		t.Fatalf("journal remains: %#v", app.Removal)
	}
	assertRefAbsent(t, fixture.control, "refs/heads/shunt/one")
	if refs := removalRefsWithPrefix(t, fixture.control, "refs/shunt/recovery"); len(refs) != 0 {
		t.Fatalf("recoveries remain: %v", refs)
	}
	if after := removalOptionalRef(t, fixture.control, "refs/shunt/witness/archive"); after != before {
		t.Fatalf("discard advanced archive: before=%q after=%q", before, after)
	}
}

func TestRemovalRetryAfterHandoffBeforeJournalClearIsIdempotent(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	safety, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	operations := removalTestOperations()
	original := operations.updateApp
	injected := errors.New("injected clear publication failure")
	failed := false
	operations.updateApp = func(ctx context.Context, dir string, update func(*state.App) error) (state.App, error) {
		current, err := state.LoadApp(dir)
		if err != nil {
			return state.App{}, err
		}
		if !failed && current.Removal != nil && current.Removal.Stage == state.RemovalOperationForgotten {
			failed = true
			return current, injected
		}
		return original(ctx, dir, update)
	}
	app := fixture.app
	if err := runRemovalWithPolicy(context.Background(), &app, "one", "", false, &safety, operations); !errors.Is(err, injected) {
		t.Fatalf("first removal = %v", err)
	}
	if refs := removalRefsWithPrefix(t, fixture.control, "refs/shunt/recovery"); len(refs) != 0 {
		t.Fatalf("handoff did not remove recoveries: %v", refs)
	}
	operations.updateApp = original
	if err := runRemovalWithPolicy(context.Background(), &app, "one", "", false, nil, operations); err != nil {
		t.Fatalf("retry = %v", err)
	}
	if app.Removal != nil {
		t.Fatalf("journal remains: %#v", app.Removal)
	}
}

func assertRefAbsent(t *testing.T, repo, ref string) {
	t.Helper()
	command := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", ref)
	if err := command.Run(); err == nil {
		t.Fatalf("ref %q remains", ref)
	}
}
func removalOptionalRef(t *testing.T, repo, ref string) string {
	t.Helper()
	command := exec.Command("git", "-C", repo, "rev-parse", "--verify", ref+"^{commit}")
	output, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}
func removalRefsWithPrefix(t *testing.T, repo, prefix string) []string {
	t.Helper()
	output := removalGitOutput(t, repo, "for-each-ref", "--format=%(refname)", prefix)
	return strings.Fields(output)
}

func TestLegacyUpgradeDoesNotInferDiscardWhenWitnessIsLost(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	fixture.app.Removal = &state.RemovalOperation{ID: "legacy", Siding: "one", Stage: state.RemovalBaselinePromoted, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Safety: "old-safe"}
	if err := state.SaveApp(fixture.app); err != nil {
		t.Fatal(err)
	}
	safety := removalSafety{Fingerprint: "old-safe", PreservationFingerprint: "new", Targets: []state.RemovalTarget{{Ref: "refs/heads/shunt/one", ExpectedOID: fixture.sourceCommit, Preserved: false, Kind: "unproven", Reason: "witness moved"}}}
	if _, err := upgradeRemovalPreservation(context.Background(), fixture.app, safety); err == nil || !strings.Contains(err.Error(), "fresh confirmation") {
		t.Fatalf("upgrade error = %v", err)
	}
	loaded, err := state.LoadApp(fixture.app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Removal == nil || loaded.Removal.ExplicitDiscard {
		t.Fatalf("legacy journal became discard-authorized: %#v", loaded.Removal)
	}
}

func TestMissingWorktreeSnapshotReachesExplicitConfirmation(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	sd := fixture.app.Sidings["one"]
	sourceRoot, _, err := siding.Paths(fixture.app, "one")
	if err != nil {
		t.Fatal(err)
	}
	if err := fsclone.RemoveWorktree(context.Background(), fixture.control, sourceRoot, ""); err != nil {
		t.Fatal(err)
	}
	safety, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"refs/heads/" + sd.Branch})
	if err != nil {
		t.Fatalf("missing snapshot: %v", err)
	}
	if safety.Fingerprint == "" || len(safety.Targets) != 1 {
		t.Fatalf("missing safety = %#v", safety)
	}
	protected, reason, err := sidingWorktreeProtection(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil || !protected || !strings.Contains(reason, "worktree is missing") {
		t.Fatalf("protection = %t, %q, %v", protected, reason, err)
	}
	safety.ExplicitDiscard = true
	if err := prepareRemovalStage(context.Background(), &fixture.app, "one", "", false, &safety, removalTestOperations()); err != nil {
		t.Fatalf("confirmed missing removal did not journal: %v", err)
	}
}

func TestRemovalRechecksGuestAfterCheckpointPublicationFails(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseGuest, nil, false)
	guest := filepath.Join(fixture.app.ConfigDir, "guest-resource")
	if err := os.WriteFile(guest, []byte("present"), 0o600); err != nil {
		t.Fatal(err)
	}
	operations := removalTestOperations()
	removeCalls := 0
	operations.observeGuest = func(context.Context, string) container.GuestObservation {
		if _, err := os.Lstat(guest); err == nil {
			return container.GuestObservation{State: container.GuestRunning}
		}
		return container.GuestObservation{State: container.GuestAbsent}
	}
	operations.removeGuest = func(context.Context, string) error {
		removeCalls++
		err := os.Remove(guest)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	failure := errors.New("injected guest checkpoint publication failure")
	restore := failRemovalPublicationBeforeUpdate(t, &operations, state.RemovalBaselinePromoted, failure)
	app := fixture.app
	if err := runRemovalWithOperations(context.Background(), &app, "one", "", operations); !errors.Is(err, failure) {
		t.Fatalf("remove = %v, want publication failure", err)
	}
	restore()
	if removeCalls != 1 {
		t.Fatalf("guest remove calls before retry = %d, want one", removeCalls)
	}
	if _, err := os.Lstat(guest); !os.IsNotExist(err) {
		t.Fatalf("guest resource remains after remove: %v", err)
	}
	persisted, err := state.LoadApp(app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Removal == nil || persisted.Removal.Stage != state.RemovalBaselinePromoted {
		t.Fatalf("persisted removal = %#v, want baseline-promoted", persisted.Removal)
	}
	if err := runRemovalWithOperations(context.Background(), &persisted, "one", "", operations); err != nil {
		t.Fatalf("retry removal: %v", err)
	}
	if removeCalls != 1 {
		t.Fatalf("guest remove calls after exact absence recheck = %d, want one", removeCalls)
	}
}

func TestRemovalDoesNotResolveDeletedWorktreeWhenCheckpointPublicationFails(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	operations := removalTestOperations()
	resolveCalls := 0
	originalResolve := operations.resolveCommit
	operations.resolveCommit = func(ctx context.Context, source string) (string, error) {
		resolveCalls++
		return originalResolve(ctx, source)
	}
	failure := errors.New("injected worktree checkpoint publication failure")
	restore := failRemovalPublicationBeforeUpdate(t, &operations, state.RemovalGuestRemoved, failure)
	app := fixture.app
	if err := runRemovalWithOperations(context.Background(), &app, "one", "", operations); !errors.Is(err, failure) {
		t.Fatalf("remove = %v, want publication failure", err)
	}
	restore()
	if resolveCalls != 1 {
		t.Fatalf("source resolutions before retry = %d, want one", resolveCalls)
	}
	if _, err := os.Lstat(filepath.Join(app.ConfigDir, "one", "src")); !os.IsNotExist(err) {
		t.Fatalf("worktree remains after remove: %v", err)
	}
	persisted, err := state.LoadApp(app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Removal == nil || persisted.Removal.Stage != state.RemovalGuestRemoved {
		t.Fatalf("persisted removal = %#v, want guest-removed", persisted.Removal)
	}
	if err := runRemovalWithOperations(context.Background(), &persisted, "one", "", operations); err != nil {
		t.Fatalf("retry removal: %v", err)
	}
	if resolveCalls != 1 {
		t.Fatalf("retry resolved the deleted worktree %d times", resolveCalls-1)
	}
}

func TestRemovalRepeatsIdempotentFileRemoveWhenCheckpointPublicationFails(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	operations := removalTestOperations()
	removeCalls := 0
	originalRemoveFiles := operations.removeFiles
	operations.removeFiles = func(app state.App, name string) error {
		removeCalls++
		return originalRemoveFiles(app, name)
	}
	failure := errors.New("injected files checkpoint publication failure")
	restore := failRemovalPublicationBeforeUpdate(t, &operations, state.RemovalWorktreeRemoved, failure)
	app := fixture.app
	if err := runRemovalWithOperations(context.Background(), &app, "one", "", operations); !errors.Is(err, failure) {
		t.Fatalf("remove = %v, want publication failure", err)
	}
	restore()
	if removeCalls != 1 {
		t.Fatalf("file remove calls before retry = %d, want one", removeCalls)
	}
	if _, err := os.Lstat(filepath.Join(app.ConfigDir, "one")); !os.IsNotExist(err) {
		t.Fatalf("siding files remain after remove: %v", err)
	}
	persisted, err := state.LoadApp(app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Removal == nil || persisted.Removal.Stage != state.RemovalWorktreeRemoved {
		t.Fatalf("persisted removal = %#v, want worktree-removed", persisted.Removal)
	}
	if err := runRemovalWithOperations(context.Background(), &persisted, "one", "", operations); err != nil {
		t.Fatalf("retry removal: %v", err)
	}
	if removeCalls != 2 {
		t.Fatalf("idempotent file remove calls = %d, want two", removeCalls)
	}
}

func TestRemovalStopsAfterCommittedDurabilityCheckpoint(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseData, []string{"db"}, true)
	operations := removalTestOperations()
	baseUpdate := operations.updateApp
	injected := errors.New("injected state directory sync failure")
	operations.updateApp = func(ctx context.Context, configDir string, update func(*state.App) error) (state.App, error) {
		updated, err := baseUpdate(ctx, configDir, update)
		if err == nil && updated.Removal != nil && updated.Removal.Stage == state.RemovalWorktreeRemoved {
			return updated, &state.CommittedDurabilityError{Path: filepath.Join(configDir, "state-v2.json"), Err: injected}
		}
		return updated, err
	}
	app := fixture.app
	err := runRemovalWithOperations(context.Background(), &app, "one", "", operations)
	var committed *state.CommittedDurabilityError
	if !errors.As(err, &committed) {
		t.Fatalf("remove = %v, want committed durability error", err)
	}
	if _, err := os.Lstat(filepath.Join(app.ConfigDir, "one", "src")); !os.IsNotExist(err) {
		t.Fatalf("worktree exists after committed checkpoint: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(app.ConfigDir, "one", "vol")); err != nil {
		t.Fatalf("file-removal stage ran after durability uncertainty: %v", err)
	}
	persisted, err := state.LoadApp(app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Removal == nil || persisted.Removal.Stage != state.RemovalWorktreeRemoved {
		t.Fatalf("persisted removal = %#v", persisted.Removal)
	}
	if err := removeSiding(context.Background(), &persisted, "one", true, ""); err != nil {
		t.Fatalf("explicit recovery after committed checkpoint: %v", err)
	}
}

func TestFinalWorktreeOnlyRemovalPreservesExistingBaseline(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, []string{"db"}, false)
	seedRoot := filepath.Join(fixture.app.ConfigDir, "seed", "vol")
	writeRemovalVolume(t, seedRoot, "db", "existing")
	manager, err := databaseline.New(fixture.app.ConfigDir, fixture.app.Volumes)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Promote(context.Background(), "seed", seedRoot)
	if err != nil || !result.Committed {
		t.Fatalf("seed baseline = %#v, %v", result, err)
	}
	before, err := os.ReadFile(fixture.baselineState)
	if err != nil {
		t.Fatal(err)
	}
	app := fixture.app
	if err := removeSiding(context.Background(), &app, "one", true, ""); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(fixture.baselineState)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("worktree-only removal changed baseline state:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestForcedStopThenFinalRemovePromotesRawHostVolumes(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseGuest, []string{"db"}, true)
	binDir := t.TempDir()
	fakeContainer := filepath.Join(binDir, "container")
	removedMarker := filepath.Join(binDir, "removed")
	script := "#!/bin/sh\nif [ \"$1\" = stop ]; then echo 'cgroup.kill operation not supported' >&2; exit 1; fi\nif [ \"$1\" = rm ]; then touch \"$SHUNT_TEST_REMOVED\"; exit 0; fi\nif [ \"$1\" = inspect ]; then if [ -f \"$SHUNT_TEST_REMOVED\" ]; then echo 'container fixture-one not found' >&2; exit 1; fi; echo '[{\"status\":{\"state\":\"running\"}}]'; exit 0; fi\nexit 0\n"
	if err := os.WriteFile(fakeContainer, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SHUNT_TEST_REMOVED", removedMarker)
	result, err := siding.Stop(context.Background(), fixture.app, "one")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Forced || result.Siding.MaterializationPhase != state.PhaseData {
		t.Fatalf("forced stop = %#v", result)
	}
	persisted, err := state.LoadApp(fixture.app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeSiding(context.Background(), &persisted, "one", true, ""); err != nil {
		t.Fatal(err)
	}
	if got := removalGenerationCount(t, filepath.Dir(fixture.baselineState)); got != 1 {
		t.Fatalf("baseline generation count = %d, want one", got)
	}
}

func TestFinalRemoveUsesRawPromotionOnlyForProvenAbsentGuest(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseGuest, []string{"db"}, true)
	operations := removalTestOperations()
	operations.observeGuest = func(context.Context, string) container.GuestObservation {
		return container.GuestObservation{State: container.GuestAbsent}
	}
	raw := false
	originalPromote := operations.promoteBaseline
	operations.promoteBaseline = func(ctx context.Context, app state.App, sd state.Siding, operationID, sourceRoot string) (databaseline.Result, error) {
		raw = sd.MaterializationPhase == state.PhaseData
		return originalPromote(ctx, app, sd, operationID, sourceRoot)
	}
	operations.removeGuest = func(context.Context, string) error { return nil }
	app := fixture.app
	if err := runRemovalWithOperations(context.Background(), &app, "one", "", operations); err != nil {
		t.Fatal(err)
	}
	if !raw {
		t.Fatal("proven absent guest did not select raw host-volume promotion")
	}
}

func TestFinalRemoveFailsClosedWhenGuestProbeUnavailable(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseGuest, []string{"db"}, true)
	operations := removalTestOperations()
	operations.observeGuest = func(context.Context, string) container.GuestObservation {
		return container.GuestObservation{State: container.GuestUnavailable}
	}
	app := fixture.app
	err := runRemovalWithOperations(context.Background(), &app, "one", "", operations)
	if err == nil || !strings.Contains(err.Error(), "runtime unavailable") {
		t.Fatalf("unavailable guest probe = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(app.ConfigDir, "one", "src")); statErr != nil {
		t.Fatalf("worktree changed after unavailable probe: %v", statErr)
	}
	persisted, loadErr := state.LoadApp(app.ConfigDir)
	if loadErr != nil || persisted.Removal == nil || persisted.Removal.Stage != state.RemovalBasePinned {
		t.Fatalf("removal journal after unavailable probe = %#v, %v", persisted.Removal, loadErr)
	}
}

func TestFinalRemovalRejectsPartialDataBeforeDeletingResources(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseData, []string{"db", "cache"}, false)
	writeRemovalVolume(t, filepath.Join(fixture.app.ConfigDir, "one", "vol"), "db", "partial")
	app := fixture.app
	err := removeSiding(context.Background(), &app, "one", true, "")
	if err == nil || !strings.Contains(err.Error(), "volume \"cache\" is incomplete") {
		t.Fatalf("remove partial data = %v", err)
	}
	persisted, loadErr := state.LoadApp(app.ConfigDir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if persisted.Removal == nil || persisted.Removal.Stage != state.RemovalBasePinned {
		t.Fatalf("partial removal journal = %#v", persisted.Removal)
	}
	if _, err := os.Stat(filepath.Join(app.ConfigDir, "one", "src")); err != nil {
		t.Fatalf("worktree was removed after partial-data rejection: %v", err)
	}
}

func TestSuccessorPinAndRemovalJournalPublishTogether(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	addRemovalSiding(t, &fixture.app, "two", state.PhaseWorktree, false)
	if err := state.SaveApp(fixture.app); err != nil {
		t.Fatal(err)
	}
	crashed := errors.New("stop after preparation")
	operations := removalTestOperations()
	operations.afterCheckpoint = func(stage state.RemovalStage) error {
		if stage == state.RemovalBasePinned {
			return crashed
		}
		return nil
	}
	app := fixture.app
	err := runRemovalWithOperations(context.Background(), &app, "one", "two", operations)
	if !errors.Is(err, crashed) {
		t.Fatalf("prepare base removal = %v", err)
	}
	persisted, err := state.LoadApp(app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.BaseSiding != "two" || persisted.BaseCommit == "" || persisted.Removal == nil || persisted.Removal.Stage != state.RemovalBasePinned {
		t.Fatalf("prepared state = %#v", persisted)
	}
	assertRemovalSourceCommit(t, fixture.control, persisted.BaseCommit)
	if err := removeSiding(context.Background(), &persisted, "one", true, ""); err != nil {
		t.Fatal(err)
	}
	completed, err := state.LoadApp(app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if completed.BaseSiding != "two" || len(completed.Sidings) != 1 {
		t.Fatalf("completed successor state = %#v", completed)
	}
}

func TestConcurrentRemovalIsSerializedByProjectJournal(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseData, []string{"db"}, true)
	entered := make(chan struct{})
	release := make(chan struct{})
	operations := removalTestOperations()
	operations.afterCheckpoint = func(stage state.RemovalStage) error {
		if stage == state.RemovalBasePinned {
			close(entered)
			<-release
		}
		return nil
	}
	firstApp := fixture.app
	first := make(chan error, 1)
	go func() {
		first <- runRemovalWithOperations(context.Background(), &firstApp, "one", "", operations)
	}()
	<-entered

	secondApp := fixture.app
	second := make(chan error, 1)
	go func() {
		second <- removeSiding(context.Background(), &secondApp, "one", true, "")
	}()
	select {
	case err := <-second:
		t.Fatalf("second removal escaped the project transaction: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err == nil || !strings.Contains(err.Error(), "no siding") {
		t.Fatalf("second removal = %v, want completed-removal observation", err)
	}
}

func TestRemoveRevalidatesGitIdentityAfterWaitingForLifecycleLock(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	expected, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	locked := make(chan error, 1)
	go func() {
		locked <- siding.WithProjectSidingOperation(context.Background(), fixture.app.ConfigDir, "one", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	result := make(chan error, 1)
	app := fixture.app
	go func() { result <- removeSiding(context.Background(), &app, "one", false, "", &expected) }()
	select {
	case err := <-result:
		t.Fatalf("remove escaped lifecycle lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	sourceRoot := filepath.Join(fixture.app.ConfigDir, "one", "src")
	if err := os.WriteFile(filepath.Join(sourceRoot, "concurrent.txt"), []byte("new work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-locked; err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "changed while waiting") {
		t.Fatalf("remove after concurrent Git work = %v", err)
	}
	if _, err := os.Stat(sourceRoot); err != nil {
		t.Fatalf("worktree was removed after identity changed: %v", err)
	}
}

func TestCleanupRevalidatesSelectedSidingAfterWaitingForLifecycleLock(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	addRemovalSiding(t, &fixture.app, "two", state.PhaseWorktree, false)
	if err := state.SaveApp(fixture.app); err != nil {
		t.Fatal(err)
	}
	expected, err := captureRemovalSafety(context.Background(), fixture.app, "two", []string{"one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	locked := make(chan error, 1)
	go func() {
		locked <- siding.WithProjectSidingOperation(context.Background(), fixture.app.ConfigDir, "two", func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	result := make(chan error, 1)
	app := fixture.app
	go func() { result <- removeSiding(context.Background(), &app, "two", false, "", &expected) }()
	select {
	case err := <-result:
		t.Fatalf("cleanup removal escaped lifecycle lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	twoRoot := filepath.Join(fixture.app.ConfigDir, "two", "src")
	if err := os.WriteFile(filepath.Join(twoRoot, "concurrent.txt"), []byte("new work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-locked; err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "changed while waiting") {
		t.Fatalf("cleanup removal after concurrent Git work = %v", err)
	}
	if _, err := os.Stat(twoRoot); err != nil {
		t.Fatalf("selected worktree was removed after identity changed: %v", err)
	}
}

func TestCleanupFingerprintsRemainStableAcrossEarlierPlannedRemoval(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	addRemovalSiding(t, &fixture.app, "two", state.PhaseWorktree, false)
	if err := state.SaveApp(fixture.app); err != nil {
		t.Fatal(err)
	}
	selected := []string{"one", "two"}
	safety := make(map[string]removalSafety, len(selected))
	for _, name := range selected {
		current, err := captureRemovalSafety(context.Background(), fixture.app, name, selected)
		if err != nil {
			t.Fatal(err)
		}
		safety[name] = current
	}

	app := fixture.app
	twoSafety := safety["two"]
	if err := removeSiding(context.Background(), &app, "two", false, "", &twoSafety); err != nil {
		t.Fatalf("remove first selected siding: %v", err)
	}
	oneSafety := safety["one"]
	if err := removeSiding(context.Background(), &app, "one", false, "", &oneSafety); err != nil {
		t.Fatalf("remove final selected siding after planned ref disappeared: %v", err)
	}
	if app.Removal != nil || len(app.Sidings) != 0 {
		t.Fatalf("batch removal did not complete: %#v", app)
	}
}

func TestCleanupFingerprintStillRejectsExternalChangeAfterEarlierRemoval(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	addRemovalSiding(t, &fixture.app, "two", state.PhaseWorktree, false)
	if err := state.SaveApp(fixture.app); err != nil {
		t.Fatal(err)
	}
	selected := []string{"one", "two"}
	oneSafety, err := captureRemovalSafety(context.Background(), fixture.app, "one", selected)
	if err != nil {
		t.Fatal(err)
	}
	twoSafety, err := captureRemovalSafety(context.Background(), fixture.app, "two", selected)
	if err != nil {
		t.Fatal(err)
	}
	app := fixture.app
	if err := removeSiding(context.Background(), &app, "two", false, "", &twoSafety); err != nil {
		t.Fatal(err)
	}
	sourceRoot := filepath.Join(app.ConfigDir, "one", "src")
	if err := os.WriteFile(filepath.Join(sourceRoot, "tracked.txt"), []byte("external change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = removeSiding(context.Background(), &app, "one", false, "", &oneSafety)
	if err == nil || !strings.Contains(err.Error(), "changed while waiting") {
		t.Fatalf("remove after external change = %v", err)
	}
	if _, statErr := os.Stat(sourceRoot); statErr != nil {
		t.Fatalf("changed worktree was removed: %v", statErr)
	}
}

func TestRemovalResumeValidatesExistingQuarantineBeforeRetirement(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    bool
		wantError bool
	}{
		{name: "matching bytes retire", mutate: false, wantError: false},
		{name: "mismatching bytes restore", mutate: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
			safety, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
			if err != nil {
				t.Fatal(err)
			}
			operations := removalTestOperations()
			originalQuarantine := operations.quarantineWorktree
			crashAfterQuarantine := errors.New("simulate crash after quarantine before validation")
			var quarantine fsclone.WorktreeQuarantine
			operations.quarantineWorktree = func(ctx context.Context, owner, source, branch, operationID string) (fsclone.WorktreeQuarantine, error) {
				var quarantineErr error
				quarantine, quarantineErr = originalQuarantine(ctx, owner, source, branch, operationID)
				if quarantineErr != nil {
					return quarantine, quarantineErr
				}
				return quarantine, crashAfterQuarantine
			}
			app := fixture.app
			if err := runRemovalWithPolicy(context.Background(), &app, "one", "", false, &safety, operations); !errors.Is(err, crashAfterQuarantine) {
				t.Fatalf("crash after quarantine: %v", err)
			}
			operations.quarantineWorktree = originalQuarantine
			sourceRoot := filepath.Join(app.ConfigDir, "one", "src")
			if app.Removal == nil || app.Removal.Stage != state.RemovalGuestRemoved {
				t.Fatalf("journal after quarantine crash = %#v", app.Removal)
			}
			if tc.mutate {
				if err := os.WriteFile(filepath.Join(quarantine.RecoveryPath, "tracked.txt"), []byte("changed while quarantined\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			err = runRemovalWithPolicy(context.Background(), &app, "one", "", false, nil, operations)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "changed after removal began") {
					t.Fatalf("mismatching quarantine resume = %v", err)
				}
				got, readErr := os.ReadFile(filepath.Join(sourceRoot, "tracked.txt"))
				if readErr != nil || string(got) != "changed while quarantined\n" {
					t.Fatalf("restored quarantine bytes = %q, %v", got, readErr)
				}
				if _, statErr := os.Lstat(quarantine.RecoveryPath); !os.IsNotExist(statErr) {
					t.Fatalf("recovery path remains after restore: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("matching quarantine resume: %v", err)
			}
			if _, statErr := os.Lstat(quarantine.RecoveryPath); !os.IsNotExist(statErr) {
				t.Fatalf("validated recovery path remains: %v", statErr)
			}
		})
	}
}

func TestPhaseDataRemovalObservesExactGuestBeforePromotionAndDeletion(t *testing.T) {
	for _, tc := range []struct {
		name         string
		initial      container.GuestObservationState
		wantPhase    state.MaterializationPhase
		wantRemovals int
		wantError    string
	}{
		{name: "running", initial: container.GuestRunning, wantPhase: state.PhaseGuest, wantRemovals: 1},
		{name: "stopped", initial: container.GuestStopped, wantPhase: state.PhaseGuest, wantRemovals: 1},
		{name: "absent", initial: container.GuestAbsent, wantPhase: state.PhaseData},
		{name: "unavailable", initial: container.GuestUnavailable, wantError: "runtime unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRemovalFixture(t, state.PhaseData, []string{"db"}, true)
			operations := removalTestOperations()
			guestState := tc.initial
			removeCalls := 0
			promotedPhase := state.MaterializationPhase("")
			operations.observeGuest = func(context.Context, string) container.GuestObservation {
				return container.GuestObservation{State: guestState}
			}
			operations.removeGuest = func(context.Context, string) error {
				removeCalls++
				guestState = container.GuestAbsent
				return nil
			}
			operations.promoteBaseline = func(_ context.Context, _ state.App, sd state.Siding, operationID, _ string) (databaseline.Result, error) {
				promotedPhase = sd.MaterializationPhase
				return databaseline.Result{Committed: true, GenerationID: "generation", OperationID: operationID}, nil
			}
			operations.forgetOperation = func(_ context.Context, _ state.App, operationID string) (databaseline.Result, error) {
				return databaseline.Result{Committed: true, OperationID: operationID}, nil
			}
			app := fixture.app
			err := runRemovalWithOperations(context.Background(), &app, "one", "", operations)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("remove = %v, want %q", err, tc.wantError)
				}
				if promotedPhase != "" || removeCalls != 0 {
					t.Fatalf("unavailable guest reached destructive stages: phase=%q removes=%d", promotedPhase, removeCalls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if promotedPhase != tc.wantPhase || removeCalls != tc.wantRemovals {
				t.Fatalf("promotion phase=%q removals=%d, want %q/%d", promotedPhase, removeCalls, tc.wantPhase, tc.wantRemovals)
			}
		})
	}
}

func TestRemovalResumedAtWorktreeRemovedRemovesReappearedGuestBeforeFiles(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseData, nil, false)
	sd := fixture.app.Sidings["one"]
	sourceRoot := filepath.Join(fixture.app.ConfigDir, "one", "src")
	if err := fsclone.RemoveWorktree(context.Background(), state.WorktreeOwner(fixture.app, sd), sourceRoot, sd.Branch); err != nil {
		t.Fatal(err)
	}
	fixture.app.Removal = &state.RemovalOperation{
		ID: "remove-one-resume", Siding: "one", Stage: state.RemovalWorktreeRemoved,
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Force: true,
	}
	if err := state.SaveApp(fixture.app); err != nil {
		t.Fatal(err)
	}
	operations := removalTestOperations()
	guestState := container.GuestRunning
	removeCalls := 0
	filesRemoved := false
	operations.observeGuest = func(context.Context, string) container.GuestObservation {
		return container.GuestObservation{State: guestState}
	}
	operations.removeGuest = func(context.Context, string) error {
		removeCalls++
		guestState = container.GuestAbsent
		return nil
	}
	operations.removeFiles = func(state.App, string) error {
		if guestState != container.GuestAbsent {
			return errors.New("files removed while guest remained present")
		}
		filesRemoved = true
		return nil
	}
	app := fixture.app
	if err := runRemovalWithOperations(context.Background(), &app, "one", "", operations); err != nil {
		t.Fatal(err)
	}
	if removeCalls != 1 || !filesRemoved {
		t.Fatalf("resume removed guest %d times, files removed=%t", removeCalls, filesRemoved)
	}
}

func TestRemovalSafetyChangesForSamePorcelainTrackedContent(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	sourceRoot := filepath.Join(fixture.app.ConfigDir, "one", "src")
	tracked := filepath.Join(sourceRoot, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("first dirty value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracked, []byte("second dirty value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("tracked content change under identical porcelain status was missed")
	}
}

func TestRemovalSafetyChangesInsideExistingUntrackedDirectory(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	untracked := filepath.Join(fixture.app.ConfigDir, "one", "src", "scratch", "value.txt")
	if err := os.MkdirAll(filepath.Dir(untracked), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(untracked, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(untracked, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("content change inside existing untracked directory was missed")
	}
}

func TestRemovalSafetyStreamsLargeUntrackedFile(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	large := filepath.Join(fixture.app.ConfigDir, "one", "src", "large.bin")
	file, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(32 * 1024 * 1024); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	first, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	file, err = os.OpenFile(large, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("changed"), 16*1024*1024); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("large streamed file change was missed")
	}
}

func TestRemovalSafetyRejectsDirtyPopulatedSubmodule(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	root := filepath.Dir(fixture.app.ConfigDir)
	submodule := filepath.Join(root, "submodule")
	runGit(t, root, "init", "-b", "main", submodule)
	if err := os.WriteFile(filepath.Join(submodule, "tracked.txt"), []byte("clean\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, submodule, "add", "tracked.txt")
	runGit(t, submodule, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "initial")
	sourceRoot := filepath.Join(fixture.app.ConfigDir, "one", "src")
	runGit(t, sourceRoot, "-c", "protocol.file.allow=always", "submodule", "add", submodule, "modules/child")
	runGit(t, sourceRoot, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-am", "add submodule")
	if err := os.WriteFile(filepath.Join(sourceRoot, "modules", "child", "untracked.txt"), []byte("do not lose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err == nil || !strings.Contains(err.Error(), "dirty populated submodule") {
		t.Fatalf("captureRemovalSafety() error = %v", err)
	}
}

func TestRemovalSafetyFailsClosedWhenPopulatedSubmoduleStatusFails(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	root := filepath.Dir(fixture.app.ConfigDir)
	submodule := filepath.Join(root, "submodule-status-error")
	runGit(t, root, "init", "-b", "main", submodule)
	if err := os.WriteFile(filepath.Join(submodule, "tracked.txt"), []byte("clean\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, submodule, "add", "tracked.txt")
	runGit(t, submodule, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "initial")
	sourceRoot := filepath.Join(fixture.app.ConfigDir, "one", "src")
	runGit(t, sourceRoot, "-c", "protocol.file.allow=always", "submodule", "add", submodule, "modules/child")
	runGit(t, sourceRoot, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-am", "add submodule")

	childRoot := filepath.Join(sourceRoot, "modules", "child")
	childGitDir := removalGitOutput(t, childRoot, "rev-parse", "--absolute-git-dir")
	if err := os.WriteFile(filepath.Join(childGitDir, "index"), []byte("invalid index"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err == nil || !strings.Contains(err.Error(), "git status failed inside a populated submodule") {
		t.Fatalf("submodule status failure = %v", err)
	}
}

func TestNonForceRemovalQuarantinesThenRestoresLateWrite(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	safety, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	operations := removalTestOperations()
	originalQuarantine := operations.quarantineWorktree
	latePath := "late-before-quarantine.txt"
	operations.quarantineWorktree = func(ctx context.Context, owner, source, branch, operationID string) (fsclone.WorktreeQuarantine, error) {
		if err := os.WriteFile(filepath.Join(source, latePath), []byte("new work\n"), 0o644); err != nil {
			return fsclone.WorktreeQuarantine{}, err
		}
		quarantine, err := originalQuarantine(ctx, owner, source, branch, operationID)
		if err != nil {
			return quarantine, err
		}
		return quarantine, nil
	}
	app := fixture.app
	err = runRemovalWithPolicy(context.Background(), &app, "one", "", false, &safety, operations)
	if err == nil || !strings.Contains(err.Error(), "changed after removal began") {
		t.Fatalf("late-write removal error = %v", err)
	}
	sourceRoot := filepath.Join(app.ConfigDir, "one", "src")
	if got, err := os.ReadFile(filepath.Join(sourceRoot, latePath)); err != nil || string(got) != "new work\n" {
		t.Fatalf("restored late write = %q, %v", got, err)
	}
	journal := app.Removal
	if journal == nil || journal.Stage != state.RemovalGuestRemoved {
		t.Fatalf("resumable journal = %#v", journal)
	}
}

func TestNonForceRemovalRetiresValidatedQuarantine(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	safety, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	app := fixture.app
	if err := runRemovalWithPolicy(context.Background(), &app, "one", "", false, &safety, removalTestOperations()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(app.ConfigDir, "one", "src")); !os.IsNotExist(err) {
		t.Fatalf("worktree remains after validated retirement: %v", err)
	}
	if app.Removal != nil || len(app.Sidings) != 0 {
		t.Fatalf("completed state = %#v", app)
	}
}

func TestResumedGuestRemovedStageRemovesReappearedGuest(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseGuest, nil, false)
	operations := removalTestOperations()
	present := true
	removeCalls := 0
	operations.observeGuest = func(context.Context, string) container.GuestObservation {
		if present {
			return container.GuestObservation{State: container.GuestRunning}
		}
		return container.GuestObservation{State: container.GuestAbsent}
	}
	operations.removeGuest = func(context.Context, string) error {
		removeCalls++
		present = false
		return nil
	}
	stopAfterGuest := errors.New("stop after guest checkpoint")
	operations.afterCheckpoint = func(stage state.RemovalStage) error {
		if stage == state.RemovalGuestRemoved {
			return stopAfterGuest
		}
		return nil
	}
	app := fixture.app
	if err := runRemovalWithOperations(context.Background(), &app, "one", "", operations); !errors.Is(err, stopAfterGuest) {
		t.Fatalf("initial removal = %v", err)
	}
	present = true
	operations.afterCheckpoint = nil
	if err := runRemovalWithOperations(context.Background(), &app, "one", "", operations); err != nil {
		t.Fatalf("resumed removal = %v", err)
	}
	if present || removeCalls != 2 {
		t.Fatalf("guest present = %t, remove calls = %d", present, removeCalls)
	}
}

func TestNonForceRemovalResumeRevalidatesJournaledSafetyAndForceCanUpgrade(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	safety, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	stopAfterJournal := errors.New("stop after journal")
	operations := removalTestOperations()
	operations.afterCheckpoint = func(stage state.RemovalStage) error {
		if stage == state.RemovalBasePinned {
			return stopAfterJournal
		}
		return nil
	}
	app := fixture.app
	if err := runRemovalWithPolicy(context.Background(), &app, "one", "", false, &safety, operations); !errors.Is(err, stopAfterJournal) {
		t.Fatalf("start non-force removal = %v", err)
	}
	tracked := filepath.Join(app.ConfigDir, "one", "src", "tracked.txt")
	if err := os.WriteFile(tracked, []byte("new work after checkpoint\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	persisted, err := state.LoadApp(app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Removal == nil || persisted.Removal.Force || persisted.Removal.Safety != safety.Fingerprint {
		t.Fatalf("journaled policy = %#v", persisted.Removal)
	}
	if err := removeSiding(context.Background(), &persisted, "one", false, ""); err == nil || !strings.Contains(err.Error(), "changed after removal began") {
		t.Fatalf("plain resume after edit = %v", err)
	}
	if err := removeSiding(context.Background(), &persisted, "one", true, ""); err != nil {
		t.Fatalf("explicit force upgrade = %v", err)
	}
	if _, err := os.Stat(filepath.Join(app.ConfigDir, "one", "src")); !os.IsNotExist(err) {
		t.Fatalf("force-upgraded removal did not complete: %v", err)
	}
}

func TestForceUpgradeRetiresRegisteredRecoveryAfterByteDeletionFailure(t *testing.T) {
	fixture := newRemovalFixture(t, state.PhaseWorktree, nil, false)
	safety, err := captureRemovalSafety(context.Background(), fixture.app, "one", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	operations := removalTestOperations()
	wantErr := errors.New("injected quarantine byte deletion failure")
	var recovery fsclone.WorktreeQuarantine
	operations.retireWorktree = func(ctx context.Context, quarantine fsclone.WorktreeQuarantine) error {
		recovery = quarantine
		command := exec.CommandContext(ctx, "git", "-C", quarantine.OwnerPath, "worktree", "repair", quarantine.RecoveryPath)
		if output, repairErr := command.CombinedOutput(); repairErr != nil {
			return errors.Join(wantErr, fmt.Errorf("repair recovery registration: %w: %s", repairErr, output))
		}
		return wantErr
	}

	app := fixture.app
	if err := runRemovalWithPolicy(context.Background(), &app, "one", "", false, &safety, operations); !errors.Is(err, wantErr) {
		t.Fatalf("initial retirement failure = %v", err)
	}
	if app.Removal == nil || app.Removal.Stage != state.RemovalGuestRemoved || app.Removal.Force {
		t.Fatalf("journal after retirement failure = %#v", app.Removal)
	}
	if _, err := os.Lstat(recovery.RecoveryPath); err != nil {
		t.Fatalf("recovery bytes were not preserved: %v", err)
	}
	if got := removalGitOutput(t, recovery.RecoveryPath, "symbolic-ref", "--short", "HEAD"); got != recovery.Branch {
		t.Fatalf("recovery branch = %q, want %q", got, recovery.Branch)
	}
	if err := os.WriteFile(filepath.Join(recovery.RecoveryPath, "tracked.txt"), []byte("force may discard this late change\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	persisted, err := state.LoadApp(app.ConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeSiding(context.Background(), &persisted, "one", true, ""); err != nil {
		t.Fatalf("force resume from registered recovery = %v", err)
	}
	if _, err := os.Lstat(recovery.RecoveryPath); !os.IsNotExist(err) {
		t.Fatalf("recovery remains after force resume: %v", err)
	}
	if persisted.Removal != nil || len(persisted.Sidings) != 0 {
		t.Fatalf("force-resumed removal did not complete: removal=%#v sidings=%#v", persisted.Removal, persisted.Sidings)
	}
}

func newRemovalFixture(t *testing.T, phase state.MaterializationPhase, volumes []string, populateVolumes bool) removalFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(root, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeContainer := filepath.Join(fakeBin, "container")
	if err := os.WriteFile(fakeContainer, []byte("#!/bin/sh\nif [ \"$1\" = inspect ]; then echo \"container $2 not found\" >&2; exit 1; fi\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	origin := filepath.Join(root, "origin")
	runGit(t, root, "init", "-b", "main", origin)
	if err := os.WriteFile(filepath.Join(origin, "tracked.txt"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, origin, "add", "tracked.txt")
	runGit(t, origin, "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "source")
	commit := removalGitOutput(t, origin, "rev-parse", "HEAD")
	configDir := filepath.Join(root, "config")
	control := filepath.Join(configDir, ".control.git")
	pinned, err := fsclone.EnsureControlRepo(context.Background(), control, origin, "", commit)
	if err != nil {
		t.Fatal(err)
	}
	app := state.App{
		Version:         state.StateVersion,
		Name:            "fixture",
		RepoPath:        origin,
		ControlRepoPath: control,
		BaseSiding:      "one",
		BaseCommit:      pinned,
		ConfigDir:       configDir,
		Volumes:         append([]string(nil), volumes...),
		Sidings:         map[string]state.Siding{},
	}
	addRemovalSiding(t, &app, "one", phase, populateVolumes)
	if err := state.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	return removalFixture{
		app:           app,
		control:       control,
		sourceCommit:  commit,
		baselineState: filepath.Join(configDir, ".shunt-baseline-state.json"),
	}
}

func removalTestOperations() removalOperations {
	operations := defaultRemovalOperations()
	operations.observeGuest = func(context.Context, string) container.GuestObservation {
		return container.GuestObservation{State: container.GuestAbsent}
	}
	operations.removeGuest = func(context.Context, string) error { return nil }
	return operations
}

func addRemovalSiding(t *testing.T, app *state.App, name string, phase state.MaterializationPhase, populateVolumes bool) {
	t.Helper()
	sourceRoot := filepath.Join(app.ConfigDir, name, "src")
	branch := "shunt/" + name
	if err := fsclone.AddWorktree(context.Background(), app.ControlRepoPath, sourceRoot, branch, fsclone.BaseRef); err != nil {
		t.Fatal(err)
	}
	app.Sidings[name] = state.Siding{
		Name:                 name,
		Branch:               branch,
		WorktreeRepoPath:     app.ControlRepoPath,
		MaterializationPhase: phase,
		Container:            "fixture-" + name,
		CreatedAt:            time.Now().UTC().Format(time.RFC3339Nano),
	}
	if populateVolumes {
		for _, volume := range app.Volumes {
			writeRemovalVolume(t, filepath.Join(app.ConfigDir, name, "vol"), volume, name+"-data")
		}
	}
}

func runRemovalWithOperations(ctx context.Context, app *state.App, name, successor string, operations removalOperations) error {
	return runRemovalWithPolicy(ctx, app, name, successor, true, nil, operations)
}

func runRemovalWithPolicy(ctx context.Context, app *state.App, name, successor string, force bool, safety *removalSafety, operations removalOperations) error {
	return siding.WithProjectSidingOperation(ctx, app.ConfigDir, name, func() error {
		current, err := state.LoadApp(app.ConfigDir)
		if err != nil {
			return err
		}
		*app = current
		return removeSidingLocked(ctx, app, name, successor, force, safety, operations)
	})
}

func failRemovalPublicationBeforeUpdate(t *testing.T, operations *removalOperations, currentStage state.RemovalStage, failure error) func() {
	t.Helper()
	originalUpdate := operations.updateApp
	failed := false
	operations.updateApp = func(ctx context.Context, configDir string, update func(*state.App) error) (state.App, error) {
		current, err := state.LoadApp(configDir)
		if err != nil {
			return state.App{}, err
		}
		if !failed && current.Removal != nil && current.Removal.Stage == currentStage {
			failed = true
			return current, failure
		}
		return originalUpdate(ctx, configDir, update)
	}
	return func() {
		t.Helper()
		operations.updateApp = originalUpdate
		if !failed {
			t.Fatalf("checkpoint publication failure at %q was not reached", currentStage)
		}
	}
}

func readRemovalBaselineOperations(t *testing.T, path string) (map[string]string, bool) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Operations map[string]string `json:"operations"`
	}
	if err := json.Unmarshal(contents, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.Operations, true
}

func removalGenerationCount(t *testing.T, configDir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(configDir, ".shunt-baseline-generations"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "generation-") {
			count++
		}
	}
	return count
}

func assertRemovalSourceCommit(t *testing.T, control, commit string) {
	t.Helper()
	cmd := exec.Command("git", "-C", control, "cat-file", "-e", commit+"^{commit}")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("preserved source commit %s: %v\n%s", commit, err, out)
	}
}

func removalGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeRemovalVolume(t *testing.T, root, volume, value string) {
	t.Helper()
	dir := filepath.Join(root, volume)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "value"), []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}
