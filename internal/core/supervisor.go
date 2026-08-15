package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type workerDone struct {
	cardID, role string
	contractHash string
	attempt      int
	baseSHA      string
	headSHA      string
	result       *RunnerResult
	err          error
}

func (s *State) RunSupervisor(ctx context.Context, executable string, once bool) error {
	lockPath := filepath.Join(s.Root, "runtime", "supervisor.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return E(2, "supervisor is already running")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := s.ClearStop(); err != nil {
		return err
	}
	runtime := map[string]any{"pid": os.Getpid(), "started_at": Now(), "executable": executable}
	b, _ := Encode(runtime)
	if err := writeAtomic(filepath.Join(s.Root, "runtime", "supervisor.json"), b, 0600); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(s.Root, "runtime", "supervisor.json"))
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	done := make(chan workerDone, 16)
	active := map[string]bool{}
	lastSync := time.Time{}
	for {
		if _, err := os.Stat(filepath.Join(s.Root, "STOP")); err == nil {
			return nil
		}
		if s.Config.Linear.Enabled && (lastSync.IsZero() || time.Since(lastSync) >= time.Duration(s.Config.Linear.SyncIntervalSec)*time.Second) {
			// Linear is authoritative for new intake, but an outage must not
			// interrupt workers that already hold a local lease. Keep the last
			// successful snapshot (and its stale marker) and retry on the next
			// interval; Claim will fail closed until a fresh snapshot exists.
			if _, syncErr := s.Sync(ctx); syncErr != nil && ExitCode(syncErr) != 8 {
				return syncErr
			}
			lastSync = time.Now()
		}
		_, _ = s.Reconcile("dev")
		_, _ = s.Reconcile("qa")
		_, _ = s.SyncDone(ctx)
		for {
			select {
			case d := <-done:
				delete(active, d.role+":"+d.cardID)
				s.finishWorker(ctx, d)
				s.flushLinearBestEffort(ctx)
			default:
				goto drained
			}
		}
	drained:
		for role, limit := range map[string]int{"dev": s.Config.Dev.MaxWorkers, "qa": s.Config.QA.MaxWorkers} {
			running := 0
			for _, key := range sortedKeys(active) {
				if strings.HasPrefix(key, role+":") {
					running++
				}
			}
			for running < limit {
				card, err := s.ClaimAndSync(ctx, role, "supervisor-"+strconv.Itoa(os.Getpid()))
				if err != nil {
					if code := ExitCode(err); code == 3 || code == 4 || code == 5 || code == 8 {
						break
					}
					return err
				}
				id, _ := card["id"].(string)
				key := role + ":" + id
				active[key] = true
				running++
				go s.launchWorker(ctx, executable, id, role, done)
			}
		}
		if once {
			if len(active) == 0 {
				return nil
			}
			select {
			case d := <-done:
				delete(active, d.role+":"+d.cardID)
				s.finishWorker(ctx, d)
				s.flushLinearBestEffort(ctx)
			case <-ctx.Done():
				return nil
			}
			continue
		}
		select {
		case d := <-done:
			delete(active, d.role+":"+d.cardID)
			s.finishWorker(ctx, d)
			s.flushLinearBestEffort(ctx)
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return nil
		}
	}
}

func (s *State) flushLinearBestEffort(ctx context.Context) {
	if s.Config.Linear.Enabled {
		_, _ = s.FlushLinearOutbox(ctx)
	}
}

func (s *State) launchWorker(ctx context.Context, executable, id, role string, done chan<- workerDone) {
	envelope, err := s.prepareEnvelope(ctx, id, role)
	if err != nil {
		done <- workerDone{cardID: id, role: role, err: err}
		return
	}
	b, _ := json.Marshal(envelope)
	cmd := exec.Command(executable, "runner", "--provider", envelope.Provider, "--role", role)
	cmd.Stdin = bytes.NewReader(b)
	logDir := filepath.Dir(envelope.OutputPath)
	_ = os.MkdirAll(logDir, 0700)
	log, logErr := os.OpenFile(filepath.Join(logDir, fmt.Sprintf("%d.%s.log", envelope.Attempt, role)), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if logErr == nil {
		defer log.Close()
		cmd.Stdout = log
		cmd.Stderr = log
	}
	err = cmd.Start()
	workerRecord := filepath.Join(s.Root, "runtime", "workers", role+"-"+id+".json")
	if err == nil {
		record, _ := Encode(map[string]any{"pid": cmd.Process.Pid, "card_id": id, "role": role, "started_at": Now()})
		_ = writeAtomic(workerRecord, record, 0600)
		err = cmd.Wait()
		_ = os.Remove(workerRecord)
	}
	var result RunnerResult
	if readErr := readJSON(envelope.OutputPath, &result); readErr != nil && err == nil {
		err = readErr
	}
	done <- workerDone{cardID: id, role: role, contractHash: envelope.ContractHash, attempt: envelope.Attempt, baseSHA: envelope.BaseSHA, headSHA: envelope.HeadSHA, result: &result, err: err}
}

func (s *State) prepareEnvelope(ctx context.Context, id, role string) (RunnerEnvelope, error) {
	_, _, raw, c, err := s.readCardPath(id)
	if err != nil {
		return RunnerEnvelope{}, err
	}
	attempt := c.Attempts + 1
	var worktree, branch, baseSHA, headSHA string
	baseSyncRequired := false
	if role == "dev" {
		branch = "loop/" + id
		worktree = filepath.Join(s.Config.WorktreeRoot, id)
		var fetchErr error
		baseSHA, fetchErr = fetchBaseSHA(ctx, s.Config.RepoPath, s.Config.Base)
		if fetchErr != nil {
			return RunnerEnvelope{}, fetchErr
		}
		headSHA = baseSHA
		if _, err := os.Stat(worktree); errors.Is(err, os.ErrNotExist) {
			args := []string{"-C", s.Config.RepoPath, "worktree", "add", "-b", branch, worktree, baseSHA}
			if gitOutput(s.Config.RepoPath, "show-ref", "--verify", "--hash", "refs/heads/"+branch) != "" {
				args = []string{"-C", s.Config.RepoPath, "worktree", "add", worktree, branch}
			}
			cmd := exec.CommandContext(ctx, "git", args...)
			if out, e := cmd.CombinedOutput(); e != nil {
				return RunnerEnvelope{}, E(8, "git worktree add failed: %s", out)
			}
		} else if branchNow := gitOutput(worktree, "rev-parse", "--abbrev-ref", "HEAD"); branchNow != branch {
			return RunnerEnvelope{}, E(7, "existing Dev worktree is on branch %q, want %q", branchNow, branch)
		}
		var syncErr error
		baseSyncRequired, syncErr = syncDevWorktreeBase(ctx, worktree, baseSHA)
		if syncErr != nil {
			return RunnerEnvelope{}, syncErr
		}
	} else {
		n, err := prNumber(c.PR)
		if err != nil {
			return RunnerEnvelope{}, E(2, "%v", err)
		}
		p, err := s.prView(ctx, n)
		if err != nil {
			return RunnerEnvelope{}, err
		}
		headSHA = p.HeadRefOid
		var fetchErr error
		baseSHA, fetchErr = fetchBaseSHA(ctx, s.Config.RepoPath, s.Config.Base)
		if fetchErr != nil {
			return RunnerEnvelope{}, fetchErr
		}
		worktree = filepath.Join(s.Config.WorktreeRoot, id+"-qa")
		if _, err := os.Stat(worktree); errors.Is(err, os.ErrNotExist) {
			cmd := exec.CommandContext(ctx, "git", "-C", s.Config.RepoPath, "worktree", "add", "--detach", worktree, headSHA)
			if out, e := cmd.CombinedOutput(); e != nil {
				return RunnerEnvelope{}, E(8, "QA worktree add failed: %s", out)
			}
		} else if headNow := gitOutput(worktree, "rev-parse", "HEAD"); headNow != headSHA {
			cmd := exec.CommandContext(ctx, "git", "-C", s.Config.RepoPath, "worktree", "remove", worktree)
			if out, e := cmd.CombinedOutput(); e != nil {
				return RunnerEnvelope{}, E(8, "stale QA worktree removal failed: %s", out)
			}
			cmd = exec.CommandContext(ctx, "git", "-C", s.Config.RepoPath, "worktree", "add", "--detach", worktree, headSHA)
			if out, e := cmd.CombinedOutput(); e != nil {
				return RunnerEnvelope{}, E(8, "QA worktree refresh failed: %s", out)
			}
		}
	}
	patch := map[string]any{"base_sha": baseSHA, "attempts": attempt}
	if role == "qa" {
		patch["tested_head_sha"] = headSHA
	} else {
		patch["worktree"] = worktree
		patch["branch"] = branch
	}
	if _, err := s.PatchInternal(id, patch, "prepare worker"); err != nil {
		return RunnerEnvelope{}, err
	}
	cardData, _ := json.Marshal(raw)
	output := filepath.Join(s.Root, "journal", "workers", id, fmt.Sprintf("%d.result.json", attempt))
	return RunnerEnvelope{Version: 1, CardID: id, Role: role, Attempt: attempt, Provider: s.Config.Runner.Provider, ProviderPath: s.Config.Runner.ProviderPath, StateRoot: s.Root, Worktree: worktree, Branch: branch, BaseRef: s.Config.Base, BaseSHA: baseSHA, BaseSyncRequired: baseSyncRequired, HeadSHA: headSHA, ContractHash: c.ContractHash, OutputPath: output, Card: cardData}, nil
}

func (s *State) PatchInternal(id string, patch map[string]any, note string) (map[string]any, error) {
	var out map[string]any
	err := s.withLock(func() error {
		status, _, _, _, e := s.readCardPath(id)
		if e != nil {
			return e
		}
		out, e = s.moveLocked(id, status, "system", note, patch, true)
		return e
	})
	return out, err
}

func (s *State) finishWorker(ctx context.Context, d workerDone) {
	if d.err != nil {
		_, _ = s.withMoveInternal(d.cardID, "needs_attention", d.role+"/supervisor", "runner failed: "+safeError(d.err.Error()), nil)
		return
	}
	if d.result == nil {
		return
	}
	status, _, _, current, readErr := s.readCardPath(d.cardID)
	expected := "claimed-" + d.role
	if readErr != nil || status != expected || current.ContractHash != d.contractHash || current.Attempts != d.attempt {
		return
	}
	if d.role == "qa" {
		latestBaseSHA, baseErr := fetchBaseSHA(ctx, s.Config.RepoPath, s.Config.Base)
		if baseErr != nil {
			_, _ = s.withMoveInternal(d.cardID, "needs_attention", "qa/supervisor", baseErr.Error(), nil)
			return
		}
		if latestBaseSHA != d.baseSHA || current.TestedHeadSHA == nil || *current.TestedHeadSHA != d.headSHA || d.result.HeadSHA != d.headSHA {
			_, _ = s.withMoveInternal(d.cardID, "in_review", "qa/supervisor", "base or review head changed during QA", map[string]any{"stale": true, "claimed_at": nil, "claimed_by": nil})
			return
		}
	}
	if d.result.Outcome == "needs_attention" {
		_, _ = s.withMoveInternal(d.cardID, "needs_attention", d.role+"/supervisor", d.result.Error, nil)
		return
	}
	if d.result.Outcome == "retryable" {
		if current.Attempts >= current.MaxAttempts {
			_, _ = s.withMoveInternal(d.cardID, "needs_attention", d.role+"/supervisor", "maximum attempts reached", nil)
			return
		}
		to := "todo"
		if d.role == "qa" {
			to = "in_review"
		}
		_, _ = s.withMoveInternal(d.cardID, to, d.role+"/supervisor", d.result.Error, map[string]any{"claimed_at": nil, "claimed_by": nil})
		return
	}
	if d.role == "dev" {
		latestBaseSHA, baseErr := fetchBaseSHA(ctx, s.Config.RepoPath, s.Config.Base)
		if baseErr != nil {
			_, _ = s.withMoveInternal(d.cardID, "needs_attention", "dev/supervisor", baseErr.Error(), nil)
			return
		}
		worktree := filepath.Join(s.Config.WorktreeRoot, d.cardID)
		if current.Worktree != nil && *current.Worktree != "" {
			worktree = *current.Worktree
		}
		headNow := gitOutput(worktree, "rev-parse", "HEAD")
		clean := strings.TrimSpace(gitOutput(worktree, "status", "--porcelain")) == ""
		baseReady := clean && headNow == d.result.HeadSHA && gitIsAncestor(ctx, worktree, latestBaseSHA, headNow)
		if !baseReady {
			to := "todo"
			if current.Attempts >= current.MaxAttempts {
				to = "needs_attention"
			}
			note := fmt.Sprintf("dev base advanced to %s; merge origin/%s into the branch and rerun verification before review", latestBaseSHA, s.Config.Base)
			if !clean {
				note = "Dev worktree is not clean; preserve the changes, sync the latest base, then rerun verification before review"
			}
			_, _ = s.withMoveInternal(d.cardID, to, "dev/supervisor", note, map[string]any{"base_sha": latestBaseSHA, "stale": true, "claimed_at": nil, "claimed_by": nil})
			return
		}
		n, err := prNumber(d.result.PR)
		if err != nil {
			_, _ = s.withMoveInternal(d.cardID, "needs_attention", "dev/supervisor", "runner did not return a PR", nil)
			return
		}
		p, err := s.prView(ctx, n)
		if err != nil || p.BaseRefName != "dev" || p.HeadRefName != "loop/"+d.cardID || p.HeadRefOid != d.result.HeadSHA {
			note := "PR fact does not match runner result"
			if err != nil {
				note = err.Error()
			}
			_, _ = s.withMoveInternal(d.cardID, "needs_attention", "dev/supervisor", note, nil)
			return
		}
		_, _ = s.withMoveInternal(d.cardID, "in_review", "dev/supervisor", "runner completed after base refresh", map[string]any{"branch": d.result.Branch, "pr": d.result.PR, "base_sha": latestBaseSHA, "tested_head_sha": d.result.HeadSHA, "stale": false, "claimed_at": nil, "claimed_by": nil})
		return
	}
	_, _ = s.PatchInternal(d.cardID, map[string]any{"qa_evidence": d.result.Evidence, "stale": false}, "QA evidence")
	if _, err := s.QAMerge(ctx, d.cardID, "qa/supervisor"); err != nil {
		_, _ = s.withMoveInternal(d.cardID, "needs_attention", "qa/supervisor", err.Error(), nil)
		return
	}
	_, _ = s.SyncDone(ctx)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
