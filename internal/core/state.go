package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type State struct {
	Root   string
	Config Config
	FS     *os.Root
}

const (
	cardStoreDir   = "cards"
	executionDir   = "runtime/execution"
	legacyStoreDir = "runtime/legacy"
	legacyManifest = "runtime/legacy-manifest.json"
)

func Open(root string) (*State, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(abs, "config.json"))
	if err != nil {
		return nil, E(2, "state is not initialized: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, E(7, "invalid config: %v", err)
	}
	if cfg.StateRoot != abs {
		return nil, E(2, "state_root binding mismatch")
	}
	if resolved, e := filepath.EvalSymlinks(cfg.RepoPath); e != nil || resolved != cfg.RepoPath {
		return nil, E(2, "bound repository path is unavailable or changed")
	}
	if current := repoIdentity(gitOutput(cfg.RepoPath, "remote", "get-url", "origin")); current == "" || current != cfg.Repo {
		return nil, E(2, "bound Git remote identity changed")
	}
	if err := prepareRuntimeLayout(abs); err != nil {
		return nil, err
	}
	rootFS, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	return &State{Root: abs, Config: cfg, FS: rootFS}, nil
}

func Init(project, repoPath, stateRoot string, bindings ...LinearConfig) (*State, error) {
	if !cardIDPattern.MatchString(project) {
		return nil, E(2, "project must match %s", cardIDPattern.String())
	}
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, E(2, "invalid repo path: %v", err)
	}
	if resolved, e := filepath.EvalSymlinks(repoAbs); e == nil {
		repoAbs = resolved
	}
	if top := gitOutput(repoAbs, "rev-parse", "--show-toplevel"); top == "" || filepath.Clean(top) != repoAbs {
		return nil, E(2, "repo path must be a Git repository root")
	}
	if stateRoot == "" {
		stateRoot = filepath.Join(repoAbs, ".loopctl")
	}
	root, err := filepath.Abs(stateRoot)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(filepath.Join(root, "config.json")); statErr == nil {
		existing, openErr := Open(root)
		if openErr != nil {
			return nil, openErr
		}
		if existing.Config.RepoPath != repoAbs {
			return nil, E(2, "state is already bound to another repo")
		}
		if len(bindings) > 0 && !sameLinearBinding(existing.Config.Linear, bindings[0]) {
			return nil, E(2, "state is already bound to another Linear workspace/team")
		}
		return existing, nil
	}
	remote := gitOutput(repoAbs, "remote", "get-url", "origin")
	repo := repoIdentity(remote)
	if repo == "" {
		return nil, E(2, "repository must have an origin remote")
	}
	if gitOutput(repoAbs, "rev-parse", "--verify", "dev") == "" {
		return nil, E(2, "repository must have a local dev branch")
	}
	providerPath, _ := exec.LookPath("codex")
	if providerPath != "" {
		providerPath, _ = filepath.Abs(providerPath)
		if resolved, e := filepath.EvalSymlinks(providerPath); e == nil {
			providerPath = resolved
		}
	}
	if providerPath == "" {
		return nil, E(2, "codex provider CLI is unavailable")
	}
	linear := DefaultLinearConfig()
	if len(bindings) > 0 {
		binding := bindings[0]
		if binding.Workspace == "" || binding.WorkspaceID == "" || binding.Team == "" || binding.TeamID == "" {
			return nil, E(2, "Linear workspace name/id and team key/id are required")
		}
		linear.Enabled = true
		linear.Workspace = binding.Workspace
		linear.WorkspaceID = binding.WorkspaceID
		linear.Team = binding.Team
		linear.TeamID = binding.TeamID
	}
	cfg := Config{
		Project: project, StateRoot: root, WorktreeRoot: filepath.Join(root, "worktrees"),
		RepoPath: repoAbs, Repo: repo, RemoteURL: remote, Base: "dev", CreatedAt: Now(),
		Dev: RoleConfig{MaxWorkers: 3, ClaimStaleMin: 30}, QA: RoleConfig{MaxWorkers: 2, ClaimStaleMin: 30},
		Runner:            RunnerConfig{Adapter: "builtin", Provider: "codex", ProviderPath: providerPath, StopGraceSec: 30},
		Limits:            LimitsConfig{MaxInFlight: 5, MaxOpenPRs: 3, ConflictSkipBoost: 5},
		HotPaths:          []string{"package.json", "pnpm-lock.yaml", "prisma/schema.prisma", "*.config.*"},
		HeartbeatStaleSec: 7200,
		Linear:            linear,
		GitHub:            GitHubConfig{Enabled: repo != "", OpenPRCacheMaxAgeSec: 300},
	}
	dirs := []string{root, filepath.Join(root, cardStoreDir), filepath.Join(root, cardStoreDir, ".tmp"), filepath.Join(root, executionDir), filepath.Join(root, filepath.Dir(legacyStoreDir)), filepath.Join(root, legacyStoreDir), filepath.Join(root, "journal", "workers"), filepath.Join(root, "runtime", "transactions"), filepath.Join(root, "runtime", "reservations"), filepath.Join(root, "runtime", "workers"), filepath.Join(root, "runtime", "merges"), cfg.WorktreeRoot}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0700); err != nil {
			return nil, err
		}
	}
	if err := sameDevice(dirs); err != nil {
		return nil, E(7, "%v", err)
	}
	b, _ := Encode(cfg)
	configPath := filepath.Join(root, "config.json")
	f, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	for _, role := range []string{"dev", "qa"} {
		_ = writeAtomic(filepath.Join(root, "runtime", role+".json"), []byte("{}\n"), 0600)
		_ = writeAtomic(filepath.Join(root, "journal", role+".md"), []byte(""), 0600)
	}
	_ = writeAtomic(filepath.Join(root, "runtime", "linear.json"), []byte("{\"outbox\":[]}\n"), 0600)
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	return &State{Root: root, Config: cfg, FS: rootFS}, nil
}

func prepareRuntimeLayout(root string) error {
	for _, dir := range []string{filepath.Join(root, cardStoreDir), filepath.Join(root, cardStoreDir, ".tmp"), filepath.Join(root, executionDir), filepath.Join(root, legacyStoreDir)} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	return migrateLegacyQueue(root)
}

func migrateLegacyQueue(root string) error {
	legacyRoot := filepath.Join(root, "queue")
	_, err := os.ReadDir(legacyRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := ensureLegacyTransactionsSettled(root); err != nil {
		return err
	}
	manifest := map[string]string{}
	seen := map[string]string{}
	manifestPath := filepath.Join(root, legacyManifest)
	if b, readErr := os.ReadFile(manifestPath); readErr == nil {
		if err := json.Unmarshal(b, &manifest); err != nil {
			return E(7, "invalid legacy migration manifest: %v", err)
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	for _, status := range Statuses {
		statusDir := filepath.Join(legacyRoot, status)
		files, readErr := os.ReadDir(statusDir)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return readErr
		}
		for _, ent := range files {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(ent.Name(), ".json")
			if !cardIDPattern.MatchString(id) {
				return E(7, "legacy queue contains invalid card %s", ent.Name())
			}
			if prior := seen[id]; prior != "" {
				return E(7, "legacy card %s exists in both %s and %s", id, prior, status)
			}
			seen[id] = status
			source := filepath.Join(statusDir, ent.Name())
			data, readErr := os.ReadFile(source)
			if readErr != nil {
				return readErr
			}
			var raw map[string]any
			if json.Unmarshal(data, &raw) != nil {
				return E(7, "legacy card %s is invalid", id)
			}
			delete(raw, "status")
			canonical, encodeErr := Encode(raw)
			if encodeErr != nil {
				return encodeErr
			}
			target := filepath.Join(root, cardStoreDir, ent.Name())
			if existing, statErr := os.Stat(target); errors.Is(statErr, os.ErrNotExist) {
				if writeErr := writeAtomic(target, canonical, 0600); writeErr != nil {
					return writeErr
				}
			} else if statErr != nil {
				return statErr
			} else if existing.IsDir() {
				return E(7, "card store target %s is a directory", id)
			} else if existingData, readErr := os.ReadFile(target); readErr != nil {
				return readErr
			} else if Hash(existingData) != Hash(canonical) {
				return E(7, "legacy card collision for %s", id)
			}
			if err := writeRuntimePhase(root, id, status); err != nil {
				return err
			}
			manifest[id] = status
			archive := filepath.Join(root, legacyStoreDir, ent.Name())
			if _, statErr := os.Stat(archive); errors.Is(statErr, os.ErrNotExist) {
				if err := os.Rename(source, archive); err != nil {
					return err
				}
			} else if statErr != nil {
				return statErr
			} else {
				archived, readErr := os.ReadFile(archive)
				if readErr != nil {
					return readErr
				}
				if Hash(archived) != Hash(data) {
					return E(7, "legacy archive collision for card %s", id)
				}
				if err := os.Remove(source); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
		if err := os.Remove(statusDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if remaining, err := os.ReadDir(legacyRoot); err == nil {
		for _, ent := range remaining {
			if ent.Name() == ".tmp" {
				tmpEntries, readErr := os.ReadDir(filepath.Join(legacyRoot, ent.Name()))
				if readErr != nil {
					return readErr
				}
				if len(tmpEntries) > 0 {
					return E(7, "legacy queue migration paused: queue/.tmp is not empty")
				}
				if removeErr := os.Remove(filepath.Join(legacyRoot, ent.Name())); removeErr != nil {
					return removeErr
				}
				continue
			}
			return E(7, "legacy queue contains unknown entry %s", ent.Name())
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if b, err := Encode(manifest); err != nil {
		return err
	} else if len(manifest) > 0 {
		if err := writeAtomic(manifestPath, b, 0600); err != nil {
			return err
		}
	}
	if files, err := os.ReadDir(legacyRoot); err == nil && len(files) == 0 {
		_ = os.Remove(legacyRoot)
	}
	return nil
}

func ensureLegacyTransactionsSettled(root string) error {
	entries, err := os.ReadDir(filepath.Join(root, "runtime", "transactions"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, "runtime", "transactions", ent.Name()))
		if err != nil {
			return err
		}
		var tx Transaction
		if err := json.Unmarshal(b, &tx); err != nil {
			return E(7, "invalid transaction %s", ent.Name())
		}
		if tx.Phase != "committed" {
			return E(7, "legacy queue migration paused: transaction %s is %s; run doctor first", tx.ID, tx.Phase)
		}
	}
	return nil
}

func writeRuntimePhase(root, id, phase string) error {
	return writeAtomic(filepath.Join(root, executionDir, id+".json"), mustEncode(RuntimePhase{CardID: id, Phase: phase, UpdatedAt: Now()}), 0600)
}

func mustEncode(v any) []byte {
	b, _ := Encode(v)
	return b
}

func (s *State) cardPath(id string) string {
	return filepath.Join(s.Root, cardStoreDir, id+".json")
}

func (s *State) runtimePhase(id string) (string, error) {
	b, err := os.ReadFile(filepath.Join(s.Root, executionDir, id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var phase RuntimePhase
	if err := json.Unmarshal(b, &phase); err != nil || phase.CardID != id || !statusValid(phase.Phase) {
		return "", E(7, "invalid runtime phase for %s", id)
	}
	return phase.Phase, nil
}

func (s *State) setRuntimePhase(id, phase string) error {
	if !statusValid(phase) {
		return E(2, "invalid runtime phase %q", phase)
	}
	return writeRuntimePhase(s.Root, id, phase)
}

func sameLinearBinding(a, b LinearConfig) bool {
	return a.Workspace == b.Workspace && a.WorkspaceID == b.WorkspaceID && a.Team == b.Team && a.TeamID == b.TeamID
}

func DefaultLinearConfig() LinearConfig {
	return LinearConfig{Enabled: false, Endpoint: "https://api.linear.app/graphql", TokenEnv: "LINEAR_API_TOKEN", ReadyLabel: "loop:ready", NeedsAttentionLabel: "loop:needs-attention", SyncIntervalSec: 300, StatusMap: map[string]string{"backlog": "Backlog", "todo": "Todo", "in_progress": "In Progress", "in_review": "In Review", "done": "Done"}}
}

func (s *State) rel(path string) (string, error) {
	rel, err := filepath.Rel(s.Root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", E(7, "path escapes state root")
	}
	return filepath.ToSlash(rel), nil
}

func sameDevice(paths []string) error {
	var dev uint64
	for i, p := range paths {
		var st syscall.Stat_t
		if err := syscall.Stat(p, &st); err != nil {
			return err
		}
		if i == 0 {
			dev = uint64(st.Dev)
		} else if uint64(st.Dev) != dev {
			return fmt.Errorf("state directories must be on one filesystem")
		}
	}
	return nil
}

func repoIdentity(remote string) string {
	s := strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	prefixes := []string{"git@github.com:", "https://github.com/", "ssh://git@github.com/"}
	matched := false
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			s = strings.TrimPrefix(s, p)
			matched = true
			break
		}
	}
	if !matched || len(strings.Split(s, "/")) != 2 {
		return ""
	}
	return s
}

func gitOutput(dir string, args ...string) string {
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	b, err := c.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *State) withLock(fn func() error) error {
	path := filepath.Join(s.Root, "runtime", "queue.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func (s *State) Locate(id string) (string, string, error) {
	if !cardIDPattern.MatchString(id) {
		return "", "", E(2, "invalid card id")
	}
	path := s.cardPath(id)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", E(2, "card %s not found", id)
		}
		return "", "", err
	}
	phase, err := s.runtimePhase(id)
	if err != nil {
		return "", "", err
	}
	if phase == "" {
		phase = "todo"
	}
	return phase, path, nil
}

func (s *State) ReadCard(id string) (string, map[string]any, *Card, error) {
	status, path, err := s.Locate(id)
	if err != nil {
		return "", nil, nil, err
	}
	rel, _ := s.rel(path)
	b, err := s.FS.ReadFile(rel)
	if err != nil {
		return "", nil, nil, err
	}
	raw, card, err := DecodeCard(b, &s.Config)
	if err == nil {
		card.Status = status
	}
	return status, raw, card, err
}

func (s *State) AllCards() ([]struct {
	Status, Path string
	Raw          map[string]any
	Card         *Card
}, error) {
	var out []struct {
		Status, Path string
		Raw          map[string]any
		Card         *Card
	}
	entries, err := fs.ReadDir(s.FS.FS(), cardStoreDir)
	if err != nil {
		return nil, err
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(ent.Name(), ".json")
		path := s.cardPath(id)
		rel, _ := s.rel(path)
		b, err := s.FS.ReadFile(rel)
		if err != nil {
			return nil, err
		}
		raw, card, err := DecodeCard(b, &s.Config)
		if err != nil {
			return nil, E(7, "%s: %v", path, err)
		}
		phase, err := s.runtimePhase(id)
		if err != nil {
			return nil, err
		}
		if phase == "" {
			phase = "todo"
		}
		card.Status = phase
		out = append(out, struct {
			Status, Path string
			Raw          map[string]any
			Card         *Card
		}{phase, path, raw, card})
	}
	return out, nil
}

func parseTime(v *string) time.Time {
	if v == nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, *v)
	return t
}
