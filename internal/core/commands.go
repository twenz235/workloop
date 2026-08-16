package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (s *State) List(status, linear string) ([]map[string]any, error) {
	cards, err := s.AllCards()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, x := range cards {
		boardStatus := s.boardStatus(x.Card, x.Status)
		if status != "" && status != boardStatus && !containsString(x.Card.LinearLabels, status) {
			continue
		}
		if linear != "" && x.Card.LinearIssueUUID != linear && x.Card.LinearIssueID != linear {
			continue
		}
		item := map[string]any{
			"id": x.Card.ID, "title": x.Card.Title, "status": boardStatus,
			"linear_state": x.Card.LinearState, "linear_labels": x.Card.LinearLabels,
			"priority": x.Card.Priority, "linear_url": x.Card.LinearURL, "pr": x.Card.PR,
			"stale": x.Card.Stale, "base_sync_pending": x.Card.BaseSyncPending, "spec_changed": x.Card.SpecChanged,
		}
		if len(x.Card.History) > 0 && strings.TrimSpace(x.Card.History[len(x.Card.History)-1].Note) != "" {
			item["last_note"] = x.Card.History[len(x.Card.History)-1].Note
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		pi, _ := out[i]["priority"].(int)
		if pi == 0 {
			pi = 5
		}
		pj, _ := out[j]["priority"].(int)
		if pj == 0 {
			pj = 5
		}
		if pi != pj {
			return pi < pj
		}
		return fmt.Sprint(out[i]["id"]) < fmt.Sprint(out[j]["id"])
	})
	return out, nil
}

func (s *State) Status(role string) (map[string]any, error) {
	if role != "dev" && role != "qa" {
		return nil, E(2, "role must be dev or qa")
	}
	counts := map[string]int{}
	for _, status := range Statuses {
		counts[status] = 0
	}
	cards, err := s.AllCards()
	if err != nil {
		return nil, err
	}
	for _, card := range cards {
		counts[card.Status]++
	}
	stop := false
	if _, err := os.Stat(filepath.Join(s.Root, "STOP")); err == nil {
		stop = true
	}
	deadline := false
	if s.Config.DeadlineAt != nil {
		if t, err := time.Parse(time.RFC3339, *s.Config.DeadlineAt); err == nil {
			deadline = time.Now().After(t)
		}
	}
	max := s.Config.Dev.MaxWorkers
	claimed := counts["claimed-dev"]
	if role == "qa" {
		max = s.Config.QA.MaxWorkers
		claimed = counts["claimed-qa"]
	}
	peer, _ := s.PeerCheck(role)
	boardCounts, err := s.boardCounts()
	if err != nil {
		return nil, err
	}
	openPRs, err := s.OpenPRCount(context.Background())
	if err != nil {
		return nil, err
	}
	statuses := []string{"in_review"}
	if role == "dev" {
		statuses = []string{"rework", "todo"}
	}
	linearSnapshotStale := false
	if err := s.requireFreshLinearSnapshot(); err != nil {
		linearSnapshotStale = s.Config.Linear.Enabled
	}
	var candidates []cardItem
	if !linearSnapshotStale {
		candidates, _ = s.candidates(statuses)
	}
	reservations, _ := s.activeReservations()
	claimable := []string{}
	blockedConflict := []string{}
	for _, item := range candidates {
		if role == "dev" && conflicts(item.Card, reservations) {
			blockedConflict = append(blockedConflict, item.Card.ID)
		} else {
			claimable = append(claimable, item.Card.ID)
		}
	}
	slots := max - claimed
	if slots < 0 {
		slots = 0
	}
	return map[string]any{"counts": boardCounts, "runtime_counts": counts, "in_flight": counts["claimed-dev"] + counts["in_review"] + counts["claimed-qa"], "open_prs": openPRs, "slots_free": slots, "claimable_now": claimable, "blocked_by_conflict": blockedConflict, "linear_snapshot_stale": linearSnapshotStale, "backpressure": s.inFlight() >= s.Config.Limits.MaxInFlight || openPRs >= s.Config.Limits.MaxOpenPRs, "stop": stop, "deadline_passed": deadline, "peer": peer}, nil
}

func (s *State) boardStatus(c *Card, runtimeStatus string) string {
	if c.LinearState != "" {
		return c.LinearState
	}
	if s.Config.Linear.Enabled {
		return "Unknown"
	}
	switch runtimeStatus {
	case "todo", "blocked":
		return s.Config.Linear.StatusMap["todo"]
	case "rework", "claimed-dev":
		return s.Config.Linear.StatusMap["in_progress"]
	case "in_review", "claimed-qa":
		return s.Config.Linear.StatusMap["in_review"]
	case "done":
		return s.Config.Linear.StatusMap["done"]
	case "cancelled":
		return "Canceled"
	default:
		return runtimeStatus
	}
}

func (s *State) boardCounts() (map[string]int, error) {
	cards, err := s.AllCards()
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, x := range cards {
		out[s.boardStatus(x.Card, x.Status)]++
	}
	return out, nil
}

func (s *State) Heartbeat(role string, patch map[string]any) (map[string]any, error) {
	if role != "dev" && role != "qa" {
		return nil, E(2, "role must be dev or qa")
	}
	allowed := map[string]bool{"active_workers": true, "claims": true, "completed": true, "failed": true, "backpressure": true}
	for key := range patch {
		if !allowed[key] {
			return nil, E(2, "heartbeat patch field %q is not allowed", key)
		}
	}
	path := filepath.Join(s.Root, "runtime", role+".json")
	raw := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &raw)
	}
	for k, v := range patch {
		raw[k] = v
	}
	raw["role"] = role
	raw["last_tick_at"] = Now()
	b, _ := Encode(raw)
	return raw, writeAtomic(path, b, 0600)
}

func (s *State) PeerCheck(role string) (map[string]any, error) {
	peer := "qa"
	if role == "qa" {
		peer = "dev"
	} else if role != "dev" {
		return nil, E(2, "role must be dev or qa")
	}
	raw := map[string]any{}
	b, err := os.ReadFile(filepath.Join(s.Root, "runtime", peer+".json"))
	if err != nil {
		return map[string]any{"role": peer, "stale": true, "peer_dead": true}, nil
	}
	_ = json.Unmarshal(b, &raw)
	last, _ := raw["last_tick_at"].(string)
	t, _ := time.Parse(time.RFC3339Nano, last)
	stale := t.IsZero() || time.Since(t) > time.Duration(s.Config.HeartbeatStaleSec)*time.Second
	return map[string]any{"role": peer, "last_tick_at": last, "stale": stale, "peer_dead": stale}, nil
}

func (s *State) Findings(id string, findings []Finding) (map[string]any, error) {
	var out map[string]any
	err := s.withLock(func() error {
		status, _, raw, c, err := s.readCardPath(id)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, f := range c.QAFindings {
			seen[fingerprint(f)] = true
		}
		allDuplicate := true
		for _, f := range findings {
			if !seen[fingerprint(f)] {
				allDuplicate = false
				break
			}
		}
		if allDuplicate && (status == "rework" || status == "needs_attention" || status == "claimed-qa") {
			out = raw
			return nil
		}
		if status != "claimed-qa" {
			return E(2, "new findings require claimed-qa")
		}
		blocking := false
		added := false
		for _, f := range findings {
			if f.Severity != "blocking" && f.Severity != "non-blocking" {
				return E(2, "invalid finding severity")
			}
			if f.Severity == "blocking" && strings.TrimSpace(f.Violates) == "" {
				return E(2, "blocking finding requires violates")
			}
			if f.Severity == "blocking" {
				blocking = true
			}
			if !seen[fingerprint(f)] {
				c.QAFindings = append(c.QAFindings, f)
				seen[fingerprint(f)] = true
				added = true
			}
		}
		raw["qa_findings"] = c.QAFindings
		if !blocking {
			b, _ := Encode(raw)
			if err := s.publish(id, status, status, b, "findings", "qa"); err != nil {
				return err
			}
			out = raw
			return nil
		}
		count := c.ReworkCount
		if added {
			count++
		}
		raw["rework_count"] = count
		to := "rework"
		if count >= c.MaxRework {
			to = "needs_attention"
		}
		var moveErr error
		out, moveErr = s.moveLocked(id, to, "qa/findings", "blocking findings", map[string]any{"qa_findings": c.QAFindings, "rework_count": count}, true)
		return moveErr
	})
	return out, err
}

func fingerprint(f Finding) string { b, _ := json.Marshal(f); return Hash(b) }

func (s *State) Resolve(ctx context.Context, id, to, by, note string, closePR bool) (map[string]any, error) {
	parts := strings.Split(by, "/")
	if len(parts) != 2 || parts[0] != "human" || !workerIDPattern.MatchString(parts[1]) || strings.TrimSpace(note) == "" {
		return nil, E(2, "human identity and note are required")
	}
	if to != "todo" && to != "rework" && to != "cancelled" {
		return nil, E(2, "invalid resolve destination")
	}
	_, _, _, before, err := s.readCardPath(id)
	if err != nil {
		return nil, err
	}
	if to == "todo" {
		if _, e := prNumber(before.PR); e == nil {
			return nil, E(2, "cannot return to todo while a PR exists")
		}
		if before.Worktree != nil && *before.Worktree != "" {
			cmd := exec.CommandContext(ctx, "git", "-C", *before.Worktree, "status", "--porcelain")
			if out, e := cmd.Output(); e == nil && len(out) > 0 {
				return nil, E(2, "cannot return to todo while worktree has changes")
			}
		}
	}
	if to == "rework" {
		if before.Branch == nil || *before.Branch == "" {
			return nil, E(2, "rework requires an existing branch")
		}
	}
	if to == "cancelled" {
		if n, e := prNumber(before.PR); e == nil {
			p, e := s.prView(ctx, n)
			if e != nil {
				return nil, e
			}
			if p.BaseRefName != "dev" {
				return nil, E(2, "refusing to close PR outside dev")
			}
			if p.State == "MERGED" {
				return nil, E(2, "merged PR must be completed through sync-done")
			}
			if p.State == "OPEN" && !closePR {
				return nil, E(2, "--close-pr is required for an open PR")
			}
			if p.State == "OPEN" {
				if _, e = s.gh(ctx, "pr", "close", strconv.Itoa(n), "--repo", s.Config.Repo); e != nil {
					return nil, e
				}
			}
		}
	}
	var out map[string]any
	err = s.withLock(func() error {
		from, _, _, current, e := s.readCardPath(id)
		if e != nil {
			return e
		}
		if from != "needs_attention" && !(to == "cancelled" && (from == "todo" || from == "blocked")) {
			return E(2, "card cannot be resolved from %s", from)
		}
		if fmt.Sprint(current.PR) != fmt.Sprint(before.PR) {
			return E(4, "card PR changed during resolve")
		}
		var x error
		out, x = s.moveLocked(id, to, by, note, map[string]any{"claimed_at": nil, "claimed_by": nil}, true)
		if x == nil && to == "cancelled" {
			x = s.releaseReservation(id)
		}
		return x
	})
	return out, err
}

// RetryQA is the explicit human recovery path for a card that is paused in
// needs_attention but still has a reviewable PR and no unresolved blocking QA
// finding. It deliberately preserves stale/specification facts and clears
// prior QA evidence so the next QA claim must establish fresh evidence.
func (s *State) RetryQA(id, by, note string) (map[string]any, error) {
	parts := strings.Split(by, "/")
	note = strings.TrimSpace(note)
	if len(parts) != 2 || parts[0] != "human" || !workerIDPattern.MatchString(parts[1]) || note == "" {
		return nil, E(2, "human identity and note are required")
	}
	status, _, _, before, err := s.readCardPath(id)
	if err != nil {
		return nil, err
	}
	if status != "needs_attention" {
		return nil, E(2, "qa-retry requires needs_attention")
	}
	if before.SpecChanged {
		return nil, E(2, "cannot retry QA while the Linear contract is changed")
	}
	if before.ExecutionMode != "rollup" {
		if _, err := prNumber(before.PR); err != nil {
			return nil, E(2, "qa-retry requires an existing PR")
		}
	}
	for _, finding := range before.QAFindings {
		if finding.Severity == "blocking" {
			return nil, E(2, "blocking QA findings require resolve --to rework before QA retry")
		}
	}

	var out map[string]any
	err = s.withLock(func() error {
		currentStatus, _, _, current, e := s.readCardPath(id)
		if e != nil {
			return e
		}
		if currentStatus != "needs_attention" {
			return E(4, "card changed before QA retry")
		}
		if fmt.Sprint(current.PR) != fmt.Sprint(before.PR) {
			return E(4, "card PR changed before QA retry")
		}
		if current.SpecChanged {
			return E(2, "cannot retry QA while the Linear contract is changed")
		}
		out, e = s.moveLocked(id, "in_review", by, "QA retry: "+note, map[string]any{
			"claimed_at":            nil,
			"claimed_by":            nil,
			"qa_evidence":           []string{},
			"qa_acceptance_results": []AcceptanceResult{},
		}, true)
		return e
	})
	return out, err
}

func (s *State) Doctor() (map[string]any, error) {
	if err := s.RecoverTransactions(); err != nil {
		return nil, err
	}
	issues := []string{}
	seenID := map[string]string{}
	seenUUID := map[string]string{}
	cards, err := s.AllCards()
	if err != nil {
		return nil, err
	}
	reservations, _ := s.activeReservations()
	for _, r := range reservations {
		if err := s.withLock(func() error {
			status, _, locateErr := s.Locate(r.CardID)
			if locateErr == nil && (status == "todo" || status == "blocked" || status == "cancelled" || status == "done") {
				return s.releaseReservation(r.CardID)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for _, x := range cards {
		if prior := seenID[x.Card.ID]; prior != "" {
			issues = append(issues, "duplicate id "+x.Card.ID+" in "+prior+" and "+x.Status)
		} else {
			seenID[x.Card.ID] = x.Status
		}
		if x.Card.LinearIssueUUID != "" {
			if prior := seenUUID[x.Card.LinearIssueUUID]; prior != "" {
				issues = append(issues, "duplicate Linear UUID "+x.Card.LinearIssueUUID)
			} else {
				seenUUID[x.Card.LinearIssueUUID] = x.Card.ID
			}
		}
		if x.Card.Status != x.Status {
			issues = append(issues, "status mismatch for "+x.Card.ID)
		}
	}
	result := map[string]any{"healthy": len(issues) == 0, "cards": len(cards), "issues": issues}
	if len(issues) > 0 {
		return result, E(7, "doctor found %d invariant violation(s)", len(issues))
	}
	return result, nil
}

func (s *State) Reconcile(role string) (map[string]any, error) {
	if role != "dev" && role != "qa" {
		return nil, E(2, "role must be dev or qa")
	}
	if err := s.requireFreshLinearSnapshot(); err != nil {
		// Recovery must not turn an old local lease into a new Linear state
		// while the authoritative snapshot is unavailable. Active workers can
		// continue; the next successful sync will make reconciliation safe.
		return nil, err
	}
	moved := []string{}
	status := "claimed-" + role
	limit := s.Config.Dev.ClaimStaleMin
	if role == "qa" {
		limit = s.Config.QA.ClaimStaleMin
	}
	cards, err := s.AllCards()
	if err != nil {
		return nil, err
	}
	for _, item := range cards {
		if item.Status != status {
			continue
		}
		id := item.Card.ID
		_, _, _, c, e := s.readCardPath(id)
		if e != nil {
			return nil, e
		}
		if time.Since(parseTime(c.ClaimedAt)) < time.Duration(limit)*time.Minute {
			continue
		}
		to, note, patch := "todo", "stale claim", map[string]any{"claimed_at": nil, "claimed_by": nil}
		if c.Attempts >= c.MaxAttempts {
			to = "needs_attention"
		} else if role == "dev" {
			var result RunnerResult
			resultPath := filepath.Join(s.Root, "journal", "workers", id, fmt.Sprintf("%d.result.json", c.Attempts))
			resultValid := readJSON(resultPath, &result) == nil && result.CardID == id && result.Role == "dev" && result.Attempt == c.Attempts
			if resultValid && result.Outcome == "needs_attention" {
				to = "needs_attention"
				note = result.Error
			} else if resultValid && result.Outcome == "retryable" {
				to = "todo"
				note = result.Error
			} else if resultValid && result.Outcome == "completed" {
				if n, prErr := prNumber(result.PR); prErr == nil {
					if p, viewErr := s.prView(context.Background(), n); viewErr == nil && p.State == "OPEN" && p.BaseRefName == "dev" && p.HeadRefName == "loop/"+id && p.HeadRefOid == result.HeadSHA {
						if _, gateErr := s.verifyDevReviewGate(context.Background(), id, c, result.BaseSHA, result.HeadSHA); gateErr != nil {
							to = "todo"
							if c.Attempts >= c.MaxAttempts {
								to = "needs_attention"
							}
							note = "base sync gate failed during stale-claim recovery: " + gateErr.Error()
							patch["base_sync_pending"] = true
						} else {
							to = "in_review"
							note = "recovered completed Dev result after origin/dev base sync"
							patch["branch"] = result.Branch
							patch["pr"] = result.PR
							patch["base_sha"] = result.BaseSHA
							patch["base_sync_pending"] = false
							patch["tested_head_sha"] = result.HeadSHA
						}
					} else {
						to = "needs_attention"
					}
				} else {
					to = "needs_attention"
				}
			} else if c.Worktree != nil && *c.Worktree != "" {
				baseSHA, fetchErr := fetchOriginDev(context.Background(), s.Config.RepoPath)
				clean, cleanErr := gitWorktreeClean(context.Background(), *c.Worktree)
				ancestor, ancestorErr := gitIsAncestor(context.Background(), *c.Worktree, originDevRef, "HEAD")
				if fetchErr != nil || cleanErr != nil || ancestorErr != nil {
					to = "needs_attention"
					note = "unable to verify Dev worktree against origin/dev"
				} else if !clean || !ancestor {
					to = "needs_attention"
					note = fmt.Sprintf("stale Dev worktree is not safely synced to origin/dev %s", baseSHA)
				}
			}
		} else {
			to = "in_review"
			if n, prErr := prNumber(c.PR); prErr == nil {
				p, viewErr := s.prView(context.Background(), n)
				if viewErr != nil {
					return nil, viewErr
				}
				switch p.State {
				case "MERGED":
					if _, receiptErr := os.Stat(filepath.Join(s.Root, "runtime", "merges", id+".json")); receiptErr != nil {
						to = "needs_attention"
						note = "PR merged without loopctl receipt"
						break
					}
					if _, syncErr := s.SyncDone(context.Background()); syncErr != nil {
						return nil, syncErr
					}
					moved = append(moved, id)
					continue
				case "OPEN":
					baseSHA, fetchErr := fetchOriginDev(context.Background(), s.Config.RepoPath)
					if fetchErr != nil {
						to = "needs_attention"
						note = "unable to refresh origin/dev before QA recovery"
					} else {
						baseChanged := c.BaseSHA == nil || *c.BaseSHA != baseSHA
						patch["stale"] = c.Stale || baseChanged || c.TestedHeadSHA == nil || *c.TestedHeadSHA != p.HeadRefOid
						if baseChanged {
							note = fmt.Sprintf("base moved during QA recovery; card base %v, current dev base %s", c.BaseSHA, baseSHA)
							patch["base_sha"] = baseSHA
						}
					}
				default:
					to = "needs_attention"
				}
			} else {
				to = "needs_attention"
			}
		}
		actor := role + "/reconcile"
		if s.Config.Linear.Enabled && to != "needs_attention" {
			// A stale-claim recovery is local bookkeeping. Do not echo a
			// guessed state back to Linear; the fresh remote snapshot remains
			// the board authority.
			if remotePhase := s.linearExecutionPhase(c.LinearState); remotePhase != "" {
				to = remotePhase
			} else {
				to = "needs_attention"
				note = "Linear state is unavailable for stale-claim recovery"
			}
			if to != "needs_attention" {
				actor = "system/sync-reconcile"
			}
		}
		_, e = s.withMoveInternal(id, to, actor, note, patch)
		if e != nil {
			return nil, e
		}
		if s.Config.Linear.Enabled && (to == "done" || to == "cancelled") {
			_ = s.releaseReservation(id)
		}
		moved = append(moved, id)
	}
	return map[string]any{"reconciled": moved}, nil
}

func (s *State) withMoveInternal(id, to, actor, note string, patch map[string]any) (map[string]any, error) {
	var out map[string]any
	err := s.withLock(func() error { var e error; out, e = s.moveLocked(id, to, actor, note, patch, true); return e })
	return out, err
}

func (s *State) Stop() error {
	f, err := os.OpenFile(filepath.Join(s.Root, "STOP"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	entries, _ := os.ReadDir(filepath.Join(s.Root, "runtime", "workers"))
	pids := []int{}
	for _, ent := range entries {
		var record struct {
			PID int `json:"pid"`
		}
		if b, e := os.ReadFile(filepath.Join(s.Root, "runtime", "workers", ent.Name())); e == nil && json.Unmarshal(b, &record) == nil && record.PID > 1 {
			pids = append(pids, record.PID)
			_ = syscall.Kill(record.PID, syscall.SIGTERM)
		}
	}
	deadline := time.Now().Add(time.Duration(s.Config.Runner.StopGraceSec) * time.Second)
	for len(pids) > 0 && time.Now().Before(deadline) {
		alive := pids[:0]
		for _, pid := range pids {
			if syscall.Kill(pid, 0) == nil {
				alive = append(alive, pid)
			}
		}
		pids = alive
		if len(pids) > 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}
func (s *State) ClearStop() error {
	err := os.Remove(filepath.Join(s.Root, "STOP"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
