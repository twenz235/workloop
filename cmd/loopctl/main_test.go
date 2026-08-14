package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLoadStrictEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	if err := os.WriteFile(path, []byte("LINEAR_API_TOKEN=value=with=equals\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINEAR_API_TOKEN", "")
	_ = os.Unsetenv("LINEAR_API_TOKEN")
	if err := loadStrictEnv(path, []string{"LINEAR_API_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("LINEAR_API_TOKEN") != "value=with=equals" {
		t.Fatal("value not loaded")
	}
}

func TestLoadStrictEnvCanOverrideAmbientToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("LINEAR_API_TOKEN=file-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINEAR_API_TOKEN", "ambient-token")
	if err := loadStrictEnv(path, []string{"LINEAR_API_TOKEN"}, true); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("LINEAR_API_TOKEN"); got != "file-token" {
		t.Fatalf("token=%q", got)
	}
}
func TestLoadStrictEnvRejectsUnsafe(t *testing.T) {
	for _, content := range []string{"LINEAR_API_TOKEN=$(whoami)\n", "export LINEAR_API_TOKEN=x\n"} {
		path := filepath.Join(t.TempDir(), "env")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := loadStrictEnv(path, []string{"LINEAR_API_TOKEN"}); err == nil {
			t.Fatalf("accepted %q", content)
		}
	}
}
func TestExtractOptionAnywhere(t *testing.T) {
	args, value := extractOption([]string{"card-1", "--to", "in_review", "--state-root", "/state", "--by", "dev/w"}, "--state-root")
	if value != "/state" || len(args) != 5 {
		t.Fatalf("value=%q args=%v", value, args)
	}
}

func TestZeroConfigOfflineInitAndStateDiscovery(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "My App")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test")
	git("remote", "add", "origin", "https://github.com/acme/my-app.git")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "README.md")
	git("commit", "-qm", "init")
	git("branch", "dev")
	provider := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(provider)+":"+os.Getenv("PATH"))
	t.Setenv("LOOPCTL_STATE_ROOT", "")
	old, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := initCmd([]string{"--offline"}); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repo, "src", "feature")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	canonicalRepo, _ := filepath.EvalSymlinks(repo)
	want := filepath.Join(canonicalRepo, ".loopctl")
	root, err := discoverStateRoot()
	if err != nil || root != want {
		t.Fatalf("root=%q want=%q err=%v", root, want, err)
	}
	if out, err := exec.Command("git", "-C", repo, "check-ignore", ".loopctl/config.json").CombinedOutput(); err != nil {
		t.Fatalf("state is not locally ignored: %v %s", err, out)
	}
}
