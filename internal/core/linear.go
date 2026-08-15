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
	"sort"
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
	ProjectID, ProjectName, ParentID                              string
}

type reusableLinearParent struct {
	ID, Identifier, Title, URL, ArchivedAt, StateName string
	TeamID, ProjectID                                 string
	Labels                                            []string
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
	const q = `query LoopIssues($team: String!, $after: String) { team(id: $team) { issues(first: 100, after: $after, includeArchived: true) { nodes { id identifier title description url updatedAt priority state { name } project { id name } parent { id } labels { nodes { name } } } pageInfo { hasNextPage endCursor } } } }`
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
						Project                                            struct{ ID, Name string }
						Parent                                             struct{ ID string }
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
			v := linearIssue{ID: n.ID, Identifier: n.Identifier, Title: n.Title, Description: n.Description, URL: n.URL, UpdatedAt: n.UpdatedAt, Priority: n.Priority, StateName: n.State.Name, ProjectID: n.Project.ID, ProjectName: n.Project.Name, ParentID: n.Parent.ID}
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

func (c *linearClient) hasComment(ctx context.Context, issueID, marker string) (bool, error) {
	const q = `query LoopIssueComments($id: String!, $after: String) { issue(id: $id) { comments(first: 100, after: $after) { nodes { body } pageInfo { hasNextPage endCursor } } } }`
	var after any
	for {
		var data struct {
			Issue *struct {
				Comments struct {
					Nodes    []struct{ Body string }
					PageInfo struct {
						HasNextPage bool
						EndCursor   string
					}
				}
			}
		}
		if err := c.graphql(ctx, q, map[string]any{"id": issueID, "after": after}, &data); err != nil {
			return false, err
		}
		if data.Issue == nil {
			return false, fmt.Errorf("Linear issue %q not found", issueID)
		}
		for _, comment := range data.Issue.Comments.Nodes {
			if strings.Contains(comment.Body, marker) {
				return true, nil
			}
		}
		if !data.Issue.Comments.PageInfo.HasNextPage {
			return false, nil
		}
		if data.Issue.Comments.PageInfo.EndCursor == "" {
			return false, fmt.Errorf("Linear comment pagination cursor missing")
		}
		after = data.Issue.Comments.PageInfo.EndCursor
	}
}

func (c *linearClient) addComment(ctx context.Context, issueID, body string) error {
	if marker := linearCommentMarker(body); marker != "" {
		seen, err := c.hasComment(ctx, issueID, marker)
		if err != nil {
			return err
		}
		if seen {
			return nil
		}
	}
	const q = `mutation AddLoopComment($input: CommentCreateInput!) { commentCreate(input: $input) { success comment { id } } }`
	var data struct {
		CommentCreate struct {
			Success bool
			Comment struct{ ID string }
		}
	}
	if err := c.graphql(ctx, q, map[string]any{"input": map[string]any{"issueId": issueID, "body": body}}, &data); err != nil {
		return err
	}
	if !data.CommentCreate.Success || data.CommentCreate.Comment.ID == "" {
		return fmt.Errorf("Linear commentCreate failed")
	}
	return nil
}
func (c *linearClient) findIssue(ctx context.Context, id string) (map[string]any, bool, error) {
	const q = `query GroomOperation($id: String!) { issue(id: $id) { id identifier title url state { name } labels { nodes { name } } } }`
	var data struct {
		Issue *struct {
			ID, Identifier, Title, URL string
			State                      struct{ Name string }
			Labels                     struct{ Nodes []struct{ Name string } }
		}
	}
	if err := c.graphql(ctx, q, map[string]any{"id": id}, &data); err != nil {
		return nil, false, err
	}
	if data.Issue == nil || data.Issue.ID == "" {
		return nil, false, nil
	}
	labels := []string{}
	for _, label := range data.Issue.Labels.Nodes {
		labels = append(labels, label.Name)
	}
	return map[string]any{"id": data.Issue.ID, "identifier": data.Issue.Identifier, "title": data.Issue.Title, "url": data.Issue.URL, "status": data.Issue.State.Name, "labels": labels}, true, nil
}

func (s *State) ensureGroomReadyLabel(ctx context.Context, client *linearClient, issue map[string]any, readyLabelID *string) error {
	labels := stringSlice(issue["labels"])
	if containsString(labels, s.Config.Linear.ReadyLabel) {
		return nil
	}
	if strings.TrimSpace(*readyLabelID) == "" {
		labelID, err := s.linearLabelID(ctx, client, s.Config.Linear.ReadyLabel)
		if err != nil {
			return E(8, "Linear ready label lookup failed: %v", err)
		}
		*readyLabelID = labelID
	}
	issueID := strings.TrimSpace(fmt.Sprint(issue["id"]))
	if issueID == "" || issueID == "<nil>" {
		return E(8, "Linear issue identity is missing while restoring %s", s.Config.Linear.ReadyLabel)
	}
	if err := client.addLabel(ctx, issueID, *readyLabelID); err != nil {
		return E(8, "Linear ready label restore failed: %v", err)
	}
	issue["labels"] = append(labels, s.Config.Linear.ReadyLabel)
	return nil
}

func (s *State) promoteApprovedLinearIssue(ctx context.Context, client *linearClient, issue *linearIssue, readyLabelID *string) (bool, error) {
	if containsString(issue.Labels, s.Config.Linear.ReadyLabel) || containsString(issue.Labels, s.Config.Linear.NeedsAttentionLabel) {
		return false, nil
	}
	if issue.StateName == "Canceled" || issue.StateName == "Cancelled" || issue.StateName == "Duplicate" || issue.StateName == s.Config.Linear.StatusMap["done"] {
		return false, nil
	}
	// parseLoopCard validates the complete Definition of Ready, including the
	// approval audit fields. A plan parent has no loop-card and therefore can
	// never be promoted by this repair path.
	if _, err := parseLoopCard(*issue, &s.Config); err != nil {
		return false, nil
	}
	if strings.TrimSpace(*readyLabelID) == "" {
		labelID, err := s.linearLabelID(ctx, client, s.Config.Linear.ReadyLabel)
		if err != nil {
			return false, E(8, "Linear ready label lookup failed: %v", err)
		}
		*readyLabelID = labelID
	}
	if err := client.addLabel(ctx, issue.ID, *readyLabelID); err != nil {
		return false, E(8, "Linear ready label restore failed for %s: %v", issue.Identifier, err)
	}
	issue.Labels = append(issue.Labels, s.Config.Linear.ReadyLabel)
	return true, nil
}

func (c *linearClient) reusableParent(ctx context.Context, id string) (reusableLinearParent, bool, error) {
	const q = `query ReusableGroomParent($id: String!) { issue(id: $id) { id identifier title url archivedAt state { name } team { id } project { id } labels { nodes { name } } } }`
	var data struct {
		Issue *struct {
			ID, Identifier, Title, URL, ArchivedAt string
			State                                  struct{ Name string }
			Team                                   struct{ ID string }
			Project                                struct{ ID string }
			Labels                                 struct{ Nodes []struct{ Name string } }
		}
	}
	if err := c.graphql(ctx, q, map[string]any{"id": id}, &data); err != nil {
		return reusableLinearParent{}, false, err
	}
	if data.Issue == nil || data.Issue.ID == "" {
		return reusableLinearParent{}, false, nil
	}
	i := reusableLinearParent{ID: data.Issue.ID, Identifier: data.Issue.Identifier, Title: data.Issue.Title, URL: data.Issue.URL, ArchivedAt: data.Issue.ArchivedAt, StateName: data.Issue.State.Name, TeamID: data.Issue.Team.ID, ProjectID: data.Issue.Project.ID}
	for _, label := range data.Issue.Labels.Nodes {
		i.Labels = append(i.Labels, label.Name)
	}
	return i, true, nil
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
	// Workflow status is owned by Linear. Never persist a legacy/local status
	// field from a hand-authored card block into the canonical snapshot.
	delete(raw, "status")
	raw["id"] = deriveCardID(issue.Identifier, issue.ID)
	raw["title"] = issue.Title
	raw["linear_issue_id"] = issue.Identifier
	raw["linear_issue_uuid"] = issue.ID
	raw["linear_url"] = issue.URL
	raw["linear_state"] = issue.StateName
	raw["linear_labels"] = linearLabels(issue.Labels)
	raw["linear_updated_at"] = issue.UpdatedAt
	raw["source_revision"] = issue.UpdatedAt
	delete(raw, "linear_parent_id")
	if issue.ParentID != "" {
		raw["linear_parent_id"] = issue.ParentID
	}
	if issue.Priority < 1 || issue.Priority > 4 {
		return nil, E(2, "%s must have Urgent, High, Medium or Low priority", issue.Identifier)
	}
	raw["priority"] = issue.Priority
	if issue.ProjectID == "" || issue.ProjectName == "" {
		return nil, E(2, "%s must belong to a Linear project", issue.Identifier)
	}
	raw["linear_project_id"] = issue.ProjectID
	raw["linear_project"] = issue.ProjectName
	workType, err := workTypeFromLabels(issue.Labels)
	if err != nil {
		return nil, E(2, "%s %v", issue.Identifier, err)
	}
	raw["work_type"] = workType
	raw["contract_hash"] = contractHash(raw)
	out, _ := json.Marshal(raw)
	if _, _, err := DecodeCard(out, cfg); err != nil {
		return nil, err
	}
	return out, nil
}

func workTypeFromLabels(labels []string) (string, error) {
	found := ""
	for _, candidate := range []string{"feature", "bug", "maintenance"} {
		if !containsString(labels, "type:"+candidate) {
			continue
		}
		if found != "" {
			return "", E(2, "must have exactly one work type label")
		}
		found = candidate
	}
	if found == "" {
		return "", E(2, "must have one of type:feature, type:bug or type:maintenance")
	}
	return found, nil
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
	Outbox      []linearAction    `json:"outbox"`
	IssueStates map[string]string `json:"issue_states,omitempty"`
	LastSyncAt  string            `json:"last_sync_at,omitempty"`
}
type linearAction struct {
	IssueID   string `json:"issue_id"`
	StateID   string `json:"state_id,omitempty"`
	StateName string `json:"state_name,omitempty"`
	Kind      string `json:"kind"`
	Body      string `json:"body,omitempty"`
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
	key := a.Kind + "|" + a.IssueID + "|" + a.StateID + "|" + a.StateName
	if a.Kind == "comment" {
		key += "|" + Hash([]byte(a.Body))
	}
	return key
}

func (s *State) flushLinearOutbox(ctx context.Context, client *linearClient) (int, error) {
	runtime, err := s.loadLinearRuntime()
	if err != nil {
		return 0, err
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
		case "comment":
			actionErr = client.addComment(ctx, a.IssueID, a.Body)
		}
		if actionErr != nil {
			pending = append(pending, a)
		} else {
			// The mutation succeeded remotely. Keep the board-facing snapshot
			// useful immediately; the next full sync will replace the empty
			// revision with Linear's authoritative updatedAt value.
			_ = s.applyLinearActionSnapshot(a)
		}
	}
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
		for _, a := range pending {
			k := actionKey(a)
			if !seen[k] {
				merged = append(merged, a)
				seen[k] = true
			}
		}
		current.Outbox = merged
	}); err != nil {
		return 0, err
	}
	final, err := s.loadLinearRuntime()
	if err != nil {
		return 0, err
	}
	return len(final.Outbox), nil
}

// FlushLinearOutbox pushes queued state and label changes without performing a
// full backlog sync. Failed remote actions remain queued for the periodic sync.
func (s *State) FlushLinearOutbox(ctx context.Context) (int, error) {
	runtime, err := s.loadLinearRuntime()
	if err != nil || len(runtime.Outbox) == 0 {
		return len(runtime.Outbox), err
	}
	client, err := s.linearClient()
	if err != nil {
		return len(runtime.Outbox), err
	}
	return s.flushLinearOutbox(ctx, client)
}

func (s *State) Sync(ctx context.Context) (map[string]any, error) {
	client, err := s.linearClient()
	if err != nil {
		return nil, err
	}
	if _, err := s.flushLinearOutbox(ctx, client); err != nil {
		return nil, err
	}
	runtime, err := s.loadLinearRuntime()
	if err != nil {
		return nil, err
	}
	original := append([]linearAction(nil), runtime.Outbox...)
	issues, err := client.issues(ctx, s.Config.Linear.TeamID)
	if err != nil {
		return nil, E(8, "Linear sync failed: %v", err)
	}
	issues = orderLinearIssuesByDependencies(issues)
	issueStates := map[string]string{}
	for _, issue := range issues {
		issueStates[issue.ID] = issue.StateName
	}
	runtime.IssueStates = issueStates
	if err := s.saveLinearRuntime(runtime); err != nil {
		return nil, err
	}
	imported := []string{}
	updated := []string{}
	attention := []string{}
	cancelled := []string{}
	autoReady := []string{}
	rejected := map[string]string{}
	todoState, err := s.linearStateID(ctx, client, s.Config.Linear.StatusMap["todo"])
	if err != nil {
		return nil, E(8, "Linear status lookup failed: %v", err)
	}
	readyLabelID := ""
	for _, issue := range issues {
		cards, cardsErr := s.AllCards()
		if cardsErr != nil {
			return nil, cardsErr
		}
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
			if existing.Status != "done" {
				promoted, promoteErr := s.promoteApprovedLinearIssue(ctx, client, &issue, &readyLabelID)
				if promoteErr != nil {
					return nil, promoteErr
				}
				if promoted {
					autoReady = appendUniqueIdentifier(autoReady, issue.Identifier)
				}
			}
			metadataChanged, metadataErr := s.syncLinearMetadata(existing.Card.ID, issue)
			if metadataErr != nil {
				return nil, metadataErr
			}
			if metadataChanged {
				updated = appendUniqueIdentifier(updated, issue.Identifier)
			}
			if issue.ParentID != existing.Card.LinearParentID {
				if changed, metadataErr := s.syncLinearParent(existing.Card.ID, issue.ParentID, issue.UpdatedAt); metadataErr != nil {
					return nil, metadataErr
				} else if changed {
					updated = appendUniqueIdentifier(updated, issue.Identifier)
				}
			}
			if existing.Status == "done" {
				continue
			}
			ready := containsString(issue.Labels, s.Config.Linear.ReadyLabel)
			isCancelled := issue.StateName == "Canceled" || issue.StateName == "Cancelled" || issue.StateName == "Duplicate"
			needsAttention := containsString(issue.Labels, s.Config.Linear.NeedsAttentionLabel)
			if ready && !isCancelled && !needsAttention {
				to := s.linearRuntimePhase(issue.StateName)
				// A live worker lease is private runtime state and must not be
				// interrupted by a stale or human-edited Linear snapshot. Every
				// other phase follows the current Linear state, including a card
				// reopened from a historical cancelled/attention phase.
				if to != "" && !runtimePhaseActive(existing.Status) && existing.Status != "done" && existing.Status != to {
					if _, reopenErr := s.withMoveInternal(existing.Card.ID, to, "system/sync", "Linear issue is approved again", map[string]any{"claimed_at": nil, "claimed_by": nil, "spec_changed": false}); reopenErr == nil {
						existing.Status = to
						existing.Card.Status = to
						updated = appendUniqueIdentifier(updated, issue.Identifier)
					}
				}
			}
			if ready && !isCancelled && issue.StateName == s.Config.Linear.StatusMap["backlog"] {
				runtime.Outbox = appendLinearAction(runtime.Outbox, linearAction{IssueID: issue.ID, StateID: todoState, StateName: s.Config.Linear.StatusMap["todo"], Kind: "state"})
			}
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
						if e := validateDependenciesWithExternal(newCard, cards, mapKeys(issueStates)); e != nil {
							if _, moveErr := s.withMoveInternal(existing.Card.ID, "needs_attention", "system/sync", e.Error(), map[string]any{"spec_changed": true}); moveErr == nil {
								attention = append(attention, issue.Identifier)
							}
							continue
						}
						patch := map[string]any{}
						for _, k := range []string{"problem", "desired_outcome", "out_of_scope", "repo", "repo_path", "base", "tier", "touches", "acceptance", "verification", "depends_on", "visuals", "risk", "rollback_notes", "contract_hash", "source_revision"} {
							patch[k] = newRaw[k]
						}
						patch["hot"] = loopglob.PatternsOverlap(newCard.Touches, s.Config.HotPaths)
						to, e := s.syncContractChange(existing.Card.ID, patch, issue.UpdatedAt)
						if e == nil {
							if to == "updated" {
								updated = appendUniqueIdentifier(updated, issue.Identifier)
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
			} else if existing.Status != "needs_attention" {
				if _, moveErr := s.withMoveInternal(existing.Card.ID, "needs_attention", "system/sync", e.Error(), map[string]any{"spec_changed": true}); moveErr == nil {
					attention = append(attention, issue.Identifier)
				}
			}
			continue
		}
		promoted, promoteErr := s.promoteApprovedLinearIssue(ctx, client, &issue, &readyLabelID)
		if promoteErr != nil {
			return nil, promoteErr
		}
		if promoted {
			autoReady = appendUniqueIdentifier(autoReady, issue.Identifier)
		}
		ready := containsString(issue.Labels, s.Config.Linear.ReadyLabel)
		isCancelled := issue.StateName == "Canceled" || issue.StateName == "Cancelled" || issue.StateName == "Duplicate"
		if !ready || isCancelled || issue.StateName == s.Config.Linear.StatusMap["done"] {
			continue
		}
		data, e := parseLoopCard(issue, &s.Config)
		if e != nil {
			rejected[issue.Identifier] = e.Error()
			if labelID, labelErr := s.linearLabelID(ctx, client, s.Config.Linear.NeedsAttentionLabel); labelErr != nil || client.addLabel(ctx, issue.ID, labelID) != nil {
				runtime.Outbox = appendLinearAction(runtime.Outbox, linearAction{IssueID: issue.ID, Kind: "attention"})
			}
			runtime.Outbox = appendLinearAction(runtime.Outbox, linearAction{IssueID: issue.ID, Kind: "comment", Body: linearImportAttentionComment(issue, e.Error())})
			continue
		}
		raw, c, e := DecodeCard(data, &s.Config)
		if e != nil {
			rejected[issue.Identifier] = e.Error()
			if labelID, labelErr := s.linearLabelID(ctx, client, s.Config.Linear.NeedsAttentionLabel); labelErr != nil || client.addLabel(ctx, issue.ID, labelID) != nil {
				runtime.Outbox = appendLinearAction(runtime.Outbox, linearAction{IssueID: issue.ID, Kind: "attention"})
			}
			runtime.Outbox = appendLinearAction(runtime.Outbox, linearAction{IssueID: issue.ID, Kind: "comment", Body: linearImportAttentionComment(issue, e.Error())})
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
		if phase := s.linearRuntimePhase(issue.StateName); phase != "" && phase != "todo" {
			if _, moveErr := s.withMoveInternal(c.ID, phase, "system/sync", "Imported from Linear runtime state", map[string]any{"claimed_at": nil, "claimed_by": nil}); moveErr != nil {
				rejected[issue.Identifier] = moveErr.Error()
				continue
			}
		}
		if issue.StateName == s.Config.Linear.StatusMap["backlog"] {
			if e = client.updateState(ctx, issue.ID, todoState); e != nil {
				runtime.Outbox = appendLinearAction(runtime.Outbox, linearAction{IssueID: issue.ID, StateID: todoState, StateName: s.Config.Linear.StatusMap["todo"], Kind: "state"})
			} else {
				_ = s.applyLinearActionSnapshot(linearAction{IssueID: issue.ID, StateName: s.Config.Linear.StatusMap["todo"], Kind: "state"})
			}
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
		current.IssueStates = issueStates
		current.LastSyncAt = runtime.LastSyncAt
	}); err != nil {
		return nil, err
	}
	final, err := s.loadLinearRuntime()
	if err != nil {
		return nil, err
	}
	if len(final.Outbox) > 0 {
		if _, flushErr := s.flushLinearOutbox(ctx, client); flushErr != nil {
			return nil, flushErr
		}
		final, err = s.loadLinearRuntime()
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{"imported": imported, "updated": updated, "auto_ready": autoReady, "needs_attention": attention, "cancelled": cancelled, "rejected": rejected, "pending": len(final.Outbox)}, nil
}

func (s *State) syncLinearParent(id, parentID, revision string) (bool, error) {
	changed := false
	err := s.withLock(func() error {
		status, _, raw, card, err := s.readCardPath(id)
		if err != nil {
			return err
		}
		if card.LinearParentID == parentID {
			return nil
		}
		if parentID == "" {
			delete(raw, "linear_parent_id")
		} else {
			raw["linear_parent_id"] = parentID
		}
		delete(raw, "status")
		raw["source_revision"] = revision
		encoded, err := Encode(raw)
		if err != nil {
			return err
		}
		if _, _, err := DecodeCard(encoded, &s.Config); err != nil {
			return err
		}
		if err := s.rewrite(id, status, encoded, "sync-linear-parent", "system/sync"); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

func (s *State) syncLinearMetadata(id string, issue linearIssue) (bool, error) {
	changed := false
	err := s.withLock(func() error {
		status, _, raw, card, err := s.readCardPath(id)
		if err != nil {
			return err
		}
		labels := linearLabels(issue.Labels)
		if card.LinearState == issue.StateName && slicesEqual(card.LinearLabels, labels) && card.LinearUpdatedAt == issue.UpdatedAt {
			return nil
		}
		raw["linear_state"] = issue.StateName
		raw["linear_labels"] = labels
		raw["linear_updated_at"] = issue.UpdatedAt
		delete(raw, "status")
		encoded, err := Encode(raw)
		if err != nil {
			return err
		}
		if _, _, err := DecodeCard(encoded, &s.Config); err != nil {
			return err
		}
		if err := s.rewrite(id, status, encoded, "sync-linear-metadata", "system/sync"); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

func (s *State) applyLinearActionSnapshot(action linearAction) error {
	return s.withLock(func() error {
		cards, err := s.AllCards()
		if err != nil {
			return err
		}
		for _, item := range cards {
			if item.Card.LinearIssueUUID != action.IssueID {
				continue
			}
			labels := append([]string(nil), item.Card.LinearLabels...)
			stateName := item.Card.LinearState
			switch action.Kind {
			case "state":
				if action.StateName == "" {
					return nil
				}
				stateName = action.StateName
			case "attention":
				labels = append(labels, s.Config.Linear.NeedsAttentionLabel)
			case "attention-remove":
				filtered := labels[:0]
				for _, label := range labels {
					if label != s.Config.Linear.NeedsAttentionLabel {
						filtered = append(filtered, label)
					}
				}
				labels = filtered
			default:
				return nil
			}
			labels = linearLabels(labels)
			if stateName == item.Card.LinearState && slicesEqual(labels, item.Card.LinearLabels) {
				return nil
			}
			raw := item.Raw
			delete(raw, "status")
			raw["linear_state"] = stateName
			raw["linear_labels"] = labels
			raw["linear_updated_at"] = ""
			encoded, err := Encode(raw)
			if err != nil {
				return err
			}
			if _, _, err := DecodeCard(encoded, &s.Config); err != nil {
				return err
			}
			return s.rewrite(item.Card.ID, item.Status, encoded, "flush-linear-snapshot", "system/linear")
		}
		return nil
	})
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func linearLabels(labels []string) []string {
	labels = uniqueStrings(labels)
	sort.Strings(labels)
	return labels
}

func appendLinearAction(actions []linearAction, candidate linearAction) []linearAction {
	for _, action := range actions {
		if actionKey(action) == actionKey(candidate) {
			return actions
		}
	}
	return append(actions, candidate)
}

func appendUniqueIdentifier(values []string, identifier string) []string {
	if containsString(values, identifier) {
		return values
	}
	return append(values, identifier)
}

func mapKeys(values map[string]string) map[string]bool {
	out := make(map[string]bool, len(values))
	for key := range values {
		out[key] = true
	}
	return out
}

func (s *State) linearRuntimePhase(state string) string {
	switch state {
	case s.Config.Linear.StatusMap["backlog"], s.Config.Linear.StatusMap["todo"]:
		return "todo"
	case s.Config.Linear.StatusMap["in_progress"]:
		return "rework"
	case s.Config.Linear.StatusMap["in_review"]:
		return "in_review"
	default:
		return ""
	}
}

func (s *State) linearExecutionPhase(state string) string {
	if phase := s.linearRuntimePhase(state); phase != "" {
		return phase
	}
	if state == s.Config.Linear.StatusMap["done"] {
		return "done"
	}
	if state == "Canceled" || state == "Cancelled" || state == "Duplicate" {
		return "cancelled"
	}
	return ""
}

func runtimePhaseActive(phase string) bool {
	return phase == "claimed-dev" || phase == "claimed-qa"
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

func (s *State) enqueueLinear(card *Card, from, status, actor, note string) error {
	if !s.Config.Linear.Enabled || card.LinearIssueUUID == "" {
		return nil
	}
	mirrorState := !strings.HasPrefix(actor, "system/sync") && !strings.HasPrefix(actor, "system/linear")
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
		kind = ""
	}
	commentNeeded := linearCommentNeeded(from, status, actor, note)
	comment := linearAction{}
	if commentNeeded {
		comment = linearAction{IssueID: card.LinearIssueUUID, Kind: "comment", Body: linearTransitionComment(card, from, status, actor, note)}
	}
	return s.updateLinearRuntime(func(runtime *linearRuntime) {
		if kind != "" && (mirrorState || kind == "attention") {
			candidate := linearAction{IssueID: card.LinearIssueUUID, StateName: stateName, Kind: kind}
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
			present := false
			for _, a := range runtime.Outbox {
				if actionKey(a) == actionKey(candidate) {
					present = true
					break
				}
			}
			if !present {
				runtime.Outbox = append(runtime.Outbox, candidate)
			}
		}
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
		if commentNeeded {
			present := false
			for _, a := range runtime.Outbox {
				if actionKey(a) == actionKey(comment) {
					present = true
					break
				}
			}
			if !present {
				runtime.Outbox = append(runtime.Outbox, comment)
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
	if !uuidPattern.MatchString(opID) {
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
	priority, ok := raw["priority"].(float64)
	if !ok || priority < 1 || priority > 4 || priority != float64(int(priority)) {
		return nil, E(2, "priority must be 1..4 (Urgent, High, Medium or Low)")
	}
	workType, _ := raw["work_type"].(string)
	if workType != "feature" && workType != "bug" && workType != "maintenance" {
		return nil, E(2, "work_type must be feature, bug or maintenance")
	}
	projectID, _ := raw["linear_project_id"].(string)
	projectName, _ := raw["linear_project"].(string)
	if projectID == "" || projectName == "" {
		return nil, E(2, "linear_project_id and linear_project are required")
	}
	parentID, _ := raw["linear_parent_id"].(string)
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
	for _, k := range []string{"status", "hot", "attempts", "max_attempts", "rework_count", "max_rework", "conflict_skips", "claimed_at", "claimed_by", "worktree", "branch", "pr", "base_sha", "tested_head_sha", "stale", "spec_changed", "qa_findings", "qa_evidence", "qa_acceptance_results", "proposed", "history"} {
		delete(raw, k)
	}
	cardBlock, _ := json.MarshalIndent(raw, "", "  ")
	problem, _ := raw["problem"].(string)
	outcome, _ := raw["desired_outcome"].(string)
	acceptance, _ := raw["acceptance"].([]any)
	description := fmt.Sprintf("## Problem\n%s\n\n## Desired outcome\n%s\n\n## Acceptance criteria\n%s%s\n\n```loop-card\n%s\n```", safeHumanMarkdown(problem), safeHumanMarkdown(outcome), markdownChecklist(acceptance), markdownVisuals(raw["visuals"]), cardBlock)
	client, err := s.linearClient()
	if err != nil {
		return nil, err
	}
	if parentID != "" {
		if _, ok, err := client.findIssue(ctx, parentID); err != nil {
			return nil, E(8, "Linear parent lookup failed: %v", err)
		} else if !ok {
			return nil, E(2, "Linear parent %s does not exist", parentID)
		}
	}
	readyLabelID := ""
	if existing, ok, e := client.findIssue(ctx, opID); e == nil && ok {
		if err := s.ensureGroomReadyLabel(ctx, client, existing, &readyLabelID); err != nil {
			return nil, err
		}
		labels := stringSlice(existing["labels"])
		if !containsString(labels, "type:"+workType) {
			labels = append(labels, "type:"+workType)
		}
		existing["labels"] = linearLabels(labels)
		return existing, nil
	}
	projects, err := s.LinearProjects(ctx)
	if err != nil {
		return nil, err
	}
	projectValid := false
	for _, project := range projects {
		if project.ID == projectID && project.Name == projectName {
			projectValid = true
			break
		}
	}
	if !projectValid {
		return nil, E(2, "Linear project %q (%s) is not available to team %s", projectName, projectID, s.Config.Linear.Team)
	}
	backlogID, err := s.linearStateID(ctx, client, s.Config.Linear.StatusMap["backlog"])
	if err != nil {
		return nil, E(8, "Linear backlog lookup failed: %v", err)
	}
	readyLabelID, err = s.linearLabelID(ctx, client, s.Config.Linear.ReadyLabel)
	if err != nil {
		return nil, E(8, "Linear label lookup failed: %v", err)
	}
	typeLabelID, err := s.linearLabelID(ctx, client, "type:"+workType)
	if err != nil {
		return nil, E(8, "Linear work type label lookup failed: %v", err)
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
	input := map[string]any{"id": opID, "teamId": s.Config.Linear.TeamID, "title": title, "description": description, "stateId": backlogID, "labelIds": []string{readyLabelID, typeLabelID}, "projectId": projectID, "priority": int(priority)}
	if parentID != "" {
		input["parentId"] = parentID
	}
	if err := client.graphql(ctx, q, map[string]any{"input": input}, &response); err != nil {
		if existing, ok, _ := client.findIssue(ctx, opID); ok {
			if readyErr := s.ensureGroomReadyLabel(ctx, client, existing, &readyLabelID); readyErr != nil {
				return nil, readyErr
			}
			labels := stringSlice(existing["labels"])
			if !containsString(labels, "type:"+workType) {
				labels = append(labels, "type:"+workType)
			}
			existing["labels"] = linearLabels(labels)
			return existing, nil
		}
		return nil, E(8, "Linear create failed: %v", err)
	}
	if !response.IssueCreate.Success {
		return nil, E(8, "Linear create failed")
	}
	i := response.IssueCreate.Issue
	return map[string]any{"id": i.ID, "identifier": i.Identifier, "title": i.Title, "url": i.URL, "status": i.State.Name, "labels": []string{s.Config.Linear.ReadyLabel, "type:" + workType}, "project": projectName, "priority": int(priority)}, nil
}

func markdownChecklist(values []any) string {
	lines := make([]string, 0, len(values))
	for _, value := range values {
		item := safeHumanMarkdown(strings.Join(strings.Fields(fmt.Sprint(value)), " "))
		lines = append(lines, "- [ ] "+item)
	}
	return strings.Join(lines, "\n")
}

func markdownVisuals(value any) string {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	lines := []string{"", "", "## Visual references"}
	for _, item := range items {
		visual, ok := item.(map[string]any)
		if !ok {
			continue
		}
		alt := safeHumanMarkdown(strings.Join(strings.Fields(fmt.Sprint(visual["alt"])), " "))
		url := strings.TrimSpace(fmt.Sprint(visual["url"]))
		description := safeHumanMarkdown(strings.Join(strings.Fields(fmt.Sprint(visual["description"])), " "))
		alt = strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]").Replace(alt)
		lines = append(lines, fmt.Sprintf("![%s](<%s>)", alt, url), "", description)
	}
	return strings.Join(lines, "\n")
}

func safeHumanMarkdown(value string) string {
	return strings.ReplaceAll(value, "```loop-card", "``` loop-card")
}

type LinearProjectOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *State) LinearProjects(ctx context.Context) ([]LinearProjectOption, error) {
	client, err := s.linearClient()
	if err != nil {
		return nil, err
	}
	const q = `query TeamProjects($team: String!, $after: String) { team(id: $team) { projects(first: 100, after: $after) { nodes { id name } pageInfo { hasNextPage endCursor } } } }`
	projects := []LinearProjectOption{}
	var after any
	for {
		var data struct {
			Team struct {
				Projects struct {
					Nodes    []LinearProjectOption
					PageInfo struct {
						HasNextPage bool
						EndCursor   string
					}
				}
			}
		}
		if err := client.graphql(ctx, q, map[string]any{"team": s.Config.Linear.TeamID, "after": after}, &data); err != nil {
			return nil, E(8, "Linear project lookup failed: %v", err)
		}
		projects = append(projects, data.Team.Projects.Nodes...)
		if !data.Team.Projects.PageInfo.HasNextPage {
			break
		}
		if data.Team.Projects.PageInfo.EndCursor == "" {
			return nil, E(8, "Linear project pagination cursor missing")
		}
		after = data.Team.Projects.PageInfo.EndCursor
	}
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].Name != projects[j].Name {
			return projects[i].Name < projects[j].Name
		}
		return projects[i].ID < projects[j].ID
	})
	return projects, nil
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
