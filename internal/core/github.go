package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	loopglob "github.com/twenz235/workloop/internal/glob"
)

var execCommandContext = exec.CommandContext

type prFact struct {
	Number      int    `json:"number"`
	State       string `json:"state"`
	BaseRefName string `json:"baseRefName"`
	HeadRefName string `json:"headRefName"`
	HeadRefOid  string `json:"headRefOid"`
	MergeCommit *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	URL string `json:"url"`
}

type prCheck struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Bucket string `json:"bucket"`
	Link   string `json:"link"`
}

type githubCache struct {
	FetchedAt string `json:"fetched_at"`
	Repo      string `json:"repo"`
	Base      string `json:"base"`
	Count     int    `json:"count"`
}

func (s *State) OpenPRCount(ctx context.Context) (int, error) {
	if !s.Config.GitHub.Enabled {
		return 0, nil
	}
	path := filepath.Join(s.Root, "runtime", "github.json")
	b, err := s.gh(ctx, "pr", "list", "--repo", s.Config.Repo, "--state", "open", "--base", "dev", "--json", "number,headRefOid")
	if err == nil {
		var prs []map[string]any
		if json.Unmarshal(b, &prs) != nil {
			return 0, E(8, "invalid GitHub PR list")
		}
		cache := githubCache{FetchedAt: Now(), Repo: s.Config.Repo, Base: "dev", Count: len(prs)}
		data, _ := Encode(cache)
		if e := writeAtomic(path, data, 0600); e != nil {
			return 0, e
		}
		return max(len(prs), s.localOpenPRCount()), nil
	}
	var cache githubCache
	if data, e := os.ReadFile(path); e == nil && json.Unmarshal(data, &cache) == nil && cache.Repo == s.Config.Repo && cache.Base == "dev" {
		t, _ := time.Parse(time.RFC3339Nano, cache.FetchedAt)
		if !t.IsZero() && time.Since(t) <= time.Duration(s.Config.GitHub.OpenPRCacheMaxAgeSec)*time.Second {
			return max(cache.Count, s.localOpenPRCount()), nil
		}
	}
	return 0, E(8, "GitHub unavailable and open-PR cache is stale")
}
func (s *State) localOpenPRCount() int {
	cards, err := s.AllCards()
	if err != nil {
		return 0
	}
	n := 0
	for _, x := range cards {
		if x.Status == "done" || x.Status == "cancelled" {
			continue
		}
		if _, e := prNumber(x.Card.PR); e == nil {
			n++
		}
	}
	return n
}

func prNumber(v any) (int, error) {
	switch x := v.(type) {
	case float64:
		return int(x), nil
	case int:
		return x, nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		return n, err
	case map[string]any:
		if n, ok := x["number"]; ok {
			return prNumber(n)
		}
	}
	return 0, fmt.Errorf("card has no PR number")
}

func (s *State) ghRaw(ctx context.Context, args ...string) ([]byte, error) {
	path, err := exec.LookPath("gh")
	if err != nil {
		return nil, E(8, "gh CLI unavailable")
	}
	cmd := execCommandContext(ctx, path, args...)
	cmd.Dir = s.Config.RepoPath
	return cmd.CombinedOutput()
}

func (s *State) gh(ctx context.Context, args ...string) ([]byte, error) {
	b, err := s.ghRaw(ctx, args...)
	if err != nil {
		return nil, E(8, "gh failed: %s", strings.TrimSpace(string(b)))
	}
	return b, nil
}

func (s *State) prView(ctx context.Context, n int) (prFact, error) {
	b, err := s.gh(ctx, "pr", "view", strconv.Itoa(n), "--repo", s.Config.Repo, "--json", "number,state,baseRefName,headRefName,headRefOid,mergeCommit,url")
	if err != nil {
		return prFact{}, err
	}
	var p prFact
	if err := json.Unmarshal(b, &p); err != nil {
		return p, E(8, "invalid gh response: %v", err)
	}
	return p, nil
}

func (s *State) verifyPRChecks(ctx context.Context, n int) ([]string, error) {
	b, commandErr := s.ghRaw(ctx, "pr", "checks", strconv.Itoa(n), "--repo", s.Config.Repo, "--json", "name,state,bucket,link")
	if len(bytes.TrimSpace(b)) == 0 {
		if commandErr != nil {
			return nil, E(8, "gh checks failed: %v", commandErr)
		}
		return nil, E(2, "GitHub returned no checks for PR %d", n)
	}
	var checks []prCheck
	if err := json.Unmarshal(b, &checks); err != nil {
		return nil, E(8, "invalid gh checks response: %v", err)
	}
	if len(checks) == 0 {
		return nil, E(2, "GitHub returned no checks for PR %d", n)
	}
	passed := []string{}
	failed := []string{}
	for _, check := range checks {
		name := strings.TrimSpace(check.Name)
		if name == "" {
			name = "<unnamed check>"
		}
		bucket := strings.ToLower(strings.TrimSpace(check.Bucket))
		state := strings.ToUpper(strings.TrimSpace(check.State))
		if bucket == "pass" || (bucket == "" && state == "SUCCESS") {
			passed = append(passed, name)
			continue
		}
		status := bucket
		if status == "" {
			status = strings.ToLower(state)
		}
		failed = append(failed, fmt.Sprintf("%s (%s)", name, status))
	}
	if len(failed) > 0 {
		return nil, E(2, "GitHub checks not passing: %s", strings.Join(failed, ", "))
	}
	sort.Strings(passed)
	return passed, nil
}

func (s *State) QAMerge(ctx context.Context, id, by string) (map[string]any, error) {
	parts := strings.Split(by, "/")
	if len(parts) != 2 || parts[0] != "qa" || !workerIDPattern.MatchString(parts[1]) {
		return nil, E(2, "--by must identify qa worker")
	}
	receiptPath := filepath.Join(s.Root, "runtime", "merges", id+".json")
	if b, err := os.ReadFile(receiptPath); err == nil {
		var receipt map[string]any
		if json.Unmarshal(b, &receipt) == nil {
			return receipt, nil
		}
	}
	status, _, _, card, err := s.readCardPath(id)
	if err != nil {
		return nil, err
	}
	if status != "claimed-qa" {
		return nil, E(2, "qa-merge requires claimed-qa")
	}
	if card.Base != "dev" {
		return nil, E(2, "qa-merge only permits base dev")
	}
	if card.Stale || card.SpecChanged {
		return nil, E(2, "stale or changed card cannot merge")
	}
	if card.BaseSyncPending {
		return nil, E(2, "Dev base sync is not complete")
	}
	for _, f := range card.QAFindings {
		if f.Severity == "blocking" {
			return nil, E(2, "blocking findings remain")
		}
	}
	if err := storedAcceptanceResultsComplete(card); err != nil {
		return nil, E(2, "QA acceptance results incomplete: %v", err)
	}
	if len(card.QAEvidence) == 0 {
		return nil, E(2, "QA acceptance evidence is required")
	}
	if err := s.publishQAReport(ctx, id); err != nil {
		return nil, err
	}
	n, err := prNumber(card.PR)
	if err != nil {
		return nil, E(2, "%v", err)
	}
	p, err := s.prView(ctx, n)
	if err != nil {
		return nil, err
	}
	if p.BaseRefName != "dev" {
		return nil, E(2, "PR base must be dev")
	}
	if card.Branch != nil && *card.Branch != "" && p.HeadRefName != *card.Branch {
		return nil, E(2, "PR branch does not match card")
	}
	if card.TestedHeadSHA == nil || *card.TestedHeadSHA == "" || p.HeadRefOid != *card.TestedHeadSHA {
		return nil, E(2, "tested head SHA does not match PR")
	}
	if p.State == "MERGED" && p.MergeCommit != nil {
		return s.writeMergeReceipt(id, n, p.MergeCommit.OID, p.HeadRefOid, by, nil)
	}
	if p.State != "OPEN" {
		return nil, E(2, "PR is not open")
	}
	latestBaseSHA, err := fetchOriginDev(ctx, s.Config.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("QA base freshness check failed: %w", err)
	}
	if card.BaseSHA == nil || *card.BaseSHA != latestBaseSHA {
		return nil, E(2, "dev base changed during QA: tested %v, current origin/dev %s", card.BaseSHA, latestBaseSHA)
	}
	if err := verifyPRIncludesOriginDev(ctx, card, p.HeadRefOid, latestBaseSHA); err != nil {
		return nil, err
	}
	checks, err := s.verifyPRChecks(ctx, n)
	if err != nil {
		return nil, err
	}
	latestBaseSHA, err = fetchOriginDev(ctx, s.Config.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("QA base freshness check failed before merge: %w", err)
	}
	if card.BaseSHA == nil || *card.BaseSHA != latestBaseSHA {
		return nil, E(2, "dev base changed during QA: tested %v, current origin/dev %s", card.BaseSHA, latestBaseSHA)
	}
	if err := verifyPRIncludesOriginDev(ctx, card, p.HeadRefOid, latestBaseSHA); err != nil {
		return nil, err
	}
	if _, err := s.gh(ctx, "pr", "merge", strconv.Itoa(n), "--repo", s.Config.Repo, "--merge", "--match-head-commit", *card.TestedHeadSHA); err != nil {
		return nil, err
	}
	p, err = s.prView(ctx, n)
	if err != nil {
		return nil, err
	}
	if p.State != "MERGED" || p.MergeCommit == nil {
		return nil, E(8, "GitHub did not confirm merge")
	}
	if p.HeadRefOid != *card.TestedHeadSHA {
		return nil, E(7, "GitHub merged a head different from the tested SHA")
	}
	return s.writeMergeReceipt(id, n, p.MergeCommit.OID, p.HeadRefOid, by, checks)
}

func (s *State) writeMergeReceipt(id string, n int, mergeSHA, headSHA, by string, checks []string) (map[string]any, error) {
	receipt := map[string]any{"card_id": id, "pr": n, "base": "dev", "merge_sha": mergeSHA, "tested_head_sha": headSHA, "by": by, "merged_at": Now()}
	if len(checks) > 0 {
		receipt["checks"] = checks
	}
	b, _ := Encode(receipt)
	path := filepath.Join(s.Root, "runtime", "merges")
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	if existing, err := os.ReadFile(filepath.Join(path, id+".json")); err == nil {
		var old map[string]any
		_ = json.Unmarshal(existing, &old)
		if old["merge_sha"] != mergeSHA {
			return nil, E(7, "merge receipt conflict")
		}
		return old, nil
	}
	return receipt, writeAtomic(filepath.Join(path, id+".json"), b, 0600)
}

func (s *State) SyncDone(ctx context.Context) (map[string]any, error) {
	dir := filepath.Join(s.Root, "runtime", "merges")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"done": []string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	done := []string{}
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(ent.Name(), ".json")
		var receipt struct {
			MergeSHA      string `json:"merge_sha"`
			TestedHeadSHA string `json:"tested_head_sha"`
		}
		receiptData, e := os.ReadFile(filepath.Join(dir, ent.Name()))
		if e != nil || json.Unmarshal(receiptData, &receipt) != nil {
			return nil, E(7, "invalid merge receipt for %s", id)
		}
		status, _, _, card, e := s.readCardPath(id)
		if e != nil {
			continue
		}
		if status == "done" {
			continue
		}
		n, e := prNumber(card.PR)
		if e != nil {
			return nil, E(7, "invalid PR for %s", id)
		}
		p, e := s.prView(ctx, n)
		if e != nil {
			return nil, e
		}
		if p.State != "MERGED" || p.BaseRefName != "dev" {
			return nil, E(7, "merge receipt for %s is not confirmed", id)
		}
		if p.MergeCommit == nil || p.MergeCommit.OID != receipt.MergeSHA || p.HeadRefOid != receipt.TestedHeadSHA {
			return nil, E(7, "merge receipt mismatch for %s", id)
		}
		if _, e = s.withMoveInternal(id, "done", "qa/sync-done", "GitHub merge confirmed", map[string]any{"claimed_at": nil, "claimed_by": nil}); e != nil {
			return nil, e
		}
		if e = s.releaseReservation(id); e != nil {
			return nil, e
		}
		staleBaseSHA := p.MergeCommit.OID
		if fetchedBaseSHA, fetchErr := fetchOriginDev(ctx, s.Config.RepoPath); fetchErr == nil {
			staleBaseSHA = fetchedBaseSHA
		}
		if _, e = s.MarkStale(id, staleBaseSHA); e != nil {
			return nil, e
		}
		done = append(done, id)
	}
	if len(done) > 0 && s.Config.Linear.Enabled {
		// A confirmed completion should refresh the full Linear snapshot now,
		// rather than waiting for the supervisor's next polling interval. Sync
		// is best-effort here: the durable outbox and the next poll recover a
		// temporary Linear outage without undoing the local Done transition.
		_, _ = s.Sync(ctx)
	}
	return map[string]any{"done": done}, nil
}

func (s *State) MarkStale(mergedID, baseSHA string) (map[string]any, error) {
	_, _, _, merged, err := s.readCardPath(mergedID)
	if err != nil {
		return nil, err
	}
	marked := []string{}
	rs, err := s.activeReservations()
	if err != nil {
		return nil, err
	}
	for _, r := range rs {
		if r.CardID == mergedID {
			continue
		}
		status, _, _, _, e := s.readCardPath(r.CardID)
		if e != nil || !(status == "in_review" || status == "claimed-qa") {
			continue
		}
		_, _, _, review, e := s.readCardPath(r.CardID)
		if e != nil {
			continue
		}
		baseChanged := review.BaseSHA == nil || *review.BaseSHA != baseSHA
		if !baseChanged && !patternsConflict(merged.Touches, r.Touches) {
			continue
		}
		to := status
		if status == "claimed-qa" {
			to = "in_review"
		}
		note := fmt.Sprintf("base moved; card base %v, current dev base %s", review.BaseSHA, baseSHA)
		if _, e = s.withMoveInternal(r.CardID, to, "system/sync", note, map[string]any{"stale": true, "base_sha": baseSHA, "base_sync_pending": false, "claimed_at": nil, "claimed_by": nil}); e != nil {
			return nil, e
		}
		marked = append(marked, r.CardID)
	}
	return map[string]any{"stale": marked}, nil
}

func patternsConflict(a, b []string) bool { return loopglob.PatternsOverlap(a, b) }

func (s *State) GCWorktrees(ctx context.Context) (map[string]any, error) {
	removed := []string{}
	cards, err := s.AllCards()
	if err != nil {
		return nil, err
	}
	for _, x := range cards {
		if x.Status != "done" && x.Status != "cancelled" {
			continue
		}
		if x.Card.Worktree == nil || *x.Card.Worktree == "" {
			continue
		}
		if n, e := prNumber(x.Card.PR); e == nil {
			p, e := s.prView(ctx, n)
			if e != nil {
				return nil, e
			}
			if p.State != "MERGED" && p.State != "CLOSED" {
				continue
			}
		}
		reservationPath := filepath.Join(s.Root, "runtime", "reservations", x.Card.ID+".json")
		if b, e := os.ReadFile(reservationPath); e == nil {
			var r Reservation
			if json.Unmarshal(b, &r) == nil && r.ReleasedAt == nil {
				continue
			}
		}
		path := filepath.Clean(*x.Card.Worktree)
		root := filepath.Clean(s.Config.WorktreeRoot) + string(os.PathSeparator)
		if !strings.HasPrefix(path, root) {
			return nil, E(7, "worktree path escapes configured root")
		}
		cmd := exec.CommandContext(ctx, "git", "-C", s.Config.RepoPath, "worktree", "remove", path)
		if out, e := cmd.CombinedOutput(); e != nil {
			return nil, E(8, "git worktree remove failed: %s", out)
		}
		removed = append(removed, path)
		qaPath := filepath.Join(s.Config.WorktreeRoot, x.Card.ID+"-qa")
		if _, e := os.Stat(qaPath); e == nil {
			cmd := exec.CommandContext(ctx, "git", "-C", s.Config.RepoPath, "worktree", "remove", qaPath)
			if out, e := cmd.CombinedOutput(); e != nil {
				return nil, E(8, "git QA worktree remove failed: %s", out)
			}
			removed = append(removed, qaPath)
		}
	}
	cmd := exec.CommandContext(ctx, "git", "-C", s.Config.RepoPath, "worktree", "prune")
	if out, e := cmd.CombinedOutput(); e != nil {
		return nil, E(8, "git worktree prune failed: %s", out)
	}
	return map[string]any{"removed": removed}, nil
}
