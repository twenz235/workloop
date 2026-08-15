package core

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

const runnerDiagnosticMarker = "WORKLOOP_RUNNER_DIAGNOSTIC"

type runnerDiagnostic struct {
	Code           string
	Role           string
	Attempt        int
	Phase          string
	Why            string
	Needed         string
	Fix            string
	Recommendation string
	Log            string
}

func runnerDiagnosticNote(d runnerDiagnostic) string {
	lines := []string{runnerDiagnosticMarker}
	appendField := func(key, value string) {
		value = diagnosticText(value)
		if value != "" {
			lines = append(lines, key+": "+value)
		}
	}
	appendField("code", d.Code)
	appendField("role", d.Role)
	if d.Attempt > 0 {
		appendField("attempt", strconv.Itoa(d.Attempt))
	}
	appendField("phase", d.Phase)
	appendField("why", d.Why)
	appendField("needed", d.Needed)
	appendField("fix", d.Fix)
	appendField("recommendation", d.Recommendation)
	appendField("log", d.Log)
	return strings.Join(lines, "\n")
}

func parseRunnerDiagnostic(note string) (runnerDiagnostic, bool) {
	var d runnerDiagnostic
	lines := strings.Split(strings.TrimSpace(note), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != runnerDiagnosticMarker {
		return d, false
	}
	for _, line := range lines[1:] {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "code":
			d.Code = value
		case "role":
			d.Role = value
		case "attempt":
			d.Attempt, _ = strconv.Atoi(value)
		case "phase":
			d.Phase = value
		case "why":
			d.Why = value
		case "needed":
			d.Needed = value
		case "fix":
			d.Fix = value
		case "recommendation":
			d.Recommendation = value
		case "log":
			d.Log = value
		}
	}
	return d, d.Why != ""
}

func diagnosticText(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	const maxLength = 1200
	runes := []rune(value)
	if len(runes) > maxLength {
		return string(runes[:maxLength]) + "…"
	}
	return value
}

func runnerLogReference(cardID, role string, attempt int) string {
	if cardID == "" || role == "" || attempt <= 0 {
		return ""
	}
	return filepath.ToSlash(filepath.Join("journal", "workers", cardID, fmt.Sprintf("%d.%s.log", attempt, role)))
}

func runnerFailureNote(cardID, role string, attempt int, stage string, err error) string {
	return runnerFailureNoteWithLog(cardID, role, attempt, stage, err, "")
}

func runnerFailureNoteWithLog(cardID, role string, attempt int, stage string, err error, logOutput string) string {
	if err == nil {
		err = fmt.Errorf("worker failed without an error")
	}
	raw := diagnosticText(err.Error())
	logOutput = diagnosticText(logOutput)
	classificationText := raw
	if logOutput != "" {
		classificationText += " " + logOutput
	}
	lower := strings.ToLower(classificationText)
	code, phase, why := "runner-failed", "worker execution", "The worker stopped before producing a trustworthy result: "+raw
	switch {
	case stage == "qa-base-check":
		code = "qa-base-freshness"
		phase = "QA base freshness gate"
		why = "QA could not prove that its evidence was checked against the latest origin/dev: " + raw
	case stage == "result-validation":
		code = "invalid-worker-result"
		phase = "worker result validation"
		why = "The worker result did not match the PR or required completion evidence: " + raw
	case strings.Contains(lower, "provider failed:"):
		code = "provider-failed"
		phase = "provider execution"
		index := strings.Index(lower, "provider failed:")
		detail := strings.TrimSpace(classificationText[index+len("provider failed:"):])
		why = "The worker provider stopped before returning a valid result: " + diagnosticText(detail)
	case strings.Contains(lower, "invalid result"), strings.Contains(lower, "identity mismatch"), strings.Contains(lower, "invalid runner outcome"), strings.Contains(lower, "requires evidence"):
		code = "invalid-worker-result"
		phase = "worker result validation"
		why = "The provider returned a result that Workloop could not safely accept: " + raw
	case strings.Contains(lower, "too vague"), strings.Contains(lower, "requires a concrete error"), strings.Contains(lower, "result error"):
		code = "invalid-worker-result"
		phase = "worker result validation"
		why = "The provider returned an incomplete failure explanation: " + raw
	case strings.Contains(lower, "origin/dev"), strings.Contains(lower, "worktree"), strings.Contains(lower, "git "), strings.Contains(lower, "merge"):
		code = "base-sync-or-worktree"
		phase = "Git worktree preparation"
		why = "Workloop could not safely prepare or verify the worker worktree: " + raw
	case stage == "prepare":
		code = "worker-setup-failed"
		phase = "worker setup"
		why = "Workloop could not prepare the worker before the provider started: " + raw
	case stage == "result-read":
		code = "missing-worker-result"
		phase = "worker result collection"
		why = "The provider exited, but Workloop could not read a valid result file: " + raw
	case stage == "provider":
		code = "provider-failed"
		phase = "provider execution"
		why = "The worker provider exited before returning a valid result: " + raw
	}
	if logOutput != "" {
		why += " Worker log excerpt: " + logOutput
	}

	needed, fix, recommendation := runnerFailureGuidance(role, "failure", cardID, attempt)
	return runnerDiagnosticNote(runnerDiagnostic{
		Code:           code,
		Role:           role,
		Attempt:        attempt,
		Phase:          phase,
		Why:            why,
		Needed:         needed,
		Fix:            fix,
		Recommendation: recommendation,
		Log:            runnerLogReference(cardID, role, attempt),
	})
}

func runnerAttemptsExhaustedNote(cardID, role string, attempt int) string {
	log := runnerLogReference(cardID, role, attempt)
	logHint := "Read the supervisor journal and inspect the latest PR/worktree state"
	if log != "" {
		logHint = "Read " + log + " and inspect the latest PR/worktree state"
	}
	return runnerDiagnosticNote(runnerDiagnostic{
		Code:           "max-attempts",
		Role:           role,
		Attempt:        attempt,
		Phase:          "supervisor retry policy",
		Why:            "The worker reached the configured retry limit without producing an accepted result.",
		Needed:         "A human must inspect the last worker evidence and decide whether to change the contract, repair the work, or cancel the card.",
		Fix:            diagnosticText(logHint + "; fix the root cause, then use loopctl resolve or qa-retry with an explicit note."),
		Recommendation: "Do not keep retrying blindly; record the owner and the concrete remediation before resuming automation.",
		Log:            log,
	})
}

func runnerResultNote(cardID string, result *RunnerResult) string {
	if result == nil {
		return runnerFailureNote(cardID, "unknown", 0, "result-read", fmt.Errorf("runner returned no result"))
	}
	outcome := result.Outcome
	why := diagnosticText(result.Error)
	if why == "" {
		why = fmt.Sprintf("The worker returned %s without explaining the blocking condition.", outcome)
	}
	code := "worker-retryable"
	if outcome == "needs_attention" {
		code = "worker-needs-attention"
	}
	needed, fix, recommendation := runnerFailureGuidance(result.Role, outcome, cardID, result.Attempt)
	return runnerDiagnosticNote(runnerDiagnostic{
		Code:           code,
		Role:           result.Role,
		Attempt:        result.Attempt,
		Phase:          "worker report",
		Why:            why,
		Needed:         needed,
		Fix:            fix,
		Recommendation: recommendation,
		Log:            runnerLogReference(cardID, result.Role, result.Attempt),
	})
}

func runnerFailureGuidance(role, outcome, cardID string, attempt int) (string, string, string) {
	log := runnerLogReference(cardID, role, attempt)
	logHint := "Inspect the supervisor journal and worker worktree."
	if log != "" {
		logHint = "Read " + log + " from the state root and inspect the worker worktree."
	}
	if role == "qa" {
		if outcome == "retryable" {
			return "A fresh QA result with evidence and the tested base/head SHAs is required before merge.",
				logHint + " Verify the exact PR head against the latest origin/dev, fix the transient cause, then use qa-retry for a fresh QA attempt.",
				"Do not merge or mark Done from stale or incomplete QA evidence."
		}
		return "A human must review the QA evidence and decide whether the PR needs a fix, a retry, or resolution.",
			logHint + " Check the exact PR head, acceptance output, and base SHA; fix the cause or run qa-retry only after the retry is justified.",
			"Keep the card in needs_attention until the decision and fresh evidence are recorded."
	}
	if outcome == "retryable" {
		return "A Dev result with verification evidence, base_sha, head_sha, and a matching PR is required before In Review.",
			logHint + " Fix the reported cause in the existing worktree, rerun verification, commit, push, and let the supervisor retry.",
			"Do not move the card to In Review manually; retry only after the cause is fixed."
	}
	return "A human must determine whether the task contract, repository, provider, or implementation is blocking progress.",
		logHint + " Inspect the branch, PR, verification output, and card contract; fix the root cause or use loopctl resolve with an explicit note.",
		"Keep needs_attention until the cause and next owner are recorded; do not discard the existing worktree."
}

func vagueRunnerError(value string) bool {
	normalized := strings.Trim(strings.ToLower(diagnosticText(value)), " .!?:;")
	switch normalized {
	case "", "blocked", "failed", "failure", "error", "needs attention", "needs_attention", "retry", "retryable", "qa failed", "dev failed":
		return true
	default:
		return false
	}
}
