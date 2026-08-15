package core

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const originDevRef = "refs/remotes/origin/dev"

func gitRun(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := execCommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	b, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(b))
	if err != nil {
		return output, E(8, "git %s failed: %s", strings.Join(args, " "), safeError(output))
	}
	return output, nil
}

func fetchOriginDev(ctx context.Context, repoPath string) (string, error) {
	if _, err := gitRun(ctx, repoPath, "fetch", "--no-tags", "origin", "dev"); err != nil {
		return "", err
	}
	sha, err := gitRun(ctx, repoPath, "rev-parse", originDevRef)
	if err != nil || sha == "" {
		if err != nil {
			return "", err
		}
		return "", E(8, "origin/dev did not resolve after fetch")
	}
	return sha, nil
}

func gitIsAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	cmd := execCommandContext(ctx, "git", "-C", dir, "merge-base", "--is-ancestor", ancestor, descendant)
	b, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, E(8, "git merge-base --is-ancestor failed: %s", safeError(strings.TrimSpace(string(b))))
}

func gitWorktreeClean(ctx context.Context, worktree string) (bool, error) {
	status, err := gitRun(ctx, worktree, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	return status == "", nil
}

func gitHead(ctx context.Context, dir string) (string, error) {
	return gitRun(ctx, dir, "rev-parse", "HEAD")
}

func gitBranch(ctx context.Context, dir string) (string, error) {
	return gitRun(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

func verifyPRIncludesOriginDev(ctx context.Context, card *Card, headSHA, baseSHA string) error {
	if card == nil || card.Worktree == nil || *card.Worktree == "" {
		return nil
	}
	ancestor, err := gitIsAncestor(ctx, *card.Worktree, originDevRef, headSHA)
	if err != nil {
		return fmt.Errorf("verify PR head against origin/dev: %w", err)
	}
	if !ancestor {
		return E(2, "dev base changed during QA: PR head %s does not include origin/dev %s", headSHA, baseSHA)
	}
	return nil
}

// syncDevWorktree merges the fetched origin/dev into an existing Dev branch.
// It never discards work: dirty worktrees and merge conflicts are returned as
// pending so the Dev worker can resolve them safely.
func syncDevWorktree(ctx context.Context, worktree, branch string) (bool, string, error) {
	branchNow, err := gitBranch(ctx, worktree)
	if err != nil {
		return false, "", err
	}
	if branchNow != branch {
		return false, "", E(7, "Dev worktree branch is %q, want %q", branchNow, branch)
	}
	clean, err := gitWorktreeClean(ctx, worktree)
	if err != nil {
		return false, "", err
	}
	if !clean {
		return true, "Dev worktree is dirty; origin/dev merge is pending for the worker", nil
	}
	ancestor, err := gitIsAncestor(ctx, worktree, originDevRef, "HEAD")
	if err != nil {
		return false, "", err
	}
	if ancestor {
		return false, "", nil
	}
	if _, err := gitRun(ctx, worktree, "merge", "--no-edit", "origin/dev"); err != nil {
		return true, fmt.Sprintf("origin/dev merge is pending; worker must resolve it safely: %v", err), nil
	}
	ancestor, err = gitIsAncestor(ctx, worktree, originDevRef, "HEAD")
	if err != nil {
		return false, "", err
	}
	if !ancestor {
		return true, "origin/dev is still not an ancestor after merge; worker must resolve base sync", nil
	}
	return false, "", nil
}
