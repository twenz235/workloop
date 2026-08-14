package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

func (s *State) ValidateLinear(ctx context.Context) error {
	c, err := s.linearClient()
	if err != nil {
		return err
	}
	issues := []string{}
	const orgQuery = `query LoopOrganization($team: String!) { organization { id name } team: team(id: $team) { id name key } }`
	var binding struct {
		Organization struct{ ID, Name string }
		Team         struct{ ID, Name, Key string }
	}
	if e := c.graphql(ctx, orgQuery, map[string]any{"team": s.Config.Linear.TeamID}, &binding); e != nil {
		issues = append(issues, e.Error())
	} else {
		if binding.Organization.ID != s.Config.Linear.WorkspaceID || binding.Organization.Name != s.Config.Linear.Workspace || binding.Team.ID != s.Config.Linear.TeamID || binding.Team.Key != s.Config.Linear.Team {
			issues = append(issues, "workspace/team binding mismatch")
		}
	}
	for _, name := range []string{"backlog", "todo", "in_progress", "in_review", "done"} {
		if _, e := s.linearStateID(ctx, c, s.Config.Linear.StatusMap[name]); e != nil {
			issues = append(issues, e.Error())
		}
	}
	if _, e := s.linearStateID(ctx, c, "Canceled"); e != nil {
		issues = append(issues, e.Error())
	}
	for _, name := range []string{s.Config.Linear.ReadyLabel, s.Config.Linear.NeedsAttentionLabel} {
		if _, e := s.linearLabelID(ctx, c, name); e != nil {
			issues = append(issues, e.Error())
		}
	}
	if len(issues) > 0 {
		return E(2, "Linear mapping invalid: %s", strings.Join(issues, "; "))
	}
	return nil
}

func ValidateLinearBinding(ctx context.Context, binding LinearConfig) error {
	defaults := DefaultLinearConfig()
	defaults.Enabled = true
	defaults.Workspace = binding.Workspace
	defaults.WorkspaceID = binding.WorkspaceID
	defaults.Team = binding.Team
	defaults.TeamID = binding.TeamID
	return (&State{Config: Config{Linear: defaults}}).ValidateLinear(ctx)
}

type LinearTeamOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  string `json:"key"`
}

func DiscoverLinearBinding(ctx context.Context, preferredTeam string) (LinearConfig, []LinearTeamOption, error) {
	defaults := DefaultLinearConfig()
	s := &State{Config: Config{Linear: defaults}}
	c, err := s.linearClient()
	if err != nil {
		return LinearConfig{}, nil, err
	}
	const query = `query WorkloopBinding($after: String) { organization { id name } teams(first: 100, after: $after) { nodes { id name key } pageInfo { hasNextPage endCursor } } }`
	var organization struct{ ID, Name string }
	teams := []LinearTeamOption{}
	var after any
	for {
		var data struct {
			Organization struct{ ID, Name string }
			Teams        struct {
				Nodes    []LinearTeamOption
				PageInfo struct {
					HasNextPage bool
					EndCursor   string
				}
			}
		}
		if err := c.graphql(ctx, query, map[string]any{"after": after}, &data); err != nil {
			return LinearConfig{}, nil, E(8, "Linear discovery failed: %v", err)
		}
		organization = data.Organization
		teams = append(teams, data.Teams.Nodes...)
		if !data.Teams.PageInfo.HasNextPage {
			break
		}
		if data.Teams.PageInfo.EndCursor == "" {
			return LinearConfig{}, teams, E(8, "Linear team pagination cursor missing")
		}
		after = data.Teams.PageInfo.EndCursor
	}
	if organization.ID == "" || organization.Name == "" || len(teams) == 0 {
		return LinearConfig{}, teams, E(2, "Linear workspace has no discoverable team")
	}
	var selected *LinearTeamOption
	if preferredTeam != "" {
		for i := range teams {
			if strings.EqualFold(teams[i].Key, preferredTeam) || strings.EqualFold(teams[i].Name, preferredTeam) || teams[i].ID == preferredTeam {
				selected = &teams[i]
				break
			}
		}
		if selected == nil {
			return LinearConfig{}, teams, E(2, "Linear team %q not found; available teams: %s", preferredTeam, linearTeamKeys(teams))
		}
	} else if len(teams) == 1 {
		selected = &teams[0]
	} else {
		return LinearConfig{}, teams, E(2, "multiple Linear teams found (%s); rerun with --linear-team KEY", linearTeamKeys(teams))
	}
	defaults.Enabled = true
	defaults.Workspace = organization.Name
	defaults.WorkspaceID = organization.ID
	defaults.Team = selected.Key
	defaults.TeamID = selected.ID
	return defaults, teams, nil
}

func linearTeamKeys(teams []LinearTeamOption) string {
	keys := make([]string, 0, len(teams))
	for _, team := range teams {
		keys = append(keys, team.Key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func (s *State) ConfigGet(key string) (any, error) {
	b, err := json.Marshal(s.Config)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	if key == "" {
		return raw, nil
	}
	var cur any = raw
	for _, part := range strings.Split(key, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, E(2, "unknown config key")
		}
		cur, ok = m[part]
		if !ok {
			return nil, E(2, "unknown config key")
		}
	}
	return cur, nil
}

func (s *State) SaveConfig() error {
	b, _ := Encode(s.Config)
	return writeAtomic(filepath.Join(s.Root, "config.json"), b, 0600)
}
func (s *State) ConfigSet(key, value string) (any, error) {
	allowed := map[string]bool{"linear.sync_interval_sec": true, "runner.provider": true, "runner.provider_path": true, "dev.max_workers": true, "qa.max_workers": true, "limits.max_in_flight": true, "limits.max_open_prs": true}
	if !allowed[key] {
		return nil, E(2, "config key is not mutable")
	}
	switch key {
	case "linear.sync_interval_sec":
		n, e := strconv.Atoi(value)
		if e != nil || n < 60 || n > 3600 {
			return nil, E(2, "sync interval must be 60..3600 seconds")
		}
		s.Config.Linear.SyncIntervalSec = n
	case "runner.provider":
		if value != "codex" && value != "claude" {
			return nil, E(2, "provider must be codex or claude")
		}
		path, e := exec.LookPath(value)
		if e != nil {
			return nil, E(2, "provider CLI unavailable")
		}
		path, _ = filepath.Abs(path)
		if resolved, e := filepath.EvalSymlinks(path); e == nil {
			path = resolved
		}
		s.Config.Runner.Provider = value
		s.Config.Runner.ProviderPath = path
	case "runner.provider_path":
		if !filepath.IsAbs(value) {
			return nil, E(2, "provider_path must be absolute")
		}
		if info, e := os.Stat(value); e != nil || info.Mode().Perm()&0111 == 0 {
			return nil, E(2, "provider_path unavailable")
		}
		s.Config.Runner.ProviderPath = value
	default:
		n, e := strconv.Atoi(value)
		if e != nil || n < 1 {
			return nil, E(2, "value must be a positive integer")
		}
		switch key {
		case "dev.max_workers":
			s.Config.Dev.MaxWorkers = n
		case "qa.max_workers":
			s.Config.QA.MaxWorkers = n
		case "limits.max_in_flight":
			s.Config.Limits.MaxInFlight = n
		case "limits.max_open_prs":
			s.Config.Limits.MaxOpenPRs = n
		}
	}
	b, _ := Encode(s.Config)
	if err := writeAtomic(filepath.Join(s.Root, "config.json"), b, 0600); err != nil {
		return nil, err
	}
	return s.ConfigGet(key)
}

func (s *State) Startup(action, executable, envFile string) error {
	if action != "enable" && action != "disable" {
		return E(2, "startup action must be enable or disable")
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	label := "app.loopctl." + s.Config.Project
	dir := filepath.Join(u.HomeDir, "Library", "LaunchAgents")
	path := filepath.Join(dir, label+".plist")
	domain := "gui/" + u.Uid
	if action == "disable" {
		_ = exec.Command("launchctl", "bootout", domain, path).Run()
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if !filepath.IsAbs(executable) || !filepath.IsAbs(envFile) {
		return E(2, "startup paths must be absolute")
	}
	if info, e := os.Stat(executable); e != nil || info.Mode().Perm()&0111 == 0 {
		return E(2, "loopctl executable is unavailable or not executable")
	}
	info, err := os.Stat(envFile)
	if err != nil {
		return E(2, "env file: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0077 != 0 {
		return E(2, "env file owner/mode must be current user and 0600 or stricter")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array><string>%s</string><string>start</string><string>--state-root</string><string>%s</string><string>--env-file</string><string>%s</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><true/></dict></plist>
`, xmlEscape(label), xmlEscape(executable), xmlEscape(s.Root), xmlEscape(envFile))
	if err := writeAtomic(path, []byte(plist), 0600); err != nil {
		return err
	}
	if out, err := exec.Command("launchctl", "bootstrap", domain, path).CombinedOutput(); err != nil && !strings.Contains(string(out), "service already loaded") {
		return E(8, "launchctl bootstrap failed: %s", out)
	}
	return nil
}
func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")
	return r.Replace(s)
}
