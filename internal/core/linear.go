package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	loopglob "github.com/twenz235/workloop/internal/glob"
)

type linearClient struct {
	endpoint, token string
	http            *http.Client
}

var defaultLinearHTTPClient = &http.Client{Timeout: 20 * time.Second}

type linearIssue struct {
	ID, Identifier, Title, Description, URL, UpdatedAt, StateName string
	Labels                                                        []string
	Priority                                                      int
}

func (s *State) linearClient() (*linearClient, error) {
	token := os.Getenv(s.Config.Linear.TokenEnv)
	if token == "" {
		return nil, E(8, "%s is not set", s.Config.Linear.TokenEnv)
	}
	endpoint := s.Config.Linear.Endpoint
	if endpoint == "" {
		endpoint = "https://api.linear.app/graphql"
	}
	return &linearClient{endpoint: endpoint, token: token, http: defaultLinearHTTPClient}, nil
}

func (c *linearClient) graphql(ctx context.Context, query string, variables any, out any) error {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": variables})
	var b []byte
	var status int
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", c.token)
		resp, err := c.http.Do(req)
		if err == nil {
			status = resp.StatusCode
			b, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			_ = resp.Body.Close()
		}
		lastErr = err
		if err == nil && status >= 200 && status < 300 {
			break
		}
		retry := err != nil || status == http.StatusTooManyRequests || status >= 500
		if !retry || attempt == 2 {
			if err != nil {
				return err
			}
			return fmt.Errorf("Linear HTTP %d", status)
		}
		delay := time.Duration(200*(1<<attempt)+int(time.Now().UnixNano()%100)) * time.Millisecond
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if lastErr != nil {
		return lastErr
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return err
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("Linear: %s", envelope.Errors[0].Message)
	}
	return json.Unmarshal(envelope.Data, out)
}

func (c *linearClient) issues(ctx context.Context, teamID string) ([]linearIssue, error) {
	const q = `query LoopIssues($team: String!, $after: String) { team(id: $team) { issues(first: 100, after: $after, includeArchived: true) { nodes { id identifier title description url updatedAt priority state { name } labels { nodes { name } } } pageInfo { hasNextPage endCursor } } } }`
	out := []linearIssue{}
	var after any = nil
	for {
		var data struct {
			Team struct {
				Issues struct {
					Nodes []struct {
						ID, Identifier, Title, Description, URL, UpdatedAt string
						Priority                                           int
						State                                              struct{ Name string }
						Labels                                             struct{ Nodes []struct{ Name string } }
					}
					PageInfo struct {
						HasNextPage bool
						EndCursor   string
					}
				}
			}
		}
		if err := c.graphql(ctx, q, map[string]any{"team": teamID, "after": after}, &data); err != nil {
			return nil, err
		}
		for _, n := range data.Team.Issues.Nodes {
			v := linearIssue{ID: n.ID, Identifier: n.Identifier, Title: n.Title, Description: n.Description, URL: n.URL, UpdatedAt: n.UpdatedAt, Priority: n.Priority, StateName: n.State.Name}
			for _, l := range n.Labels.Nodes {
				v.Labels = append(v.Labels, l.Name)
			}
			out = append(out, v)
		}
		if !data.Team.Issues.PageInfo.HasNextPage {
			break
		}
		if data.Team.Issues.PageInfo.EndCursor == "" {
			return nil, fmt.Errorf("Linear pagination cursor missing")
		}
		after = data.Team.Issues.PageInfo.EndCursor
	}
	return out, nil
}

func (c *linearClient) updateState(ctx context.Context, issueID, stateID string) error {
	const q = `mutation MoveLoopIssue($id: String!, $state: String!) { issueUpdate(id: $id, input: {stateId: $state}) { success } }`
	var data struct{ IssueUpdate struct{ Success bool } }
	if err := c.graphql(ctx, q, map[string]any{"id": issueID, "state": stateID}, &data); err != nil {
		return err
	}
	if !data.IssueUpdate.Success {
		return fmt.Errorf("Linear issueUpdate failed")
	}
	return nil
}

func (c *linearClient) addLabel(ctx context.Context, issueID, labelID string) error {
	const q = `mutation AddLoopLabel($id: String!, $label: String!) { issueAddLabel(id: $id, labelId: $label) { success } }`
	var data struct{ IssueAddLabel struct{ Success bool } }
	if err := c.graphql(ctx, q, map[string]any{"id": issueID, "label": labelID}, &data); err != nil {
		return err
	}
	if !data.IssueAddLabel.Success {
		return fmt.Errorf("Linear issueAddLabel failed")
	}
	return nil
}
func (c *linearClient) removeLabel(ctx context.Context, issueID, labelID string) error {
	const q = `mutation RemoveLoopLabel($id: String!, $label: String!) { issueRemoveLabel(id: $id, labelId: $label) { success } }`
	var data struct{ IssueRemoveLabel struct{ Success bool } }
	if err := c.graphql(ctx, q, map[string]any{"id": issueID, "label": labelID}, &data); err != nil {
		return err
	}
	if !data.IssueRemoveLabel.Success {
		return fmt.Errorf("Linear issueRemoveLabel failed")
	}
	return nil
}
func (c *linearClient) findIssue(ctx context.Context, id string) (map[string]any, bool, error) {
	const q = `query GroomOperation($id: String!) { issue(id: $id) { id identifier title url state { name } } }`
	var data struct {
		Issue *struct {
			ID, Identifier, Title, URL string
			State                      struct{ Name string }
		}
	}
	if err := c.graphql(ctx, q, map[string]any{"id": id}, &data); err != nil {
		return nil, false, err
	}
	if data.Issue == nil || data.Issue.ID == "" {
		return nil, false, nil
	}
	return map[string]any{"id": data.Issue.ID, "identifier": data.Issue.Identifier, "title": data.Issue.Title, "url": data.Issue.URL, "status": data.Issue.State.Name}, true, nil
}

var loopCardRE = regexp.MustCompile("(?s)```loop-card\\s*(\\{.*?\\})\\s*```")

func parseLoopCard(issue linearIssue, cfg *Config) ([]byte, error) {
	m := loopCardRE.FindStringSubmatch(issue.Description)
	if len(m) != 2 {
		return nil, E(2, "%s has no loop-card block", issue.Identifier)
	}
	raw := map[string]any{}
	if err := json.Unmarshal([]byte(m[1]), &raw); err != nil {
		return nil, E(2, "%s loop-card is invalid: %v", issue.Identifier, err)
	}
	raw["id"] = deriveCardID(issue.Identifier, issue.ID)
	raw["title"] = issue.Title
	raw["linear_issue_id"] = issue.Identifier
	raw["linear_issue_uuid"] = issue.ID
	raw["linear_url"] = issue.URL
	raw["source_revision"] = issue.UpdatedAt
	raw["priority"] = issue.Priority
	raw["contract_hash"] = contractHash(raw)
	out, _ := json.Marshal(raw)
	if _, _, err := DecodeCard(out, cfg); err != nil {
		return nil, err
	}
	return out, nil
}
func deriveCardID(identifier, uuid string) string {
	id := strings.ToLower(identifier)
	id = regexp.MustCompile(`[^a-z0-9-]`).ReplaceAllString(id, "-")
	if len(id) > 32 {
		id = id[:32]
	}
	if cardIDPattern.MatchString(id) {
		return id
	}
	suffix := strings.ReplaceAll(uuid, "-", "")
	if len(suffix) > 6 {
		suffix = suffix[:6]
	}
	return "card-" + suffix
}
func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

type linearRuntime struct {
	Outbox     []linearAction `json:"outbox"`
	LastSyncAt string         `json:"last_sync_at,omitempty"`
}
type linearAction struct {
	IssueID   string `json:"issue_id"`
	StateID   string `json:"state_id,omitempty"`
	StateName string `json:"state_name,omitempty"`
	Kind      string `json:"kind"`
}

func (s *State) loadLinearRuntime() (linearRuntime, error) {
	var r linearRuntime
	b, err := os.ReadFile(s.Root + "/runtime/linear.json")
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, E(7, "invalid Linear runtime: %v", err)
	}
	return r, nil
}
func (s *State) saveLinearRuntime(r linearRuntime) error {
	b, _ := Encode(r)
	return writeAtomic(s.Root+"/runtime/linear.json", b, 0600)
}
func (s *State) updateLinearRuntime(fn func(*linearRuntime)) error {
	f, err := os.OpenFile(filepath.Join(s.Root, "runtime", "linear.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	r, err := s.loadLinearRuntime()
	if err != nil {
		return err
	}
	fn(&r)
	return s.saveLinearRuntime(r)
}
func actionKey(a linearAction) string {
	return a.Kind + "|" + a.IssueID + "|" + a.StateID + "|" + a.StateName
}

func (s *State) Sync(ctx context.Context) (map[string]any, error) {
	client, err := s.linearClient()
	if err != nil {
		return nil, err
	}
	runtime, err := s.loadLinearRuntime()
	if err != nil {
		return nil, err
	}
	original := append([]linearAction(nil), runtime.Outbox...)
	pending := runtime.Outbox[:0]
	for _, a := range runtime.Outbox {
		var actionErr error
		switch a.Kind {
		case "state":
			stateID := a.StateID
			if stateID == "" {
				stateID, actionErr = s.linearStateID(ctx, client, a.StateName)
			}
			if actionErr == nil {
				actionErr = client.updateState(ctx, a.IssueID, stateID)
			}
		case "attention":
			var labelID string
			labelID, actionErr = s.linearLabelID(ctx, client, s.Config.Linear.NeedsAttentionLabel)
			if actionErr == nil {
				actionErr = client.addLabel(ctx, a.IssueID, labelID)
			}
		case "attention-remove":
			var labelID string
			labelID, actionErr = s.linearLabelID(ctx, client, s.Config.Linear.NeedsAttentionLabel)
			if actionErr == nil {
				actionErr = client.removeLabel(ctx, a.IssueID, labelID)
			}
		}
		if actionErr != nil {
			pending = append(pending, a)
		}
	}
	runtime.Outbox = pending
	issues, err := client.issues(ctx, s.Config.Linear.TeamID)
	if err != nil {
		return nil, E(8, "Linear sync failed: %v", err)
	}
	imported := []string{}
	updated := []string{}
	attention := []string{}
	cancelled := []string{}
	rejected := map[string]string{}
	todoState, err := s.linearStateID(ctx, client, s.Config.Linear.StatusMap["todo"])
	if err != nil {
		return nil, E(8, "Linear status lookup failed: %v", err)
	}
	for _, issue := range issues {
		cards, _ := s.AllCards()
		var existing *struct {
			Status, Path string
			Raw          map[string]any
			Card         *Card
		}
		for i := range cards {
			if cards[i].Card.LinearIssueUUID == issue.ID {
				existing = &cards[i]
				break
			}
		}
		if existing != nil {
			if existing.Status == "done" || existing.Status == "cancelled" {
				continue
			}
			ready := containsString(issue.Labels, s.Config.Linear.ReadyLabel)
			isCancelled := issue.StateName == "Canceled" || issue.StateName == "Cancelled" || issue.StateName == "Duplicate"
			if !ready || isCancelled {
				to, e := s.syncCancellation(existing.Card.ID)
				if e == nil {
					if to == "cancelled" {
						_ = s.releaseReservation(existing.Card.ID)
						cancelled = append(cancelled, issue.Identifier)
					} else {
						attention = append(attention, issue.Identifier)
					}
				}
				continue
			}
			if data, e := parseLoopCard(issue, &s.Config); e == nil {
				newRaw, newCard, _ := DecodeCard(data, &s.Config)
				if newCard.ContractHash != existing.Card.ContractHash {
					if existing.Status == "todo" || existing.Status == "blocked" {
						if e := validateDependencies(newCard, cards); e != nil {
							if _, moveErr := s.withMoveInternal(existing.Card.ID, "needs_attention", "system/sync", e.Error(), map[string]any{"spec_changed": true}); moveErr == nil {
								attention = append(attention, issue.Identifier)
							}
							continue
						}
						patch := map[string]any{}
						for _, k := range []string{"problem", "desired_outcome", "out_of_scope", "repo", "repo_path", "base", "tier", "touches", "acceptance", "verification", "depends_on", "risk", "rollback_notes", "contract_hash", "source_revision"} {
							patch[k] = newRaw[k]
						}
						patch["hot"] = loopglob.PatternsOverlap(newCard.Touches, s.Config.HotPaths)
						to, e := s.syncContractChange(existing.Card.ID, patch, issue.UpdatedAt)
						if e == nil {
							if to == "updated" {
								updated = append(updated, issue.Identifier)
							} else {
								attention = append(attention, issue.Identifier)
							}
						}
					} else {
						if _, e = s.withMoveInternal(existing.Card.ID, "needs_attention", "system/sync", "Linear contract changed after claim", map[string]any{"spec_changed": true, "source_revision": issue.UpdatedAt}); e == nil {
							attention = append(attention, issue.Identifier)
						}
					}
				}
			}
			continue
		}
		if issue.StateName != s.Config.Linear.StatusMap["backlog"] || !containsString(issue.Labels, s.Config.Linear.ReadyLabel) {
			continue
		}
		data, e := parseLoopCard(issue, &s.Config)
		if e != nil {
			rejected[issue.Identifier] = e.Error()
			if labelID, labelErr := s.linearLabelID(ctx, client, s.Config.Linear.NeedsAttentionLabel); labelErr != nil || client.addLabel(ctx, issue.ID, labelID) != nil {
				runtime.Outbox = append(runtime.Outbox, linearAction{IssueID: issue.ID, Kind: "attention"})
			}
			continue
		}
		raw, c, e := DecodeCard(data, &s.Config)
		if e != nil {
			rejected[issue.Identifier] = e.Error()
			if labelID, labelErr := s.linearLabelID(ctx, client, s.Config.Linear.NeedsAttentionLabel); labelErr != nil || client.addLabel(ctx, issue.ID, labelID) != nil {
				runtime.Outbox = append(runtime.Outbox, linearAction{IssueID: issue.ID, Kind: "attention"})
			}
			continue
		}
		if _, _, locErr := s.Locate(c.ID); locErr == nil {
			suffix := strings.TrimPrefix(Hash([]byte(issue.ID)), "sha256:")[:6]
			base := c.ID
			if len(base) > 25 {
				base = base[:25]
			}
			raw["id"] = strings.TrimRight(base, "-") + "-" + suffix
			encoded, _ := json.Marshal(raw)
			raw, c, e = DecodeCard(encoded, &s.Config)
			if e != nil {
				rejected[issue.Identifier] = e.Error()
				continue
			}
		} else if ExitCode(locErr) == 7 {
			rejected[issue.Identifier] = locErr.Error()
			continue
		}
		normalizeNew(raw, c, &s.Config)
		encoded, _ := Encode(raw)
		if _, e = s.Add(encoded, "system/sync"); e != nil {
			rejected[issue.Identifier] = e.Error()
			continue
		}
		imported = append(imported, issue.Identifier)
		if e = client.updateState(ctx, issue.ID, todoState); e != nil {
			runtime.Outbox = append(runtime.Outbox, linearAction{IssueID: issue.ID, StateID: todoState, Kind: "state"})
		}
	}
	runtime.LastSyncAt = Now()
	desired := append([]linearAction(nil), runtime.Outbox...)
	originalKeys := map[string]bool{}
	for _, a := range original {
		originalKeys[actionKey(a)] = true
	}
	if err := s.updateLinearRuntime(func(current *linearRuntime) {
		merged := []linearAction{}
		seen := map[string]bool{}
		for _, a := range current.Outbox {
			if originalKeys[actionKey(a)] {
				continue
			}
			k := actionKey(a)
			if !seen[k] {
				merged = append(merged, a)
				seen[k] = true
			}
		}
		for _, a := range desired {
			k := actionKey(a)
			if !seen[k] {
				merged = append(merged, a)
				seen[k] = true
			}
		}
		current.Outbox = merged
		current.LastSyncAt = runtime.LastSyncAt
	}); err != nil {
		return nil, err
	}
	final, err := s.loadLinearRuntime()
	if err != nil {
		return nil, err
	}
	return map[string]any{"imported": imported, "updated": updated, "needs_attention": attention, "cancelled": cancelled, "rejected": rejected, "pending": len(final.Outbox)}, nil
}

func (s *State) syncCancellation(id string) (string, error) {
	result := ""
	err := s.withLock(func() error {
		status, _, _, _, e := s.readCardPath(id)
		if e != nil {
			return e
		}
		if status == "done" || status == "cancelled" {
			result = status
			return nil
		}
		to := "needs_attention"
		patch := map[string]any{"spec_changed": true}
		if status == "todo" || status == "blocked" {
			to = "cancelled"
			patch = map[string]any{}
		}
		if _, e = s.moveLocked(id, to, "system/sync", "Linear approval removed or issue cancelled", patch, true); e != nil {
			return e
		}
		result = to
		return nil
	})
	if result == "cancelled" {
		_ = s.releaseReservation(id)
	}
	return result, err
}
func (s *State) syncContractChange(id string, patch map[string]any, revision string) (string, error) {
	result := ""
	err := s.withLock(func() error {
		status, _, _, _, e := s.readCardPath(id)
		if e != nil {
			return e
		}
		if status == "todo" || status == "blocked" {
			if _, e = s.moveLocked(id, status, "system/sync", "Linear contract updated before claim", patch, true); e != nil {
				return e
			}
			result = "updated"
			return nil
		}
		if status == "done" || status == "cancelled" {
			result = status
			return nil
		}
		if _, e = s.moveLocked(id, "needs_attention", "system/sync", "Linear contract changed after claim", map[string]any{"spec_changed": true, "source_revision": revision}, true); e != nil {
			return e
		}
		result = "attention"
		return nil
	})
	return result, err
}

func (s *State) enqueueLinear(card *Card, from, status string) error {
	if card.LinearIssueUUID == "" {
		return nil
	}
	kind, stateName := "state", ""
	switch status {
	case "todo", "blocked":
		stateName = s.Config.Linear.StatusMap["todo"]
	case "claimed-dev", "rework":
		stateName = s.Config.Linear.StatusMap["in_progress"]
	case "in_review", "claimed-qa":
		stateName = s.Config.Linear.StatusMap["in_review"]
	case "done":
		stateName = s.Config.Linear.StatusMap["done"]
	case "cancelled":
		stateName = "Canceled"
	case "needs_attention":
		kind = "attention"
	default:
		return nil
	}
	candidate := linearAction{IssueID: card.LinearIssueUUID, StateName: stateName, Kind: kind}
	return s.updateLinearRuntime(func(runtime *linearRuntime) {
		if candidate.Kind == "state" {
			kept := runtime.Outbox[:0]
			for _, a := range runtime.Outbox {
				if a.IssueID == candidate.IssueID && a.Kind == "state" {
					continue
				}
				kept = append(kept, a)
			}
			runtime.Outbox = kept
		}
		for _, a := range runtime.Outbox {
			if actionKey(a) == actionKey(candidate) {
				return
			}
		}
		runtime.Outbox = append(runtime.Outbox, candidate)
		if from == "needs_attention" && status != "needs_attention" {
			remove := linearAction{IssueID: card.LinearIssueUUID, Kind: "attention-remove"}
			present := false
			for _, a := range runtime.Outbox {
				if actionKey(a) == actionKey(remove) {
					present = true
				}
			}
			if !present {
				runtime.Outbox = append(runtime.Outbox, remove)
			}
		}
	})
}

func (s *State) linearStateID(ctx context.Context, c *linearClient, name string) (string, error) {
	const q = `query TeamStates($team: String!) { team(id: $team) { states { nodes { id name } } } }`
	var data struct {
		Team struct {
			States struct{ Nodes []struct{ ID, Name string } }
		}
	}
	if err := c.graphql(ctx, q, map[string]any{"team": s.Config.Linear.TeamID}, &data); err != nil {
		return "", err
	}
	for _, x := range data.Team.States.Nodes {
		if x.Name == name {
			return x.ID, nil
		}
	}
	return "", fmt.Errorf("status %q not found", name)
}

func (s *State) GroomCreate(ctx context.Context, data []byte, approvedBy string) (map[string]any, error) {
	if strings.TrimSpace(approvedBy) == "" {
		return nil, E(2, "--approved-by is required")
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, E(2, "invalid card JSON: %v", err)
	}
	opID, _ := raw["operation_id"].(string)
	if opID == "" {
		opID = deterministicUUID(append(append([]byte(nil), data...), []byte("|"+approvedBy)...))
	}
	if !regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`).MatchString(opID) {
		return nil, E(2, "operation_id must be a UUID")
	}
	delete(raw, "operation_id")
	raw["id"] = "preview"
	raw["repo"] = s.Config.Repo
	raw["repo_path"] = s.Config.RepoPath
	raw["base"] = "dev"
	raw["linear_issue_id"] = "pending"
	raw["linear_issue_uuid"] = "pending"
	raw["linear_url"] = "pending"
	raw["source_revision"] = Now()
	raw["approved_at"] = Now()
	raw["approved_by"] = approvedBy
	b, _ := json.Marshal(raw)
	if _, _, err := DecodeCard(b, &s.Config); err != nil {
		return nil, err
	}
	title, _ := raw["title"].(string)
	delete(raw, "id")
	delete(raw, "linear_issue_id")
	delete(raw, "linear_issue_uuid")
	delete(raw, "linear_url")
	delete(raw, "source_revision")
	delete(raw, "contract_hash")
	for _, k := range []string{"status", "hot", "attempts", "max_attempts", "rework_count", "max_rework", "conflict_skips", "claimed_at", "claimed_by", "worktree", "branch", "pr", "base_sha", "tested_head_sha", "stale", "spec_changed", "qa_findings", "proposed", "history"} {
		delete(raw, k)
	}
	cardBlock, _ := json.MarshalIndent(raw, "", "  ")
	problem, _ := raw["problem"].(string)
	outcome, _ := raw["desired_outcome"].(string)
	description := fmt.Sprintf("## Problem\n%s\n\n## Desired outcome\n%s\n\n```loop-card\n%s\n```", problem, outcome, cardBlock)
	client, err := s.linearClient()
	if err != nil {
		return nil, err
	}
	if existing, ok, e := client.findIssue(ctx, opID); e == nil && ok {
		existing["label"] = s.Config.Linear.ReadyLabel
		return existing, nil
	}
	backlogID, err := s.linearStateID(ctx, client, s.Config.Linear.StatusMap["backlog"])
	if err != nil {
		return nil, E(8, "Linear backlog lookup failed: %v", err)
	}
	labelID, err := s.linearLabelID(ctx, client, s.Config.Linear.ReadyLabel)
	if err != nil {
		return nil, E(8, "Linear label lookup failed: %v", err)
	}
	const q = `mutation CreateGroomedIssue($input: IssueCreateInput!) { issueCreate(input: $input) { success issue { id identifier title url updatedAt state { name } } } }`
	var response struct {
		IssueCreate struct {
			Success bool
			Issue   struct {
				ID, Identifier, Title, URL, UpdatedAt string
				State                                 struct{ Name string }
			}
		}
	}
	input := map[string]any{"id": opID, "teamId": s.Config.Linear.TeamID, "title": title, "description": description, "stateId": backlogID, "labelIds": []string{labelID}}
	if err := client.graphql(ctx, q, map[string]any{"input": input}, &response); err != nil {
		if existing, ok, _ := client.findIssue(ctx, opID); ok {
			existing["label"] = s.Config.Linear.ReadyLabel
			return existing, nil
		}
		return nil, E(8, "Linear create failed: %v", err)
	}
	if !response.IssueCreate.Success {
		return nil, E(8, "Linear create failed")
	}
	i := response.IssueCreate.Issue
	return map[string]any{"id": i.ID, "identifier": i.Identifier, "title": i.Title, "url": i.URL, "status": i.State.Name, "label": s.Config.Linear.ReadyLabel}, nil
}

func deterministicUUID(data []byte) string {
	hex := strings.TrimPrefix(Hash(data), "sha256:")
	b := []byte(hex[:32])
	b[12] = '4'
	b[16] = '8'
	return string(b[:8]) + "-" + string(b[8:12]) + "-" + string(b[12:16]) + "-" + string(b[16:20]) + "-" + string(b[20:32])
}

func (s *State) linearLabelID(ctx context.Context, c *linearClient, name string) (string, error) {
	const q = `query TeamLabels($team: String!) { team(id: $team) { labels { nodes { id name } } } }`
	var data struct {
		Team struct {
			Labels struct{ Nodes []struct{ ID, Name string } }
		}
	}
	if err := c.graphql(ctx, q, map[string]any{"team": s.Config.Linear.TeamID}, &data); err != nil {
		return "", err
	}
	for _, x := range data.Team.Labels.Nodes {
		if x.Name == name {
			return x.ID, nil
		}
	}
	return "", fmt.Errorf("label %q not found", name)
}
