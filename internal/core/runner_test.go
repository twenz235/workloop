package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestRunProviderCodexAdapter(t *testing.T) {
	dir := t.TempDir()
	provider := filepath.Join(dir, "fake-codex")
	script := `#!/bin/sh
out=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output-last-message" ]; then out=$2; shift 2; else shift; fi
done
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
