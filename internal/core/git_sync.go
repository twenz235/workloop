package core

import (
	"context"
	"os/exec"
	"strings"
)

var fetchBaseSHAImpl = fetchBaseSHARemote

// fetchBaseSHA refreshes the immutable review base before a worker starts or
// hands work to QA. Reading the local base branch alone is unsafe when another
// card has been merged since the last supervisor tick.
func fetchBaseSHA(ctx context.Context, repoPath, base string) (string, error) {
	return fetchBaseSHAImpl(ctx, repoPath, base)
}

func fetchBaseSHARemote(ctx context.Context, repoPath, base string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "fetch", "--no-tags", "origin", base)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", E(8, "git fetch origin %s failed: %s", base, safeError(string(out)))
	}
	sha := gitOutput(repoPath, "rev-parse", "--verify", "refs/remotes/origin/"+base+"^{commit}")
	if sha == "" {
		return "", E(8, "cannot resolve origin/%s after fetch", base)
	}
	return sha, nil
}

func gitIsAncestor(ctx context.Context, dir, ancestor, descendant string) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "merge-base", "--is-ancestor", ancestor, descendant)
	return cmd.Run() == nil
}

// syncDevWorktreeBase makes a clean existing Dev worktree include the latest
// base before the worker runs. Dirty worktrees and merge conflicts are left for
// the worker to resolve explicitly; no changes are discarded automatically.
func syncDevWorktreeBase(ctx context.Context, worktree, baseSHA string) (bool, error) {
	if strings.TrimSpace(gitOutput(worktree, "status", "--porcelain")) != "" {
		return true, nil
	}
	head := gitOutput(worktree, "rev-parse", "HEAD")
	if head == "" || gitIsAncestor(ctx, worktree, baseSHA, head) {
		return false, nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "merge", "--no-edit", baseSHA)
	if _, err := cmd.CombinedOutput(); err == nil {
		return false, nil
	}
	// Leave the worktree usable for the worker. The worker prompt will retry
	// the merge and resolve the conflict; this path must never auto-discard
	// source changes or manufacture an In Review result.
	_ = exec.CommandContext(ctx, "git", "-C", worktree, "merge", "--abort").Run()
	return true, nil
}
