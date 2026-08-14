package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunProviderWritesValidatedResultAndStripsLinearToken(t *testing.T) {
	dir := t.TempDir()
	provider := filepath.Join(dir, "fake-claude")
	script := "#!/bin/sh\n[ -z \"$LINEAR_API_TOKEN\" ] || exit 9\nfor arg do [ \"$arg\" != '-' ] || exit 8; done\nprompt=$(sed -n '1p')\n[ -n \"$prompt\" ] || exit 7\nprintf '%s' '{\"structured_output\":{\"version\":1,\"card_id\":\"card-1\",\"role\":\"dev\",\"attempt\":1,\"outcome\":\"completed\",\"evidence\":[\"tests pass\"],\"branch\":\"loop/card-1\",\"pr\":1,\"head_sha\":\"abc\"}}'\n"
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
	result := []byte(`{"version":1,"card_id":"card-null","role":"qa","attempt":1,"outcome":"needs_attention","evidence":[],"branch":null,"pr":null,"head_sha":null,"error":"blocked"}`)
	r, err := parseRunnerResult(result, e)
	if err != nil {
		t.Fatal(err)
	}
	if r.PR != nil || r.Branch != "" || r.HeadSHA != "" || r.Error != "blocked" {
		t.Fatalf("result=%+v", r)
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
printf '%s' '{"version":1,"card_id":"card-2","role":"qa","attempt":3,"outcome":"completed","evidence":["ok"],"head_sha":"head"}' > "$out"
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
printf '%s' '{"version":1,"card_id":"card-4","role":"dev","attempt":1,"outcome":"completed","evidence":["ok"],"branch":"loop/card-4","pr":4,"head_sha":"head"}' > "$out"
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
