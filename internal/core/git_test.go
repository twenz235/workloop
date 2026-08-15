package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type gitFixture struct {
	root   string
	repo   string
	origin string
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	origin := filepath.Join(root, "origin.git")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, "", "init", "--bare", "-q", origin)
	runGitTest(t, repo, "init", "-q")
	runGitTest(t, repo, "config", "user.email", "test@example.com")
	runGitTest(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "README.md")
	runGitTest(t, repo, "commit", "-q", "-m", "init")
	runGitTest(t, repo, "branch", "-M", "dev")
	runGitTest(t, repo, "remote", "add", "origin", origin)
	runGitTest(t, repo, "push", "-q", origin, "dev:dev")
	runGitTest(t, repo, "fetch", "--no-tags", "origin", "dev")
	return &gitFixture{root: root, repo: repo, origin: origin}
}

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{}, args...)
	if dir != "" {
		commandArgs = append([]string{"-C", dir}, commandArgs...)
	}
	cmd := exec.Command("git", commandArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(commandArgs, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *gitFixture) newWorktree(t *testing.T, id string) string {
	t.Helper()
	worktree := filepath.Join(f.root, id)
	runGitTest(t, f.repo, "worktree", "add", "-q", "-b", "loop/"+id, worktree, originDevRef)
	return worktree
}

func (f *gitFixture) commitWorktree(t *testing.T, worktree, name, content, message string) string {
	t.Helper()
	path := filepath.Join(worktree, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, worktree, "add", name)
	runGitTest(t, worktree, "commit", "-q", "-m", message)
	return runGitTest(t, worktree, "rev-parse", "HEAD")
}

func (f *gitFixture) advanceDev(t *testing.T, name, content, message string) string {
	t.Helper()
	path := filepath.Join(f.repo, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, f.repo, "add", name)
	runGitTest(t, f.repo, "commit", "-q", "-m", message)
	sha := runGitTest(t, f.repo, "rev-parse", "HEAD")
	runGitTest(t, f.repo, "push", "-q", f.origin, "dev:dev")
	return sha
}

func advanceTestOriginDev(t *testing.T, s *State, name, content string) string {
	t.Helper()
	repo := s.Config.RepoPath
	origin := filepath.Join(filepath.Dir(repo), "origin.git")
	branch := "test-origin-update"
	runGitTest(t, repo, "checkout", "-q", "-b", branch)
	path := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", name)
	runGitTest(t, repo, "commit", "-q", "-m", "advance origin dev")
	sha := runGitTest(t, repo, "rev-parse", "HEAD")
	runGitTest(t, repo, "push", "-q", origin, branch+":dev")
	runGitTest(t, repo, "checkout", "-q", "dev")
	runGitTest(t, repo, "branch", "-D", branch)
	return sha
}

func TestSyncDevWorktreeMergesFetchedOriginDev(t *testing.T) {
	f := newGitFixture(t)
	worktree := f.newWorktree(t, "merge")
	f.commitWorktree(t, worktree, "worker.txt", "worker\n", "worker work")
	remoteSHA := f.advanceDev(t, "upstream.txt", "upstream\n", "upstream work")
	if _, err := fetchOriginDev(context.Background(), f.repo); err != nil {
		t.Fatal(err)
	}
	pending, note, err := syncDevWorktree(context.Background(), worktree, "loop/merge")
	if err != nil || pending {
		t.Fatalf("pending=%v note=%q err=%v", pending, note, err)
	}
	if ancestor, err := gitIsAncestor(context.Background(), worktree, originDevRef, "HEAD"); err != nil || !ancestor {
		t.Fatalf("origin/dev is not an ancestor: ancestor=%v err=%v", ancestor, err)
	}
	if got := runGitTest(t, worktree, "rev-parse", "HEAD"); got == remoteSHA {
		t.Fatalf("expected a merge commit, got fast-forward %s", got)
	}
	parents := strings.Fields(runGitTest(t, worktree, "rev-list", "--parents", "-n", "1", "HEAD"))
	if len(parents) != 3 {
		t.Fatalf("parents=%v, want a merge commit", parents)
	}
	if clean, err := gitWorktreeClean(context.Background(), worktree); err != nil || !clean {
		t.Fatalf("worktree clean=%v err=%v", clean, err)
	}
}

func TestSyncDevWorktreePreservesDirtyWorktree(t *testing.T) {
	f := newGitFixture(t)
	worktree := f.newWorktree(t, "dirty")
	before := runGitTest(t, worktree, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(worktree, "uncommitted.txt"), []byte("keep me\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.advanceDev(t, "upstream.txt", "upstream\n", "upstream work")
	if _, err := fetchOriginDev(context.Background(), f.repo); err != nil {
		t.Fatal(err)
	}
	pending, note, err := syncDevWorktree(context.Background(), worktree, "loop/dirty")
	if err != nil || !pending || !strings.Contains(note, "dirty") {
		t.Fatalf("pending=%v note=%q err=%v", pending, note, err)
	}
	if got := runGitTest(t, worktree, "rev-parse", "HEAD"); got != before {
		t.Fatalf("dirty worktree moved from %s to %s", before, got)
	}
	content, err := os.ReadFile(filepath.Join(worktree, "uncommitted.txt"))
	if err != nil || string(content) != "keep me\n" {
		t.Fatalf("uncommitted content=%q err=%v", content, err)
	}
}

func TestSyncDevWorktreeLeavesMergeConflictForWorker(t *testing.T) {
	f := newGitFixture(t)
	worktree := f.newWorktree(t, "conflict")
	f.commitWorktree(t, worktree, "README.md", "worker change\n", "worker change")
	f.advanceDev(t, "README.md", "upstream change\n", "upstream change")
	if _, err := fetchOriginDev(context.Background(), f.repo); err != nil {
		t.Fatal(err)
	}
	pending, note, err := syncDevWorktree(context.Background(), worktree, "loop/conflict")
	if err != nil || !pending || !strings.Contains(note, "resolve") {
		t.Fatalf("pending=%v note=%q err=%v", pending, note, err)
	}
	if mergeHead := gitOutput(worktree, "rev-parse", "MERGE_HEAD"); mergeHead == "" {
		t.Fatal("merge conflict was discarded instead of left for the worker")
	}
	conflicted, err := os.ReadFile(filepath.Join(worktree, "README.md"))
	if err != nil || !strings.Contains(string(conflicted), "<<<<<<<") {
		t.Fatalf("conflict markers=%q err=%v", conflicted, err)
	}
}

func TestSyncDevWorktreeDoesNotMergeWhenAlreadyCurrent(t *testing.T) {
	f := newGitFixture(t)
	worktree := f.newWorktree(t, "current")
	if _, err := fetchOriginDev(context.Background(), f.repo); err != nil {
		t.Fatal(err)
	}
	before := runGitTest(t, worktree, "rev-parse", "HEAD")
	pending, note, err := syncDevWorktree(context.Background(), worktree, "loop/current")
	if err != nil || pending || note != "" {
		t.Fatalf("pending=%v note=%q err=%v", pending, note, err)
	}
	if after := runGitTest(t, worktree, "rev-parse", "HEAD"); after != before {
		t.Fatalf("already-current branch changed from %s to %s", before, after)
	}
}

func TestPrepareEnvelopeStartsDevFromOriginDev(t *testing.T) {
	s := testState(t)
	useLocalOriginFetch(t, s)
	remoteSHA := advanceTestOriginDev(t, s, "origin-only.txt", "origin\n")
	addTestCard(t, s, "origin-base", []string{"origin-only.txt"})
	if _, err := s.Claim("dev", "worker"); err != nil {
		t.Fatal(err)
	}
	envelope, err := s.prepareEnvelope(context.Background(), "origin-base", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if envelope.BaseRef != "dev" || envelope.BaseSHA != remoteSHA {
		t.Fatalf("envelope base ref/sha=%q/%q, want dev/%s", envelope.BaseRef, envelope.BaseSHA, remoteSHA)
	}
	if got := gitOutput(envelope.Worktree, "rev-parse", "HEAD"); got != remoteSHA {
		t.Fatalf("new Dev worktree head=%s, want origin/dev=%s", got, remoteSHA)
	}
	if local := gitOutput(s.Config.RepoPath, "rev-parse", "dev"); local == remoteSHA {
		t.Fatal("test did not leave local dev stale")
	}
}

func TestPrepareQADoesNotHideBaseMovement(t *testing.T) {
	s := testState(t)
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	head := gitOutput(s.Config.RepoPath, "rev-parse", "dev")
	script := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
  "pr view") printf '%%s\n' '{"number":12,"state":"OPEN","baseRefName":"dev","headRefName":"loop/qa-stale","headRefOid":"%s","mergeCommit":null,"url":"https://github.test/pr/12"}' ;;
  *) exit 2 ;;
esac
`, head)
	if err := os.WriteFile(gh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	oldBase := testOriginDevSHA(t, s)
	addTestCard(t, s, "qa-stale", []string{"qa-stale.go"})
	if _, err := s.Claim("dev", "worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move("qa-stale", "in_review", "dev/worker", "PR ready", map[string]any{"pr": 12}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("qa", "qa-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchInternal("qa-stale", map[string]any{"base_sha": oldBase}, "record tested base"); err != nil {
		t.Fatal(err)
	}
	newBase := advanceTestOriginDev(t, s, "qa-stale-upstream.txt", "upstream\n")
	envelope, err := s.prepareEnvelope(context.Background(), "qa-stale", "qa")
	if err != nil {
		t.Fatal(err)
	}
	if envelope.BaseSHA != newBase {
		t.Fatalf("QA envelope base=%s, want current origin/dev=%s", envelope.BaseSHA, newBase)
	}
	_, _, card, err := s.ReadCard("qa-stale")
	if err != nil || card.BaseSHA == nil || *card.BaseSHA != oldBase {
		t.Fatalf("QA preparation hid old base: card=%+v err=%v", card, err)
	}
}

func TestDevReviewGateStopsWhenOriginDevMovesAfterVerification(t *testing.T) {
	s := testState(t)
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	if err := os.WriteFile(gh, []byte(`#!/bin/sh
case "$1 $2" in
  "pr view") printf '%s\n' '{"number":12,"state":"OPEN","baseRefName":"dev","headRefName":"loop/gate","headRefOid":"HEAD_PLACEHOLDER","mergeCommit":null,"url":"https://github.test/pr/12"}' ;;
  *) exit 2 ;;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	useLocalOriginFetch(t, s)
	addTestCard(t, s, "gate", []string{"gate.go"})
	if _, err := s.Claim("dev", "worker"); err != nil {
		t.Fatal(err)
	}
	envelope, err := s.prepareEnvelope(context.Background(), "gate", "dev")
	if err != nil {
		t.Fatal(err)
	}
	head := (&gitFixture{repo: s.Config.RepoPath}).commitWorktree(t, envelope.Worktree, "gate.go", "worker\n", "worker complete")
	data, err := os.ReadFile(gh)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gh, []byte(strings.ReplaceAll(string(data), "HEAD_PLACEHOLDER", head)), 0700); err != nil {
		t.Fatal(err)
	}
	advanceTestOriginDev(t, s, "upstream-gate.txt", "upstream\n")
	s.finishWorker(context.Background(), workerDone{
		cardID:       "gate",
		role:         "dev",
		contractHash: envelope.ContractHash,
		attempt:      envelope.Attempt,
		result: &RunnerResult{
			Version: 1, CardID: "gate", Role: "dev", Attempt: envelope.Attempt,
			Outcome: "completed", Evidence: []string{"verified"}, Branch: envelope.Branch,
			PR: 12, BaseSHA: envelope.BaseSHA, HeadSHA: head,
		},
	})
	status, _, card, err := s.ReadCard("gate")
	if err != nil || status != "todo" {
		t.Fatalf("status=%q card=%+v err=%v", status, card, err)
	}
	if !card.BaseSyncPending || len(card.History) == 0 || !strings.Contains(card.History[len(card.History)-1].Note, "base sync gate failed") {
		t.Fatalf("base gate recovery state=%+v", card)
	}
}

func TestMarkStaleMarksReviewWhenDevBaseMovesWithoutTouchConflict(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "z-merged-card", []string{"merged.go"})
	addTestCard(t, s, "a-review-card", []string{"review.go"})
	if _, err := s.Claim("dev", "worker-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move("a-review-card", "in_review", "dev/worker-a", "PR ready", map[string]any{"pr": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("dev", "worker-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move("z-merged-card", "in_review", "dev/worker-b", "PR ready", map[string]any{"pr": 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchInternal("a-review-card", map[string]any{"base_sha": "old-base"}, "record tested base"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("qa", "qa-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkStale("z-merged-card", "new-base"); err != nil {
		t.Fatal(err)
	}
	status, _, card, err := s.ReadCard("a-review-card")
	if err != nil || status != "in_review" || !card.Stale || card.BaseSHA == nil || *card.BaseSHA != "new-base" {
		t.Fatalf("stale review state status=%q card=%+v err=%v", status, card, err)
	}
	if card.ClaimedBy != nil {
		t.Fatalf("stale QA claim was not released: %+v", card)
	}
}

func TestQAMergeStopsWhenDevBaseChangedBeforeMerge(t *testing.T) {
	s := testState(t)
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	merged := filepath.Join(dir, "merged")
	script := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
	  "pr view") printf '%%s\n' '{"number":12,"state":"OPEN","baseRefName":"dev","headRefName":"loop/qa-base","headRefOid":"head123","mergeCommit":null,"url":"https://github.test/pr/12"}' ;;
	  "pr checks") printf '%%s\n' '[{"name":"CI / test","state":"SUCCESS","bucket":"pass"}]' ;;
  "pr merge") touch %q ;;
  *) exit 2 ;;
esac
`, merged)
	if err := os.WriteFile(gh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	oldBase := testOriginDevSHA(t, s)
	addTestCard(t, s, "qa-base", []string{"qa-base.go"})
	if _, err := s.Claim("dev", "worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move("qa-base", "in_review", "dev/worker", "PR ready", map[string]any{"pr": 12}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("qa", "qa-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchInternal("qa-base", map[string]any{"base_sha": oldBase, "tested_head_sha": "head123", "qa_evidence": []string{"passed"}, "qa_acceptance_results": []AcceptanceResult{{CriterionIndex: 1, Status: "passed", Evidence: "passed"}}}, "QA evidence"); err != nil {
		t.Fatal(err)
	}
	advanceTestOriginDev(t, s, "qa-upstream.txt", "upstream\n")
	if _, err := s.QAMerge(context.Background(), "qa-base", "qa/qa-worker"); err == nil || !strings.Contains(err.Error(), "base changed during QA") {
		t.Fatalf("QAMerge err=%v", err)
	}
	if _, err := os.Stat(merged); err == nil {
		t.Fatal("QA merged a PR after origin/dev changed")
	}
}

func TestQAMergeRejectsPRHeadThatDoesNotContainCurrentOriginDev(t *testing.T) {
	s := testState(t)
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	merged := filepath.Join(dir, "merged")
	oldHead := gitOutput(s.Config.RepoPath, "rev-parse", "dev")
	script := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
  "pr view") printf '%%s\n' '{"number":12,"state":"OPEN","baseRefName":"dev","headRefName":"loop/qa-ancestry","headRefOid":"HEAD_PLACEHOLDER","mergeCommit":null,"url":"https://github.test/pr/12"}' ;;
  "pr checks") printf '%%s\n' '[{"name":"CI / test","state":"SUCCESS","bucket":"pass"}]' ;;
  "pr merge") touch %q ;;
  *) exit 2 ;;
esac
`, merged)
	if err := os.WriteFile(gh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	useLocalOriginFetch(t, s)
	oldBase := testOriginDevSHA(t, s)
	addTestCard(t, s, "qa-ancestry", []string{"qa-ancestry.go"})
	if _, err := s.Claim("dev", "worker"); err != nil {
		t.Fatal(err)
	}
	envelope, err := s.prepareEnvelope(context.Background(), "qa-ancestry", "dev")
	if err != nil {
		t.Fatal(err)
	}
	workerHead := (&gitFixture{repo: s.Config.RepoPath}).commitWorktree(t, envelope.Worktree, "qa-ancestry.go", "worker\n", "worker complete")
	if workerHead == oldHead {
		t.Fatal("worker commit did not advance the PR head")
	}
	data, err := os.ReadFile(gh)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gh, []byte(strings.ReplaceAll(string(data), "HEAD_PLACEHOLDER", workerHead)), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move("qa-ancestry", "in_review", "dev/worker", "PR ready", map[string]any{"pr": 12}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("qa", "qa-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchInternal("qa-ancestry", map[string]any{"base_sha": oldBase, "tested_head_sha": workerHead, "qa_evidence": []string{"passed"}, "qa_acceptance_results": []AcceptanceResult{{CriterionIndex: 1, Status: "passed", Evidence: "passed"}}}, "QA evidence"); err != nil {
		t.Fatal(err)
	}
	newBase := advanceTestOriginDev(t, s, "qa-ancestry-upstream.txt", "upstream\n")
	if _, err := s.PatchInternal("qa-ancestry", map[string]any{"base_sha": newBase}, "refresh observed base"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.QAMerge(context.Background(), "qa-ancestry", "qa/qa-worker"); err == nil || !strings.Contains(err.Error(), "does not include origin/dev") {
		t.Fatalf("QAMerge err=%v", err)
	}
	if _, err := os.Stat(merged); err == nil {
		t.Fatal("QA merged a PR whose head does not include current origin/dev")
	}
}
