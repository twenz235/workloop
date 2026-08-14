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
			if err := validateDependencies(card, cards); err != nil {
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
			return E(2, "dependency %s does not exist", dep)
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
	raw["status"] = "todo"
	raw["hot"] = loopglob.PatternsOverlap(c.Touches, cfg.HotPaths)
	defaults := map[string]any{"attempts": 0, "max_attempts": 2, "rework_count": 0, "max_rework": 2, "conflict_skips": 0, "claimed_at": nil, "claimed_by": nil, "worktree": nil, "branch": nil, "pr": nil, "base_sha": nil, "tested_head_sha": nil, "stale": false, "spec_changed": false, "qa_findings": []any{}, "qa_evidence": []any{}, "proposed": []any{}, "history": []any{}}
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
		return E(2, "invalid destination status")
	}
	if sourceStatus != "" && sourceStatus == destStatus {
		return s.rewrite(id, sourceStatus, data, operation, actor)
	}
	txid := txID()
	tmp := filepath.Join(s.Root, "queue", ".tmp", txid+".json")
	dest := filepath.Join(s.Root, "queue", destStatus, id+".json")
	source := ""
	if sourceStatus != "" {
		source = filepath.Join(s.Root, "queue", sourceStatus, id+".json")
	}
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
	tx := Transaction{ID: txid, CardID: id, Source: source, Destination: dest, Hash: Hash(data), Operation: operation, Actor: actor, Phase: "prepared", Temp: tmp, CreatedAt: Now()}
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
	if source != "" {
		sourceRel, _ := s.rel(source)
		if err := s.FS.Remove(sourceRel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		_ = syncDir(filepath.Dir(source))
	}
	_ = s.FS.Remove(tmpRel)
	tx.Phase = "source-removed"
	if err := writeTx(intent, &tx); err != nil {
		return err
	}
	fault("source-removed")
	tx.Phase = "committed"
	return writeTx(intent, &tx)
}

func (s *State) rewrite(id, status string, data []byte, operation, actor string) error {
	txid := txID()
	tmp := filepath.Join(s.Root, "queue", ".tmp", txid+".json")
	stage := filepath.Join(s.Root, "queue", ".tmp", txid+".published")
	source := filepath.Join(s.Root, "queue", status, id+".json")
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
	tx := Transaction{ID: txid, CardID: id, Source: source, Destination: source, Stage: stage, Hash: Hash(data), Operation: operation, Actor: actor, Phase: "prepared", Temp: tmp, CreatedAt: Now()}
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
	return writeTx(intent, &tx)
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
	raw["status"] = to
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
	if err := s.enqueueLinear(card, from, to); err != nil {
		return raw, E(7, "move committed but Linear outbox failed: %v", err)
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
	entries, err := os.ReadDir(filepath.Join(s.Root, "queue", "todo"))
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(ent.Name(), ".json")
		_, _, _, c, e := s.readCardPath(id)
		if e != nil {
			return e
		}
		if len(c.DependsOn) > 0 && !s.dependenciesDone(c) {
			if _, e = s.moveLocked(id, "blocked", "system", "waiting for dependencies", nil, true); e != nil {
				return e
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
	for rank, status := range statuses {
		entries, err := os.ReadDir(filepath.Join(s.Root, "queue", status))
		if err != nil {
			return nil, err
		}
		for _, ent := range entries {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(s.Root, "queue", status, ent.Name()))
			if err != nil {
				return nil, err
			}
			_, c, err := DecodeCard(b, &s.Config)
			if err != nil {
				return nil, err
			}
			if s.dependenciesDone(c) {
				out = append(out, cardItem{Status: fmt.Sprintf("%d:%s", rank, status), Card: c})
			}
		}
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

func (s *State) dependenciesDone(c *Card) bool {
	if len(c.DependsOn) == 0 {
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
	entries, err := os.ReadDir(filepath.Join(s.Root, "queue", "blocked"))
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(ent.Name(), ".json")
		_, _, _, c, err := s.readCardPath(id)
		if err != nil {
			return err
		}
		if s.dependenciesDone(c) {
			if _, err := s.moveLocked(id, "todo", "system", "dependencies completed", nil, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *State) inFlight() int {
	n := 0
	for _, status := range []string{"claimed-dev", "in_review", "claimed-qa"} {
		entries, _ := os.ReadDir(filepath.Join(s.Root, "queue", status))
		n += len(entries)
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
	return writeTx(intent, tx)
}
