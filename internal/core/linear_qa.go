package core

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var acceptanceChecklistLineRE = regexp.MustCompile(`^(\s*-\s*)\[[ xX]\](\s+.*)$`)

func (c *linearClient) issueDescription(ctx context.Context, issueID string) (string, error) {
	const q = `query LoopIssueDescription($id: String!) { issue(id: $id) { id description } }`
	var data struct {
		Issue *struct {
			ID          string
			Description string
		}
	}
	if err := c.graphql(ctx, q, map[string]any{"id": issueID}, &data); err != nil {
		return "", err
	}
	if data.Issue == nil || data.Issue.ID == "" {
		return "", fmt.Errorf("Linear issue %q not found", issueID)
	}
	return data.Issue.Description, nil
}

func (c *linearClient) updateDescription(ctx context.Context, issueID, description string) error {
	const q = `mutation UpdateLoopIssueDescription($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success } }`
	var data struct {
		IssueUpdate struct {
			Success bool
		}
	}
	if err := c.graphql(ctx, q, map[string]any{
		"id":    issueID,
		"input": map[string]any{"description": description},
	}, &data); err != nil {
		return err
	}
	if !data.IssueUpdate.Success {
		return fmt.Errorf("Linear issueUpdate description failed")
	}
	return nil
}

// mapAcceptanceChecklist changes only the generated Markdown checklist under
// the Acceptance criteria heading. The loop-card JSON block is deliberately
// left byte-for-byte intact so the contract hash remains stable.
func mapAcceptanceChecklist(description string, results []AcceptanceResult) (string, error) {
	lines := strings.Split(description, "\n")
	inAcceptance := false
	foundHeading := false
	checklistCount := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "## Acceptance criteria" {
			inAcceptance = true
			foundHeading = true
			continue
		}
		if inAcceptance && (strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "```loop-card")) {
			inAcceptance = false
		}
		if !inAcceptance {
			continue
		}
		match := acceptanceChecklistLineRE.FindStringSubmatch(line)
		if len(match) != 3 {
			continue
		}
		checklistCount++
		result, ok := acceptanceResultAt(results, checklistCount)
		checked := ok && result.Status == "passed"
		mark := " "
		if checked {
			mark = "x"
		}
		lines[i] = match[1] + "[" + mark + "]" + match[2]
	}
	if !foundHeading || checklistCount == 0 {
		return "", fmt.Errorf("Linear issue has no Acceptance criteria checklist")
	}
	for _, result := range results {
		if result.CriterionIndex > checklistCount {
			return "", fmt.Errorf("QA acceptance result criterion_index %d has no Linear checkbox", result.CriterionIndex)
		}
	}
	return strings.Join(lines, "\n"), nil
}

func storedAcceptanceResultsComplete(card *Card) error {
	if card == nil {
		return fmt.Errorf("QA card is missing")
	}
	if len(card.Acceptance) == 0 {
		return nil
	}
	if err := validateAcceptanceResults(card.QAAcceptanceResults, len(card.Acceptance), "completed"); err != nil {
		return err
	}
	return nil
}

func cardSHA(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func qaReportComment(card *Card) string {
	parts := []string{card.ID, cardSHA(card.BaseSHA), cardSHA(card.TestedHeadSHA)}
	for index, criterion := range card.Acceptance {
		result, ok := acceptanceResultAt(card.QAAcceptanceResults, index+1)
		status := "not_run"
		evidence := "No result was returned for this criterion."
		if ok {
			status = result.Status
			evidence = result.Evidence
		}
		parts = append(parts, fmt.Sprintf("%d|%s|%s|%s", index+1, status, criterion, evidence))
	}
	marker := Hash([]byte(strings.Join(parts, "\n")))
	lines := []string{
		fmt.Sprintf("<!-- workloop-comment:qa-report:%s -->", marker),
		"## Workloop QA acceptance report",
		"",
		fmt.Sprintf("- **Base SHA:** `%s`", linearCommentValue(cardSHA(card.BaseSHA))),
		fmt.Sprintf("- **Tested head:** `%s`", linearCommentValue(cardSHA(card.TestedHeadSHA))),
		"",
	}
	for index, criterion := range card.Acceptance {
		result, ok := acceptanceResultAt(card.QAAcceptanceResults, index+1)
		status := "not_run"
		evidence := "No result was returned for this criterion."
		if ok {
			status = result.Status
			evidence = result.Evidence
		}
		mark := " "
		if status == "passed" {
			mark = "x"
		}
		lines = append(lines,
			fmt.Sprintf("- [%s] %d. %s — **%s**", mark, index+1, linearCommentValue(criterion), linearCommentValue(status)),
			fmt.Sprintf("  - Evidence: %s", linearCommentValue(evidence)),
		)
	}
	return strings.Join(lines, "\n")
}

// publishQAReport is idempotent: the description update is a deterministic
// replacement and addComment deduplicates using the report marker. It is kept
// outside the Linear outbox because a QA merge must not proceed without the
// remote acceptance evidence being recorded.
func (s *State) publishQAReport(ctx context.Context, id string) error {
	if !s.Config.Linear.Enabled {
		return nil
	}
	_, _, _, card, err := s.readCardPath(id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(card.LinearIssueUUID) == "" || len(card.QAAcceptanceResults) == 0 {
		return nil
	}
	client, err := s.linearClient()
	if err != nil {
		return err
	}
	description, err := client.issueDescription(ctx, card.LinearIssueUUID)
	if err != nil {
		return fmt.Errorf("read Linear acceptance checklist: %w", err)
	}
	updated, err := mapAcceptanceChecklist(description, card.QAAcceptanceResults)
	if err != nil {
		return err
	}
	if updated != description {
		if err := client.updateDescription(ctx, card.LinearIssueUUID, updated); err != nil {
			return fmt.Errorf("update Linear acceptance checklist: %w", err)
		}
	}
	if err := client.addComment(ctx, card.LinearIssueUUID, qaReportComment(card)); err != nil {
		return fmt.Errorf("post Linear QA report: %w", err)
	}
	return nil
}
