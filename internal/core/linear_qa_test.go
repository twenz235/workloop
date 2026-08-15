package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMapAcceptanceChecklistPreservesLoopCardAndMapsOnlyPassed(t *testing.T) {
	description := "## Problem\nproblem\n\n## Acceptance criteria\n- [ ] first\n- [x] second\n\n```loop-card\n{\"acceptance\":[\"first\",\"second\"]}\n```\n"
	updated, err := mapAcceptanceChecklist(description, []AcceptanceResult{
		{CriterionIndex: 1, Status: "passed", Evidence: "test one"},
		{CriterionIndex: 2, Status: "failed", Evidence: "test two failed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "- [x] first") || !strings.Contains(updated, "- [ ] second") {
		t.Fatalf("updated checklist=%q", updated)
	}
	if !strings.Contains(updated, "```loop-card\n{\"acceptance\":[\"first\",\"second\"]}\n```") {
		t.Fatalf("loop-card changed: %q", updated)
	}
}

func TestQAReportCommentListsEveryAcceptanceResult(t *testing.T) {
	base := "base-sha"
	head := "head-sha"
	card := &Card{
		ID: "qa-report", BaseSHA: &base, TestedHeadSHA: &head,
		Acceptance: []string{"first criterion", "second criterion"},
		QAAcceptanceResults: []AcceptanceResult{
			{CriterionIndex: 1, Status: "passed", Evidence: "go test ./first"},
			{CriterionIndex: 2, Status: "failed", Evidence: "go test ./second failed"},
		},
	}
	body := qaReportComment(card)
	for _, marker := range []string{"workloop-comment:qa-report:", "[x] 1.", "passed", "[ ] 2.", "failed", "go test ./second failed"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("report missing %q: %s", marker, body)
		}
	}
	if linearCommentMarker(body) == "" {
		t.Fatalf("report marker is not idempotency-compatible: %s", body)
	}
}

func TestPublishQAReportUpdatesLinearDescriptionAndDeduplicatesComment(t *testing.T) {
	s := testState(t)
	t.Setenv("LINEAR_API_TOKEN", "linear-test-secret")
	s.Config.Linear.Enabled = true
	s.Config.Linear.Endpoint = "https://linear.test/graphql"
	var raw map[string]any
	if err := json.Unmarshal(testCard("qa-report", []string{"qa-report.go"}), &raw); err != nil {
		t.Fatal(err)
	}
	raw["repo"] = s.Config.Repo
	raw["repo_path"] = s.Config.RepoPath
	raw["acceptance"] = []string{"first criterion", "second criterion"}
	b, _ := json.Marshal(raw)
	if _, err := s.Add(b, "human/test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchInternal("qa-report", map[string]any{
		"qa_evidence": []string{"both criteria reviewed"},
		"qa_acceptance_results": []AcceptanceResult{
			{CriterionIndex: 1, Status: "passed", Evidence: "go test ./first"},
			{CriterionIndex: 2, Status: "failed", Evidence: "go test ./second failed"},
		},
	}, "QA evidence"); err != nil {
		t.Fatal(err)
	}

	description := "## Acceptance criteria\n- [ ] first criterion\n- [ ] second criterion\n\n```loop-card\n{\"contract_hash\":\"must-stay\"}\n```"
	comments := []string{}
	updates := 0
	oldClient := defaultLinearHTTPClient
	defaultLinearHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		request := string(body)
		switch {
		case strings.Contains(request, "UpdateLoopIssueDescription"):
			updates++
			var payload struct {
				Variables struct {
					Input struct {
						Description string `json:"description"`
					} `json:"input"`
				} `json:"variables"`
			}
			_ = json.Unmarshal(body, &payload)
			description = payload.Variables.Input.Description
			return linearTestResponse(`{"data":{"issueUpdate":{"success":true}}}`), nil
		case strings.Contains(request, "LoopIssueDescription"):
			return linearTestResponse(`{"data":{"issue":{"id":"uuid-qa-report","description":` + mustJSONString(description) + `}}}`), nil
		case strings.Contains(request, "LoopIssueComments"):
			nodes, _ := json.Marshal(func() []map[string]string {
				out := []map[string]string{}
				for _, comment := range comments {
					out = append(out, map[string]string{"body": comment})
				}
				return out
			}())
			return linearTestResponse(`{"data":{"issue":{"comments":{"nodes":` + string(nodes) + `,"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`), nil
		case strings.Contains(request, "AddLoopComment"):
			var payload struct {
				Variables struct {
					Input struct {
						Body string `json:"body"`
					} `json:"input"`
				} `json:"variables"`
			}
			_ = json.Unmarshal(body, &payload)
			comments = append(comments, payload.Variables.Input.Body)
			return linearTestResponse(`{"data":{"commentCreate":{"success":true,"comment":{"id":"comment-1"}}}}`), nil
		default:
			t.Fatalf("unexpected Linear request: %s", request)
			return nil, nil
		}
	})}
	t.Cleanup(func() { defaultLinearHTTPClient = oldClient })

	if err := s.publishQAReport(context.Background(), "qa-report"); err != nil {
		t.Fatal(err)
	}
	if err := s.publishQAReport(context.Background(), "qa-report"); err != nil {
		t.Fatal(err)
	}
	if updates != 1 || len(comments) != 1 {
		t.Fatalf("updates=%d comments=%d", updates, len(comments))
	}
	if !strings.Contains(description, "- [x] first criterion") || !strings.Contains(description, "- [ ] second criterion") {
		t.Fatalf("description=%q", description)
	}
	if !strings.Contains(comments[0], "[x] 1.") || !strings.Contains(comments[0], "[ ] 2.") {
		t.Fatalf("comment=%q", comments[0])
	}
}

func linearTestResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": []string{"application/json"}}}
}

func mustJSONString(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}
