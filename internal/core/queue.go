package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	loopglob "github.com/twenz235/workloop/internal/glob"
)

func (s *State) Add(data []byte, actor string) (map[string]any, error) {
	var result map[string]any
	err := s.withLock(func() error {
		raw, card, err := DecodeCard(data, &s.Config)
		if err != nil {
			return err
		}
		expected := contractHash(raw)
		if card.ContractHash != "" && card.ContractHash != expected {
			return E(2, "contract_hash does not match card contract")
		}
		raw["contract_hash"] = expected
		card.ContractHash = expected
		if _, _, err := s.Locate(card.ID); err == nil {
			return E(2, "card id already exists")
		}
		if card.LinearIssueUUID != "" {
			cards, err := s.AllCards()
			if err != nil {
				return err
			}
			for _, existing := range cards {
				if existing.Card.LinearIssueUUID == card.LinearIssueUUID {
					return E(2, "Linear issue UUID already imported")
				}
			}
			external := map[string]bool{}
			if s.Config.Linear.Enabled {
				if linearRuntime, runtimeErr := s.loadLinearRuntime(); runtimeErr == nil {
					for dependencyID := range linearRuntime.IssueStates {
						external[dependencyID] = true
					}
				}
			}
			if err := validateDependenciesWithExternal(card, cards, external); err != nil {
				return err
			}
		}
		normalizeNew(raw, card, &s.Config)
		encoded, err := Encode(raw)
		if err != nil {
			return err
		}
		if err := s.publish(card.ID, "", "todo", encoded, "add", actor); err != nil {
			return err
		}
		result = raw
		if err := s.appendJournal(roleFromActor(actor), card.ID, "add", "todo", actor, ""); err != nil {
			return E(7, "card committed but journal failed: %v", err)
		}
		return nil
	})
	return result, err
}

func contractHash(raw map[string]any) string {
	contract := map[string]any{}
	for _, k := range []string{"problem", "desired_outcome", "out_of_scope", "repo", "repo_path", "base", "tier", "touches", "acceptance", "verification", "depends_on", "visuals", "risk", "rollback_notes"} {
		if k == "visuals" {
			if _, exists := raw[k]; !exists {
				continue
			}
		}
		contract[k] = raw[k]
	}
	b, _ := json.Marshal(contract)
	return Hash(b)
}

func validateDependencies(card *Card, cards []struct {
	Status, Path string
	Raw          map[string]any
	Card         *Card
}) error {
	return validateDependenciesWithExternal(card, cards, nil)
}

func validateDependenciesWithExternal(card *Card, cards []struct {
	Status, Path string
	Raw          map[string]any
	Card         *Card
}, external map[string]bool) error {
	graph := map[string][]string{}
	for _, x := range cards {
		if x.Card.LinearIssueUUID != "" {
			graph[x.Card.LinearIssueUUID] = x.Card.DependsOn
		}
	}
	if card.LinearIssueUUID != "" {
		graph[card.LinearIssueUUID] = card.DependsOn
	}
	for _, dep := range card.DependsOn {
		if _, ok := graph[dep]; !ok {
			if !external[dep] {
				return E(2, "dependency %s does not exist", dep)
			}
			graph[dep] = nil
		}
	}
	visiting := map[string]bool{}
	done := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if visiting[id] {
			return true
		}
		if done[id] {
			return false
		}
		visiting[id] = true
		for _, d := range graph[id] {
			if visit(d) {
				return true
			}
		}
		visiting[id] = false
		done[id] = true
		return false
	}
	for id := range graph {
		if visit(id) {
			return E(2, "dependency graph contains a cycle")
		}
	}
	return nil
}

func normalizeNew(raw map[string]any, c *Card, cfg *Config) {
	delete(raw, "status")
	raw["hot"] = loopglob.PatternsOverlap(c.Touches, cfg.HotPaths)
	defaults := map[string]any{"attempts": 0, "max_attempts": 2, "rework_count": 0, "max_rework": 2, "conflict_skips": 0, "claimed_at": nil, "claimed_by": nil, "worktree": nil, "branch": nil, "pr": nil, "base_sha": nil, "tested_head_sha": nil, "stale": false, "spec_changed": false, "qa_findings": []any{}, "qa_evidence": []any{}, "proposed": []any{}, "history": []any{}, "linear_labels": []string{}}
	for k, v := range defaults {
		if _, ok := raw[k]; !ok {
			raw[k] = v
		}
	}
}

func txID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(b))
}

func (s *State) publish(id, sourceStatus, destStatus string, data []byte, operation, actor string) error {
	if !statusValid(destStatus) {
		return E(2, "invalid runtime phase")
	}
	dest := s.cardPath(id)
	if _, err := os.Stat(dest); err == nil {
		if sourceStatus == "" {
			return E(4, "card already exists")
		}
		if err := s.rewritePhase(id, sourceStatus, data, operation, actor, destStatus); err != nil {
			return err
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	txid := txID()
	tmp := filepath.Join(s.Root, cardStoreDir, ".tmp", txid+".json")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	_ = syncDir(filepath.Dir(tmp))
	tx := Transaction{ID: txid, CardID: id, Destination: dest, Hash: Hash(data), Operation: operation, Actor: actor, Phase: "prepared", Temp: tmp, RuntimePhase: destStatus, CreatedAt: Now()}
	intent := filepath.Join(s.Root, "runtime", "transactions", txid+".json")
	if err := writeTx(intent, &tx); err != nil {
		os.Remove(tmp)
		return err
	}
	fault("prepared")
	tmpRel, _ := s.rel(tmp)
	destRel, _ := s.rel(dest)
	if err := s.FS.Link(tmpRel, destRel); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return E(4, "destination already exists")
		}
		return err
	}
	_ = syncDir(filepath.Dir(dest))
	tx.Phase = "destination-created"
	if err := writeTx(intent, &tx); err != nil {
		return err
	}
	fault("destination-created")
	_ = s.FS.Remove(tmpRel)
	tx.Phase = "source-removed"
	if err := writeTx(intent, &tx); err != nil {
		return err
	}
	fault("source-removed")
	tx.Phase = "committed"
	if err := writeTx(intent, &tx); err != nil {
		return err
	}
	return s.setRuntimePhase(id, destStatus)
}

func (s *State) rewrite(id, status string, data []byte, operation, actor string) error {
	return s.rewritePhase(id, status, data, operation, actor, "")
}

func (s *State) rewritePhase(id, status string, data []byte, operation, actor, phase string) error {
	txid := txID()
	tmp := filepath.Join(s.Root, cardStoreDir, ".tmp", txid+".json")
	stage := filepath.Join(s.Root, cardStoreDir, ".tmp", txid+".published")
	source := s.cardPath(id)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	tx := Transaction{ID: txid, CardID: id, Source: source, Destination: source, Stage: stage, Hash: Hash(data), Operation: operation, Actor: actor, Phase: "prepared", Temp: tmp, RuntimePhase: phase, CreatedAt: Now()}
	intent := filepath.Join(s.Root, "runtime", "transactions", txid+".json")
	if err := writeTx(intent, &tx); err != nil {
		return err
	}
	fault("prepared")
	tmpRel, _ := s.rel(tmp)
	stageRel, _ := s.rel(stage)
	sourceRel, _ := s.rel(source)
	if err := s.FS.Link(tmpRel, stageRel); err != nil {
		return err
	}
	tx.Phase = "destination-created"
	if err := writeTx(intent, &tx); err != nil {
		return err
	}
	fault("destination-created")
	if err := s.FS.Remove(sourceRel); err != nil {
		return err
	}
	tx.Phase = "source-removed"
	if err := writeTx(intent, &tx); err != nil {
		return err
	}
	fault("source-removed")
	if err := s.FS.Link(stageRel, sourceRel); err != nil {
		return err
	}
	_ = s.FS.Remove(stageRel)
	_ = s.FS.Remove(tmpRel)
	_ = syncDir(filepath.Dir(source))
	tx.Phase = "committed"
	if err := writeTx(intent, &tx); err != nil {
		return err
	}
	if phase != "" {
		return s.setRuntimePhase(id, phase)
	}
	return nil
}

func fault(phase string) {
	if os.Getenv("LOOPCTL_FAULT_PHASE") == phase {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
}

func writeTx(path string, tx *Transaction) error {
	b, err := Encode(tx)
	if err != nil {
		return err
	}
	return writeAtomic(path, b, 0600)
}

func (s *State) moveLocked(id, to, actor, note string, patch map[string]any, internal bool) (map[string]any, error) {
	from, _, raw, card, err := s.readCardPath(id)
	if err != nil {
		return nil, err
	}
	if !internal {
		if err := allowedTransition(from, to, actor); err != nil {
			return nil, err
		}
	}
	if to == "done" && !internal {
		return nil, E(2, "done requires qa-merge receipt and sync-done")
	}
	for k, v := range patch {
		raw[k] = v
	}
	delete(raw, "status")
	history, _ := raw["history"].([]any)
	history = append(history, map[string]any{"at": Now(), "from": from, "to": to, "by": actor, "note": note})
	raw["history"] = history
	encoded, err := Encode(raw)
	if err != nil {
		return nil, err
	}
	if _, _, err := DecodeCard(encoded, &s.Config); err != nil {
		return nil, err
	}
	if err := s.publish(card.ID, from, to, encoded, "move", actor); err != nil {
		return nil, err
	}
	// A sync transition mirrors facts read from Linear into local runtime only;
	// it must never write the remote board back based on that same observation.
	if !strings.HasPrefix(actor, "system/sync") && !strings.HasPrefix(actor, "system/linear") {
		if err := s.enqueueLinear(card, from, to); err != nil {
			return raw, E(7, "move committed but Linear outbox failed: %v", err)
		}
	}
	if err := s.appendJournal(roleFromActor(actor), id, from, to, actor, note); err != nil {
		return raw, E(7, "move committed but journal failed: %v", err)
	}
	return raw, nil
}

func (s *State) Move(id, to, actor, note string, patch map[string]any) (map[string]any, error) {
	allowed := map[string]bool{"branch": true, "pr": true, "base_sha": true, "tested_head_sha": true, "worktree": true, "stale": true, "spec_changed": true, "qa_evidence": true}
	for key := range patch {
		if !allowed[key] {
			return nil, E(2, "patch field %q is not mutable", key)
		}
	}
	var out map[string]any
	err := s.withLock(func() error {
		var err error
		out, err = s.moveLocked(id, to, actor, note, patch, false)
		return err
	})
	return out, err
}

func (s *State) readCardPath(id string) (string, string, map[string]any, *Card, error) {
	status, path, err := s.Locate(id)
	if err != nil {
		return "", "", nil, nil, err
	}
	rel, _ := s.rel(path)
	b, err := s.FS.ReadFile(rel)
	if err != nil {
		return "", "", nil, nil, err
	}
	raw, card, err := DecodeCard(b, &s.Config)
	if err == nil {
		card.Status = status
	}
	return status, path, raw, card, err
}

func allowedTransition(from, to, actor string) error {
	role := roleFromActor(actor)
	parts := strings.Split(actor, "/")
	if len(parts) != 2 || !workerIDPattern.MatchString(parts[1]) {
		return E(2, "actor must be role/worker")
	}
	allowed := false
	switch role {
	case "dev":
		allowed = from == "claimed-dev" && (to == "in_review" || to == "needs_attention" || to == "todo" || to == "blocked")
	case "qa":
		allowed = from == "claimed-qa" && (to == "rework" || to == "in_review" || to == "needs_attention")
	}
	if !allowed {
		return E(2, "transition %s -> %s is not allowed for %s", from, to, actor)
	}
	return nil
}

func roleFromActor(actor string) string {
	if strings.HasPrefix(actor, "dev/") || actor == "dev" {
		return "dev"
	}
	if strings.HasPrefix(actor, "qa/") || actor == "qa" {
		return "qa"
	}
	return "system"
}

func (s *State) appendJournal(role, id, from, to, actor, note string) error {
	if role != "dev" && role != "qa" {
		return nil
	}
	line := fmt.Sprintf("- %s `%s` %s → %s by %s", Now(), id, from, to, actor)
	if note != "" {
		line += ": " + strings.ReplaceAll(note, "\n", " ")
	}
	line += "\n"
	f, err := os.OpenFile(filepath.Join(s.Root, "journal", role+".md"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		return err
	}
	return f.Sync()
}

func (s *State) Claim(role, worker string) (map[string]any, error) {
	if role != "dev" && role != "qa" {
		return nil, E(2, "role must be dev or qa")
	}
	if worker == "" {
		return nil, E(2, "worker is required")
	}
	if !workerIDPattern.MatchString(worker) {
		return nil, E(2, "invalid worker id")
	}
	var result map[string]any
	if _, err := os.Stat(filepath.Join(s.Root, "STOP")); err == nil {
		return nil, E(6, "stopped")
	}
	if s.Config.DeadlineAt != nil {
		if t, e := time.Parse(time.RFC3339, *s.Config.DeadlineAt); e == nil && time.Now().After(t) {
			return nil, E(6, "deadline passed")
		}
	}
	// Linear owns intake eligibility. Check the snapshot only after the
	// stop/deadline guards so an explicit stop remains the authoritative
	// operator signal even while the network is unavailable.
	if err := s.requireFreshLinearSnapshot(); err != nil {
		return nil, err
	}
	openPRs := 0
	if role == "dev" {
		var err error
		openPRs, err = s.OpenPRCount(context.Background())
		if err != nil {
			return nil, err
		}
	}
	err := s.withLock(func() error {
		if _, err := os.Stat(filepath.Join(s.Root, "STOP")); err == nil {
			return E(6, "stopped")
		}
		if s.Config.DeadlineAt != nil {
			if t, err := time.Parse(time.RFC3339, *s.Config.DeadlineAt); err == nil && time.Now().After(t) {
				return E(6, "deadline passed")
			}
		}
		if err := s.blockWaitingCards(); err != nil {
			return err
		}
		if err := s.autoUnblock(); err != nil {
			return err
		}
		if role == "dev" && (s.inFlight() >= s.Config.Limits.MaxInFlight || openPRs >= s.Config.Limits.MaxOpenPRs) {
			return E(5, "backpressure")
		}
		statuses := []string{"in_review"}
		to := "claimed-qa"
		if role == "dev" {
			statuses, to = []string{"rework", "todo"}, "claimed-dev"
		}
		cards, err := s.candidates(statuses)
		if err != nil {
			return err
		}
		if len(cards) == 0 {
			return E(3, `{"reason":"empty"}`)
		}
		reservations, err := s.activeReservations()
		if err != nil {
			return err
		}
		for _, item := range cards {
			if role == "dev" && conflicts(item.Card, reservations) {
				_ = s.bumpConflict(item.Card.ID)
				continue
			}
			if role == "dev" {
				if err := s.createReservation(item.Card); err != nil {
					if errors.Is(err, fs.ErrExist) {
						continue
					}
					return err
				}
			}
			claimed := Now()
			patch := map[string]any{"claimed_at": claimed, "claimed_by": worker}
			out, err := s.moveLocked(item.Card.ID, to, role+"/"+worker, "claim", patch, true)
			if err != nil {
				if ExitCode(err) == 7 {
					return err
				}
				if role == "dev" {
					_ = s.releaseReservation(item.Card.ID)
				}
				continue
			}
			result = out
			return nil
		}
		return E(3, `{"reason":"all_conflicted"}`)
	})
	return result, err
}

func (s *State) requireFreshLinearSnapshot() error {
	if !s.Config.Linear.Enabled {
		return nil
	}
	runtime, err := s.loadLinearRuntime()
	if err != nil {
		return E(8, "Linear snapshot unavailable: %v", err)
	}
	last, err := time.Parse(time.RFC3339Nano, runtime.LastSyncAt)
	if err != nil || last.IsZero() {
		return E(8, "Linear snapshot unavailable; run loopctl sync")
	}
	interval := time.Duration(s.Config.Linear.SyncIntervalSec) * time.Second
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if time.Since(last) > interval {
		return E(8, "Linear snapshot is stale; run loopctl sync")
	}
	return nil
}

// ClaimAndSync claims locally first, then immediately mirrors the resulting
// state to Linear. Linear failures are intentionally left in the outbox so a
// remote outage never strands a locally claimed card without a worker.
func (s *State) ClaimAndSync(ctx context.Context, role, worker string) (map[string]any, error) {
	result, err := s.Claim(role, worker)
	if err != nil {
		return nil, err
	}
	if s.Config.Linear.Enabled {
		_, _ = s.FlushLinearOutbox(ctx)
	}
	return result, nil
}

func (s *State) blockWaitingCards() error {
	// Dependency readiness is derived from Linear states. There is no local
	// todo queue to mutate or block anymore.
	if s.Config.Linear.Enabled {
		return nil
	}
	cards, err := s.AllCards()
	if err != nil {
		return err
	}
	for _, item := range cards {
		if item.Status == "todo" && len(item.Card.DependsOn) > 0 && !s.dependenciesDone(item.Card) {
			if _, err := s.moveLocked(item.Card.ID, "blocked", "system", "waiting for dependencies", nil, true); err != nil {
				return err
			}
		}
	}
	return nil
}

type cardItem struct {
	Status string
	Card   *Card
}

func (s *State) candidates(statuses []string) ([]cardItem, error) {
	var out []cardItem
	cards, err := s.AllCards()
	if err != nil {
		return nil, err
	}
	for _, item := range cards {
		rank, ok := s.linearCandidateRank(item.Card, item.Status, statuses)
		if !ok || !s.dependenciesDone(item.Card) {
			continue
		}
		out = append(out, cardItem{Status: fmt.Sprintf("%d:%s", rank, item.Status), Card: item.Card})
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := strings.SplitN(out[i].Status, ":", 2)[0], strings.SplitN(out[j].Status, ":", 2)[0]
		if ri != rj {
			return ri < rj
		}
		boost := s.Config.Limits.ConflictSkipBoost
		bi, bj := out[i].Card.ConflictSkips >= boost, out[j].Card.ConflictSkips >= boost
		if bi != bj {
			return bi
		}
		pi, pj := out[i].Card.Priority, out[j].Card.Priority
		if pi == 0 {
			pi = 5
		}
		if pj == 0 {
			pj = 5
		}
		if pi != pj {
			return pi < pj
		}
		return out[i].Card.ID < out[j].Card.ID
	})
	return out, nil
}

func (s *State) linearCandidateRank(c *Card, runtimePhase string, statuses []string) (int, bool) {
	if !s.Config.Linear.Enabled {
		for rank, status := range statuses {
			if runtimePhase == status {
				return rank, true
			}
		}
		return 0, false
	}
	if c.LinearState == "" || containsString(c.LinearLabels, s.Config.Linear.NeedsAttentionLabel) {
		return 0, false
	}
	// A reopened Linear issue is eligible even when its historical local phase
	// was cancelled/done; only an active lease can veto a new claim.
	for rank, status := range statuses {
		switch status {
		case "in_review":
			if c.LinearState == s.Config.Linear.StatusMap["in_review"] && runtimePhase != "claimed-qa" {
				return rank, true
			}
		case "rework", "todo":
			if c.LinearState == s.Config.Linear.StatusMap["in_progress"] && status == "rework" && runtimePhase != "claimed-dev" && runtimePhase != "in_review" && runtimePhase != "claimed-qa" {
				return rank, true
			}
			if c.LinearState == s.Config.Linear.StatusMap["todo"] && status == "todo" && runtimePhase != "claimed-dev" && runtimePhase != "in_review" && runtimePhase != "claimed-qa" {
				return rank, true
			}
		}
	}
	return 0, false
}

func (s *State) dependenciesDone(c *Card) bool {
	if len(c.DependsOn) == 0 {
		return true
	}
	if s.Config.Linear.Enabled {
		runtime, err := s.loadLinearRuntime()
		if err != nil {
			return false
		}
		for _, dep := range c.DependsOn {
			if runtime.IssueStates[dep] != s.Config.Linear.StatusMap["done"] {
				return false
			}
		}
		return true
	}
	cards, err := s.AllCards()
	if err != nil {
		return false
	}
	for _, dep := range c.DependsOn {
		found := false
		for _, x := range cards {
			if x.Card.LinearIssueUUID == dep && x.Status == "done" {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *State) autoUnblock() error {
	if s.Config.Linear.Enabled {
		return nil
	}
	cards, err := s.AllCards()
	if err != nil {
		return err
	}
	for _, item := range cards {
		if item.Status == "blocked" && s.dependenciesDone(item.Card) {
			if _, err := s.moveLocked(item.Card.ID, "todo", "system", "dependencies completed", nil, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *State) inFlight() int {
	cards, err := s.AllCards()
	if err != nil {
		return 0
	}
	n := 0
	for _, item := range cards {
		if item.Status == "claimed-dev" || item.Status == "in_review" || item.Status == "claimed-qa" {
			n++
		}
	}
	return n
}

func (s *State) activeReservations() ([]Reservation, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "runtime", "reservations"))
	if err != nil {
		return nil, err
	}
	var out []Reservation
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.Root, "runtime", "reservations", ent.Name()))
		if err != nil {
			return nil, err
		}
		var r Reservation
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, E(7, "invalid reservation: %v", err)
		}
		if r.ReleasedAt == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func conflicts(c *Card, rs []Reservation) bool {
	for _, r := range rs {
		if r.CardID == c.ID {
			continue
		}
		if c.Hot || r.Hot || loopglob.PatternsOverlap(c.Touches, r.Touches) {
			return true
		}
	}
	return false
}

func (s *State) createReservation(c *Card) error {
	r := Reservation{CardID: c.ID, Touches: c.Touches, Hot: c.Hot, CreatedAt: Now()}
	b, _ := Encode(r)
	path := filepath.Join(s.Root, "runtime", "reservations", c.ID+".json")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, fs.ErrExist) {
		existingData, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var existing Reservation
		if json.Unmarshal(existingData, &existing) != nil || existing.CardID != c.ID {
			return E(7, "invalid reservation for %s", c.ID)
		}
		// A reservation belongs to the card, not to one claim attempt. Reusing it
		// keeps rework exclusive; replacing a released record starts a new cycle.
		return writeAtomic(path, b, 0600)
	}
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (s *State) releaseReservation(id string) error {
	path := filepath.Join(s.Root, "runtime", "reservations", id+".json")
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var r Reservation
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	now := Now()
	r.ReleasedAt = &now
	out, _ := Encode(r)
	return writeAtomic(path, out, 0600)
}

func (s *State) bumpConflict(id string) error {
	from, _, raw, c, err := s.readCardPath(id)
	if err != nil {
		return err
	}
	raw["conflict_skips"] = c.ConflictSkips + 1
	b, _ := Encode(raw)
	return s.publish(id, from, from, b, "conflict-skip", "system")
}

func (s *State) RecoverTransactions() error {
	return s.withLock(func() error {
		entries, err := os.ReadDir(filepath.Join(s.Root, "runtime", "transactions"))
		if err != nil {
			return err
		}
		for _, ent := range entries {
			if !strings.HasSuffix(ent.Name(), ".json") {
				continue
			}
			path := filepath.Join(s.Root, "runtime", "transactions", ent.Name())
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var tx Transaction
			if err := json.Unmarshal(b, &tx); err != nil {
				return E(7, "invalid transaction %s", ent.Name())
			}
			if tx.Phase == "committed" {
				if tx.RuntimePhase != "" {
					if err := s.setRuntimePhase(tx.CardID, tx.RuntimePhase); err != nil {
						return err
					}
				}
				continue
			}
			if tx.Stage != "" {
				if err := s.recoverRewrite(path, &tx); err != nil {
					return err
				}
				continue
			}
			destRel, relErr := s.rel(tx.Destination)
			if relErr != nil {
				return relErr
			}
			destData, destErr := s.FS.ReadFile(destRel)
			if tx.Phase == "prepared" && errors.Is(destErr, fs.ErrNotExist) {
				tempRel, _ := s.rel(tx.Temp)
				_ = s.FS.Remove(tempRel)
				_ = os.Remove(path)
				continue
			}
			if destErr != nil || Hash(destData) != tx.Hash {
				return E(7, "transaction %s destination cannot be verified", tx.ID)
			}
			if tx.Source != "" {
				sourceRel, relErr := s.rel(tx.Source)
				if relErr != nil {
					return relErr
				}
				if err := s.FS.Remove(sourceRel); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
			tempRel, _ := s.rel(tx.Temp)
			_ = s.FS.Remove(tempRel)
			tx.Phase = "committed"
			if err := writeTx(path, &tx); err != nil {
				return err
			}
			if tx.RuntimePhase != "" {
				if err := s.setRuntimePhase(tx.CardID, tx.RuntimePhase); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *State) recoverRewrite(intent string, tx *Transaction) error {
	stageRel, err := s.rel(tx.Stage)
	if err != nil {
		return err
	}
	tempRel, err := s.rel(tx.Temp)
	if err != nil {
		return err
	}
	sourceRel, err := s.rel(tx.Source)
	if err != nil {
		return err
	}
	destRel, err := s.rel(tx.Destination)
	if err != nil {
		return err
	}
	stageData, stageErr := s.FS.ReadFile(stageRel)
	if tx.Phase == "prepared" && errors.Is(stageErr, fs.ErrNotExist) {
		_ = s.FS.Remove(tempRel)
		_ = os.Remove(intent)
		return nil
	}
	if stageErr != nil || Hash(stageData) != tx.Hash {
		return E(7, "rewrite transaction %s stage cannot be verified", tx.ID)
	}
	if tx.Phase == "destination-created" {
		if err := s.FS.Remove(sourceRel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		tx.Phase = "source-removed"
		if err := writeTx(intent, tx); err != nil {
			return err
		}
	}
	dest, err := s.FS.ReadFile(destRel)
	if errors.Is(err, fs.ErrNotExist) {
		if err := s.FS.Link(stageRel, destRel); err != nil {
			return err
		}
	} else if err != nil || Hash(dest) != tx.Hash {
		return E(7, "rewrite transaction %s destination conflict", tx.ID)
	}
	_ = s.FS.Remove(stageRel)
	_ = s.FS.Remove(tempRel)
	tx.Phase = "committed"
	if err := writeTx(intent, tx); err != nil {
		return err
	}
	if tx.RuntimePhase != "" {
		return s.setRuntimePhase(tx.CardID, tx.RuntimePhase)
	}
	return nil
}
