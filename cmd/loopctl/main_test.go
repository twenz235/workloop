package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/twenz235/workloop/internal/core"
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

func TestOnlineInitRequiresExplicitLinearBinding(t *testing.T) {
	t.Setenv("LINEAR_API_TOKEN", "test-token")
	err := initCmd([]string{"--project", "test", "--repo-path", "/missing", "--state-root", t.TempDir()})
	if core.ExitCode(err) != 2 || !strings.Contains(err.Error(), "--linear-workspace") {
		t.Fatalf("exit=%d err=%v", core.ExitCode(err), err)
	}
}
