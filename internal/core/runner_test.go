package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerPromptIncludesOriginDevBaseSyncContract(t *testing.T) {
	prompt := runnerPrompt(RunnerEnvelope{
		Version: 1, CardID: "card-1", Role: "dev", Attempt: 2,
		BaseRef: "dev", BaseSHA: "base-sha", BaseSyncPending: true,
		BaseSyncNote: "worker must resolve the merge", HeadSHA: "head-sha",
		Card: []byte(`{"id":"card-1","contract_hash":"hash"}`),
	})
	for _, marker := range []string{"origin/dev", "--no-tags", "--no-edit", "git pull", "base-sha", "rebase", "discard", "reset", "resolve", "head_sha", "failed command", "next action"} {
		if !strings.Contains(prompt, marker) {
			t.Fatalf("prompt missing %q: %s", marker, prompt)
		}
	}
}

func TestRunProviderWritesValidatedResultAndStripsLinearToken(t *testing.T) {
	dir := t.TempDir()
	provider := filepath.Join(dir, "fake-claude")
	script := "#!/bin/sh\n[ -z \"$LINEAR_API_TOKEN\" ] || exit 9\nfor arg do [ \"$arg\" != '-' ] || exit 8; done\nprompt=$(sed -n '1p')\n[ -n \"$prompt\" ] || exit 7\nprintf '%s' '{\"structured_output\":{\"version\":1,\"card_id\":\"card-1\",\"role\":\"dev\",\"attempt\":1,\"outcome\":\"completed\",\"evidence\":[\"tests pass\"],\"acceptance_results\":[],\"branch\":\"loop/card-1\",\"pr\":1,\"base_sha\":\"base\",\"head_sha\":\"abc\"}}'\n"
	if err := os.WriteFile(provider, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINEAR_API_TOKEN", "secret")
	output := filepath.Join(dir, "results", "1.json")
	worktree := filepath.Join(dir, "worktrees", "card-1")
	_ = os.MkdirAll(worktree, 0700)
	e := RunnerEnvelope{Version: 1, CardID: "card-1", Role: "dev", Attempt: 1, Provider: "claude", ProviderPath: provider, StateRoot: dir, Worktree: worktree, ContractHash: "hash", OutputPath: output, Card: json.RawMessage(`{"id":"card-1","contract_hash":"hash"}`)}
	b, _ := json.Marshal(e)
	r, err := RunProvider(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if r.HeadSHA != "abc" || r.Outcome != "completed" {
		t.Fatalf("result=%+v", r)
	}
	var saved RunnerResult
	if err := readJSON(output, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.CardID != "card-1" {
		t.Fatalf("saved=%+v", saved)
	}
}

func TestRunnerRejectsIdentityMismatch(t *testing.T) {
	e := RunnerEnvelope{Version: 1, CardID: "card-1", Role: "qa", Attempt: 2}
	_, err := parseRunnerResult([]byte(`{"version":1,"card_id":"other","role":"qa","attempt":2,"outcome":"completed","evidence":[]}`), e)
	if ExitCode(err) != 11 {
		t.Fatalf("exit=%d err=%v", ExitCode(err), err)
	}
}

func TestResultSchemaTypesAndRequiresEveryProperty(t *testing.T) {
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if err := json.Unmarshal([]byte(resultSchema), &schema); err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{}
	for _, key := range schema.Required {
		required[key] = true
	}
	for key, property := range schema.Properties {
		if !required[key] {
			t.Fatalf("property %s is not required", key)
		}
		if _, ok := property["type"]; !ok {
			t.Fatalf("property %s has no type", key)
		}
	}
}

func TestRunnerAcceptsNullableOptionalResultFields(t *testing.T) {
	e := RunnerEnvelope{Version: 1, CardID: "card-null", Role: "qa", Attempt: 1}
	result := []byte(`{"version":1,"card_id":"card-null","role":"qa","attempt":1,"outcome":"needs_attention","evidence":[],"acceptance_results":[],"branch":null,"pr":null,"head_sha":null,"error":"acceptance command go test ./... failed: assertion output requires review"}`)
	r, err := parseRunnerResult(result, e)
	if err != nil {
		t.Fatal(err)
	}
	if r.PR != nil || r.Branch != "" || r.HeadSHA != "" || !strings.Contains(r.Error, "go test ./...") {
		t.Fatalf("result=%+v", r)
	}
}

func TestCompletedQARequiresEveryAcceptanceCriterionToPass(t *testing.T) {
	e := RunnerEnvelope{
		Version: 1, CardID: "card-qa", Role: "qa", Attempt: 1,
		Card: json.RawMessage(`{"id":"card-qa","contract_hash":"hash","acceptance":["first","second"]}`),
	}
	valid := []byte(`{"version":1,"card_id":"card-qa","role":"qa","attempt":1,"outcome":"completed","evidence":["verification"],"acceptance_results":[{"criterion_index":1,"status":"passed","evidence":"test first"},{"criterion_index":2,"status":"passed","evidence":"test second"}],"branch":null,"pr":null,"base_sha":"base","head_sha":"head","error":null}`)
	if _, err := parseRunnerResult(valid, e); err != nil {
		t.Fatal(err)
	}
	failed := []byte(`{"version":1,"card_id":"card-qa","role":"qa","attempt":1,"outcome":"completed","evidence":["verification"],"acceptance_results":[{"criterion_index":1,"status":"passed","evidence":"test first"},{"criterion_index":2,"status":"failed","evidence":"test second failed"}],"branch":null,"pr":null,"base_sha":"base","head_sha":"head","error":null}`)
	if _, err := parseRunnerResult(failed, e); ExitCode(err) != 11 || !strings.Contains(err.Error(), "criterion 2") {
		t.Fatalf("exit=%d err=%v", ExitCode(err), err)
	}
}

func TestQAAcceptanceResultsRejectDuplicatesAndMissingEvidence(t *testing.T) {
	if err := validateAcceptanceResults([]AcceptanceResult{
		{CriterionIndex: 1, Status: "passed", Evidence: "ok"},
		{CriterionIndex: 1, Status: "passed", Evidence: "again"},
	}, 1, "needs_attention"); err == nil || !strings.Contains(err.Error(), "repeats") {
		t.Fatalf("duplicate validation err=%v", err)
	}
	if err := validateAcceptanceResults([]AcceptanceResult{{CriterionIndex: 1, Status: "failed"}}, 1, "needs_attention"); err == nil || !strings.Contains(err.Error(), "requires evidence") {
		t.Fatalf("evidence validation err=%v", err)
	}
}

func TestRunnerRejectsVagueFailureReason(t *testing.T) {
	e := RunnerEnvelope{Version: 1, CardID: "card-vague", Role: "qa", Attempt: 1}
	_, err := parseRunnerResult([]byte(`{"version":1,"card_id":"card-vague","role":"qa","attempt":1,"outcome":"needs_attention","evidence":[],"branch":null,"pr":null,"base_sha":null,"head_sha":null,"error":"blocked"}`), e)
	if ExitCode(err) != 11 || !strings.Contains(err.Error(), "too vague") {
		t.Fatalf("exit=%d err=%v", ExitCode(err), err)
	}
}

func TestRunnerDiagnosticIsActionableAndCarriesLogReference(t *testing.T) {
	note := runnerFailureNoteWithLog("card-1", "dev", 2, "provider", errors.New("exit status 10"), "provider failed: invalid provider flag")
	diagnostic, ok := parseRunnerDiagnostic(note)
	if !ok {
		t.Fatalf("note is not a runner diagnostic: %s", note)
	}
	if diagnostic.Code != "provider-failed" || diagnostic.Phase != "provider execution" {
		t.Fatalf("diagnostic=%+v", diagnostic)
	}
	for _, value := range []string{diagnostic.Why, diagnostic.Needed, diagnostic.Fix, diagnostic.Recommendation, diagnostic.Log} {
		if strings.TrimSpace(value) == "" {
			t.Fatalf("diagnostic field is empty: %+v", diagnostic)
		}
	}
	if !strings.Contains(diagnostic.Log, "journal/workers/card-1/2.dev.log") || !strings.Contains(diagnostic.Why, "invalid provider flag") {
		t.Fatalf("diagnostic=%+v", diagnostic)
	}
}

func TestRunProviderCodexAdapter(t *testing.T) {
	dir := t.TempDir()
	provider := filepath.Join(dir, "fake-codex")
	script := `#!/bin/sh
out=""
sandbox=""
approve="no"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output-last-message) out=$2; shift 2 ;;
    --sandbox) sandbox=$2; shift 2 ;;
    --approve-for-me) approve="yes"; shift ;;
    *) shift ;;
  esac
done
[ "$sandbox" = "read-only" ] || { printf 'QA sandbox mismatch' >&2; exit 2; }
[ "$approve" = "no" ] || { printf 'QA must not auto-approve' >&2; exit 2; }
	printf '%s' '{"version":1,"card_id":"card-2","role":"qa","attempt":3,"outcome":"completed","evidence":["ok"],"acceptance_results":[],"base_sha":"base","head_sha":"head"}' > "$out"
`
	if err := os.WriteFile(provider, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "result.json")
	worktree := filepath.Join(dir, "worktrees", "card-2")
	_ = os.MkdirAll(worktree, 0700)
	e := RunnerEnvelope{Version: 1, CardID: "card-2", Role: "qa", Attempt: 3, Provider: "codex", ProviderPath: provider, StateRoot: dir, Worktree: worktree, ContractHash: "hash", OutputPath: output, Card: json.RawMessage(`{"id":"card-2","contract_hash":"hash"}`)}
	b, _ := json.Marshal(e)
	r, err := RunProvider(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if r.Role != "qa" || r.HeadSHA != "head" {
		t.Fatalf("result=%+v", r)
	}
}

func TestRunProviderCodexDevUsesCompatibleApprovalFlags(t *testing.T) {
	dir := t.TempDir()
	provider := filepath.Join(dir, "fake-codex")
	script := `#!/bin/sh
out=""
sandbox="no"
approve="no"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output-last-message) out=$2; shift 2 ;;
    --sandbox) sandbox=$2; shift 2 ;;
    --approve-for-me) approve="yes"; shift ;;
    *) shift ;;
  esac
done
[ "$approve" = "yes" ] || { printf 'Dev approval missing' >&2; exit 2; }
[ "$sandbox" = "no" ] || { printf 'incompatible Dev sandbox flag' >&2; exit 2; }
	printf '%s' '{"version":1,"card_id":"card-4","role":"dev","attempt":1,"outcome":"completed","evidence":["ok"],"acceptance_results":[],"branch":"loop/card-4","pr":4,"base_sha":"base","head_sha":"head"}' > "$out"
`
	if err := os.WriteFile(provider, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(dir, "worktrees", "card-4")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	e := RunnerEnvelope{Version: 1, CardID: "card-4", Role: "dev", Attempt: 1, Provider: "codex", ProviderPath: provider, StateRoot: dir, Worktree: worktree, ContractHash: "hash", OutputPath: filepath.Join(dir, "result.json"), Card: json.RawMessage(`{"id":"card-4","contract_hash":"hash"}`)}
	b, _ := json.Marshal(e)
	r, err := RunProvider(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if r.Role != "dev" || r.Branch != "loop/card-4" {
		t.Fatalf("result=%+v", r)
	}
}

func TestRunProviderIncludesProviderStderr(t *testing.T) {
	dir := t.TempDir()
	provider := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf 'invalid provider flag' >&2\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(dir, "worktrees", "card-3")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	e := RunnerEnvelope{Version: 1, CardID: "card-3", Role: "dev", Attempt: 1, Provider: "claude", ProviderPath: provider, StateRoot: dir, Worktree: worktree, ContractHash: "hash", OutputPath: filepath.Join(dir, "result.json"), Card: json.RawMessage(`{"id":"card-3","contract_hash":"hash"}`)}
	b, _ := json.Marshal(e)
	_, err := RunProvider(context.Background(), b)
	if ExitCode(err) != 10 || !strings.Contains(err.Error(), "invalid provider flag") {
		t.Fatalf("exit=%d err=%v", ExitCode(err), err)
	}
}
