package core

import (
	"fmt"
	"strings"
)

// linearCommentNeeded keeps normal claim/progress noise out of Linear while
// making every exceptional or human-owned transition explain itself.
func linearCommentNeeded(from, to, actor, note string) bool {
	if from == to {
		return to == "in_review" && linearNeedsReviewComment(note)
	}
	switch to {
	case "needs_attention", "blocked", "rework", "cancelled":
		return true
	case "in_review":
		if from == "claimed-qa" || linearNeedsReviewComment(note) {
			return true
		}
	case "todo":
		lower := strings.ToLower(note)
		return strings.Contains(lower, "retry") || strings.Contains(lower, "runner") || strings.Contains(lower, "timeout") || strings.Contains(lower, "base sync") || strings.Contains(lower, "origin/dev")
	}
	if from == "needs_attention" && !strings.HasPrefix(actor, "system/sync") && !strings.HasPrefix(actor, "system/linear") {
		return true
	}
	return false
}

func linearNeedsReviewComment(note string) bool {
	lower := strings.ToLower(note)
	for _, marker := range []string{"stale", "base moved", "retry", "recovered", "changed during qa", "review head changed"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func linearDefaultTransitionNote(from, to string) string {
	switch to {
	case "needs_attention":
		return "automation paused: human review is required"
	case "blocked":
		return "dependencies are not complete"
	case "rework":
		return "QA found a blocking issue; Dev must rework the PR"
	case "cancelled":
		return "work was cancelled before completion"
	case "in_review":
		return "QA must re-check the PR"
	case "todo":
		return "automation will retry the Dev attempt"
	default:
		return fmt.Sprintf("transition %s -> %s requires review", from, to)
	}
}

func linearCommentValue(value string) string {
	value = safeHumanMarkdown(strings.Join(strings.Fields(value), " "))
	const maxRunes = 1600
	runes := []rune(value)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "…"
	}
	return value
}

func linearCommentMarker(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "<!-- workloop-comment:") && strings.HasSuffix(line, "-->") {
			return line
		}
	}
	return ""
}

func linearBlockingFindings(card *Card) string {
	if card == nil {
		return ""
	}
	lines := []string{}
	for _, finding := range card.QAFindings {
		if finding.Severity != "blocking" {
			continue
		}
		location := finding.File
		if finding.Line > 0 {
			location = fmt.Sprintf("%s:%d", finding.File, finding.Line)
		}
		line := fmt.Sprintf("- `%s`: %s", linearCommentValue(location), linearCommentValue(finding.Issue))
		if finding.Violates != "" {
			line += fmt.Sprintf(" (violates %s)", linearCommentValue(finding.Violates))
		}
		if finding.Evidence != "" {
			line += fmt.Sprintf(" — evidence: %s", linearCommentValue(finding.Evidence))
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func linearTransitionComment(card *Card, from, to, actor, note string) string {
	reason := strings.TrimSpace(note)
	if reason == "" {
		reason = linearDefaultTransitionNote(from, to)
	}
	needed := "The owner must review the evidence before automation continues."
	fix := "Inspect the card, PR, and verification output, then apply the safe workflow command for this status."
	recommendation := "Keep the Linear issue visible and record the decision in the next transition note."
	diagnosticCode, diagnosticPhase, diagnosticLog := "", "", ""

	switch to {
	case "needs_attention":
		needed = "A human decision and any missing evidence are required before automation resumes."
		fix = "Review the PR, contract, and QA findings; fix the cause or choose `resolve`/`qa-retry` explicitly."
		recommendation = "Keep `loop:needs-attention` until the decision and remediation are recorded."
		if card != nil && card.SpecChanged {
			fix = "Re-groom or re-approve the changed Linear contract before sending the card back to a worker."
			recommendation = "Do not QA-merge the old implementation while `spec_changed` is true."
		}
		if findings := linearBlockingFindings(card); findings != "" {
			needed = "The following blocking QA findings need an owner and a verified fix:\n" + findings
		}
	case "rework":
		needed = "Dev must fix every blocking QA finding and push a new PR head."
		if findings := linearBlockingFindings(card); findings != "" {
			fix = "Address these findings, rerun verification, and keep the fix on the existing branch:\n" + findings
		} else {
			fix = "Inspect the QA evidence, fix the PR on its existing branch, and rerun the recorded verification."
		}
		recommendation = "Return the card to In Review only after the new head is ready for a fresh QA claim."
	case "blocked":
		needed = "The dependency or external condition blocking this card must be completed or clarified."
		fix = "Check the dependency cards and Linear approval; do not bypass the dependency gate with a manual claim."
		recommendation = "Let the next sync/unblock pass move the card when its prerequisites are complete."
	case "cancelled":
		needed = "No further automation is expected for this card."
		fix = "If the work is still needed, create or re-approve the appropriate Linear issue instead of reviving local state."
		recommendation = "Keep the cancellation decision in the audit trail and do not merge the card."
	case "in_review":
		needed = "QA must claim the card and verify the current PR head against the latest `dev` base."
		fix = "Refresh the QA worktree, rerun acceptance and named GitHub checks, and record fresh evidence before merge."
		recommendation = "Use `loopctl claim --role qa` next; merge only through `qa-merge` after the head is revalidated."
	case "todo":
		needed = "A Dev attempt should inspect the recorded failure before retrying."
		fix = "Read the worker result/journal, correct the transient failure if needed, and retry while attempts remain."
		recommendation = "Keep the existing recovery note and avoid creating a duplicate card or branch."
		lower := strings.ToLower(reason)
		if strings.Contains(lower, "base sync") || strings.Contains(lower, "origin/dev") {
			needed = "Dev must bring the existing branch up to the latest origin/dev and rerun verification."
			fix = "In the existing worktree, fetch --no-tags origin dev, merge --no-edit origin/dev without rebase/reset, resolve conflicts, rerun verification, commit, and push."
			recommendation = "Do not move the card to In Review until the base-sync gate passes."
		}
	}
	if diagnostic, ok := parseRunnerDiagnostic(reason); ok {
		reason = diagnostic.Why
		diagnosticCode = diagnostic.Code
		diagnosticPhase = diagnostic.Phase
		diagnosticLog = diagnostic.Log
		if diagnostic.Needed != "" {
			needed = linearCommentValue(diagnostic.Needed)
		}
		if diagnostic.Fix != "" {
			fix = linearCommentValue(diagnostic.Fix)
		}
		if diagnostic.Recommendation != "" {
			recommendation = linearCommentValue(diagnostic.Recommendation)
		}
	}

	marker := Hash([]byte(fmt.Sprintf("%s|%s|%s|%s|%d", cardIDForComment(card), from, to, actor, historyLengthForComment(card))))
	diagnosticBlock := ""
	if diagnosticCode != "" {
		diagnosticBlock = fmt.Sprintf("- **Code:** `%s`\n- **Phase:** %s\n", linearCommentValue(diagnosticCode), linearCommentValue(diagnosticPhase))
		if diagnosticLog != "" {
			diagnosticBlock += fmt.Sprintf("- **Log:** `%s`\n", linearCommentValue(diagnosticLog))
		}
	}
	return fmt.Sprintf("<!-- workloop-comment:%s -->\n## Workloop status: `%s`\n\n%s- **Why:** %s\n- **Needed:** %s\n- **Fix:** %s\n- **Recommendation:** %s\n\nTransition: `%s` → `%s` by `%s`.",
		marker,
		linearCommentValue(to),
		diagnosticBlock,
		linearCommentValue(reason),
		needed,
		fix,
		linearCommentValue(recommendation),
		linearCommentValue(from),
		linearCommentValue(to),
		linearCommentValue(actor),
	)
}

func cardIDForComment(card *Card) string {
	if card == nil {
		return "unknown"
	}
	return card.ID
}

func historyLengthForComment(card *Card) int {
	if card == nil {
		return 0
	}
	return len(card.History)
}

func linearImportAttentionComment(issue linearIssue, reason string) string {
	return fmt.Sprintf("<!-- workloop-comment:%s -->\n## Workloop status: `needs_attention`\n\n- **Why:** %s\n- **Needed:** A human must repair the issue contract before Workloop can import it.\n- **Fix:** %s\n- **Recommendation:** Keep `loop:needs-attention` and add `loop:ready` only after the corrected contract passes validation.",
		Hash([]byte(issue.ID+"|"+reason)),
		linearCommentValue(reason),
		linearCommentValue("Restore a valid loop-card block, required labels, project, priority, and dependency data; then run loopctl sync again."),
	)
}
