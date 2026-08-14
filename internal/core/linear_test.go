package core

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLinearSyncImportsReadyIssueIdempotently(t *testing.T) {
	s := testState(t)
	token := "linear-test-secret"
	t.Setenv("LINEAR_API_TOKEN", token)
	var mu sync.Mutex
	updates := 0
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
			mu.Unlock()
			response = `{"data":{"issueUpdate":{"success":true}}}`
		default:
			desc := fmt.Sprintf("Summary\n```loop-card\n{\"problem\":\"p\",\"desired_outcome\":\"o\",\"out_of_scope\":[\"x\"],\"repo\":%q,\"repo_path\":%q,\"base\":\"dev\",\"tier\":\"L1\",\"touches\":[\"a.go\"],\"acceptance\":[\"works\"],\"verification\":[\"go test ./...\"],\"depends_on\":[],\"risk\":{\"level\":\"low\"},\"rollback_notes\":\"revert\",\"approved_at\":\"now\",\"approved_by\":\"u\"}\n```", s.Config.Repo, s.Config.RepoPath)
			response = fmt.Sprintf(`{"data":{"team":{"issues":{"nodes":[{"id":"uuid-1","identifier":"FLO-1","title":"Issue","description":%q,"url":"https://linear.test/1","updatedAt":"2026-01-01T00:00:00Z","priority":1,"state":{"name":"Backlog"},"labels":{"nodes":[{"name":"loop:ready"}]}}]}}}}`, desc)
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
			response = `{"data":{"team":{"labels":{"nodes":[{"id":"ready-id","name":"loop:ready"}]}}}}`
		default:
			createBody = q
			response = `{"data":{"issueCreate":{"success":true,"issue":{"id":"u1","identifier":"FLO-9","title":"Ready card","url":"https://linear.test/FLO-9","updatedAt":"now","state":{"name":"Backlog"}}}}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { defaultLinearHTTPClient = old })
	input := fmt.Sprintf(`{"title":"Ready card","problem":"p","desired_outcome":"o","out_of_scope":["x"],"repo":%q,"repo_path":%q,"base":"dev","tier":"L1","touches":["a"],"acceptance":["works"],"verification":["test"],"depends_on":[],"priority":1,"risk":{"level":"low"},"rollback_notes":"revert"}`, s.Config.Repo, s.Config.RepoPath)
	if _, err := s.GroomCreate(context.Background(), []byte(input), ""); ExitCode(err) != 2 {
		t.Fatalf("approval exit=%d err=%v", ExitCode(err), err)
	}
	result, err := s.GroomCreate(context.Background(), []byte(input), "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "Backlog" || result["label"] != "loop:ready" {
		t.Fatalf("result=%v", result)
	}
	if !strings.Contains(createBody, "ready-id") || !strings.Contains(createBody, "loop-card") {
		t.Fatalf("create body missing contract: %s", createBody)
	}
}
