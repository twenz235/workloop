package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestQAMergeAndSyncDone(t *testing.T) {
	s := testState(t)
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	state := filepath.Join(dir, "merged")
	script := `#!/bin/sh
case "$1 $2" in
  "pr view")
    if [ -f "` + state + `" ]; then
      printf '%s\n' '{"number":12,"state":"MERGED","baseRefName":"dev","headRefName":"loop/a","headRefOid":"head123","mergeCommit":{"oid":"merge123"},"url":"https://github.test/pr/12"}'
    else
      printf '%s\n' '{"number":12,"state":"OPEN","baseRefName":"dev","headRefName":"loop/a","headRefOid":"head123","mergeCommit":null,"url":"https://github.test/pr/12"}'
    fi ;;
  "pr checks") exit 0 ;;
  "pr merge") touch "` + state + `" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(gh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	addTestCard(t, s, "a", []string{"a"})
	if _, err := s.Claim("dev", "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move("a", "in_review", "dev/d", "", map[string]any{"pr": 12}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("qa", "q"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchInternal("a", map[string]any{"tested_head_sha": "head123", "qa_evidence": []string{"acceptance passed"}}, "test"); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.QAMerge(context.Background(), "a", "qa/q")
	if err != nil {
		t.Fatal(err)
	}
	if receipt["merge_sha"] != "merge123" {
		t.Fatalf("receipt=%v", receipt)
	}
	result, err := s.SyncDone(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result["done"].([]string)) != 1 {
		t.Fatalf("result=%v", result)
	}
	status, _, err := s.Locate("a")
	if err != nil || status != "done" {
		t.Fatalf("status=%s err=%v", status, err)
	}
}

func TestOpenPRCountUsesFreshCacheAndFailsClosedWhenStale(t *testing.T) {
	s := testState(t)
	s.Config.Repo = "test/repo"
	s.Config.GitHub.Enabled = true
	s.Config.GitHub.OpenPRCacheMaxAgeSec = 300
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	fail := filepath.Join(dir, "fail")
	script := `#!/bin/sh
if [ -f "` + fail + `" ]; then exit 1; fi
printf '%s\n' '[{"number":1,"headRefOid":"a"},{"number":2,"headRefOid":"b"}]'
`
	if err := os.WriteFile(gh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	count, err := s.OpenPRCount(context.Background())
	if err != nil || count != 2 {
		t.Fatalf("first count=%d err=%v", count, err)
	}
	if err := os.WriteFile(fail, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	count, err = s.OpenPRCount(context.Background())
	if err != nil || count != 2 {
		t.Fatalf("cached count=%d err=%v", count, err)
	}
	cache := githubCache{FetchedAt: time.Now().Add(-time.Hour).Format(time.RFC3339Nano), Repo: s.Config.Repo, Base: "dev", Count: 2}
	b, _ := Encode(cache)
	if err := writeAtomic(filepath.Join(s.Root, "runtime", "github.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.OpenPRCount(context.Background()); ExitCode(err) != 8 {
		t.Fatalf("stale cache exit=%d err=%v", ExitCode(err), err)
	}
}
