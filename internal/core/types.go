package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const Version = "0.2.4"

var cardIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
var workerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

var Statuses = []string{
	"todo", "rework", "claimed-dev", "in_review", "claimed-qa",
	"needs_attention", "blocked", "cancelled", "done",
}

type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

func E(code int, format string, args ...any) error {
	return &ExitError{Code: code, Err: fmt.Errorf(format, args...)}
}

func ExitCode(err error) int {
	var e *ExitError
	if errors.As(err, &e) {
		return e.Code
	}
	return 1
}

type Config struct {
	Project           string       `json:"project"`
	StateRoot         string       `json:"state_root"`
	WorktreeRoot      string       `json:"worktree_root"`
	RepoPath          string       `json:"repo_path"`
	Repo              string       `json:"repo"`
	RemoteURL         string       `json:"remote_url"`
	Base              string       `json:"base"`
	CreatedAt         string       `json:"created_at"`
	Dev               RoleConfig   `json:"dev"`
	QA                RoleConfig   `json:"qa"`
	Runner            RunnerConfig `json:"runner"`
	Limits            LimitsConfig `json:"limits"`
	HotPaths          []string     `json:"hot_paths"`
	DeadlineAt        *string      `json:"deadline_at"`
	HeartbeatStaleSec int          `json:"heartbeat_stale_sec"`
	Linear            LinearConfig `json:"linear"`
	GitHub            GitHubConfig `json:"github"`
}

type RoleConfig struct {
	MaxWorkers    int `json:"max_workers"`
	ClaimStaleMin int `json:"claim_stale_min"`
}

type RunnerConfig struct {
	Adapter      string `json:"adapter"`
	Provider     string `json:"provider"`
	ProviderPath string `json:"provider_path"`
	StopGraceSec int    `json:"stop_grace_sec"`
}

type LimitsConfig struct {
	MaxInFlight       int `json:"max_in_flight"`
	MaxOpenPRs        int `json:"max_open_prs"`
	ConflictSkipBoost int `json:"conflict_skip_boost"`
}

type LinearConfig struct {
	Enabled             bool              `json:"enabled"`
	Endpoint            string            `json:"endpoint"`
	TokenEnv            string            `json:"token_env"`
	Workspace           string            `json:"workspace"`
	Team                string            `json:"team"`
	WorkspaceID         string            `json:"workspace_id"`
	TeamID              string            `json:"team_id"`
	ReadyLabel          string            `json:"ready_label"`
	NeedsAttentionLabel string            `json:"needs_attention_label"`
	SyncIntervalSec     int               `json:"sync_interval_sec"`
	StatusMap           map[string]string `json:"status_map"`
}

type GitHubConfig struct {
	Enabled              bool `json:"enabled"`
	OpenPRCacheMaxAgeSec int  `json:"open_pr_cache_max_age_sec"`
}

type Card struct {
	ID              string          `json:"id"`
	Title           string          `json:"title"`
	Problem         string          `json:"problem"`
	DesiredOutcome  string          `json:"desired_outcome"`
	OutOfScope      []string        `json:"out_of_scope"`
	Repo            string          `json:"repo"`
	RepoPath        string          `json:"repo_path"`
	Base            string          `json:"base"`
	Tier            string          `json:"tier"`
	Touches         []string        `json:"touches"`
	Acceptance      []string        `json:"acceptance"`
	Verification    []string        `json:"verification"`
	DependsOn       []string        `json:"depends_on"`
	Priority        int             `json:"priority"`
	WorkType        string          `json:"work_type,omitempty"`
	LinearProjectID string          `json:"linear_project_id,omitempty"`
	LinearProject   string          `json:"linear_project,omitempty"`
	LinearParentID  string          `json:"linear_parent_id,omitempty"`
	Visuals         []Visual        `json:"visuals,omitempty"`
	Risk            json.RawMessage `json:"risk"`
	RollbackNotes   string          `json:"rollback_notes"`
	LinearIssueID   string          `json:"linear_issue_id"`
	LinearIssueUUID string          `json:"linear_issue_uuid"`
	LinearURL       string          `json:"linear_url"`
	LinearState     string          `json:"linear_state,omitempty"`
	LinearLabels    []string        `json:"linear_labels,omitempty"`
	LinearUpdatedAt string          `json:"linear_updated_at,omitempty"`
	SourceRevision  string          `json:"source_revision"`
	ContractHash    string          `json:"contract_hash"`
	ApprovedAt      string          `json:"approved_at"`
	ApprovedBy      string          `json:"approved_by"`
	Status          string          `json:"status"`
	Hot             bool            `json:"hot"`
	Attempts        int             `json:"attempts"`
	MaxAttempts     int             `json:"max_attempts"`
	ReworkCount     int             `json:"rework_count"`
	MaxRework       int             `json:"max_rework"`
	ConflictSkips   int             `json:"conflict_skips"`
	ClaimedAt       *string         `json:"claimed_at"`
	ClaimedBy       *string         `json:"claimed_by"`
	Worktree        *string         `json:"worktree"`
	Branch          *string         `json:"branch"`
	PR              any             `json:"pr"`
	BaseSHA         *string         `json:"base_sha"`
	TestedHeadSHA   *string         `json:"tested_head_sha"`
	Stale           bool            `json:"stale"`
	SpecChanged     bool            `json:"spec_changed"`
	QAFindings      []Finding       `json:"qa_findings"`
	QAEvidence      []string        `json:"qa_evidence,omitempty"`
	History         []History       `json:"history"`
}

type Visual struct {
	Alt         string `json:"alt"`
	URL         string `json:"url"`
	Description string `json:"description"`
}

type Finding struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Issue    string `json:"issue"`
	Violates string `json:"violates,omitempty"`
	Evidence string `json:"evidence"`
	Severity string `json:"severity"`
}

type History struct {
	At   string `json:"at"`
	From string `json:"from"`
	To   string `json:"to"`
	By   string `json:"by"`
	Note string `json:"note"`
}

type Reservation struct {
	CardID     string   `json:"card_id"`
	Touches    []string `json:"touches"`
	Hot        bool     `json:"hot"`
	CreatedAt  string   `json:"created_at"`
	ReleasedAt *string  `json:"released_at"`
}

type Transaction struct {
	ID          string `json:"id"`
	CardID      string `json:"card_id"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Hash        string `json:"hash"`
	Operation   string `json:"operation"`
	Actor       string `json:"actor"`
	Phase       string `json:"phase"`
	Temp        string `json:"temp"`
	Stage       string `json:"stage,omitempty"`
	CreatedAt   string `json:"created_at"`
}

func Now() string { return time.Now().Format(time.RFC3339Nano) }

func ValidateCard(c *Card, cfg *Config) error {
	if !cardIDPattern.MatchString(c.ID) {
		return E(2, "invalid card id %q", c.ID)
	}
	if strings.TrimSpace(c.Title) == "" || strings.TrimSpace(c.Problem) == "" || strings.TrimSpace(c.DesiredOutcome) == "" {
		return E(2, "title, problem and desired_outcome are required")
	}
	if strings.TrimSpace(c.Repo) == "" || !filepath.IsAbs(c.RepoPath) || strings.TrimSpace(c.RollbackNotes) == "" {
		return E(2, "repo, absolute repo_path and rollback_notes are required")
	}
	var risk map[string]any
	if len(c.Risk) == 0 || json.Unmarshal(c.Risk, &risk) != nil || strings.TrimSpace(fmt.Sprint(risk["level"])) == "" {
		return E(2, "risk.level is required")
	}
	if strings.TrimSpace(c.LinearIssueUUID) == "" || strings.TrimSpace(c.LinearIssueID) == "" || strings.TrimSpace(c.LinearURL) == "" || strings.TrimSpace(c.SourceRevision) == "" || strings.TrimSpace(c.ApprovedAt) == "" || strings.TrimSpace(c.ApprovedBy) == "" {
		return E(2, "Linear identity, source revision and approval audit are required")
	}
	if len(c.OutOfScope) == 0 || len(c.Touches) == 0 || len(c.Acceptance) == 0 || len(c.Verification) == 0 {
		return E(2, "out_of_scope, touches, acceptance and verification must be non-empty")
	}
	for _, list := range [][]string{c.OutOfScope, c.Touches, c.Acceptance, c.Verification} {
		for _, v := range list {
			if strings.TrimSpace(v) == "" {
				return E(2, "card lists cannot contain empty values")
			}
		}
	}
	for _, pattern := range c.Touches {
		clean := filepath.ToSlash(pattern)
		if filepath.IsAbs(pattern) || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") || strings.Contains(pattern, "\\") {
			return E(2, "touches must be repository-relative patterns")
		}
	}
	if c.Base != "dev" {
		return E(2, "base must be dev")
	}
	if c.Tier != "L0" && c.Tier != "L1" && c.Tier != "L2" {
		return E(2, "tier must be L0, L1 or L2")
	}
	if c.Priority < 0 || c.Priority > 4 {
		return E(2, "priority must be 0..4")
	}
	if c.WorkType != "" || c.LinearProjectID != "" || c.LinearProject != "" {
		if c.WorkType != "feature" && c.WorkType != "bug" && c.WorkType != "maintenance" {
			return E(2, "work_type must be feature, bug or maintenance")
		}
		if strings.TrimSpace(c.LinearProjectID) == "" || strings.TrimSpace(c.LinearProject) == "" {
			return E(2, "linear_project_id and linear_project are required")
		}
	}
	if c.LinearParentID != "" && !uuidPattern.MatchString(c.LinearParentID) {
		return E(2, "linear_parent_id must be a UUID")
	}
	for _, visual := range c.Visuals {
		if err := validateVisual(visual); err != nil {
			return err
		}
	}
	if cfg != nil && (c.Repo != cfg.Repo || filepath.Clean(c.RepoPath) != cfg.RepoPath || c.Base != cfg.Base) {
		return E(2, "card repo/base does not match immutable state binding")
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 2
	}
	if c.MaxRework == 0 {
		c.MaxRework = 2
	}
	return nil
}

func validateVisual(visual Visual) error {
	parsed, err := url.ParseRequestURI(visual.URL)
	if strings.TrimSpace(visual.Alt) == "" || strings.TrimSpace(visual.Description) == "" || err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || strings.ContainsAny(visual.URL, "\r\n<>") {
		return E(2, "each visual requires alt, description and an https URL")
	}
	return nil
}

func DecodeCard(data []byte, cfg *Config) (map[string]any, *Card, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, E(2, "invalid card JSON: %v", err)
	}
	canonical, _ := json.Marshal(raw)
	var c Card
	if err := json.Unmarshal(canonical, &c); err != nil {
		return nil, nil, E(2, "invalid card fields: %v", err)
	}
	if err := ValidateCard(&c, cfg); err != nil {
		return nil, nil, err
	}
	return raw, &c, nil
}

func Encode(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func Hash(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

func statusValid(s string) bool {
	for _, x := range Statuses {
		if x == s {
			return true
		}
	}
	return false
}

func sortedKeys[M ~map[string]V, V any](m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
