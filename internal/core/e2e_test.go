package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSupervisorFakeEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds loopctl binary")
	}
	s := testState(t)
	repo := s.Config.RepoPath
	head := gitOutput(repo, "rev-parse", "dev")
	dir := t.TempDir()
	provider := filepath.Join(dir, "fake-claude")
	providerScript := fmt.Sprintf(`#!/bin/sh
prompt=$(cat)
attempt=$(printf '%%s' "$prompt" | sed -n 's/.*attempt=\([0-9][0-9]*\).*/\1/p' | head -1)
if [ "$LOOPCTL_ROLE" = dev ]; then
  printf '{"structured_output":{"version":1,"card_id":"%%s","role":"dev","attempt":%%s,"outcome":"completed","evidence":["dev verification"],"branch":"loop/%%s","pr":12,"head_sha":"%s"}}' "$LOOPCTL_CARD_ID" "$attempt" "$LOOPCTL_CARD_ID"
else
  printf '{"structured_output":{"version":1,"card_id":"%%s","role":"qa","attempt":%%s,"outcome":"completed","evidence":["acceptance passed"],"head_sha":"%s"}}' "$LOOPCTL_CARD_ID" "$attempt"
fi
`, head, head)
	if err := os.WriteFile(provider, []byte(providerScript), 0700); err != nil {
		t.Fatal(err)
	}
	merged := filepath.Join(dir, "merged")
	gh := filepath.Join(dir, "gh")
	ghScript := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
 "pr view") if [ -f %q ]; then printf '{"number":12,"state":"MERGED","baseRefName":"dev","headRefName":"loop/e2e","headRefOid":"%s","mergeCommit":{"oid":"merge-e2e"},"url":"https://github.test/12"}'; else printf '{"number":12,"state":"OPEN","baseRefName":"dev","headRefName":"loop/e2e","headRefOid":"%s","mergeCommit":null,"url":"https://github.test/12"}'; fi ;;
 "pr checks") printf '[{"name":"CI / test","state":"SUCCESS","bucket":"pass","link":"https://github.test/checks/1"}]\n' ;;
 "pr merge") touch %q ;;
 *) exit 2 ;;
esac
`, merged, head, head, merged)
	if err := os.WriteFile(gh, []byte(ghScript), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	s.Config.Linear.Enabled = false
	s.Config.GitHub.Enabled = false
	s.Config.Runner.Provider = "claude"
	s.Config.Runner.ProviderPath = provider
	s.Config.Dev.MaxWorkers = 1
	s.Config.QA.MaxWorkers = 1
	if err := s.SaveConfig(); err != nil {
		t.Fatal(err)
	}
	addTestCard(t, s, "e2e", []string{"README.md"})
	bin := filepath.Join(dir, "loopctl")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/loopctl")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.RunSupervisor(ctx, bin, true); err != nil {
		t.Fatal(err)
	}
	status, _, err := s.Locate("e2e")
	if err != nil || status != "done" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	if _, err := os.Stat(merged); err != nil {
		t.Fatalf("merge not invoked: %v", err)
	}
}
