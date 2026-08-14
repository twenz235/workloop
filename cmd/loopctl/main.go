package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/twenz235/workloop/internal/core"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(core.ExitCode(err))
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(help)
		return nil
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "version":
		return output(map[string]any{"version": core.Version})
	case "init":
		return initCmd(args)
	case "add":
		return stateCmd(cmd, args, addCmd)
	case "list":
		return stateCmd(cmd, args, listCmd)
	case "status":
		return stateCmd(cmd, args, statusCmd)
	case "claim":
		return stateCmd(cmd, args, claimCmd)
	case "move":
		return stateCmd(cmd, args, moveCmd)
	case "findings":
		return stateCmd(cmd, args, findingsCmd)
	case "resolve":
		return stateCmd(cmd, args, resolveCmd)
	case "heartbeat":
		return stateCmd(cmd, args, heartbeatCmd)
	case "peer-check":
		return stateCmd(cmd, args, peerCmd)
	case "doctor":
		return stateCmd(cmd, args, doctorCmd)
	case "reconcile":
		return stateCmd(cmd, args, reconcileCmd)
	case "sync":
		return stateCmd(cmd, args, syncCmd)
	case "qa-merge":
		return stateCmd(cmd, args, qaMergeCmd)
	case "sync-done":
		return stateCmd(cmd, args, syncDoneCmd)
	case "mark-stale":
		return stateCmd(cmd, args, markStaleCmd)
	case "groom":
		return stateCmd(cmd, args, groomCmd)
	case "config":
		return stateCmd(cmd, args, configCmd)
	case "startup":
		return stateCmd(cmd, args, startupCmd)
	case "gc-worktrees":
		return stateCmd(cmd, args, gcCmd)
	case "stop":
		return stateCmd(cmd, args, stopCmd)
	case "start":
		return stateCmd(cmd, args, startCmd)
	case "restart":
		return stateCmd(cmd, args, restartCmd)
	case "runner":
		return runnerCmd(args)
	default:
		return core.E(2, "unknown command %q", cmd)
	}
}

type handler func(*core.State, []string) error

func stateCmd(cmd string, args []string, h handler) error {
	var root string
	args, root = extractOption(args, "--state-root")
	if root == "" {
		var err error
		root, err = discoverStateRoot()
		if err != nil {
			return err
		}
	}
	if commandNeedsLinearToken(cmd) {
		if envFile := defaultEnvFile(); envFile != "" {
			if err := loadStrictEnv(envFile, []string{"LINEAR_API_TOKEN"}, true); err != nil {
				return err
			}
		}
	}
	s, err := core.Open(root)
	if err != nil {
		return err
	}
	return h(s, args)
}

func extractOption(args []string, name string) ([]string, string) {
	out := make([]string, 0, len(args))
	var value string
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			value = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(args[i], name+"=") {
			value = strings.TrimPrefix(args[i], name+"=")
			continue
		}
		out = append(out, args[i])
	}
	return out, value
}

func initCmd(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	project := fs.String("project", "", "project name (default: repository name)")
	repo := fs.String("repo-path", "", "repository path (default: current Git root)")
	root := fs.String("state-root", "", "state root (default: <repo>/.loopctl)")
	envFile := fs.String("env-file", "", "strict env file (default: ~/.env)")
	linearWorkspace := fs.String("linear-workspace", "", "Linear workspace name")
	linearWorkspaceID := fs.String("linear-workspace-id", "", "Linear workspace UUID")
	linearTeam := fs.String("linear-team", "", "Linear team key")
	linearTeamID := fs.String("linear-team-id", "", "Linear team UUID")
	offline := fs.Bool("offline", false, "skip live Linear validation (tests only)")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	repoPath, err := discoverGitRoot(*repo)
	if err != nil {
		return err
	}
	*repo = repoPath
	if *project == "" {
		*project = projectFromRepo(repoPath)
	}
	if *root == "" {
		*root = filepath.Join(repoPath, ".loopctl")
	}
	if *envFile == "" && !*offline {
		*envFile = defaultEnvFile()
	}
	if *envFile != "" {
		if err := loadStrictEnv(*envFile, []string{"LINEAR_API_TOKEN"}, true); err != nil {
			return err
		}
	}
	var bindings []core.LinearConfig
	if !*offline {
		if os.Getenv("LINEAR_API_TOKEN") == "" {
			return core.E(8, "LINEAR_API_TOKEN is not set; add it to ~/.env or pass --env-file")
		}
		explicitBinding := *linearWorkspace != "" || *linearWorkspaceID != "" || *linearTeamID != ""
		if explicitBinding {
			if *linearWorkspace == "" || *linearWorkspaceID == "" || *linearTeam == "" || *linearTeamID == "" {
				return core.E(2, "explicit Linear binding requires workspace name/id and team key/id")
			}
			bindings = append(bindings, core.LinearConfig{Workspace: *linearWorkspace, WorkspaceID: *linearWorkspaceID, Team: *linearTeam, TeamID: *linearTeamID})
		} else {
			binding, _, discoverErr := core.DiscoverLinearBinding(context.Background(), *linearTeam)
			if discoverErr != nil {
				return discoverErr
			}
			bindings = append(bindings, binding)
		}
		if err := core.ValidateLinearBinding(context.Background(), bindings[0]); err != nil {
			return err
		}
	}
	if err := ensureStateIgnored(repoPath, *root); err != nil {
		return err
	}
	s, err := core.Init(*project, *repo, *root, bindings...)
	if err != nil {
		return err
	}
	if *offline {
		s.Config.GitHub.Enabled = false
		if err := s.SaveConfig(); err != nil {
			return err
		}
	}
	return output(s.Config)
}

func discoverGitRoot(path string) (string, error) {
	if path == "" {
		var err error
		path, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	out, err := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", core.E(2, "run loopctl from inside a Git repository or pass --repo-path")
	}
	root := filepath.Clean(strings.TrimSpace(string(out)))
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}
	return root, nil
}

func projectFromRepo(repo string) string {
	name := strings.ToLower(filepath.Base(repo))
	name = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if len(name) > 32 {
		name = strings.TrimRight(name[:32], "-")
	}
	return name
}

func discoverStateRoot() (string, error) {
	if root := os.Getenv("LOOPCTL_STATE_ROOT"); root != "" {
		return root, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(cwd); resolveErr == nil {
		cwd = resolved
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, ".loopctl")
		if _, statErr := os.Stat(filepath.Join(candidate, "config.json")); statErr == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return "", core.E(2, "Workloop state not found; run 'loopctl init' from the repository")
}

func defaultEnvFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, ".env")
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return ""
}

func commandNeedsLinearToken(cmd string) bool {
	return cmd == "sync" || cmd == "groom"
}

func ensureStateIgnored(repo, stateRoot string) error {
	rel, err := filepath.Rel(repo, stateRoot)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return err
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(repo, path)
	}
	pattern := "/" + filepath.ToSlash(rel) + "/"
	b, readErr := os.ReadFile(path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == pattern {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if len(b) > 0 && b[len(b)-1] != '\n' {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}
	_, err = f.WriteString(pattern + "\n")
	return err
}

func addCmd(s *core.State, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	file := fs.String("file", "", "card JSON")
	stdin := fs.Bool("stdin", false, "read stdin")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	var b []byte
	var err error
	if *stdin {
		b, err = io.ReadAll(os.Stdin)
	} else if *file != "" {
		b, err = os.ReadFile(*file)
	} else {
		return core.E(2, "--file or --stdin is required")
	}
	if err != nil {
		return err
	}
	v, err := s.Add(b, "human/add")
	if err != nil {
		return err
	}
	return output(v)
}
func listCmd(s *core.State, args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	status := fs.String("status", "", "status")
	linear := fs.String("linear", "", "Linear id")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	v, err := s.List(*status, *linear)
	if err != nil {
		return err
	}
	return output(v)
}
func statusCmd(s *core.State, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	role := fs.String("role", "", "role")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	v, err := s.Status(*role)
	if err != nil {
		return err
	}
	return output(v)
}
func claimCmd(s *core.State, args []string) error {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	role := fs.String("role", "", "role")
	worker := fs.String("worker", "", "worker id")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	v, err := s.Claim(*role, *worker)
	if err != nil {
		return err
	}
	return output(v)
}
func moveCmd(s *core.State, args []string) error {
	id, args := leadingID(args)
	fs := flag.NewFlagSet("move", flag.ContinueOnError)
	to := fs.String("to", "", "destination")
	by := fs.String("by", "", "actor")
	patch := fs.String("patch", "{}", "JSON merge patch")
	note := fs.String("note", "", "note")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	if id == "" {
		return core.E(2, "card id is required")
	}
	p := map[string]any{}
	if err := json.Unmarshal([]byte(*patch), &p); err != nil {
		return core.E(2, "invalid patch: %v", err)
	}
	v, err := s.Move(id, *to, *by, *note, p)
	if err != nil {
		return err
	}
	return output(v)
}
func findingsCmd(s *core.State, args []string) error {
	id, args := leadingID(args)
	fs := flag.NewFlagSet("findings", flag.ContinueOnError)
	file := fs.String("file", "", "findings JSON")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	if id == "" || *file == "" {
		return core.E(2, "card id and --file required")
	}
	b, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var f []core.Finding
	if err := json.Unmarshal(b, &f); err != nil {
		return core.E(2, "invalid findings: %v", err)
	}
	v, err := s.Findings(id, f)
	if err != nil {
		return err
	}
	return output(v)
}
func resolveCmd(s *core.State, args []string) error {
	id, args := leadingID(args)
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	to := fs.String("to", "", "destination")
	by := fs.String("by", "", "human identity")
	note := fs.String("note", "", "note")
	closePR := fs.Bool("close-pr", false, "close the card PR when cancelling")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	if id == "" {
		return core.E(2, "card id required")
	}
	v, err := s.Resolve(context.Background(), id, *to, *by, *note, *closePR)
	if err != nil {
		return err
	}
	return output(v)
}

func leadingID(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}
func heartbeatCmd(s *core.State, args []string) error {
	fs := flag.NewFlagSet("heartbeat", flag.ContinueOnError)
	role := fs.String("role", "", "role")
	patch := fs.String("patch", "{}", "JSON patch")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	p := map[string]any{}
	if err := json.Unmarshal([]byte(*patch), &p); err != nil {
		return core.E(2, "invalid patch")
	}
	v, err := s.Heartbeat(*role, p)
	if err != nil {
		return err
	}
	return output(v)
}
func peerCmd(s *core.State, args []string) error {
	fs := flag.NewFlagSet("peer-check", flag.ContinueOnError)
	role := fs.String("role", "", "role")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	v, err := s.PeerCheck(*role)
	if err != nil {
		return err
	}
	return output(v)
}
func doctorCmd(s *core.State, args []string) error {
	if len(args) > 0 {
		return core.E(2, "doctor takes no arguments")
	}
	v, err := s.Doctor()
	if err != nil {
		if v != nil {
			_ = output(v)
		}
		return err
	}
	return output(v)
}
func reconcileCmd(s *core.State, args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	role := fs.String("role", "", "role")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	v, err := s.Reconcile(*role)
	if err != nil {
		return err
	}
	return output(v)
}
func syncCmd(s *core.State, args []string) error {
	if len(args) > 0 {
		return core.E(2, "sync takes no arguments")
	}
	v, err := s.Sync(context.Background())
	if err != nil {
		return err
	}
	return output(v)
}
func qaMergeCmd(s *core.State, args []string) error {
	id, args := leadingID(args)
	fs := flag.NewFlagSet("qa-merge", flag.ContinueOnError)
	by := fs.String("by", "", "QA worker")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	if id == "" {
		return core.E(2, "card id required")
	}
	v, err := s.QAMerge(context.Background(), id, *by)
	if err != nil {
		return err
	}
	return output(v)
}
func syncDoneCmd(s *core.State, args []string) error {
	if len(args) > 0 {
		return core.E(2, "sync-done takes no arguments")
	}
	v, err := s.SyncDone(context.Background())
	if err != nil {
		return err
	}
	return output(v)
}
func markStaleCmd(s *core.State, args []string) error {
	fs := flag.NewFlagSet("mark-stale", flag.ContinueOnError)
	moved := fs.Bool("base-moved", false, "base moved")
	id := fs.String("merged-card", "", "merged card")
	sha := fs.String("base-sha", "", "new base SHA")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	if !*moved || *id == "" || *sha == "" {
		return core.E(2, "--base-moved, --merged-card and --base-sha required")
	}
	v, err := s.MarkStale(*id, *sha)
	if err != nil {
		return err
	}
	return output(v)
}

func groomCmd(s *core.State, args []string) error {
	fs := flag.NewFlagSet("groom", flag.ContinueOnError)
	file := fs.String("file", "", "approved card JSON")
	planFile := fs.String("plan-file", "", "approved parent and sub-issue plan JSON")
	by := fs.String("approved-by", "", "approver identity")
	listProjects := fs.Bool("list-projects", false, "list available Linear projects")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	if *listProjects {
		if *file != "" || *planFile != "" || *by != "" {
			return core.E(2, "--list-projects cannot be combined with card creation")
		}
		v, err := s.LinearProjects(context.Background())
		if err != nil {
			return err
		}
		return output(v)
	}
	if *file != "" && *planFile != "" {
		return core.E(2, "--file and --plan-file are mutually exclusive")
	}
	if *planFile != "" {
		b, err := os.ReadFile(*planFile)
		if err != nil {
			return err
		}
		v, err := s.GroomPlanCreate(context.Background(), b, *by)
		if err != nil {
			if v != nil {
				_ = output(v)
			}
			return err
		}
		return output(v)
	}
	if *file == "" {
		return core.E(2, "--file required")
	}
	b, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	v, err := s.GroomCreate(context.Background(), b, *by)
	if err != nil {
		return err
	}
	return output(v)
}
func configCmd(s *core.State, args []string) error {
	if len(args) < 1 {
		return core.E(2, "config get|set required")
	}
	switch args[0] {
	case "get":
		key := ""
		if len(args) > 1 {
			key = args[1]
		}
		v, err := s.ConfigGet(key)
		if err != nil {
			return err
		}
		return output(v)
	case "set":
		if len(args) != 3 {
			return core.E(2, "config set KEY VALUE")
		}
		v, err := s.ConfigSet(args[1], args[2])
		if err != nil {
			return err
		}
		return output(map[string]any{"key": args[1], "value": v})
	default:
		return core.E(2, "config get|set required")
	}
}
func startupCmd(s *core.State, args []string) error {
	if len(args) != 1 {
		return core.E(2, "startup enable|disable required")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if err := s.Startup(args[0], exe, filepath.Join(home, ".env")); err != nil {
		return err
	}
	return output(map[string]any{"startup": args[0]})
}

func gcCmd(s *core.State, args []string) error {
	if len(args) > 0 {
		return core.E(2, "gc-worktrees takes no arguments")
	}
	v, err := s.GCWorktrees(context.Background())
	if err != nil {
		return err
	}
	return output(v)
}
func stopCmd(s *core.State, args []string) error {
	if len(args) > 0 {
		return core.E(2, "stop takes no arguments")
	}
	if err := s.Stop(); err != nil {
		return err
	}
	return output(map[string]any{"stopped": true})
}
func startCmd(s *core.State, args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "strict env file")
	once := fs.Bool("once", false, "run until current workers finish")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	if *envFile == "" {
		*envFile = defaultEnvFile()
	}
	if *envFile != "" {
		if err := loadStrictEnv(*envFile, []string{"LINEAR_API_TOKEN"}, true); err != nil {
			return err
		}
	}
	if s.Config.Runner.ProviderPath == "" {
		return core.E(2, "provider_path is not configured")
	}
	if info, err := os.Stat(s.Config.Runner.ProviderPath); err != nil || info.Mode().Perm()&0111 == 0 {
		return core.E(2, "provider CLI unavailable")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return s.RunSupervisor(context.Background(), exe, *once)
}
func restartCmd(s *core.State, args []string) error {
	if err := s.Stop(); err != nil {
		return err
	}
	return startCmd(s, args)
}

func loadStrictEnv(path string, allow []string, override ...bool) error {
	info, err := os.Stat(path)
	if err != nil {
		return core.E(2, "env file: %v", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return core.E(2, "env file permissions must be 0600 or stricter")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return core.E(2, "env file must be owned by current user")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, k := range allow {
		allowed[k] = true
	}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 || !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(parts[0]) {
			return core.E(2, "invalid env file line %d", i+1)
		}
		if !allowed[parts[0]] {
			continue
		}
		if strings.ContainsAny(parts[1], "`\n\r") || strings.Contains(parts[1], "$(") {
			return core.E(2, "unsafe env value at line %d", i+1)
		}
		_, exists := os.LookupEnv(parts[0])
		if !exists || (len(override) > 0 && override[0]) {
			if err := os.Setenv(parts[0], parts[1]); err != nil {
				return err
			}
		}
	}
	return nil
}
func runnerCmd(args []string) error {
	fs := flag.NewFlagSet("runner", flag.ContinueOnError)
	provider := fs.String("provider", "", "codex or claude")
	role := fs.String("role", "", "dev or qa")
	if err := fs.Parse(args); err != nil {
		return core.E(2, "%v", err)
	}
	if (*provider != "codex" && *provider != "claude") || (*role != "dev" && *role != "qa") {
		return core.E(2, "runner requires valid --provider and --role")
	}
	b, err := core.ReadEnvelope(os.Stdin)
	if err != nil {
		return err
	}
	var envelope core.RunnerEnvelope
	if err := json.Unmarshal(b, &envelope); err != nil {
		return core.E(2, "invalid envelope: %v", err)
	}
	if envelope.Provider != *provider || envelope.Role != *role {
		return core.E(2, "runner argv/envelope mismatch")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	result, err := core.RunProvider(ctx, b)
	if err != nil {
		return err
	}
	return output(result)
}
func output(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

const help = `loopctl - durable local AI workflow queue

Usage: loopctl <command> [options]

Commands:
  init       initialize one state root for one repository
  add        add a validated recovery card
  list       list cards
  status     report orchestrator state
  claim      atomically claim a card
  move       apply an allowed role transition
  findings   record QA findings
  resolve    resolve attention/cancellation as a human
  reconcile  recover stale claims
  sync       import approved Linear backlog issues
  qa-merge   merge a QA-verified PR into dev
  sync-done  confirm merges and close cards
  mark-stale selectively invalidate overlapping reviews
  groom      create an explicitly approved Linear backlog issue
  config     read or update safe configuration keys
  startup    enable or disable the macOS LaunchAgent
  gc-worktrees remove verified terminal worktrees
  heartbeat  update role heartbeat
  peer-check inspect peer liveness
  doctor     verify and recover durable state
  start      start the local supervisor
  stop       safely stop new claims
  restart    stop and start
  version    print version

Exit codes: 0 success; 2 invalid; 3 empty; 4 contention; 5 backpressure;
6 stopped/deadline; 7 invariant failure; 8 external integration unavailable;
10 retryable runner failure; 11 runner result needs human attention.
`
