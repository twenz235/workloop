package core

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func testState(t *testing.T) *State {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", "https://github.com/test/repo.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote: %v: %s", err, out)
	}
	_ = exec.Command("git", "-C", repo, "config", "user.email", "test@example.com").Run()
	_ = exec.Command("git", "-C", repo, "config", "user.name", "Test").Run()
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = exec.Command("git", "-C", repo, "add", "README.md").Run()
	if out, err := exec.Command("git", "-C", repo, "commit", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	_ = exec.Command("git", "-C", repo, "branch", "dev").Run()
	s, err := Init("test", repo, filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	s.Config.GitHub.Enabled = false
	if err := s.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	return s
}

func testCard(id string, touches []string) []byte {
	c := map[string]any{
		"id": id, "title": "Test " + id, "problem": "problem", "desired_outcome": "outcome",
		"out_of_scope": []string{"other"}, "repo": "", "repo_path": "", "base": "dev", "tier": "L1",
		"touches": touches, "acceptance": []string{"works"}, "verification": []string{"go test ./..."},
		"depends_on": []string{}, "priority": 2, "risk": map[string]any{"level": "low"}, "rollback_notes": "revert",
		"linear_issue_id": "TEST-" + id, "linear_issue_uuid": "uuid-" + id, "linear_url": "https://linear.test/" + id, "source_revision": "2026-01-01T00:00:00Z", "approved_at": "2026-01-01T00:00:00Z", "approved_by": "user",
	}
	b, _ := json.Marshal(c)
	return b
}

func addTestCard(t *testing.T, s *State, id string, touches []string) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(testCard(id, touches), &raw); err != nil {
		t.Fatal(err)
	}
	raw["repo"] = s.Config.Repo
	raw["repo_path"] = s.Config.RepoPath
	b, _ := json.Marshal(raw)
	if _, err := s.Add(b, "human/test"); err != nil {
		t.Fatal(err)
	}
}

func TestContractHashPreservesCardsWithoutVisuals(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal(testCard("hash", []string{"a.go"}), &raw); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{}
	for _, key := range []string{"problem", "desired_outcome", "out_of_scope", "repo", "repo_path", "base", "tier", "touches", "acceptance", "verification", "depends_on", "risk", "rollback_notes"} {
		legacy[key] = raw[key]
	}
	b, _ := json.Marshal(legacy)
	if got, want := contractHash(raw), Hash(b); got != want {
		t.Fatalf("legacy hash changed: got=%s want=%s", got, want)
	}
	raw["visuals"] = []any{map[string]any{"alt": "flow", "url": "https://example.com/flow.png", "description": "flow"}}
	if got := contractHash(raw); got == Hash(b) {
		t.Fatalf("visual did not change contract hash: %s", got)
	}
}

func TestListUsesLinearBoardStatusAndKeepsRuntimeStatusSeparate(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "board", []string{"src/board.go"})
	if _, err := s.PatchInternal("board", map[string]any{"linear_state": "In Review", "linear_labels": []string{"loop:ready", "type:feature"}}, "Linear snapshot"); err != nil {
		t.Fatal(err)
	}
	items, err := s.List("In Review", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["status"] != "In Review" || items[0]["runtime_status"] != "todo" {
		t.Fatalf("items=%v", items)
	}
}

func TestStatusUsesLinearBoardCounts(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "status-board", []string{"src/status.go"})
	if _, err := s.PatchInternal("status-board", map[string]any{"linear_state": "In Review"}, "Linear snapshot"); err != nil {
		t.Fatal(err)
	}
	result, err := s.Status("qa")
	if err != nil {
		t.Fatal(err)
	}
	counts, ok := result["counts"].(map[string]int)
	if !ok || counts["In Review"] != 1 {
		t.Fatalf("board counts=%v", result["counts"])
	}
	runtimeCounts, ok := result["runtime_counts"].(map[string]int)
	if !ok || runtimeCounts["todo"] != 1 {
		t.Fatalf("runtime counts=%v", result["runtime_counts"])
	}
}

func TestVisualURLValidationRejectsUnsafeMarkdown(t *testing.T) {
	s := testState(t)
	var raw map[string]any
	if err := json.Unmarshal(testCard("visual", []string{"a.go"}), &raw); err != nil {
		t.Fatal(err)
	}
	raw["repo"] = s.Config.Repo
	raw["repo_path"] = s.Config.RepoPath
	raw["visuals"] = []any{map[string]any{"alt": "flow", "url": "https://example.com/a.png\n## injected", "description": "flow"}}
	b, _ := json.Marshal(raw)
	if _, _, err := DecodeCard(b, &s.Config); ExitCode(err) != 2 {
		t.Fatalf("unsafe visual exit=%d err=%v", ExitCode(err), err)
	}
}

func TestInitIdempotentAndRepoBinding(t *testing.T) {
	s := testState(t)
	again, err := Init("test", s.Config.RepoPath, s.Root)
	if err != nil || again.Root != s.Root {
		t.Fatalf("idempotent init: %v", err)
	}
	other := filepath.Join(t.TempDir(), "other")
	_ = os.Mkdir(other, 0700)
	_, _ = exec.Command("git", "-C", other, "init", "-q").CombinedOutput()
	if _, err := Init("test", other, s.Root); ExitCode(err) != 2 {
		t.Fatalf("repo mismatch exit=%d err=%v", ExitCode(err), err)
	}
}

func TestInitLinearBindingIsExplicitAndImmutable(t *testing.T) {
	base := testState(t)
	binding := LinearConfig{Workspace: "Acme", WorkspaceID: "workspace-1", Team: "ENG", TeamID: "team-1"}
	root := filepath.Join(t.TempDir(), "bound-state")
	s, err := Init("bound", base.Config.RepoPath, root, binding)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Config.Linear.Enabled || !sameLinearBinding(s.Config.Linear, binding) {
		t.Fatalf("binding=%+v", s.Config.Linear)
	}
	if _, err := Init("bound", base.Config.RepoPath, root, binding); err != nil {
		t.Fatalf("matching binding should be idempotent: %v", err)
	}
	binding.TeamID = "another-team"
	if _, err := Init("bound", base.Config.RepoPath, root, binding); ExitCode(err) != 2 {
		t.Fatalf("binding mismatch exit=%d err=%v", ExitCode(err), err)
	}
}

func TestConcurrentClaimOneWinner(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "one", []string{"src/a.go"})
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Claim("dev", "w")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				success++
			} else if c := ExitCode(err); c != 3 && c != 4 {
				t.Errorf("unexpected error %v", err)
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Fatalf("success=%d want 1", success)
	}
	status, _, err := s.Locate("one")
	if err != nil || status != "claimed-dev" {
		t.Fatalf("status=%s err=%v", status, err)
	}
}

func TestConflictReservation(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "a", []string{"src/api/**"})
	addTestCard(t, s, "b", []string{"src/api/user.go"})
	first, err := s.Claim("dev", "w1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim("dev", "w2"); ExitCode(err) != 3 {
		t.Fatalf("second claim should be unavailable: first=%v err=%v", first, err)
	}
}

func TestClaimReusesOwnActiveReservation(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "retry", []string{"src/retry.go"})
	if _, err := s.Claim("dev", "w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.withMoveInternal("retry", "needs_attention", "dev/test", "retry requested", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.withMoveInternal("retry", "todo", "human/test", "approved retry", map[string]any{"claimed_at": nil, "claimed_by": nil}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("dev", "w2"); err != nil {
		t.Fatalf("reclaim with active reservation: %v", err)
	}
	if status, _, err := s.Locate("retry"); err != nil || status != "claimed-dev" {
		t.Fatalf("status=%s err=%v", status, err)
	}
}

func TestClaimReopensReleasedReservation(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "released", []string{"src/released.go"})
	if _, err := s.Claim("dev", "w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.withMoveInternal("released", "needs_attention", "dev/test", "retry requested", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.releaseReservation("released"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.withMoveInternal("released", "todo", "human/test", "approved retry", map[string]any{"claimed_at": nil, "claimed_by": nil}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("dev", "w2"); err != nil {
		t.Fatalf("reclaim with released reservation: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(s.Root, "runtime", "reservations", "released.json"))
	if err != nil {
		t.Fatal(err)
	}
	var reservation Reservation
	if err := json.Unmarshal(b, &reservation); err != nil || reservation.ReleasedAt != nil {
		t.Fatalf("reservation=%+v err=%v", reservation, err)
	}
}

func TestMoveRulesAndFindings(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "a", []string{"a"})
	if _, err := s.Claim("dev", "w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move("a", "done", "dev/w1", "", nil); ExitCode(err) != 2 {
		t.Fatalf("direct done allowed: %v", err)
	}
	if _, err := s.Move("a", "in_review", "dev/w1", "", map[string]any{"pr": 12}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("qa", "q1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Findings("a", []Finding{{File: "a", Issue: "bad", Severity: "blocking"}}); ExitCode(err) != 2 {
		t.Fatalf("blocking finding without violates: %v", err)
	}
	finding := Finding{File: "a", Issue: "bad", Severity: "blocking", Violates: "acceptance 1"}
	if _, err := s.Findings("a", []Finding{finding}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Findings("a", []Finding{finding}); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	status, _, _ := s.Locate("a")
	if status != "rework" {
		t.Fatalf("status=%s", status)
	}
}

func TestDoctor(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "a", []string{"a"})
	result, err := s.Doctor()
	if err != nil {
		t.Fatal(err)
	}
	if result["healthy"] != true {
		t.Fatalf("result=%v", result)
	}
}

func TestHotCardIsExclusive(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "normal", []string{"src/a.go"})
	addTestCard(t, s, "hot", []string{"package.json"})
	if _, err := s.Claim("dev", "w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("dev", "w2"); ExitCode(err) != 3 {
		t.Fatalf("hot card should wait, err=%v", err)
	}
}

func TestDependenciesBlockAndAutoUnblock(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "dep", []string{"dep.go"})
	raw := map[string]any{}
	_ = json.Unmarshal(testCard("child", []string{"child.go"}), &raw)
	raw["repo"] = s.Config.Repo
	raw["repo_path"] = s.Config.RepoPath
	raw["depends_on"] = []string{"uuid-dep"}
	b, _ := json.Marshal(raw)
	if _, err := s.Add(b, "human/test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("dev", "w"); err != nil {
		t.Fatal(err)
	}
	status, _, _ := s.Locate("child")
	if status != "blocked" {
		t.Fatalf("status=%s", status)
	}
	if _, err := s.Claim("dev", "w2"); ExitCode(err) != 3 {
		t.Fatalf("blocked claim err=%v", err)
	}
	status, _, _ = s.Locate("child")
	if status != "blocked" {
		t.Fatalf("status=%s", status)
	}
	if _, err := s.withMoveInternal("dep", "done", "qa/sync-done", "test receipt", nil); err != nil {
		t.Fatal(err)
	}
	_ = s.releaseReservation("dep")
	if _, err := s.Claim("dev", "w3"); err != nil {
		t.Fatal(err)
	}
	status, _, _ = s.Locate("child")
	if status != "claimed-dev" {
		t.Fatalf("status=%s", status)
	}
}

func TestMissingDependencyRejected(t *testing.T) {
	s := testState(t)
	raw := map[string]any{}
	_ = json.Unmarshal(testCard("child", []string{"child.go"}), &raw)
	raw["repo"] = s.Config.Repo
	raw["repo_path"] = s.Config.RepoPath
	raw["depends_on"] = []string{"missing"}
	b, _ := json.Marshal(raw)
	if _, err := s.Add(b, "human/test"); ExitCode(err) != 2 {
		t.Fatalf("exit=%d err=%v", ExitCode(err), err)
	}
}

func TestLateWorkerResultCannotOverrideAttention(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "late", []string{"a"})
	if _, err := s.Claim("dev", "w"); err != nil {
		t.Fatal(err)
	}
	_, _, _, card, err := s.readCardPath("late")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.withMoveInternal("late", "needs_attention", "system/sync", "contract changed", map[string]any{"spec_changed": true}); err != nil {
		t.Fatal(err)
	}
	s.finishWorker(context.Background(), workerDone{cardID: "late", role: "dev", contractHash: card.ContractHash, attempt: card.Attempts, result: &RunnerResult{Version: 1, CardID: "late", Role: "dev", Attempt: card.Attempts, Outcome: "completed", PR: 1, HeadSHA: "x"}})
	status, _, _ := s.Locate("late")
	if status != "needs_attention" {
		t.Fatalf("late result moved card to %s", status)
	}
}
