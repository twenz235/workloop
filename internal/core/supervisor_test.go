package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRunnerPromptRequiresBaseRefreshBeforeReview(t *testing.T) {
	prompt := runnerPrompt(RunnerEnvelope{Role: "dev", BaseRef: "dev", BaseSyncRequired: true})
	for _, want := range []string{"fetch origin/dev", "merge it into the branch", "never rebase", "rerun every verification", "synchronizing and resolving"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %s", want, prompt)
		}
	}
}

func TestPrepareEnvelopeRefreshesExistingDevWorktreeFromOriginBase(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "sync-base", []string{"sync.go"})
	if _, err := s.Claim("dev", "worker"); err != nil {
		t.Fatal(err)
	}
	first, err := s.prepareEnvelope(context.Background(), "sync-base", "dev")
	if err != nil {
		t.Fatal(err)
	}
	baseBefore := first.BaseSHA

	repo := s.Config.RepoPath
	runTestGit(t, repo, "checkout", "-q", "dev")
	if err := os.WriteFile(filepath.Join(repo, "base-refresh.txt"), []byte("latest\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", "base-refresh.txt")
	runTestGit(t, repo, "commit", "-m", "advance dev base")

	second, err := s.prepareEnvelope(context.Background(), "sync-base", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if second.BaseSHA == baseBefore {
		t.Fatalf("base did not refresh: before=%s after=%s", baseBefore, second.BaseSHA)
	}
	head := gitOutput(second.Worktree, "rev-parse", "HEAD")
	if !gitIsAncestor(context.Background(), second.Worktree, second.BaseSHA, head) {
		t.Fatalf("worktree %s does not contain refreshed base %s", head, second.BaseSHA)
	}
	if second.BaseSyncRequired {
		t.Fatalf("clean worktree should have been refreshed automatically: %+v", second)
	}
}

func TestFinishWorkerDoesNotEnterReviewWhenDevBaseMoved(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "base-moved", []string{"base.go"})
	if _, err := s.Claim("dev", "worker"); err != nil {
		t.Fatal(err)
	}
	envelope, err := s.prepareEnvelope(context.Background(), "base-moved", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envelope.Worktree, "feature.txt"), []byte("feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, envelope.Worktree, "add", "feature.txt")
	runTestGit(t, envelope.Worktree, "commit", "-m", "feature commit")
	head := gitOutput(envelope.Worktree, "rev-parse", "HEAD")

	repo := s.Config.RepoPath
	runTestGit(t, repo, "checkout", "-q", "dev")
	if err := os.WriteFile(filepath.Join(repo, "base-moved.txt"), []byte("new base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", "base-moved.txt")
	runTestGit(t, repo, "commit", "-m", "move dev base")

	_, _, _, card, err := s.readCardPath("base-moved")
	if err != nil {
		t.Fatal(err)
	}
	s.finishWorker(context.Background(), workerDone{
		cardID:       "base-moved",
		role:         "dev",
		contractHash: card.ContractHash,
		attempt:      card.Attempts,
		baseSHA:      envelope.BaseSHA,
		headSHA:      head,
		result: &RunnerResult{
			Version:  1,
			CardID:   "base-moved",
			Role:     "dev",
			Attempt:  card.Attempts,
			Outcome:  "completed",
			Evidence: []string{"tests passed"},
			Branch:   "loop/base-moved",
			HeadSHA:  head,
		},
	})

	status, _, updated, err := s.ReadCard("base-moved")
	if err != nil {
		t.Fatal(err)
	}
	if status != "todo" {
		t.Fatalf("stale completed work must be retried before review, status=%s", status)
	}
	if !updated.Stale || updated.BaseSHA == nil || *updated.BaseSHA == envelope.BaseSHA {
		t.Fatalf("stale base was not recorded: %+v", updated)
	}
}
