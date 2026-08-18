// Package gitpreservation conservatively proves that committed work survives
// outside the exact refs an operation intends to delete.
package gitpreservation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gordonbeeming/shunt/internal/proc"
)

const (
	MaxTopicCommits       = 512
	MaxIntegrationCommits = 4096
	DefaultMaxPatchBytes  = 256 << 20
	DefaultTimeout        = 30 * time.Second
)

type Kind string

const (
	KindUnproven   Kind = "unproven"
	KindReachable  Kind = "reachable"
	KindEquivalent Kind = "equivalent"
	KindSquash     Kind = "squash"
)

type Request struct {
	Repo         string
	TargetRef    string
	DeletionRefs []string
}

type Result struct {
	Preserved      bool
	Kind           Kind
	MatchingRef    string
	MatchingCommit string
	Reason         string
}

type runner interface {
	run(context.Context, ...string) (string, error)
	patchID(context.Context, ...string) (string, error)
	patchIDs(context.Context, string, int) (string, error)
}

type gitRunner struct {
	repo          string
	maxPatchBytes int64
}

func (g gitRunner) run(ctx context.Context, args ...string) (string, error) {
	r, err := proc.RunInDir(ctx, g.repo, "git", args...)
	return r.Stdout, err
}

func (g gitRunner) patchID(ctx context.Context, diffArgs ...string) (string, error) {
	args := append([]string{"diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--no-renames"}, diffArgs...)
	r, err := proc.RunPipelineInDirLimited(ctx, g.repo, g.maxPatchBytes, "git", args, "git", []string{"patch-id", "--verbatim"})
	if err != nil {
		return "", err
	}
	fields := strings.Fields(r.Stdout)
	if len(fields) == 0 {
		return "", nil
	}
	if len(fields) != 2 || !objectID(fields[0]) || !objectID(fields[1]) {
		return "", fmt.Errorf("malformed git patch-id output")
	}
	return fields[0], nil
}

func (g gitRunner) patchIDs(ctx context.Context, revision string, maxCount int) (string, error) {
	args := []string{"log", "--reverse", "--topo-order", "--format=commit %H", "-p", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--no-renames"}
	if maxCount > 0 {
		args = append(args, fmt.Sprintf("--max-count=%d", maxCount))
	}
	args = append(args, revision)
	r, err := proc.RunPipelineInDirLimited(ctx, g.repo, g.maxPatchBytes, "git", args, "git", []string{"patch-id", "--verbatim"})
	if err != nil {
		return "", err
	}
	return r.Stdout, nil
}

type Options struct {
	MaxPatchBytes int64
	Timeout       time.Duration
}

// Analyzer caches integration evidence for a single command invocation. It is
// safe for concurrent use; callers should create a fresh Analyzer after refs
// may have changed.
type Analyzer struct {
	repo       string
	options    Options
	git        runner
	mu         sync.Mutex
	refs       map[string][]refInfo
	patches    map[string]integrationEvidence
	refFills   map[string]*refFill
	patchFills map[string]*patchFill
}

func NewAnalyzer(repo string, options Options) *Analyzer {
	if options.MaxPatchBytes <= 0 {
		options.MaxPatchBytes = DefaultMaxPatchBytes
	}
	if options.Timeout <= 0 {
		options.Timeout = DefaultTimeout
	}
	return &Analyzer{
		repo:       repo,
		options:    options,
		git:        gitRunner{repo: repo, maxPatchBytes: options.MaxPatchBytes},
		refs:       make(map[string][]refInfo),
		patches:    make(map[string]integrationEvidence),
		refFills:   make(map[string]*refFill),
		patchFills: make(map[string]*patchFill),
	}
}

func Analyze(ctx context.Context, req Request) Result {
	if strings.TrimSpace(req.Repo) == "" || strings.TrimSpace(req.TargetRef) == "" {
		return unproven("repository and target ref are required")
	}
	return NewAnalyzer(req.Repo, Options{}).Analyze(ctx, req.TargetRef, req.DeletionRefs)
}

func (a *Analyzer) Analyze(ctx context.Context, targetRef string, deletionRefs []string) Result {
	if strings.TrimSpace(a.repo) == "" || strings.TrimSpace(targetRef) == "" {
		return unproven("repository and target ref are required")
	}
	ctx, cancel := context.WithTimeout(ctx, a.options.Timeout)
	defer cancel()
	return a.analyze(ctx, Request{Repo: a.repo, TargetRef: targetRef, DeletionRefs: deletionRefs})
}

func analyze(ctx context.Context, git runner, req Request) Result {
	a := &Analyzer{
		repo:       req.Repo,
		git:        git,
		refs:       make(map[string][]refInfo),
		patches:    make(map[string]integrationEvidence),
		refFills:   make(map[string]*refFill),
		patchFills: make(map[string]*patchFill),
	}
	return a.analyze(ctx, req)
}

func (a *Analyzer) analyze(ctx context.Context, req Request) Result {
	git := a.git
	if err := ctx.Err(); err != nil {
		return stageFailure(ctx, "start", err)
	}
	target, err := oneLine(git.run(ctx, "rev-parse", "--verify", req.TargetRef+"^{commit}"))
	if err != nil || target == "" {
		return stageFailure(ctx, "target resolution", err)
	}
	deleted := make(map[string]bool, len(req.DeletionRefs))
	for _, ref := range req.DeletionRefs {
		deleted[ref] = true
	}
	deleted[req.TargetRef] = true

	refs, err := a.survivingRefs(ctx, deleted)
	if err != nil {
		return stageFailure(ctx, "surviving ref enumeration", err)
	}
	containingOut, err := git.run(ctx, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)", "--contains="+target, "refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		return stageFailure(ctx, "reachability inspection", err)
	}
	containing, err := parseRefs(containingOut, deleted)
	if err != nil {
		return stageFailure(ctx, "reachable ref parsing", err)
	}
	if len(containing) > 0 {
		ref := containing[0]
		return Result{Preserved: true, Kind: KindReachable, MatchingRef: ref.name, MatchingCommit: ref.oid, Reason: "target commit is reachable from " + ref.name}
	}

	integrationRef, integrationCommit := integration(refs)
	if integrationRef == "" {
		return unproven("no surviving origin/HEAD or origin/main integration ref")
	}
	base, err := oneLine(git.run(ctx, "merge-base", target, integrationCommit))
	if err != nil || base == "" {
		return stageFailure(ctx, "merge-base resolution", err)
	}
	topic, overflow, err := commits(git, ctx, MaxTopicCommits, base+".."+target)
	if err != nil || overflow {
		if overflow {
			return unproven("topic history exceeds scan limit")
		}
		return stageFailure(ctx, "topic history scan", err)
	}
	if len(topic) == 0 {
		return unproven("topic history contains no unique commits")
	}
	merges, err := git.run(ctx, "rev-list", "--max-count=1", "--min-parents=2", base+".."+target)
	if err != nil {
		return stageFailure(ctx, "topic merge scan", err)
	}
	if strings.TrimSpace(merges) != "" {
		return unproven("topic history contains a merge or malformed commit")
	}

	topicPatches, err := historyPatchIDs(ctx, git, base+".."+target, topic)
	if err != nil {
		return stageFailure(ctx, "topic patch analysis", err)
	}
	if len(topicPatches) == 0 {
		return unproven("topic history contains no comparable patches")
	}
	integrationPatches, err := a.integrationPatches(ctx, base, integrationCommit)
	if err != nil {
		if errors.Is(err, errIntegrationOverflow) {
			return unproven("integration history exceeds scan limit")
		}
		var staged stageError
		if errors.As(err, &staged) {
			return stageFailure(ctx, staged.stage, staged.err)
		}
		return stageFailure(ctx, "integration patch analysis", err)
	}
	match, matchCommit := equivalentCommits(topicPatches, integrationPatches)
	if match {
		return Result{Preserved: true, Kind: KindEquivalent, MatchingRef: integrationRef, MatchingCommit: matchCommit, Reason: "every topic commit has an equivalent patch on " + integrationRef}
	}

	aggregate, err := git.patchID(ctx, base, target)
	if err != nil {
		return stageFailure(ctx, "aggregate patch analysis", err)
	}
	if aggregate == "" {
		return unproven("topic has no comparable aggregate patch")
	}
	for _, patch := range integrationPatches {
		if patch.id == aggregate {
			return Result{Preserved: true, Kind: KindSquash, MatchingRef: integrationRef, MatchingCommit: patch.commit, Reason: "aggregate topic patch matches squash commit " + patch.commit + " on " + integrationRef}
		}
	}
	return unproven("committed work is not proven on a surviving ref")
}

type refInfo struct{ name, oid string }

type refFill struct {
	done chan struct{}
	refs []refInfo
	err  error
}

func deletionKey(deleted map[string]bool) string {
	refs := make([]string, 0, len(deleted))
	for ref := range deleted {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return strings.Join(refs, "\x00")
}

func (a *Analyzer) survivingRefs(ctx context.Context, deleted map[string]bool) ([]refInfo, error) {
	key := deletionKey(deleted)
	a.mu.Lock()
	if a.refs == nil {
		a.refs = make(map[string][]refInfo)
	}
	if a.refFills == nil {
		a.refFills = make(map[string]*refFill)
	}
	if refs, ok := a.refs[key]; ok {
		copyOfRefs := append([]refInfo(nil), refs...)
		a.mu.Unlock()
		return copyOfRefs, nil
	}
	if fill, ok := a.refFills[key]; ok {
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-fill.done:
			return append([]refInfo(nil), fill.refs...), fill.err
		}
	}
	fill := &refFill{done: make(chan struct{})}
	a.refFills[key] = fill
	a.mu.Unlock()
	out, err := a.git.run(ctx, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)", "refs/heads", "refs/remotes", "refs/tags")
	var refs []refInfo
	if err == nil {
		refs, err = parseRefs(out, deleted)
	}
	a.mu.Lock()
	if err == nil {
		a.refs[key] = append([]refInfo(nil), refs...)
	}
	fill.refs = append([]refInfo(nil), refs...)
	fill.err = err
	delete(a.refFills, key)
	close(fill.done)
	a.mu.Unlock()
	return refs, err
}

func parseRefs(out string, deleted map[string]bool) ([]refInfo, error) {
	var refs []refInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x00")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("malformed ref")
		}
		if deleted[parts[0]] || (parts[2] != "" && deleted[parts[2]]) {
			continue
		}
		refs = append(refs, refInfo{parts[0], parts[1]})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	return refs, nil
}

func integration(refs []refInfo) (string, string) {
	for _, wanted := range []string{"refs/remotes/origin/HEAD", "refs/remotes/origin/main"} {
		for _, ref := range refs {
			if ref.name == wanted {
				return ref.name, ref.oid
			}
		}
	}
	return "", ""
}

func commits(git runner, ctx context.Context, limit int, revision string) ([]string, bool, error) {
	out, err := git.run(ctx, "rev-list", "--reverse", "--topo-order", fmt.Sprintf("--max-count=%d", limit+1), revision)
	if err != nil {
		return nil, false, err
	}
	list := strings.Fields(out)
	return list, len(list) > limit, nil
}

type commitPatch struct{ commit, id string }

var errIntegrationOverflow = errors.New("integration history exceeds scan limit")

type integrationEvidence struct {
	commits []string
	patches []commitPatch
}

type patchFill struct {
	done    chan struct{}
	commits []string
	patches []commitPatch
	err     error
}

type stageError struct {
	stage string
	err   error
}

func (e stageError) Error() string { return e.stage + ": " + e.err.Error() }
func (e stageError) Unwrap() error { return e.err }

func (a *Analyzer) integrationPatches(ctx context.Context, base, integrationCommit string) ([]commitPatch, error) {
	evidence, err := a.integrationWindow(ctx, integrationCommit)
	if err != nil {
		return nil, err
	}
	baseIndex := slices.Index(evidence.commits, base)
	if baseIndex < 0 {
		return nil, errIntegrationOverflow
	}
	rangeCommits, overflow, err := commits(a.git, ctx, MaxIntegrationCommits, base+".."+integrationCommit)
	if err != nil {
		return nil, stageError{stage: "integration range scan", err: err}
	}
	if overflow {
		return nil, errIntegrationOverflow
	}
	windowCommits := make(map[string]bool, len(evidence.commits)-baseIndex)
	for _, commit := range evidence.commits[baseIndex:] {
		windowCommits[commit] = true
	}
	patchByCommit := make(map[string]commitPatch, len(evidence.patches))
	for _, patch := range evidence.patches {
		patchByCommit[patch.commit] = patch
	}
	patches := make([]commitPatch, 0, len(rangeCommits))
	for _, commit := range rangeCommits {
		if !windowCommits[commit] {
			return nil, errIntegrationOverflow
		}
		if patch, ok := patchByCommit[commit]; ok {
			patches = append(patches, patch)
		}
	}
	return patches, nil
}

func (a *Analyzer) integrationWindow(ctx context.Context, integrationCommit string) (integrationEvidence, error) {
	key := integrationCommit
	a.mu.Lock()
	if a.patches == nil {
		a.patches = make(map[string]integrationEvidence)
	}
	if a.patchFills == nil {
		a.patchFills = make(map[string]*patchFill)
	}
	if evidence, ok := a.patches[key]; ok {
		copyOfEvidence := copyIntegrationEvidence(evidence)
		a.mu.Unlock()
		return copyOfEvidence, nil
	}
	if fill, ok := a.patchFills[key]; ok {
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return integrationEvidence{}, ctx.Err()
		case <-fill.done:
			return integrationEvidence{commits: append([]string(nil), fill.commits...), patches: append([]commitPatch(nil), fill.patches...)}, fill.err
		}
	}
	fill := &patchFill{done: make(chan struct{})}
	a.patchFills[key] = fill
	a.mu.Unlock()
	windowSize := MaxIntegrationCommits + 1
	out, err := a.git.run(ctx, "rev-list", "--topo-order", fmt.Sprintf("--max-count=%d", windowSize), integrationCommit)
	if err != nil {
		err = stageError{stage: "integration history scan", err: err}
		return a.finishPatchFill(key, fill, integrationEvidence{}, err)
	}
	commits := strings.Fields(out)
	slices.Reverse(commits)
	if len(commits) == 0 {
		return a.finishPatchFill(key, fill, integrationEvidence{}, stageError{stage: "integration history scan", err: fmt.Errorf("empty integration history")})
	}
	patches, err := historyPatchIDsLimited(ctx, a.git, integrationCommit, windowSize, commits)
	if err != nil {
		err = stageError{stage: "integration patch parsing", err: err}
		return a.finishPatchFill(key, fill, integrationEvidence{}, err)
	}
	return a.finishPatchFill(key, fill, integrationEvidence{commits: commits, patches: patches}, nil)
}

func (a *Analyzer) finishPatchFill(key string, fill *patchFill, evidence integrationEvidence, err error) (integrationEvidence, error) {
	a.mu.Lock()
	if err == nil {
		a.patches[key] = copyIntegrationEvidence(evidence)
	}
	fill.commits = append([]string(nil), evidence.commits...)
	fill.patches = append([]commitPatch(nil), evidence.patches...)
	fill.err = err
	delete(a.patchFills, key)
	close(fill.done)
	a.mu.Unlock()
	return evidence, err
}

func copyIntegrationEvidence(evidence integrationEvidence) integrationEvidence {
	return integrationEvidence{commits: append([]string(nil), evidence.commits...), patches: append([]commitPatch(nil), evidence.patches...)}
}

func historyPatchIDs(ctx context.Context, git runner, revision string, commits []string) ([]commitPatch, error) {
	return historyPatchIDsLimited(ctx, git, revision, 0, commits)
}

func historyPatchIDsLimited(ctx context.Context, git runner, revision string, maxCount int, commits []string) ([]commitPatch, error) {
	out, err := git.patchIDs(ctx, revision, maxCount)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	if len(lines) > len(commits) {
		return nil, fmt.Errorf("patch row count %d exceeds commit count %d", len(lines), len(commits))
	}
	seen := make(map[string]bool, len(lines))
	patches := make([]commitPatch, 0, len(lines))
	nextCommit := 0
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) != 2 || !objectID(parts[0]) || !objectID(parts[1]) {
			return nil, fmt.Errorf("malformed patch-id row")
		}
		if seen[parts[1]] {
			return nil, fmt.Errorf("duplicate patch-id commit row")
		}
		for nextCommit < len(commits) && commits[nextCommit] != parts[1] {
			nextCommit++
		}
		if nextCommit == len(commits) {
			return nil, fmt.Errorf("unexpected patch-id commit row")
		}
		nextCommit++
		seen[parts[1]] = true
		patches = append(patches, commitPatch{commit: parts[1], id: parts[0]})
	}
	return patches, nil
}

func objectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func equivalentCommits(topic, integration []commitPatch) (bool, string) {
	next := 0
	last := ""
	for _, patch := range integration {
		if next < len(topic) && patch.id == topic[next].id {
			next++
			last = patch.commit
		}
	}
	return next == len(topic), last
}

func oneLine(out string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	lines := strings.Fields(out)
	if len(lines) != 1 {
		return "", fmt.Errorf("expected one value")
	}
	return lines[0], nil
}

func stageFailure(ctx context.Context, stage string, err error) Result {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return unproven("analysis timed out during " + stage)
	}
	if errors.Is(err, proc.ErrPipelineInputLimit) {
		return unproven("patch data exceeds size limit during " + stage)
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return unproven("analysis cancelled during " + stage)
	}
	if err == nil {
		return unproven("no valid result during " + stage)
	}
	return unproven("git failure during " + stage)
}

func unproven(reason string) Result { return Result{Kind: KindUnproven, Reason: reason} }
