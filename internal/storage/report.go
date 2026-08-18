package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gordonbeeming/shunt/internal/container"
	"github.com/gordonbeeming/shunt/internal/gitpreservation"
	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/siding"
	"github.com/gordonbeeming/shunt/internal/state"
)

type Report struct {
	GeneratedAt time.Time                 `json:"generatedAt"`
	Projects    []ProjectReport           `json:"projects"`
	Container   container.SystemDiskUsage `json:"container"`
}

type ProjectReport struct {
	Name         string         `json:"name"`
	ConfigDir    string         `json:"configDir"`
	Filesystem   FilesystemView `json:"filesystem"`
	Logical      Measurement    `json:"logical"`
	Source       SourceReport   `json:"source"`
	Managed      []Measurement  `json:"managed"`
	Unclassified []Measurement  `json:"unclassified"`
	Baselines    []Measurement  `json:"baselines"`
	Sidings      []SidingReport `json:"sidings"`
	fatalErr     error
}

type Measurement struct {
	Name         string `json:"name,omitempty"`
	Path         string `json:"path"`
	Measurement  string `json:"measurement"` // logical
	LogicalBytes int64  `json:"logicalBytes"`
	Shared       bool   `json:"shared,omitempty"`
	Protected    bool   `json:"protected,omitempty"`
	Overlaps     string `json:"overlaps,omitempty"`
	Observation  string `json:"observation"` // observed | missing | error
	Detail       string `json:"detail,omitempty"`
}

type SourceReport struct {
	Measurement Measurement `json:"measurement"`
	Git         GitEvidence `json:"git"`
}

type SidingReport struct {
	Name      string          `json:"name"`
	Logical   Measurement     `json:"logical"`
	Source    SourceReport    `json:"source"`
	Generated Measurement     `json:"generated"`
	Artifacts []TrimCandidate `json:"artifacts,omitempty"`
	Output    Measurement     `json:"output"`
	Data      Measurement     `json:"data"`
}

type GitEvidence struct {
	Observation   string                `json:"observation"`
	Branch        string                `json:"branch,omitempty"`
	Head          string                `json:"head,omitempty"`
	Upstream      string                `json:"upstream,omitempty"`
	Ahead         int                   `json:"ahead"`
	Behind        int                   `json:"behind"`
	Dirty         bool                  `json:"dirty"`
	Untracked     int                   `json:"untracked"`
	UniqueCommits int                   `json:"uniqueCommits"`
	LastCommit    string                `json:"lastCommit,omitempty"`
	Detail        string                `json:"detail,omitempty"`
	Preservation  *PreservationEvidence `json:"preservation,omitempty"`
}

type PreservationEvidence struct {
	Preserved      bool                         `json:"preserved"`
	Kind           string                       `json:"kind"`
	MatchingRef    string                       `json:"matchingRef,omitempty"`
	MatchingCommit string                       `json:"matchingCommit,omitempty"`
	Reason         string                       `json:"reason"`
	Targets        []PreservationTargetEvidence `json:"targets,omitempty"`
}

type PreservationTargetEvidence struct {
	Ref            string `json:"ref"`
	Preserved      bool   `json:"preserved"`
	Kind           string `json:"kind"`
	MatchingRef    string `json:"matchingRef,omitempty"`
	MatchingCommit string `json:"matchingCommit,omitempty"`
	Reason         string `json:"reason"`
}

const projectSidingScanLimit = 4

var (
	reportFilesystemProbe         = Filesystem
	filterTrimCandidatesForReport = filterTrimCandidates
)

// Collect builds a read-only report. Host scans are always labelled logical;
// only Apple's official system-df payload is allowed to claim reclaimability.
func Collect(ctx context.Context, apps []state.App) (Report, error) {
	report := Report{GeneratedAt: time.Now().UTC(), Projects: make([]ProjectReport, 0, len(apps))}
	for _, app := range apps {
		project := collectProject(ctx, app)
		report.Projects = append(report.Projects, project)
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if project.fatalErr != nil {
			return report, fmt.Errorf("scan project %q: %w", project.Name, project.fatalErr)
		}
	}
	sort.Slice(report.Projects, func(i, j int) bool { return report.Projects[i].Name < report.Projects[j].Name })
	report.Container = container.ObserveSystemDiskUsage(ctx)
	if err := ctx.Err(); err != nil {
		return report, err
	}
	return report, nil
}

func collectProject(ctx context.Context, app state.App) ProjectReport {
	project := ProjectReport{
		Name:         app.Name,
		ConfigDir:    app.ConfigDir,
		Logical:      newMeasurement("project", app.ConfigDir, true, false, ""),
		Source:       collectSource(ctx, app.RepoPath),
		Baselines:    collectBaselineMeasurements(app.ConfigDir),
		Sidings:      []SidingReport{},
		Managed:      []Measurement{},
		Unclassified: []Measurement{},
	}
	if project.Source.Measurement.Observation == "error" {
		project.fatalErr = fmt.Errorf("scan registered source %s: %s", project.Source.Measurement.Path, project.Source.Measurement.Detail)
	}
	if fs, err := reportFilesystemProbe(app.ConfigDir); err == nil {
		project.Filesystem = fs
	} else if fs, fallbackErr := reportFilesystemProbe(app.RepoPath); fallbackErr == nil {
		project.Filesystem = fs
	} else {
		project.Filesystem = FilesystemView{
			Path:        app.ConfigDir,
			Measurement: "physical",
			Observation: "unavailable",
			Detail:      fmt.Sprintf("config probe: %v; source fallback: %v", err, fallbackErr),
		}
	}
	names := make([]string, 0, len(app.Sidings))
	for name := range app.Sidings {
		names = append(names, name)
	}
	sort.Strings(names)
	project.Sidings = buildSidingReports(ctx, app, names)
	project.Managed, project.Unclassified = classifyProjectEntries(app, names)
	classified := []*Measurement{&project.Logical}
	for index := range project.Managed {
		classified = append(classified, &project.Managed[index])
	}
	for index := range project.Unclassified {
		classified = append(classified, &project.Unclassified[index])
	}
	for index := range project.Baselines {
		if project.Baselines[index].Observation != "error" {
			classified = append(classified, &project.Baselines[index])
		}
	}
	for index := range project.Sidings {
		sd := &project.Sidings[index]
		classified = append(classified, &sd.Logical, &sd.Source.Measurement, &sd.Output, &sd.Data)
	}
	generated := newGeneratedCollector(project.Sidings)
	if err := measureClassified(ctx, app.ConfigDir, generated, classified...); err != nil {
		project.Logical.Observation = "error"
		project.Logical.Detail = err.Error()
		project.fatalErr = err
		return project
	}
	for index := range project.Sidings {
		if project.Sidings[index].Source.Measurement.Observation != "observed" {
			project.Sidings[index].Generated.Observation = project.Sidings[index].Source.Measurement.Observation
			project.Sidings[index].Generated.Detail = project.Sidings[index].Source.Measurement.Detail
		}
	}
	if err := collectSidingCandidates(ctx, project.Sidings, generated); err != nil {
		project.Logical.Observation = "error"
		project.Logical.Detail = err.Error()
		project.fatalErr = err
	}
	return project
}

func buildSidingReports(ctx context.Context, app state.App, names []string) []SidingReport {
	reports := make([]SidingReport, len(names))
	analyzers := map[string]*gitpreservation.Analyzer{}
	for _, name := range names {
		owner := state.WorktreeOwner(app, app.Sidings[name])
		if analyzers[owner] == nil {
			analyzers[owner] = gitpreservation.NewAnalyzer(owner, gitpreservation.Options{})
		}
	}
	workers := projectSidingScanLimit
	if workers > len(names) {
		workers = len(names)
	}
	if workers == 0 {
		return reports
	}
	jobs := make(chan int)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			for index := range jobs {
				name := names[index]
				owner := state.WorktreeOwner(app, app.Sidings[name])
				reports[index] = collectSidingReport(ctx, app, name, analyzers[owner])
			}
		}()
	}
	for index := range names {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	return reports
}

func collectSidingCandidates(ctx context.Context, reports []SidingReport, generated *generatedCollector) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	names := make([]int, len(reports))
	for index := range reports {
		names[index] = index
	}
	if len(names) == 0 {
		return nil
	}
	workers := projectSidingScanLimit
	if workers > len(reports) {
		workers = len(reports)
	}
	jobs := make(chan int)
	errs := make(chan error, 1)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			for index := range jobs {
				if err := ctx.Err(); err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
				artifacts, err := filterTrimCandidatesForReport(ctx, reports[index].Source.Measurement.Path, generated.candidates[reports[index].Source.Measurement.Path])
				if err != nil {
					select {
					case errs <- err:
					default:
					}
					return
				}
				reports[index].Artifacts = artifacts
				for _, artifact := range artifacts {
					reports[index].Generated.LogicalBytes += artifact.LogicalBytes
				}
			}
		}()
	}
	for _, index := range names {
		select {
		case <-ctx.Done():
			close(jobs)
			wait.Wait()
			return ctx.Err()
		case err := <-errs:
			close(jobs)
			wait.Wait()
			return err
		case jobs <- index:
		}
	}
	close(jobs)
	wait.Wait()
	select {
	case err := <-errs:
		return err
	default:
		return ctx.Err()
	}
}

func collectSidingReport(ctx context.Context, app state.App, name string, analyzer *gitpreservation.Analyzer) SidingReport {
	base, err := siding.SidingBase(app, name)
	if err != nil {
		return SidingReport{Name: name, Logical: errorMeasurement(name, app.ConfigDir, err)}
	}
	src := filepath.Join(base, "src")
	generated := Measurement{Name: "generated", Path: src, Measurement: "logical", Overlaps: "source", Observation: "observed"}
	// The classified project walk also records generated-directory identity and
	// logical size. Git eligibility is checked afterward, so source entries are
	// visited once while candidates still retain their safety checks.
	gitEvidence := InspectGit(ctx, src)
	if gitEvidence.Observation == "observed" && gitEvidence.Branch != "" && gitEvidence.Branch != "(detached)" {
		recorded := app.Sidings[name].Branch
		refs := []string{"refs/heads/" + recorded}
		if gitEvidence.Branch != recorded {
			refs = append(refs, "refs/heads/"+gitEvidence.Branch)
		}
		preserved := true
		kind := ""
		reasons := make([]string, 0, len(refs))
		matchingRef, matchingCommit := "", ""
		targetEvidence := make([]PreservationTargetEvidence, 0, len(refs))
		for _, ref := range refs {
			result := analyzer.Analyze(ctx, ref, refs)
			targetEvidence = append(targetEvidence, PreservationTargetEvidence{Ref: ref, Preserved: result.Preserved, Kind: string(result.Kind), MatchingRef: result.MatchingRef, MatchingCommit: result.MatchingCommit, Reason: result.Reason})
			preserved = preserved && result.Preserved
			if kind == "" {
				kind = string(result.Kind)
				matchingRef, matchingCommit = result.MatchingRef, result.MatchingCommit
			} else if kind != string(result.Kind) {
				kind = "mixed"
				matchingRef, matchingCommit = "", ""
			}
			reasons = append(reasons, ref+": "+result.Reason)
		}
		if len(refs) > 1 {
			matchingRef, matchingCommit = "", ""
		}
		gitEvidence.Preservation = &PreservationEvidence{Preserved: preserved, Kind: kind, MatchingRef: matchingRef, MatchingCommit: matchingCommit, Reason: strings.Join(reasons, "; "), Targets: targetEvidence}
	}
	return SidingReport{
		Name:      name,
		Logical:   newMeasurement(name, base, true, false, ""),
		Source:    SourceReport{Measurement: newMeasurement("source", src, false, false, ""), Git: gitEvidence},
		Generated: generated,
		Output:    newMeasurement("out", filepath.Join(base, "out"), false, false, ""),
		Data:      newMeasurement("data", filepath.Join(base, "vol"), true, false, ""),
	}
}

func classifyProjectEntries(app state.App, sidingNames []string) ([]Measurement, []Measurement) {
	managed := make([]Measurement, 0, 2)
	unclassified := make([]Measurement, 0)
	known := map[string]bool{
		".control.git":                     true,
		".shunt-baseline-generations":      true,
		".shunt-baseline-generation-locks": true,
		".shunt-baseline-state.json":       true,
		".shunt-baseline-state.json.lock":  true,
		".shunt-operation.lock":            true,
		"state.json":                       true,
		"state.json.lock":                  true,
		"state-v2.json":                    true,
		"state-v2.json.lock":               true,
	}
	for _, name := range sidingNames {
		known[name] = true
	}
	controlPath := app.ControlRepoPath
	if controlPath == "" {
		controlPath = filepath.Join(app.ConfigDir, ".control.git")
	}
	if pathWithin(app.ConfigDir, controlPath) {
		if filepath.Dir(controlPath) == filepath.Clean(app.ConfigDir) {
			known[filepath.Base(controlPath)] = true
		}
		if _, err := os.Lstat(controlPath); err == nil {
			managed = append(managed, newMeasurement("source control store", controlPath, true, true, "project"))
		}
	}
	basePath := filepath.Join(app.ConfigDir, "base")
	if entries, err := os.ReadDir(basePath); err == nil {
		for _, entry := range entries {
			path := filepath.Join(basePath, entry.Name())
			if entry.Name() == "images" {
				managed = append(managed, newMeasurement("image cache", path, true, false, "project"))
				continue
			}
			measurement := newMeasurement(filepath.ToSlash(filepath.Join("base", entry.Name())), path, true, false, "project")
			measurement.Detail = "ownership and reclaimability are unverified"
			unclassified = append(unclassified, measurement)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		unclassified = append(unclassified, errorMeasurement("base", basePath, err))
	}
	known["base"] = true
	entries, err := os.ReadDir(app.ConfigDir)
	if err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if known[name] || strings.HasPrefix(name, ".shunt-siding-") || strings.HasPrefix(name, ".shunt-baseline-state.tmp-") || strings.HasPrefix(name, ".tmp-") {
				continue
			}
			path := filepath.Join(app.ConfigDir, name)
			measurement := newMeasurement(name, path, true, false, "project")
			measurement.Detail = "ownership and reclaimability are unverified"
			unclassified = append(unclassified, measurement)
		}
	}
	sort.Slice(managed, func(i, j int) bool { return managed[i].Path < managed[j].Path })
	sort.Slice(unclassified, func(i, j int) bool { return unclassified[i].Path < unclassified[j].Path })
	return managed, unclassified
}

func collectSource(ctx context.Context, path string) SourceReport {
	return SourceReport{Measurement: measurePath(ctx, "source", path, false, false, ""), Git: InspectGit(ctx, path)}
}

func collectBaselineMeasurements(configDir string) []Measurement {
	root := filepath.Join(configDir, ".shunt-baseline-generations")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []Measurement{}
	}
	if err != nil {
		return []Measurement{errorMeasurement("baselines", root, err)}
	}
	current, previous, pointerErr := readBaselinePointers(configDir)
	result := make([]Measurement, 0, len(entries))
	if pointerErr != nil {
		result = append(result, errorMeasurement("baseline state", filepath.Join(configDir, ".shunt-baseline-state.json"), pointerErr))
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		name := entry.Name()
		role := "baseline"
		if name == current {
			role = "current"
		} else if name == previous {
			role = "previous"
		}
		result = append(result, newMeasurement(role, filepath.Join(root, name), true, true, "project"))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}

func readBaselinePointers(configDir string) (current, previous string, err error) {
	contents, err := os.ReadFile(filepath.Join(configDir, ".shunt-baseline-state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	var value struct {
		Current  string `json:"current"`
		Previous string `json:"previous"`
	}
	if err := json.Unmarshal(contents, &value); err != nil {
		return "", "", err
	}
	return value.Current, value.Previous, nil
}

func newMeasurement(name, path string, shared, protected bool, overlaps string) Measurement {
	return Measurement{Name: name, Path: path, Measurement: "logical", Shared: shared, Protected: protected, Overlaps: overlaps}
}

func measurePath(ctx context.Context, name, path string, shared, protected bool, overlaps string) Measurement {
	measurement := Measurement{Name: name, Path: path, Measurement: "logical", Shared: shared, Protected: protected, Overlaps: overlaps}
	bytes, err := LogicalSize(ctx, path)
	switch {
	case err == nil:
		measurement.Observation = "observed"
		measurement.LogicalBytes = bytes
	case errors.Is(err, os.ErrNotExist):
		measurement.Observation = "missing"
	default:
		measurement.Observation = "error"
		measurement.Detail = err.Error()
	}
	return measurement
}

func errorMeasurement(name, path string, err error) Measurement {
	return Measurement{Name: name, Path: path, Measurement: "logical", Observation: "error", Detail: err.Error()}
}

// InspectGit captures source-control evidence without interpreting whether the
// siding is finished.
func InspectGit(ctx context.Context, worktree string) GitEvidence {
	return inspectGit(ctx, worktree, proc.Run)
}

type gitEvidenceRunner func(context.Context, string, ...string) (proc.Result, error)

func inspectGit(ctx context.Context, worktree string, runner gitEvidenceRunner) GitEvidence {
	view := GitEvidence{Observation: "observed"}
	if worktree == "" {
		return GitEvidence{Observation: "missing", Detail: "source path is empty"}
	}
	if _, err := os.Stat(worktree); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return GitEvidence{Observation: "missing", Detail: err.Error()}
		}
		return GitEvidence{Observation: "error", Detail: err.Error()}
	}
	run := func(args ...string) (string, error) {
		result, err := runner(ctx, "git", append([]string{"-C", worktree}, args...)...)
		return strings.TrimSpace(result.Stdout), err
	}
	var err error
	if view.Branch, err = run("symbolic-ref", "--quiet", "--short", "HEAD"); err != nil {
		view.Branch = "(detached)"
	}
	if view.Head, err = run("rev-parse", "HEAD"); err != nil {
		appendGitError(&view, err)
	}
	status, err := run("status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		appendGitError(&view, err)
	} else {
		view.Dirty = status != ""
		for _, line := range strings.Split(status, "\n") {
			if strings.HasPrefix(line, "?? ") {
				view.Untracked++
			}
		}
	}
	if upstream, upstreamErr := run("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); upstreamErr == nil {
		view.Upstream = upstream
		if counts, countErr := run("rev-list", "--left-right", "--count", "HEAD...@{upstream}"); countErr == nil {
			fields := strings.Fields(counts)
			if len(fields) != 2 {
				appendGitError(&view, fmt.Errorf("parse upstream divergence %q", counts))
			} else if ahead, aheadErr := strconv.Atoi(fields[0]); aheadErr != nil {
				appendGitError(&view, fmt.Errorf("parse upstream ahead count %q: %w", fields[0], aheadErr))
			} else if behind, behindErr := strconv.Atoi(fields[1]); behindErr != nil {
				appendGitError(&view, fmt.Errorf("parse upstream behind count %q: %w", fields[1], behindErr))
			} else {
				view.Ahead, view.Behind = ahead, behind
			}
		} else {
			appendGitError(&view, countErr)
		}
	}
	if view.UniqueCommits, err = uniqueCommitCount(ctx, worktree, view.Branch, runner); err != nil {
		appendGitError(&view, err)
	}
	lastCommit, err := run("show", "--no-show-signature", "-s", "--format=%cI", "HEAD")
	if err != nil {
		appendGitError(&view, err)
	} else if view.LastCommit, err = parseLastCommitTimestamp(lastCommit); err != nil {
		appendGitError(&view, err)
	}
	return view
}

func parseLastCommitTimestamp(output string) (string, error) {
	output = strings.TrimSpace(output)
	if output == "" || strings.ContainsAny(output, "\r\n") {
		return "", fmt.Errorf("parse last commit timestamp %q: expected exactly one RFC3339 value", output)
	}
	parsed, err := time.Parse(time.RFC3339, output)
	if err != nil {
		return "", fmt.Errorf("parse last commit timestamp %q: %w", output, err)
	}
	return parsed.Format(time.RFC3339), nil
}

func appendGitError(view *GitEvidence, err error) {
	view.Observation = "error"
	if view.Detail != "" {
		view.Detail += "; "
	}
	view.Detail += err.Error()
}

func uniqueCommitCount(ctx context.Context, worktree, branch string, runner gitEvidenceRunner) (int, error) {
	refsResult, err := runner(ctx, "git", "-C", worktree, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/remotes")
	if err != nil {
		return 0, err
	}
	current := "refs/heads/" + branch
	args := []string{"-C", worktree, "rev-list", "--count", "HEAD", "--not"}
	for _, ref := range strings.Fields(refsResult.Stdout) {
		if ref != current && !strings.HasSuffix(ref, "/HEAD") {
			args = append(args, ref)
		}
	}
	result, err := runner(ctx, "git", args...)
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(result.Stdout))
	return count, err
}

// ValidateReport makes the no-false-reclaimability invariant explicit for
// callers that construct or transform a report.
func ValidateReport(report Report) error {
	if report.Container.Observation == "observed" && !json.Valid(report.Container.Data) {
		return fmt.Errorf("observed container disk usage is not valid JSON")
	}
	return nil
}
