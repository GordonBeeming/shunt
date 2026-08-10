package fsclone

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/gordonbeeming/shunt/internal/proc"
)

const BaseRef = "refs/shunt/base"

// EnsureControlRepo creates an independent bare repository atomically and pins
// seedRef at refs/shunt/base. The clone explicitly copies objects rather than
// borrowing them through hardlinks or alternates from the legacy repository.
// Existing control repositories are validated and never replaced.
func EnsureControlRepo(ctx context.Context, controlRepoPath, sourceRepoPath, originURL, seedRef string) (string, error) {
	if controlRepoPath == "" || sourceRepoPath == "" {
		return "", errors.New("control and source repository paths are required")
	}
	if seedRef == "" {
		seedRef = "HEAD"
	}
	if originURL == "" {
		remotes, err := proc.Run(ctx, "git", "-C", sourceRepoPath, "remote")
		if err != nil {
			return "", fmt.Errorf("list source remotes: %w", err)
		}
		for _, remote := range strings.Fields(remotes.Stdout) {
			if remote != "origin" {
				continue
			}
			result, err := proc.Run(ctx, "git", "-C", sourceRepoPath, "remote", "get-url", "origin")
			if err != nil {
				return "", fmt.Errorf("resolve source origin: %w", err)
			}
			originURL = strings.TrimSpace(result.Stdout)
			break
		}
	}

	if _, err := os.Lstat(controlRepoPath); err == nil {
		return validateControlRepo(ctx, controlRepoPath, originURL)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect control repository %q: %w", controlRepoPath, err)
	}

	parent := filepath.Dir(controlRepoPath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("create control repository parent: %w", err)
	}
	stageRoot, err := os.MkdirTemp(parent, ".control-stage-")
	if err != nil {
		return "", fmt.Errorf("create control repository stage: %w", err)
	}
	defer os.RemoveAll(stageRoot)
	stage := filepath.Join(stageRoot, "repo.git")
	if _, err := proc.Run(ctx, "git", "clone", "--bare", "--no-hardlinks", "--", sourceRepoPath, stage); err != nil {
		return "", fmt.Errorf("clone independent control repository: %w", err)
	}
	if originURL != "" {
		if _, err := proc.Run(ctx, "git", "-C", stage, "remote", "set-url", "origin", originURL); err != nil {
			return "", fmt.Errorf("set control repository origin: %w", err)
		}
		if err := configureOriginFetch(ctx, stage); err != nil {
			return "", err
		}
	} else if _, err := proc.Run(ctx, "git", "-C", stage, "remote", "remove", "origin"); err != nil {
		return "", fmt.Errorf("remove synthetic local control origin: %w", err)
	}
	if err := copyIdentityConfig(ctx, sourceRepoPath, stage); err != nil {
		return "", err
	}
	commit, err := PinBaseCommit(ctx, stage, sourceRepoPath, seedRef)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(filepath.Join(stage, "objects", "info", "alternates")); err == nil {
		return "", errors.New("refuse control repository with an object alternates dependency")
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect control repository alternates: %w", err)
	}
	if err := secureControlRepo(stage); err != nil {
		return "", err
	}
	if err := os.Rename(stage, controlRepoPath); err != nil {
		if _, statErr := os.Lstat(controlRepoPath); statErr == nil {
			return validateControlRepo(ctx, controlRepoPath, originURL)
		}
		return "", fmt.Errorf("publish control repository: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return "", fmt.Errorf("durably publish control repository: %w", err)
	}
	return commit, nil
}

func copyIdentityConfig(ctx context.Context, sourceRepoPath, controlRepoPath string) error {
	allowlist := []string{
		"user.name",
		"user.email",
		"user.signingkey",
		"commit.gpgsign",
		"tag.gpgsign",
		"gpg.format",
		"gpg.ssh.allowedSignersFile",
	}
	for _, key := range allowlist {
		result, err := proc.Run(ctx, "git", "-C", sourceRepoPath, "config", "--local", "--get", key)
		if err != nil {
			if result.ExitCode == 1 {
				continue
			}
			return fmt.Errorf("read allow-listed source Git config %q: %w", key, err)
		}
		value := strings.TrimSuffix(result.Stdout, "\n")
		if _, err := proc.Run(ctx, "git", "-C", controlRepoPath, "config", "--local", key, value); err != nil {
			return fmt.Errorf("copy allow-listed Git config %q: %w", key, err)
		}
		verified, err := proc.Run(ctx, "git", "-C", controlRepoPath, "config", "--local", "--get", key)
		if err != nil || strings.TrimSuffix(verified.Stdout, "\n") != value {
			return fmt.Errorf("verify allow-listed Git config %q", key)
		}
	}
	return nil
}

// PinBaseCommit imports commitish from its owning repository and atomically
// updates the protected Shunt base ref. Importing the object makes the control
// repository independent of the owner's continued existence.
func PinBaseCommit(ctx context.Context, controlRepoPath, ownerRepoPath, commitish string) (string, error) {
	commit, err := ImportCommit(ctx, controlRepoPath, ownerRepoPath, commitish)
	if err != nil {
		return "", err
	}
	if _, err := proc.Run(ctx, "git", "-C", controlRepoPath, "update-ref", BaseRef, commit); err != nil {
		return "", fmt.Errorf("pin base commit %s: %w", commit, err)
	}
	return commit, nil
}

// ImportCommit resolves commitish in its owning repository and copies the
// object into the independent control repository without changing the source
// base ref.
func ImportCommit(ctx context.Context, controlRepoPath, ownerRepoPath, commitish string) (string, error) {
	if controlRepoPath == "" || ownerRepoPath == "" || commitish == "" {
		return "", errors.New("control repository, owner repository, and commit are required")
	}
	var commit string
	same, err := samePath(controlRepoPath, ownerRepoPath)
	if err != nil {
		return "", err
	}
	if same {
		result, err := proc.Run(ctx, "git", "-C", controlRepoPath, "rev-parse", "--verify", commitish+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("resolve base commit %q: %w", commitish, err)
		}
		commit = strings.TrimSpace(result.Stdout)
	} else {
		result, err := proc.Run(ctx, "git", "-C", ownerRepoPath, "rev-parse", "--verify", commitish+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("resolve owner base commit %q: %w", commitish, err)
		}
		commit = strings.TrimSpace(result.Stdout)
		if _, err := proc.Run(ctx, "git", "-C", controlRepoPath, "fetch", "--no-tags", "--no-write-fetch-head", "--", ownerRepoPath, commit); err != nil {
			return "", fmt.Errorf("import base commit %s from owner %q: %w", commit, ownerRepoPath, err)
		}
	}
	if _, err := proc.Run(ctx, "git", "-C", controlRepoPath, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return "", fmt.Errorf("verify imported base commit %s: %w", commit, err)
	}
	return commit, nil
}

// ResolveStartPoint makes an explicit new-siding start ref available in the
// control repository without changing refs/shunt/base. It first checks managed
// refs, then the legacy owner for migration compatibility, then fetches the
// exact ref from the configured origin into a short-lived private ref.
func ResolveStartPoint(ctx context.Context, controlRepoPath, ownerRepoPath, ref string) (string, error) {
	if controlRepoPath == "" || ref == "" {
		return "", errors.New("control repository and start ref are required")
	}
	if result, err := proc.Run(ctx, "git", "-C", controlRepoPath, "rev-parse", "--verify", ref+"^{commit}"); err == nil {
		return strings.TrimSpace(result.Stdout), nil
	}
	var ownerErr error
	if ownerRepoPath != "" {
		if commit, err := ImportCommit(ctx, controlRepoPath, ownerRepoPath, ref); err == nil {
			return commit, nil
		} else {
			ownerErr = err
		}
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return "", fmt.Errorf("create temporary import ref: %w", err)
	}
	temporaryRef := "refs/shunt/import/" + hex.EncodeToString(token)
	defer func() {
		_, _ = proc.Run(context.WithoutCancel(ctx), "git", "-C", controlRepoPath, "update-ref", "-d", temporaryRef)
	}()
	refspec := ref + ":" + temporaryRef
	if _, err := proc.Run(ctx, "git", "-C", controlRepoPath, "fetch", "--no-tags", "--no-write-fetch-head", "origin", refspec); err != nil {
		err = redactRemoteError(ctx, controlRepoPath, err)
		if ownerErr != nil {
			return "", fmt.Errorf("resolve start ref %q locally (%v) or from origin: %w", ref, ownerErr, err)
		}
		return "", fmt.Errorf("fetch start ref %q from origin: %w", ref, err)
	}
	result, err := proc.Run(ctx, "git", "-C", controlRepoPath, "rev-parse", "--verify", temporaryRef+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve fetched start ref %q: %w", ref, err)
	}
	commit := strings.TrimSpace(result.Stdout)
	if _, err := proc.Run(ctx, "git", "-C", controlRepoPath, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return "", fmt.Errorf("verify fetched start commit %s: %w", commit, err)
	}
	return commit, nil
}

// AddWorktreeFromRemoteBranch fetches a named branch and resets its local
// tracking branch to the fetched commit before creating the worktree.
func AddWorktreeFromRemoteBranch(ctx context.Context, controlRepoPath, destination, branch string) (string, error) {
	if controlRepoPath == "" || destination == "" || branch == "" {
		return "", errors.New("control repository, worktree destination, and remote branch are required")
	}
	if _, err := proc.Run(ctx, "git", "check-ref-format", "refs/heads/"+branch); err != nil {
		return "", fmt.Errorf("invalid remote branch %q: %w", branch, err)
	}
	if err := configureOriginFetch(ctx, controlRepoPath); err != nil {
		return "", err
	}
	remoteRef := "refs/remotes/origin/" + branch
	refspec := "+refs/heads/" + branch + ":" + remoteRef
	if _, err := proc.Run(ctx, "git", "-C", controlRepoPath, "fetch", "--no-tags", "--no-write-fetch-head", "origin", refspec); err != nil {
		return "", fmt.Errorf("fetch remote branch %q: %w", branch, redactRemoteError(ctx, controlRepoPath, err))
	}
	result, err := proc.Run(ctx, "git", "-C", controlRepoPath, "rev-parse", "--verify", remoteRef+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve fetched remote branch %q: %w", branch, err)
	}
	commit := strings.TrimSpace(result.Stdout)
	if err := AddWorktree(ctx, controlRepoPath, destination, branch, commit); err != nil {
		return "", err
	}
	if _, err := proc.Run(ctx, "git", "-C", destination, "branch", "--set-upstream-to=origin/"+branch, branch); err != nil {
		cleanupErr := RemoveWorktree(context.WithoutCancel(ctx), controlRepoPath, destination, branch)
		return "", errors.Join(fmt.Errorf("set upstream for branch %q: %w", branch, err), cleanupErr)
	}
	return commit, nil
}

func configureOriginFetch(ctx context.Context, repoPath string) error {
	const refspec = "+refs/heads/*:refs/remotes/origin/*"
	if _, err := proc.Run(ctx, "git", "-C", repoPath, "config", "--local", "--replace-all", "remote.origin.fetch", refspec); err != nil {
		return fmt.Errorf("configure control repository origin fetch: %w", err)
	}
	return nil
}

func validateControlRepo(ctx context.Context, repoPath, originURL string) (string, error) {
	result, err := proc.Run(ctx, "git", "-C", repoPath, "rev-parse", "--is-bare-repository")
	if err != nil || strings.TrimSpace(result.Stdout) != "true" {
		return "", fmt.Errorf("control repository %q is not a valid bare repository", repoPath)
	}
	if _, err := os.Lstat(filepath.Join(repoPath, "objects", "info", "alternates")); err == nil {
		return "", errors.New("control repository depends on an alternate object store")
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect control repository alternates: %w", err)
	}
	if err := secureControlRepo(repoPath); err != nil {
		return "", err
	}
	if originURL != "" {
		origin, err := proc.Run(ctx, "git", "-C", repoPath, "remote", "get-url", "origin")
		if err != nil {
			return "", fmt.Errorf("read control repository origin: %w", err)
		}
		if strings.TrimSpace(origin.Stdout) != originURL {
			return "", fmt.Errorf("control repository origin is %q, expected %q", remoteDiagnostic(strings.TrimSpace(origin.Stdout)), remoteDiagnostic(originURL))
		}
	}
	base, err := proc.Run(ctx, "git", "-C", repoPath, "rev-parse", "--verify", BaseRef+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve protected base ref: %w", err)
	}
	return strings.TrimSpace(base.Stdout), nil
}

func secureControlRepo(repoPath string) error {
	info, err := os.Lstat(repoPath)
	if err != nil {
		return fmt.Errorf("inspect control repository permissions: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("control repository %q must be a directory, not a symlink", repoPath)
	}
	if err := os.Chmod(repoPath, 0o700); err != nil {
		return fmt.Errorf("secure control repository %q: %w", repoPath, err)
	}
	configPath := filepath.Join(repoPath, "config")
	configInfo, err := os.Lstat(configPath)
	if err != nil {
		return fmt.Errorf("inspect control repository config: %w", err)
	}
	if configInfo.Mode()&os.ModeSymlink != 0 || !configInfo.Mode().IsRegular() {
		return fmt.Errorf("control repository config %q must be a regular file, not a symlink", configPath)
	}
	if err := os.Chmod(configPath, 0o600); err != nil {
		return fmt.Errorf("secure control repository config: %w", err)
	}
	return nil
}

func redactRemoteError(ctx context.Context, repoPath string, err error) error {
	if err == nil {
		return nil
	}
	result, getErr := proc.Run(context.WithoutCancel(ctx), "git", "-C", repoPath, "remote", "get-url", "origin")
	if getErr != nil {
		return err
	}
	raw := strings.TrimSpace(result.Stdout)
	return errors.New(strings.ReplaceAll(err.Error(), raw, remoteDiagnostic(raw)))
}

func remoteDiagnostic(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil {
		return "redacted remote URL"
	}
	if parsed.Scheme != "" {
		parsed.User = nil
		query := parsed.Query()
		for key := range query {
			query.Set(key, "redacted")
		}
		parsed.RawQuery = query.Encode()
		if parsed.Fragment != "" {
			parsed.Fragment = "redacted"
		}
		return parsed.String()
	}
	if colon := strings.IndexByte(raw, ':'); colon > 0 {
		if at := strings.LastIndexByte(raw[:colon], '@'); at >= 0 {
			return "redacted@" + raw[at+1:]
		}
	}
	return raw
}

func samePath(left, right string) (bool, error) {
	leftAbs, err := filepath.Abs(left)
	if err != nil {
		return false, err
	}
	rightAbs, err := filepath.Abs(right)
	if err != nil {
		return false, err
	}
	return filepath.Clean(leftAbs) == filepath.Clean(rightAbs), nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
