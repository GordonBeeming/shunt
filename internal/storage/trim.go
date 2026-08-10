// Package storage provides evidence-based disk reporting and narrowly-scoped
// removal of regenerable worktree artifacts.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/gordonbeeming/shunt/internal/proc"
)

// GeneratedDirNames is intentionally an exact-name allowlist. A directory is
// still ineligible unless Git also proves it is ignored and contains no tracked
// files.
var GeneratedDirNames = map[string]struct{}{
	"bin":          {},
	"obj":          {},
	"node_modules": {},
	".next":        {},
	".nuxt":        {},
	".vite":        {},
	".turbo":       {},
	"dist":         {},
	"build":        {},
	"coverage":     {},
}

type TrimCandidate struct {
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
	LogicalBytes int64  `json:"logicalBytes"`
	identity     directoryIdentity
}

// SameTrimCandidateSet compares the filesystem identities that were previewed
// with a fresh locked scan without exposing device/inode details to callers.
func SameTrimCandidateSet(preview, current []TrimCandidate) bool {
	if len(preview) != len(current) {
		return false
	}
	byPath := make(map[string]TrimCandidate, len(preview))
	for _, candidate := range preview {
		byPath[candidate.RelativePath] = candidate
	}
	if len(byPath) != len(preview) {
		return false
	}
	for _, candidate := range current {
		confirmed, ok := byPath[candidate.RelativePath]
		if !ok || confirmed.Path != candidate.Path || confirmed.identity != candidate.identity {
			return false
		}
	}
	return true
}

type TrimResult struct {
	Candidates            []TrimCandidate `json:"candidates"`
	CandidateBytes        int64           `json:"candidateBytes"`
	RemovedBytes          int64           `json:"removedBytes"`
	FilesystemFreeBefore  int64           `json:"filesystemFreeBefore"`
	FilesystemFreeAfter   int64           `json:"filesystemFreeAfter"`
	FilesystemFreeDelta   int64           `json:"filesystemFreeDelta"`
	FilesystemObservation string          `json:"filesystemObservation"` // observed | unavailable
	FilesystemDetail      string          `json:"filesystemDetail,omitempty"`
}

type directoryIdentity struct {
	device   uint64
	inode    uint64
	modified int64
	valid    bool
}

type discoveredCandidate struct {
	TrimCandidate
}

type gitBatchResult struct {
	stdout   []byte
	exitCode int
}

type quarantinedCandidate struct {
	candidate  TrimCandidate
	quarantine string
}

const (
	maxGitPathspecs     = 128
	maxGitPathspecBytes = 24 * 1024
)

var (
	walkTrimTree            = filepath.WalkDir
	onTrimCandidateAncestor = func() {}
	runGitBatch             = runGitBatchCommand
	filesystemProbe         = Filesystem
	quarantineTrimCandidate = quarantineCandidate
	quarantineSeq           uint64
)

// ScanTrimCandidates finds exact allowlisted directories that are both ignored
// and wholly untracked. It does not follow symlinks and never walks .git.
func ScanTrimCandidates(ctx context.Context, worktree string) ([]TrimCandidate, error) {
	root, err := validateWorktree(ctx, worktree)
	if err != nil {
		return nil, err
	}
	discovered, err := discoverTrimCandidates(ctx, root)
	if err != nil {
		return nil, err
	}
	return filterTrimCandidates(ctx, root, discovered)
}

// discoverTrimCandidates walks once and uses only each file's component
// ancestors. Completed sibling directories are never revisited.
func discoverTrimCandidates(ctx context.Context, root string) ([]*discoveredCandidate, error) {
	discovered := make([]*discoveredCandidate, 0)
	byPath := make(map[string]*discoveredCandidate)
	err := walkTrimTree(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			if _, ok := GeneratedDirNames[entry.Name()]; ok {
				rel, err := safeRelative(root, path)
				if err != nil {
					return err
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				identity, err := identifyDirectory(info)
				if err != nil {
					return fmt.Errorf("identify generated directory %s: %w", path, err)
				}
				candidate := &discoveredCandidate{TrimCandidate: TrimCandidate{Path: path, RelativePath: filepath.ToSlash(rel), identity: identity}}
				discovered = append(discovered, candidate)
				byPath[path] = candidate
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		for ancestor := filepath.Dir(path); ancestor != root; ancestor = filepath.Dir(ancestor) {
			onTrimCandidateAncestor()
			if candidate := byPath[ancestor]; candidate != nil {
				candidate.LogicalBytes += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan generated directories under %s: %w", root, err)
	}
	return discovered, nil
}

// filterTrimCandidates performs the only Git work after discovery. Keeping it
// separate lets project reporting reuse an already-classified source walk.
func filterTrimCandidates(ctx context.Context, root string, discovered []*discoveredCandidate) ([]TrimCandidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(discovered) == 0 {
		return []TrimCandidate{}, nil
	}
	paths := make([]string, len(discovered))
	for index, candidate := range discovered {
		paths[index] = candidate.RelativePath
	}
	ignored, tracked, err := gitEligibility(ctx, root, paths)
	if err != nil {
		return nil, err
	}
	sort.Slice(discovered, func(i, j int) bool { return discovered[i].RelativePath < discovered[j].RelativePath })
	candidates := make([]TrimCandidate, 0, len(discovered))
	selected := make(map[string]struct{}, len(discovered))
	for _, candidate := range discovered {
		if !ignored[candidate.RelativePath] || tracked[candidate.RelativePath] || hasSelectedAncestorPath(selected, candidate.RelativePath) {
			continue
		}
		candidates = append(candidates, candidate.TrimCandidate)
		selected[candidate.RelativePath] = struct{}{}
	}
	return candidates, nil
}

// RemoveTrimCandidates revalidates every candidate immediately before removal.
// It removes no path that is a symlink, outside worktree, newly tracked, or no
// longer ignored.
func RemoveTrimCandidates(ctx context.Context, worktree string, candidates []TrimCandidate) (TrimResult, error) {
	root, err := validateWorktree(ctx, worktree)
	if err != nil {
		return TrimResult{}, err
	}
	result := TrimResult{Candidates: append([]TrimCandidate(nil), candidates...)}
	for _, candidate := range candidates {
		result.CandidateBytes += candidate.LogicalBytes
	}
	before, beforeErr := filesystemProbe(root)
	if beforeErr == nil {
		result.FilesystemFreeBefore = before.AvailableBytes
	}
	relativePaths := make([]string, 0, len(candidates))
	for index := range candidates {
		candidate := &candidates[index]
		path := filepath.Clean(candidate.Path)
		rel, err := safeRelative(root, path)
		if err != nil {
			return result, err
		}
		if filepath.ToSlash(rel) != candidate.RelativePath {
			return result, fmt.Errorf("trim candidate changed identity: %q", candidate.RelativePath)
		}
		if _, ok := GeneratedDirNames[filepath.Base(path)]; !ok {
			return result, fmt.Errorf("trim candidate %q is not allowlisted", candidate.RelativePath)
		}
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return result, fmt.Errorf("trim candidate %q disappeared before removal", candidate.RelativePath)
		}
		if err != nil {
			return result, fmt.Errorf("revalidate trim candidate %s: %w", path, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return result, fmt.Errorf("trim candidate %s is not a real directory", path)
		}
		identity, err := identifyDirectory(info)
		if err != nil {
			return result, fmt.Errorf("identify trim candidate %s: %w", path, err)
		}
		if candidate.identity.valid && candidate.identity != identity {
			return result, fmt.Errorf("trim candidate changed identity: %q", candidate.RelativePath)
		}
		candidate.identity = identity
		relativePaths = append(relativePaths, filepath.ToSlash(rel))
	}
	ignored, tracked, err := gitEligibility(ctx, root, relativePaths)
	if err != nil {
		return result, err
	}
	for _, candidate := range candidates {
		if !ignored[candidate.RelativePath] || tracked[candidate.RelativePath] {
			return result, fmt.Errorf("trim candidate %q is no longer safely ignored and untracked", candidate.RelativePath)
		}
	}
	quarantined := make([]quarantinedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		quarantine, err := quarantineTrimCandidate(candidate)
		if err != nil {
			return result, restoreQuarantined(quarantined, err)
		}
		quarantined = append(quarantined, quarantinedCandidate{candidate: candidate, quarantine: quarantine})
		quarantinedInfo, err := os.Lstat(quarantine)
		if err != nil {
			return result, restoreQuarantined(quarantined, fmt.Errorf("inspect quarantined trim candidate %s: %w", quarantine, err))
		}
		quarantinedIdentity, err := identifyDirectory(quarantinedInfo)
		if err != nil || quarantinedIdentity != candidate.identity {
			if err != nil {
				return result, restoreQuarantined(quarantined, fmt.Errorf("identify quarantined trim candidate %s: %w", quarantine, err))
			}
			return result, restoreQuarantined(quarantined, fmt.Errorf("trim candidate changed identity while quarantining: %q", candidate.RelativePath))
		}
	}
	ignored, tracked, err = gitEligibility(ctx, root, relativePaths)
	if err != nil {
		return result, restoreQuarantined(quarantined, err)
	}
	for _, candidate := range candidates {
		if !ignored[candidate.RelativePath] || tracked[candidate.RelativePath] {
			return result, restoreQuarantined(quarantined, fmt.Errorf("trim candidate %q changed Git eligibility while quarantined", candidate.RelativePath))
		}
	}
	for _, candidate := range quarantined {
		if err := os.RemoveAll(candidate.quarantine); err != nil {
			return result, fmt.Errorf("remove quarantined generated directory %s: %w", candidate.quarantine, err)
		}
		result.RemovedBytes += candidate.candidate.LogicalBytes
	}
	if beforeErr != nil {
		result.FilesystemObservation = "unavailable"
		result.FilesystemDetail = beforeErr.Error()
	} else if after, err := filesystemProbe(root); err != nil {
		result.FilesystemObservation = "unavailable"
		result.FilesystemDetail = err.Error()
		result.FilesystemFreeBefore = 0
	} else {
		result.FilesystemObservation = "observed"
		result.FilesystemFreeAfter = after.AvailableBytes
		result.FilesystemFreeDelta = result.FilesystemFreeAfter - result.FilesystemFreeBefore
	}
	return result, nil
}

func validateWorktree(ctx context.Context, worktree string) (string, error) {
	if worktree == "" {
		return "", errors.New("worktree path is required")
	}
	abs, err := filepath.Abs(worktree)
	if err != nil {
		return "", fmt.Errorf("resolve worktree: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("inspect worktree %s: %w", abs, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("worktree %s is not a real directory", abs)
	}
	result, err := proc.Run(ctx, "git", "-C", abs, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return "", fmt.Errorf("%s is not a Git worktree: %w", abs, err)
	}
	if strings.TrimSpace(result.Stdout) != "true" {
		return "", fmt.Errorf("%s is not a Git worktree", abs)
	}
	return filepath.Clean(abs), nil
}

func safeRelative(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside worktree %q", path, root)
	}
	for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		if part == ".git" {
			return "", fmt.Errorf("refusing path inside .git: %q", path)
		}
	}
	return rel, nil
}

func gitEligibility(ctx context.Context, root string, paths []string) (map[string]bool, map[string]bool, error) {
	ignored := make(map[string]bool, len(paths))
	tracked := make(map[string]bool, len(paths))
	if len(paths) == 0 {
		return ignored, tracked, nil
	}
	input := nulDelimited(ignoreDirectoryPaths(paths))
	// --no-index evaluates ignore rules for quarantined (therefore currently
	// absent) original paths; trackedness is checked independently below.
	ignoredResult, err := runGitBatch(ctx, root, []string{"check-ignore", "--no-index", "-z", "--stdin"}, input)
	if err != nil && ignoredResult.exitCode != 1 {
		return nil, nil, fmt.Errorf("batch check ignored generated directories: %w", err)
	}
	for _, path := range splitNUL(ignoredResult.stdout) {
		ignored[strings.TrimSuffix(filepath.ToSlash(path), "/")] = true
	}
	tracked, err = gitTrackedPaths(ctx, root, paths)
	if err != nil {
		return nil, nil, err
	}
	return ignored, tracked, nil
}

func ignoreDirectoryPaths(paths []string) []string {
	result := make([]string, len(paths))
	for index, path := range paths {
		result[index] = strings.TrimSuffix(path, "/") + "/"
	}
	return result
}

func gitTrackedPaths(ctx context.Context, root string, paths []string) (map[string]bool, error) {
	tracked := make(map[string]bool, len(paths))
	// ls-files has no --stdin mode. Bound argv size deterministically while
	// preserving one NUL-delimited index response per pathspec chunk.
	for _, chunk := range chunkPathspecs(paths) {
		trackedArgs := append([]string{"ls-files", "--cached", "-z", "--"}, chunk...)
		trackedResult, err := runGitBatch(ctx, root, trackedArgs, nil)
		if err != nil {
			return nil, fmt.Errorf("batch check tracked generated directories: %w", err)
		}
		for _, file := range splitNUL(trackedResult.stdout) {
			file = filepath.ToSlash(file)
			for _, path := range chunk {
				if file == path || strings.HasPrefix(file, path+"/") {
					tracked[path] = true
				}
			}
		}
	}
	return tracked, nil
}

func chunkPathspecs(paths []string) [][]string {
	chunks := make([][]string, 0, (len(paths)+maxGitPathspecs-1)/maxGitPathspecs)
	chunk := make([]string, 0, maxGitPathspecs)
	bytes := 0
	for _, path := range paths {
		pathBytes := len(path) + 1
		if len(chunk) > 0 && (len(chunk) == maxGitPathspecs || bytes+pathBytes > maxGitPathspecBytes) {
			chunks = append(chunks, chunk)
			chunk = make([]string, 0, maxGitPathspecs)
			bytes = 0
		}
		chunk = append(chunk, path)
		bytes += pathBytes
	}
	if len(chunk) > 0 {
		chunks = append(chunks, chunk)
	}
	return chunks
}

func runGitBatchCommand(ctx context.Context, root string, args []string, input []byte) (gitBatchResult, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := gitBatchResult{stdout: stdout.Bytes()}
	if command.ProcessState != nil {
		result.exitCode = command.ProcessState.ExitCode()
	}
	if err != nil {
		return result, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return result, nil
}

func nulDelimited(paths []string) []byte {
	var input bytes.Buffer
	for _, path := range paths {
		input.WriteString(path)
		input.WriteByte(0)
	}
	return input.Bytes()
}

func splitNUL(value []byte) []string {
	parts := bytes.Split(value, []byte{0})
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 0 {
			result = append(result, string(part))
		}
	}
	return result
}

func hasSelectedAncestor(selected []TrimCandidate, candidate string) bool {
	for _, parent := range selected {
		if strings.HasPrefix(candidate, parent.RelativePath+"/") {
			return true
		}
	}
	return false
}

func hasSelectedAncestorPath(selected map[string]struct{}, candidate string) bool {
	for parent := filepath.ToSlash(filepath.Dir(candidate)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
		if _, ok := selected[parent]; ok {
			return true
		}
	}
	return false
}

func identifyDirectory(info os.FileInfo) (directoryIdentity, error) {
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return directoryIdentity{}, errors.New("not a real directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return directoryIdentity{}, fmt.Errorf("unexpected file identity %T", info.Sys())
	}
	return directoryIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino), modified: info.ModTime().UnixNano(), valid: true}, nil
}

func quarantineCandidate(candidate TrimCandidate) (string, error) {
	parent := filepath.Dir(candidate.Path)
	base := filepath.Base(candidate.Path)
	for attempts := 0; attempts < 100; attempts++ {
		sequence := atomic.AddUint64(&quarantineSeq, 1)
		quarantine := filepath.Join(parent, fmt.Sprintf(".%s.shunt-trim-%d-%d", base, os.Getpid(), sequence))
		if err := os.Rename(candidate.Path, quarantine); err == nil {
			return quarantine, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("quarantine trim candidate %s: %w", candidate.Path, err)
		}
	}
	return "", fmt.Errorf("reserve quarantine path for trim candidate %s", candidate.Path)
}

func restoreQuarantined(candidates []quarantinedCandidate, operationErr error) error {
	var restoreErrs []error
	for index := len(candidates) - 1; index >= 0; index-- {
		candidate := candidates[index]
		if err := os.Rename(candidate.quarantine, candidate.candidate.Path); err != nil {
			restoreErrs = append(restoreErrs, fmt.Errorf("restore quarantined trim candidate %s: %w", candidate.quarantine, err))
		}
	}
	return errors.Join(append([]error{operationErr}, restoreErrs...)...)
}
