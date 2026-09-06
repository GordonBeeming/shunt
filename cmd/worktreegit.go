package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/gordonbeeming/shunt/internal/fsclone"
	"github.com/gordonbeeming/shunt/internal/proc"
	"github.com/gordonbeeming/shunt/internal/state"
)

func currentWorktreeBranch(ctx context.Context, src string) (string, error) {
	branch, detached, err := currentWorktreeBranchState(ctx, src)
	if err != nil {
		return "", err
	}
	if detached {
		return "", fmt.Errorf("HEAD is detached")
	}
	return branch, nil
}

func currentWorktreeBranchState(ctx context.Context, src string) (branch string, detached bool, err error) {
	result, err := proc.Run(ctx, "git", "-C", src, "symbolic-ref", "--quiet", "HEAD")
	if err != nil {
		if result.ExitCode == 1 {
			return "", true, nil
		}
		return "", false, err
	}
	return strings.TrimPrefix(strings.TrimSpace(result.Stdout), "refs/heads/"), false, nil
}

func ensureControlRepository(ctx context.Context, app *state.App, source, seed string) error {
	if app.ControlRepoPath == "" {
		return fmt.Errorf("managed Git control repository path is not configured")
	}
	if seed == "" {
		seed = "HEAD"
	}
	if _, err := fsclone.EnsureControlRepo(ctx, app.ControlRepoPath, source, app.RepoOrigin, seed); err != nil {
		return err
	}
	return nil
}

func gitText(ctx context.Context, repo string, args ...string) (string, error) {
	full := append([]string{"-C", repo}, args...)
	result, err := proc.Run(ctx, "git", full...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(result.Stdout), nil
}
