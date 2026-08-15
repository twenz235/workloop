package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	loopglob "github.com/twenz235/workloop/internal/glob"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLinearSyncImportsReadyIssueIdempotently(t *testing.T) {
	s := testState(t)
	token := "linear-test-secret"
	t.Setenv("LINEAR_API_TOKEN", token)
	var mu sync.Mutex
	updates := 0
	issueState := "Backlog"
	oldClient := defaultLinearHTTPClient
	defaultLinearHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != token {
			t.Errorf("bad auth")
		}
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		q := string(b)
		var response string
		switch {
		case strings.Contains(q, "TeamStates"):
			response = `{"data":{"team":{"states":{"nodes":[{"id":"todo-id","name":"Todo"}]}}}}`
		case strings.Contains(q, "MoveLoopIssue"):
			mu.Lock()
			updates++
			issueState = "Todo"
			mu.Unlock()
			response = `{"data":{"issueUpdate":{"success":true}}}`
		default:
			mu.Lock()
			state := issueState
			mu.Unlock()
			desc := fmt.Sprintf("Summary\n```loop-card\n{\"problem\":\"p\",\"desired_outcome\":\"o\",\"out_of_scope\":[\"x\"],\"repo\":%q,\"repo_path\":%q,\"base\":\"dev\",\"tier\":\"L1\",\"touches\":[\"a.go\"],\"acceptance\":[\"works\"],\"verification\":[\"go test ./...\"],\"depends_on\":[],\"risk\":{\"level\":\"low\"},\"rollback_notes\":\"revert\",\"approved_at\":\"now\",\"approved_by\":\"u\"}\n```", s.Config.Repo, s.Config.RepoPath)
			response = fmt.Sprintf(`{"data":{"team":{"issues":{"nodes":[{"id":"uuid-1","identifier":"FLO-1","title":"Issue","description":%q,"url":"https://linear.test/1","updatedAt":"2026-01-01T00:00:00Z","priority":1,"state":{"name":%q},"project":{"id":"project-1","name":"Acme"},"labels":{"nodes":[{"name":"loop:ready"},{"name":"type:feature"}]}}]}}}}`, desc, state)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
	})}
	t.Cleanup(func() { defaultLinearHTTPClient = oldClient })
	s.Config.Linear.Endpoint = "https://linear.test/graphql"
	b, _ := Encode(s.Config)
	if err := writeAtomic(s.Root+"/config.json", b, 0600); err != nil {
		t.Fatal(err)
	}
	first, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first["imported"].([]string)) != 1 {
		t.Fatalf("first=%v", first)
	}
	second, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(second["imported"].([]string)) != 0 {
		t.Fatalf("second=%v", second)
	}
	mu.Lock()
	defer mu.Unlock()
	if updates != 1 {
		t.Fatalf("updates=%d", updates)
	}
	if status, _, err := s.Locate("flo-1"); err != nil || status != "todo" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	_, _, card, err := s.ReadCard("flo-1")
	if err != nil || card.LinearState != "Todo" {
		t.Fatalf("linear state=%q err=%v", card.LinearState, err)
	}
	for _, path := range []string{s.Root + "/config.json", s.Root + "/runtime/linear.json"} {
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), token) {
			t.Fatalf("secret leaked to %s", path)
		}
	}
}

func TestLinearFailureReturnsEight(t *testing.T) {
	s := testState(t)
	t.Setenv("LINEAR_API_TOKEN", "x")
	oldClient := defaultLinearHTTPClient
	defaultLinearHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("offline") })}
	t.Cleanup(func() { defaultLinearHTTPClient = oldClient })
	s.Config.Linear.Endpoint = "https://linear.test/graphql"
	_, err := s.Sync(context.Background())
	if ExitCode(err) != 8 {
		t.Fatalf("exit=%d err=%v", ExitCode(err), err)
	}
}

func TestClaimAndSyncFlushesLinearStateImmediately(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "claim-sync", []string{"claim.go"})
	t.Setenv("LINEAR_API_TOKEN", "x")
	s.Config.Linear.Endpoint = "https://linear.test/graphql"
	// Seed the Linear snapshot without generating a remote mutation; the claim
	// below is the only state write this test should observe.
	if _, err := s.PatchInternal("claim-sync", map[string]any{"linear_state": "Todo", "linear_labels": []string{"loop:ready", "type:feature"}}, "Linear snapshot"); err != nil {
		t.Fatal(err)
	}
	s.Config.Linear.Enabled = true
	if err := s.updateLinearRuntime(func(runtime *linearRuntime) { runtime.LastSyncAt = Now() }); err != nil {
		t.Fatal(err)
	}
	updates := 0
	oldClient := defaultLinearHTTPClient
	defaultLinearHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		response := `{"data":{"issueUpdate":{"success":true}}}`
		if strings.Contains(string(body), "TeamStates") {
			response = `{"data":{"team":{"states":{"nodes":[{"id":"progress-id","name":"In Progress"}]}}}}`
		} else if strings.Contains(string(body), "MoveLoopIssue") {
			updates++
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { defaultLinearHTTPClient = oldClient })

	if _, err := s.ClaimAndSync(context.Background(), "dev", "worker"); err != nil {
		t.Fatal(err)
	}
	if updates != 1 {
		t.Fatalf("Linear updates=%d, want 1", updates)
	}
	runtime, err := s.loadLinearRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.Outbox) != 0 {
		t.Fatalf("outbox=%v, want empty", runtime.Outbox)
	}
	_, _, card, err := s.ReadCard("claim-sync")
	if err != nil || card.LinearState != "In Progress" {
		t.Fatalf("linear snapshot=%+v err=%v", card, err)
	}
}

func TestClaimAndSyncKeepsFailedLinearStateQueued(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "claim-retry", []string{"retry.go"})
	t.Setenv("LINEAR_API_TOKEN", "x")
	s.Config.Linear.Endpoint = "https://linear.test/graphql"
	if _, err := s.PatchInternal("claim-retry", map[string]any{"linear_state": "Todo", "linear_labels": []string{"loop:ready", "type:feature"}}, "Linear snapshot"); err != nil {
		t.Fatal(err)
	}
	s.Config.Linear.Enabled = true
	if err := s.updateLinearRuntime(func(runtime *linearRuntime) { runtime.LastSyncAt = Now() }); err != nil {
		t.Fatal(err)
	}
	oldClient := defaultLinearHTTPClient
	defaultLinearHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("offline")
	})}
	t.Cleanup(func() { defaultLinearHTTPClient = oldClient })

	if _, err := s.ClaimAndSync(context.Background(), "dev", "worker"); err != nil {
		t.Fatalf("local claim should survive Linear outage: %v", err)
	}
	if status, _, err := s.Locate("claim-retry"); err != nil || status != "claimed-dev" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	runtime, err := s.loadLinearRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.Outbox) != 1 || runtime.Outbox[0].StateName != "In Progress" {
		t.Fatalf("outbox=%v, want queued In Progress", runtime.Outbox)
	}
}

func TestLinearSyncRehydratesAfterNeedsAttentionLabelRemoved(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "attention", []string{"src/attention.go"})
	if _, err := s.withMoveInternal("attention", "needs_attention", "system/test", "QA failed", nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINEAR_API_TOKEN", "x")
	s.Config.Linear.Endpoint = "https://linear.test/graphql"
	oldClient := defaultLinearHTTPClient
	defaultLinearHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		query := string(body)
		var response string
		switch {
		case strings.Contains(query, "TeamStates"):
			response = `{"data":{"team":{"states":{"nodes":[{"id":"todo-id","name":"Todo"}]}}}}`
		default:
			var cardRaw map[string]any
			if err := json.Unmarshal(testCard("attention", []string{"src/attention.go"}), &cardRaw); err != nil {
				t.Fatal(err)
			}
			cardRaw["repo"] = s.Config.Repo
			cardRaw["repo_path"] = s.Config.RepoPath
			cardBytes, _ := json.Marshal(cardRaw)
			card := string(cardBytes)
			description := fmt.Sprintf("```loop-card\n%s\n```", card)
			response = fmt.Sprintf(`{"data":{"team":{"issues":{"nodes":[{"id":"uuid-attention","identifier":"FLO-ATTENTION","title":"Attention","description":%q,"url":"https://linear.test/attention","updatedAt":"2026-08-14T00:00:00Z","priority":2,"state":{"name":"In Review"},"project":{"id":"project-1","name":"Acme"},"labels":{"nodes":[{"name":"loop:ready"},{"name":"type:feature"}]}}]}}}}`, description)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { defaultLinearHTTPClient = oldClient })
	result, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result["pending"] != 0 || len(result["needs_attention"].([]string)) != 0 {
		t.Fatalf("result=%v", result)
	}
	runtime, err := s.loadLinearRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.Outbox) != 0 {
		t.Fatalf("outbox=%v", runtime.Outbox)
	}
	status, _, _, card, err := s.readCardPath("attention")
	if err != nil || status != "in_review" || card.LinearState != "In Review" {
		t.Fatalf("card=%+v err=%v", card, err)
	}
}

func TestLinearNeedsAttentionQueuesDiagnosticCommentWithoutEchoingSyncState(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "linear-comment", []string{"src/comment.go"})
	s.Config.Linear.Enabled = true
	s.Config.Linear.Endpoint = "https://linear.test/graphql"
	s.Config.Linear.TeamID = "team-1"
	t.Setenv("LINEAR_API_TOKEN", "x")
	if _, err := s.withMoveInternal("linear-comment", "claimed-dev", "system/sync", "Linear claim snapshot", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.updateLinearRuntime(func(runtime *linearRuntime) { runtime.Outbox = nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.withMoveInternal("linear-comment", "needs_attention", "system/sync", "Linear contract changed after claim", map[string]any{"spec_changed": true}); err != nil {
		t.Fatal(err)
	}
	runtime, err := s.loadLinearRuntime()
	if err != nil {
		t.Fatal(err)
	}
	if len(runtime.Outbox) != 2 || runtime.Outbox[0].Kind != "attention" || runtime.Outbox[1].Kind != "comment" {
		t.Fatalf("outbox=%+v", runtime.Outbox)
	}

	var commentBody string
	oldClient := defaultLinearHTTPClient
	defaultLinearHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			return nil, err
		}
		switch {
		case strings.Contains(request.Query, "TeamLabels"):
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"team":{"labels":{"nodes":[{"id":"attention-id","name":"loop:needs-attention"}]}}}}`)), Header: http.Header{}}, nil
		case strings.Contains(request.Query, "AddLoopLabel"):
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"issueAddLabel":{"success":true}}}`)), Header: http.Header{}}, nil
		case strings.Contains(request.Query, "LoopIssueComments"):
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"issue":{"comments":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`)), Header: http.Header{}}, nil
		case strings.Contains(request.Query, "AddLoopComment"):
			if input, ok := request.Variables["input"].(map[string]any); ok {
				commentBody, _ = input["body"].(string)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"commentCreate":{"success":true,"comment":{"id":"comment-1"}}}}`)), Header: http.Header{}}, nil
		default:
			return nil, fmt.Errorf("unexpected Linear request: %s", request.Query)
		}
	})}
	t.Cleanup(func() { defaultLinearHTTPClient = oldClient })
	if pending, err := s.FlushLinearOutbox(context.Background()); err != nil || pending != 0 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
	if !strings.Contains(commentBody, "Linear contract changed after claim") ||
		!strings.Contains(commentBody, "**Why:**") ||
		!strings.Contains(commentBody, "**Needed:**") ||
		!strings.Contains(commentBody, "**Fix:**") ||
		!strings.Contains(commentBody, "**Recommendation:**") {
		t.Fatalf("comment=%q", commentBody)
	}
	_, _, card, err := s.ReadCard("linear-comment")
	if err != nil || !containsString(card.LinearLabels, "loop:needs-attention") {
		t.Fatalf("card=%+v err=%v", card, err)
	}
}

func TestClaimFailsClosedWithoutFreshLinearSnapshot(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "stale-snapshot", []string{"stale.go"})
	s.Config.Linear.Enabled = true
	s.Config.Linear.Endpoint = "https://linear.test/graphql"
	if _, err := s.Claim("dev", "worker"); ExitCode(err) != 8 {
		t.Fatalf("exit=%d err=%v, want fail-closed Linear snapshot", ExitCode(err), err)
	}
}

func TestSupervisorDoesNotStopOnLinearOutage(t *testing.T) {
	s := testState(t)
	s.Config.Linear.Enabled = true
	s.Config.Linear.Endpoint = "https://linear.test/graphql"
	t.Setenv("LINEAR_API_TOKEN", "x")
	oldClient := defaultLinearHTTPClient
	defaultLinearHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("offline")
	})}
	t.Cleanup(func() { defaultLinearHTTPClient = oldClient })
	if err := s.RunSupervisor(context.Background(), "unused", true); err != nil {
		t.Fatalf("supervisor returned outage as fatal error: %v", err)
	}
}

func TestLinearReconcileDoesNotEchoLocalStateToBoard(t *testing.T) {
	s := testState(t)
	addTestCard(t, s, "reconcile-linear", []string{"reconcile.go"})
	s.Config.Linear.Enabled = true
	s.Config.Linear.Endpoint = "https://linear.test/graphql"
	if _, err := s.PatchInternal("reconcile-linear", map[string]any{
		"linear_state":  "In Progress",
		"linear_labels": []string{"loop:ready", "type:feature"},
	}, "Linear snapshot"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := s.withMoveInternal("reconcile-linear", "claimed-dev", "system/test", "claim", map[string]any{
		"claimed_at": &old,
		"claimed_by": "worker",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.updateLinearRuntime(func(runtime *linearRuntime) {
		runtime.Outbox = nil
		runtime.LastSyncAt = Now()
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reconcile("dev"); err != nil {
		t.Fatal(err)
	}
	status, _, card, err := s.ReadCard("reconcile-linear")
	if err != nil || status != "rework" || card.LinearState != "In Progress" {
		t.Fatalf("status=%q card=%+v err=%v", status, card, err)
	}
	runtime, err := s.loadLinearRuntime()
	if err != nil || len(runtime.Outbox) != 1 || runtime.Outbox[0].Kind != "comment" {
		t.Fatalf("runtime=%+v err=%v; reconcile must not echo a state mutation but should explain the recovery", runtime, err)
	}
}

func TestLinearSyncRefreshesParentFromLinearRelationship(t *testing.T) {
	s := testState(t)
	t.Setenv("LINEAR_API_TOKEN", "x")
	oldClient := defaultLinearHTTPClient
	parentID := "11111111-1111-4111-8111-111111111111"
	defaultLinearHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		query := string(body)
		response := ""
		switch {
		case strings.Contains(query, "TeamStates"):
			response = `{"data":{"team":{"states":{"nodes":[{"id":"todo-id","name":"Todo"}]}}}}`
		case strings.Contains(query, "MoveLoopIssue"):
			response = `{"data":{"issueUpdate":{"success":true}}}`
		default:
			desc := fmt.Sprintf("```loop-card\n{\"problem\":\"p\",\"desired_outcome\":\"o\",\"out_of_scope\":[\"x\"],\"repo\":%q,\"repo_path\":%q,\"base\":\"dev\",\"tier\":\"L1\",\"touches\":[\"a.go\"],\"acceptance\":[\"works\"],\"verification\":[\"go test ./...\"],\"depends_on\":[],\"linear_parent_id\":\"99999999-9999-4999-8999-999999999999\",\"risk\":{\"level\":\"low\"},\"rollback_notes\":\"revert\",\"approved_at\":\"now\",\"approved_by\":\"u\"}\n```", s.Config.Repo, s.Config.RepoPath)
			response = fmt.Sprintf(`{"data":{"team":{"issues":{"nodes":[{"id":"uuid-1","identifier":"FLO-1","title":"Issue","description":%q,"url":"https://linear.test/1","updatedAt":"2026-01-01T00:00:00Z","priority":1,"state":{"name":"Backlog"},"project":{"id":"project-1","name":"Acme"},"parent":{"id":%q},"labels":{"nodes":[{"name":"loop:ready"},{"name":"type:feature"}]}}]}}}}`, desc, parentID)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { defaultLinearHTTPClient = oldClient })

	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, _, card, err := s.readCardPath("flo-1")
	if err != nil || card.LinearParentID != parentID {
		t.Fatalf("first parent=%q err=%v", card.LinearParentID, err)
	}
	parentID = "22222222-2222-4222-8222-222222222222"
	result, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, card, err = s.readCardPath("flo-1")
	if err != nil || card.LinearParentID != parentID {
		t.Fatalf("moved parent=%q err=%v", card.LinearParentID, err)
	}
	if got := result["updated"].([]string); len(got) != 1 || got[0] != "FLO-1" {
		t.Fatalf("updated=%v", got)
	}
	if _, err := s.withMoveInternal("flo-1", "cancelled", "system/test", "test terminal metadata sync", nil); err != nil {
		t.Fatal(err)
	}
	parentID = "33333333-3333-4333-8333-333333333333"
	result, err = s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, card, err = s.readCardPath("flo-1")
	if err != nil || card.LinearParentID != parentID || card.Status != "todo" {
		t.Fatalf("terminal parent=%q status=%q err=%v", card.LinearParentID, card.Status, err)
	}
	if got := result["updated"].([]string); len(got) != 1 || got[0] != "FLO-1" {
		t.Fatalf("terminal updated=%v", got)
	}
}

func TestGroomCreateRequiresApprovalAndCreatesReadyBacklog(t *testing.T) {
	s := testState(t)
	t.Setenv("LINEAR_API_TOKEN", "x")
	old := defaultLinearHTTPClient
	var createBody string
	defaultLinearHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		q := string(b)
		response := ""
		switch {
		case strings.Contains(q, "TeamStates"):
			response = `{"data":{"team":{"states":{"nodes":[{"id":"backlog-id","name":"Backlog"}]}}}}`
		case strings.Contains(q, "TeamLabels"):
			response = `{"data":{"team":{"labels":{"nodes":[{"id":"ready-id","name":"loop:ready"},{"id":"feature-id","name":"type:feature"}]}}}}`
		case strings.Contains(q, "TeamProjects"):
			response = `{"data":{"team":{"projects":{"nodes":[{"id":"project-1","name":"Acme"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`
		default:
			createBody = q
			response = `{"data":{"issueCreate":{"success":true,"issue":{"id":"u1","identifier":"FLO-9","title":"Ready card","url":"https://linear.test/FLO-9","updatedAt":"now","state":{"name":"Backlog"}}}}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { defaultLinearHTTPClient = old })
	input := fmt.Sprintf(`{"title":"Ready card","problem":"p","desired_outcome":"o","out_of_scope":["x"],"repo":%q,"repo_path":%q,"base":"dev","tier":"L1","touches":["a"],"acceptance":["works"],"verification":["test"],"depends_on":[],"priority":1,"work_type":"feature","linear_project_id":"project-1","linear_project":"Acme","visuals":[{"alt":"Pricing flow","url":"https://example.com/pricing.png","description":"Current pricing flow"}],"risk":{"level":"low"},"rollback_notes":"revert"}`, s.Config.Repo, s.Config.RepoPath)
	if _, err := s.GroomCreate(context.Background(), []byte(input), ""); ExitCode(err) != 2 {
		t.Fatalf("approval exit=%d err=%v", ExitCode(err), err)
	}
	result, err := s.GroomCreate(context.Background(), []byte(input), "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "Backlog" || result["project"] != "Acme" || result["priority"] != 1 {
		t.Fatalf("result=%v", result)
	}
	if !strings.Contains(createBody, "ready-id") || !strings.Contains(createBody, "feature-id") || !strings.Contains(createBody, `"projectId":"project-1"`) || !strings.Contains(createBody, `"priority":1`) || !strings.Contains(createBody, "- [ ] works") || !strings.Contains(createBody, "pricing.png") || !strings.Contains(createBody, "loop-card") {
		t.Fatalf("create body missing contract: %s", createBody)
	}
}

func TestWorkTypeRequiresExactlyOneLabel(t *testing.T) {
	if _, err := workTypeFromLabels([]string{"loop:ready"}); ExitCode(err) != 2 {
		t.Fatalf("missing type exit=%d err=%v", ExitCode(err), err)
	}
	if _, err := workTypeFromLabels([]string{"type:feature", "type:bug"}); ExitCode(err) != 2 {
		t.Fatalf("multiple types exit=%d err=%v", ExitCode(err), err)
	}
	if got, err := workTypeFromLabels([]string{"loop:ready", "type:maintenance"}); err != nil || got != "maintenance" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestHumanMarkdownCannotInjectLoopCardFence(t *testing.T) {
	input := "before ```loop-card\n{\"problem\":\"injected\"}\n``` after"
	if output := safeHumanMarkdown(input); strings.Contains(output, "```loop-card") {
		t.Fatalf("unsafe markdown=%q", output)
	}
}

func TestGroomPlanCreatesParentAndOrderedSubIssues(t *testing.T) {
	s := testState(t)
	t.Setenv("LINEAR_API_TOKEN", "x")
	old := defaultLinearHTTPClient
	creates := []map[string]any{}
	created := map[string]map[string]any{}
	failedAPIOnce := false
	defaultLinearHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &request)
		response := ""
		switch {
		case strings.Contains(request.Query, "GroomOperation"):
			id := fmt.Sprint(request.Variables["id"])
			if existing := created[id]; existing != nil {
				response = fmt.Sprintf(`{"data":{"issue":{"id":%q,"identifier":"FLO-X","title":%q,"url":"https://linear.test/existing","state":{"name":"Backlog"}}}}`, id, existing["title"])
			} else {
				response = `{"data":{"issue":null}}`
			}
		case strings.Contains(request.Query, "TeamProjects"):
			response = `{"data":{"team":{"projects":{"nodes":[{"id":"project-1","name":"Acme"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`
		case strings.Contains(request.Query, "TeamLabels"):
			response = `{"data":{"team":{"labels":{"nodes":[{"id":"ready-id","name":"loop:ready"},{"id":"feature-id","name":"type:feature"}]}}}}`
		case strings.Contains(request.Query, "TeamStates"):
			response = `{"data":{"team":{"states":{"nodes":[{"id":"backlog-id","name":"Backlog"}]}}}}`
		case strings.Contains(request.Query, "CreateGroomParent"), strings.Contains(request.Query, "CreateGroomedIssue"):
			input, _ := request.Variables["input"].(map[string]any)
			if input["title"] == "Build API" && !failedAPIOnce {
				failedAPIOnce = true
				response = `{"errors":[{"message":"temporary failure"}]}`
				break
			}
			creates = append(creates, input)
			id := fmt.Sprint(input["id"])
			created[id] = input
			title := fmt.Sprint(input["title"])
			response = fmt.Sprintf(`{"data":{"issueCreate":{"success":true,"issue":{"id":%q,"identifier":"FLO-%d","title":%q,"url":"https://linear.test/%d","updatedAt":"now","state":{"name":"Backlog"}}}}}`, id, len(creates), title, len(creates))
		default:
			t.Fatalf("unexpected query: %s", request.Query)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { defaultLinearHTTPClient = old })
	card := func(key, title string, dependencies []string) map[string]any {
		return map[string]any{
			"key": key, "title": title, "problem": "p", "desired_outcome": "o", "out_of_scope": []string{"x"},
			"tier": "L1", "touches": []string{key + ".go"}, "acceptance": []string{"works"}, "verification": []string{"go test ./..."},
			"depends_on": []string{}, "depends_on_keys": dependencies, "risk": map[string]any{"level": "low"}, "rollback_notes": "revert",
		}
	}
	plan := map[string]any{
		"operation_id": "00000000-0000-4000-8000-000000000099",
		"parent": map[string]any{
			"title": "Large feature", "problem": "p", "desired_outcome": "o", "acceptance": []string{"all sub-issues complete"},
			"work_type": "feature", "linear_project_id": "project-1", "linear_project": "Acme", "priority": 2,
		},
		"cards": []map[string]any{card("api", "Build API", []string{"schema"}), card("schema", "Add schema", nil)},
	}
	b, _ := json.Marshal(plan)
	partial, err := s.GroomPlanCreate(context.Background(), b, "user-1")
	if err == nil || partial["complete"] != false || partial["failed_key"] != "api" || len(creates) != 2 {
		t.Fatalf("partial=%v err=%v creates=%v", partial, err, creates)
	}
	result, err := s.GroomPlanCreate(context.Background(), b, "user-1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if result["complete"] != true || len(creates) != 3 {
		t.Fatalf("result=%v creates=%v", result, creates)
	}
	if fmt.Sprint(result["execution_waves"]) != "[[schema] [api]]" {
		t.Fatalf("waves=%v", result["execution_waves"])
	}
	parentID := fmt.Sprint(creates[0]["id"])
	if strings.Contains(fmt.Sprint(creates[0]["labelIds"]), "ready-id") {
		t.Fatalf("parent must not be ready: %v", creates[0])
	}
	if creates[1]["title"] != "Add schema" || creates[2]["title"] != "Build API" {
		t.Fatalf("wrong creation order: %v", creates)
	}
	if creates[1]["parentId"] != parentID || creates[2]["parentId"] != parentID {
		t.Fatalf("sub-issues missing parent: %v", creates)
	}
	schemaID := fmt.Sprint(creates[1]["id"])
	if !strings.Contains(fmt.Sprint(creates[2]["description"]), schemaID) || !strings.Contains(fmt.Sprint(creates[2]["labelIds"]), "ready-id") {
		t.Fatalf("dependent card is incomplete: %v", creates[2])
	}
}

func TestPlanExecutionWavesOnlyGroupConflictFreeCards(t *testing.T) {
	cards := map[string]map[string]any{
		"api-a":    {"touches": []string{"apps/api/a.go"}, "depends_on_keys": []string{}},
		"api-b":    {"touches": []string{"apps/api/a.go"}, "depends_on_keys": []string{}},
		"web":      {"touches": []string{"apps/web/page.tsx"}, "depends_on_keys": []string{}},
		"hot":      {"touches": []string{"package.json"}, "depends_on_keys": []string{}},
		"api-test": {"touches": []string{"apps/api/a_test.go"}, "depends_on_keys": []string{"api-a"}},
	}
	waves := planExecutionWaves(
		[]string{"api-a", "api-b", "web", "hot", "api-test"},
		cards,
		[]string{"package.json", "pnpm-lock.yaml"},
	)
	if got := fmt.Sprint(waves); got != "[[api-a web] [api-b api-test] [hot]]" {
		t.Fatalf("waves=%s", got)
	}
	for _, wave := range waves {
		for i, left := range wave {
			leftTouches := stringSlice(cards[left]["touches"])
			leftHot := loopglob.PatternsOverlap(leftTouches, []string{"package.json", "pnpm-lock.yaml"})
			for _, right := range wave[i+1:] {
				rightTouches := stringSlice(cards[right]["touches"])
				if leftHot || loopglob.PatternsOverlap(rightTouches, []string{"package.json", "pnpm-lock.yaml"}) || loopglob.PatternsOverlap(leftTouches, rightTouches) {
					t.Fatalf("conflicting cards share wave: %s and %s", left, right)
				}
			}
		}
	}
}

func TestGroomPlanReusesExistingParentWithoutCreatingDuplicate(t *testing.T) {
	s := testState(t)
	t.Setenv("LINEAR_API_TOKEN", "x")
	old := defaultLinearHTTPClient
	parentID := "11111111-1111-4111-8111-111111111111"
	created := map[string]map[string]any{}
	creates := []map[string]any{}
	parentReady := false
	defaultLinearHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &request)
		response := ""
		switch {
		case strings.Contains(request.Query, "ReusableGroomParent"):
			labels := `[{"name":"type:feature"}]`
			if parentReady {
				labels = `[{"name":"type:feature"},{"name":"loop:ready"}]`
			}
			response = fmt.Sprintf(`{"data":{"issue":{"id":%q,"identifier":"FLO-19","title":"Existing parent","url":"https://linear.test/FLO-19","archivedAt":null,"state":{"name":"Backlog"},"team":{"id":%q},"project":{"id":"project-1"},"labels":{"nodes":%s}}}}`, parentID, s.Config.Linear.TeamID, labels)
		case strings.Contains(request.Query, "GroomOperation"):
			id := fmt.Sprint(request.Variables["id"])
			if id == parentID {
				response = fmt.Sprintf(`{"data":{"issue":{"id":%q,"identifier":"FLO-19","title":"Existing parent","url":"https://linear.test/FLO-19","state":{"name":"Backlog"}}}}`, parentID)
			} else if existing := created[id]; existing != nil {
				response = fmt.Sprintf(`{"data":{"issue":{"id":%q,"identifier":"FLO-X","title":%q,"url":"https://linear.test/existing","state":{"name":"Backlog"}}}}`, id, existing["title"])
			} else {
				response = `{"data":{"issue":null}}`
			}
		case strings.Contains(request.Query, "TeamProjects"):
			response = `{"data":{"team":{"projects":{"nodes":[{"id":"project-1","name":"Acme"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`
		case strings.Contains(request.Query, "TeamLabels"):
			response = `{"data":{"team":{"labels":{"nodes":[{"id":"ready-id","name":"loop:ready"},{"id":"feature-id","name":"type:feature"}]}}}}`
		case strings.Contains(request.Query, "TeamStates"):
			response = `{"data":{"team":{"states":{"nodes":[{"id":"backlog-id","name":"Backlog"}]}}}}`
		case strings.Contains(request.Query, "CreateGroomParent"):
			t.Fatal("reuse mode must not create a parent")
		case strings.Contains(request.Query, "CreateGroomedIssue"):
			input := request.Variables["input"].(map[string]any)
			creates = append(creates, input)
			created[fmt.Sprint(input["id"])] = input
			response = fmt.Sprintf(`{"data":{"issueCreate":{"success":true,"issue":{"id":%q,"identifier":"FLO-%d","title":%q,"url":"https://linear.test/%d","updatedAt":"now","state":{"name":"Backlog"}}}}}`, input["id"], len(creates)+19, input["title"], len(creates)+19)
		default:
			t.Fatalf("unexpected query: %s", request.Query)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { defaultLinearHTTPClient = old })

	card := func(key string) map[string]any {
		return map[string]any{"key": key, "title": key, "problem": "p", "desired_outcome": "o", "out_of_scope": []string{"x"}, "tier": "L1", "touches": []string{key + ".go"}, "acceptance": []string{"works"}, "verification": []string{"test"}, "depends_on": []string{}, "depends_on_keys": []string{}, "risk": map[string]any{"level": "low"}, "rollback_notes": "revert"}
	}
	plan := map[string]any{
		"operation_id": "00000000-0000-4000-8000-000000000099",
		"parent":       map[string]any{"mode": "reuse", "linear_issue_uuid": parentID, "linear_issue_id": "FLO-19", "title": "Existing parent", "problem": "p", "desired_outcome": "o", "acceptance": []string{"done"}, "work_type": "feature", "linear_project_id": "project-1", "linear_project": "Acme", "priority": 2},
		"cards":        []map[string]any{card("one"), card("two")},
	}
	b, _ := json.Marshal(plan)
	result, err := s.GroomPlanCreate(context.Background(), b, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(creates) != 2 || creates[0]["parentId"] != parentID || creates[1]["parentId"] != parentID {
		t.Fatalf("creates=%v", creates)
	}
	parent := result["parent"].(map[string]any)
	if parent["identifier"] != "FLO-19" || parent["reused"] != true {
		t.Fatalf("parent=%v", parent)
	}
	parentReady = true
	if _, err := s.GroomPlanCreate(context.Background(), b, "user-1"); ExitCode(err) != 2 || !strings.Contains(err.Error(), "must not have loop:ready") {
		t.Fatalf("ready parent exit=%d err=%v", ExitCode(err), err)
	}
	if len(creates) != 2 {
		t.Fatalf("preflight failure created more children: %v", creates)
	}
}

func TestGroomPlanRejectsDependencyCycleBeforeLinearWrites(t *testing.T) {
	s := testState(t)
	base := func(key, dependency string) map[string]any {
		return map[string]any{
			"key": key, "title": key, "problem": "p", "desired_outcome": "o", "out_of_scope": []string{"x"}, "tier": "L1",
			"touches": []string{key + ".go"}, "acceptance": []string{"works"}, "verification": []string{"test"}, "depends_on": []string{},
			"depends_on_keys": []string{dependency}, "risk": map[string]any{"level": "low"}, "rollback_notes": "revert",
		}
	}
	plan := map[string]any{
		"parent": map[string]any{"title": "p", "problem": "p", "desired_outcome": "o", "acceptance": []string{"done"}, "work_type": "feature", "linear_project_id": "p1", "linear_project": "P", "priority": 2},
		"cards":  []map[string]any{base("one", "two"), base("two", "one")},
	}
	b, _ := json.Marshal(plan)
	if _, err := s.GroomPlanCreate(context.Background(), b, "user-1"); ExitCode(err) != 2 || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("exit=%d err=%v", ExitCode(err), err)
	}
}

func TestGroomPlanRejectsAmbiguousLegacyParentID(t *testing.T) {
	parent := map[string]any{
		"linear_parent_id": "11111111-1111-4111-8111-111111111111",
		"title":            "p",
	}
	if _, err := normalizePlanParent(parent); ExitCode(err) != 2 || !strings.Contains(err.Error(), "parent.mode reuse") {
		t.Fatalf("exit=%d err=%v", ExitCode(err), err)
	}
}

func TestLinearIssuesAreOrderedByDependencies(t *testing.T) {
	issues := []linearIssue{
		{ID: "child", Description: "```loop-card\n{\"depends_on\":[\"parent\"]}\n```"},
		{ID: "independent", Description: ""},
		{ID: "parent", Description: "```loop-card\n{\"depends_on\":[]}\n```"},
	}
	ordered := orderLinearIssuesByDependencies(issues)
	positions := map[string]int{}
	for i, issue := range ordered {
		positions[issue.ID] = i
	}
	if positions["parent"] >= positions["child"] {
		t.Fatalf("dependency order=%v", ordered)
	}
}

func TestDiscoverLinearBindingSelectsOnlyTeam(t *testing.T) {
	t.Setenv("LINEAR_API_TOKEN", "x")
	old := defaultLinearHTTPClient
	defaultLinearHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{"data":{"organization":{"id":"workspace-1","name":"Acme"},"teams":{"nodes":[{"id":"team-1","name":"Engineering","key":"ENG"}]}}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { defaultLinearHTTPClient = old })
	binding, teams, err := DiscoverLinearBinding(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(teams) != 1 || binding.WorkspaceID != "workspace-1" || binding.Team != "ENG" || binding.TeamID != "team-1" {
		t.Fatalf("binding=%+v teams=%+v", binding, teams)
	}
}

func TestDiscoverLinearBindingRequiresChoiceForMultipleTeams(t *testing.T) {
	t.Setenv("LINEAR_API_TOKEN", "x")
	old := defaultLinearHTTPClient
	defaultLinearHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{"data":{"organization":{"id":"workspace-1","name":"Acme"},"teams":{"nodes":[{"id":"team-2","name":"Product","key":"PRD"},{"id":"team-1","name":"Engineering","key":"ENG"}]}}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { defaultLinearHTTPClient = old })
	if _, _, err := DiscoverLinearBinding(context.Background(), ""); ExitCode(err) != 2 || !strings.Contains(err.Error(), "ENG, PRD") {
		t.Fatalf("exit=%d err=%v", ExitCode(err), err)
	}
	binding, _, err := DiscoverLinearBinding(context.Background(), "prd")
	if err != nil || binding.TeamID != "team-2" {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}
}
