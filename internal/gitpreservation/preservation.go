// Package gitpreservation conservatively proves that committed work survives
// outside the exact refs an operation intends to delete.
package gitpreservation

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gordonbeeming/shunt/internal/proc"
)

const (
	MaxTopicCommits       = 512
	MaxIntegrationCommits = 4096
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
}

type gitRunner struct{ repo string }

func (g gitRunner) run(ctx context.Context, args ...string) (string, error) {
	r, err := proc.RunInDir(ctx, g.repo, "git", args...)
	return r.Stdout, err
}

func (g gitRunner) patchID(ctx context.Context, diffArgs ...string) (string, error) {
	args := append([]string{"diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--no-renames"}, diffArgs...)
	r, err := proc.RunPipelineInDir(ctx, g.repo, "git", args, "git", []string{"patch-id", "--verbatim"})
	if err != nil {
		return "", err
	}
	fields := strings.Fields(r.Stdout)
	if len(fields) == 0 {
		return "", nil
	}
	if len(fields) != 2 || len(fields[0]) != 40 {
		return "", fmt.Errorf("malformed git patch-id output")
	}
	return fields[0], nil
}

func Analyze(ctx context.Context, req Request) Result {
	if strings.TrimSpace(req.Repo) == "" || strings.TrimSpace(req.TargetRef) == "" {
		return unproven("repository and target ref are required")
	}
	return analyze(ctx, gitRunner{repo: req.Repo}, req)
}

func analyze(ctx context.Context, git runner, req Request) Result {
	if err := ctx.Err(); err != nil {
		return unproven("analysis cancelled")
	}
	target, err := oneLine(git.run(ctx, "rev-parse", "--verify", req.TargetRef+"^{commit}"))
	if err != nil || target == "" {
		return unproven("target ref is missing or is not a commit")
	}
	deleted := make(map[string]bool, len(req.DeletionRefs))
	for _, ref := range req.DeletionRefs {
		deleted[ref] = true
	}
	deleted[req.TargetRef] = true

	refsOut, err := git.run(ctx, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)", "refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		return unproven("could not enumerate surviving refs")
	}
	refs, err := parseRefs(refsOut, deleted)
	if err != nil {
		return unproven("could not parse surviving refs")
	}
	containingOut, err := git.run(ctx, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)", "--contains="+target, "refs/heads", "refs/remotes", "refs/tags")
	if err != nil {
		return unproven("could not inspect ref reachability")
	}
	containing, err := parseRefs(containingOut, deleted)
	if err != nil {
		return unproven("could not parse reachable refs")
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
		return unproven("target and integration ref have no merge base")
	}
	topic, overflow, err := commits(git, ctx, MaxTopicCommits, base+".."+target)
	if err != nil || overflow {
		if overflow {
			return unproven("topic history exceeds scan limit")
		}
		return unproven("could not inspect topic history")
	}
	if len(topic) == 0 {
		return unproven("topic history contains no unique commits")
	}
	for _, commit := range topic {
		parents, err := fields(git.run(ctx, "rev-list", "--parents", "-n", "1", commit))
		if err != nil || len(parents) != 2 {
			return unproven("topic history contains a merge or malformed commit")
		}
	}
	integrationCommits, overflow, err := commits(git, ctx, MaxIntegrationCommits, base+".."+integrationCommit)
	if err != nil || overflow {
		if overflow {
			return unproven("integration history exceeds scan limit")
		}
		return unproven("could not inspect integration history")
	}

	match, matchCommit, err := equivalentCommits(ctx, git, topic, integrationCommits)
	if err != nil {
		return unproven("could not compare commit patches")
	}
	if match {
		return Result{Preserved: true, Kind: KindEquivalent, MatchingRef: integrationRef, MatchingCommit: matchCommit, Reason: "every topic commit has an equivalent patch on " + integrationRef}
	}

	aggregate, err := git.patchID(ctx, base, target)
	if err != nil || aggregate == "" {
		return unproven("topic has no comparable aggregate patch")
	}
	for _, commit := range integrationCommits {
		id, err := git.patchID(ctx, commit+"^", commit)
		if err != nil {
			return unproven("could not compare squash patches")
		}
		if id == aggregate {
			return Result{Preserved: true, Kind: KindSquash, MatchingRef: integrationRef, MatchingCommit: commit, Reason: "aggregate topic patch matches squash commit " + commit + " on " + integrationRef}
		}
	}
	return unproven("committed work is not proven on a surviving ref")
}

type refInfo struct{ name, oid string }

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

func equivalentCommits(ctx context.Context, git runner, topic, integration []string) (bool, string, error) {
	topicIDs := make([]string, 0, len(topic))
	for _, commit := range topic {
		id, err := git.patchID(ctx, commit+"^", commit)
		if err != nil {
			return false, "", err
		}
		if id == "" {
			return false, "", nil
		}
		topicIDs = append(topicIDs, id)
	}
	next := 0
	last := ""
	for _, commit := range integration {
		id, err := git.patchID(ctx, commit+"^", commit)
		if err != nil {
			return false, "", err
		}
		if next < len(topicIDs) && id == topicIDs[next] {
			next++
			last = commit
		}
	}
	return next == len(topicIDs), last, nil
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

func fields(out string, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func unproven(reason string) Result { return Result{Kind: KindUnproven, Reason: reason} }
