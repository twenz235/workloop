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
		if status != "" && status != boardStatus && status != x.Status && !containsString(x.Card.LinearLabels, status) {
			continue
		}
		if linear != "" && x.Card.LinearIssueUUID != linear && x.Card.LinearIssueID != linear {
			continue
		}
		out = append(out, map[string]any{
			"id": x.Card.ID, "title": x.Card.Title, "status": boardStatus, "runtime_status": x.Status,
			"linear_state": x.Card.LinearState, "linear_labels": x.Card.LinearLabels,
			"priority": x.Card.Priority, "linear_url": x.Card.LinearURL, "pr": x.Card.PR,
			"stale": x.Card.Stale, "spec_changed": x.Card.SpecChanged,
		})
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
		entries, err := os.ReadDir(filepath.Join(s.Root, "queue", status))
		if err != nil {
			return nil, err
		}
		counts[status] = len(entries)
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
	candidates, _ := s.candidates(statuses)
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
	return map[string]any{"counts": boardCounts, "runtime_counts": counts, "in_flight": counts["claimed-dev"] + counts["in_review"] + counts["claimed-qa"], "open_prs": openPRs, "slots_free": slots, "claimable_now": claimable, "blocked_by_conflict": blockedConflict, "backpressure": s.inFlight() >= s.Config.Limits.MaxInFlight || openPRs >= s.Config.Limits.MaxOpenPRs, "stop": stop, "deadline_passed": deadline, "peer": peer}, nil
}

func (s *State) boardStatus(c *Card, runtimeStatus string) string {
	if c.LinearState != "" {
		return c.LinearState
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
	moved := []string{}
	status := "claimed-" + role
	limit := s.Config.Dev.ClaimStaleMin
	if role == "qa" {
		limit = s.Config.QA.ClaimStaleMin
	}
	entries, err := os.ReadDir(filepath.Join(s.Root, "queue", status))
	if err != nil {
		return nil, err
	}
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(ent.Name(), ".json")
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
						to = "in_review"
						note = "recovered completed Dev result"
						patch["branch"] = result.Branch
						patch["pr"] = result.PR
						patch["tested_head_sha"] = result.HeadSHA
					} else {
						to = "needs_attention"
					}
				} else {
					to = "needs_attention"
				}
			} else if c.Worktree != nil && *c.Worktree != "" {
				dirty := gitOutput(*c.Worktree, "status", "--porcelain") != ""
				ahead := gitOutput(*c.Worktree, "rev-list", "--count", s.Config.Base+"..HEAD")
				if dirty || ahead != "0" {
					to = "needs_attention"
					note = "stale Dev worktree contains unconfirmed changes"
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
					patch["stale"] = c.Stale || c.TestedHeadSHA == nil || *c.TestedHeadSHA != p.HeadRefOid
				default:
					to = "needs_attention"
				}
			} else {
				to = "needs_attention"
			}
		}
		_, e = s.withMoveInternal(id, to, role+"/reconcile", note, patch)
		if e != nil {
			return nil, e
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
