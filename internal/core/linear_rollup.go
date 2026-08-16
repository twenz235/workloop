package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// annotatePlanParents marks issues that own one or more Linear child issues.
// A parent becomes eligible for roll-up QA only after every direct child is
// Done. Linear remains authoritative for the child states.
func annotatePlanParents(issues []linearIssue, doneState string) {
	childrenByParent := map[string][]string{}
	stateByID := map[string]string{}
	for _, issue := range issues {
		stateByID[issue.ID] = issue.StateName
		if issue.ParentID != "" {
			childrenByParent[issue.ParentID] = append(childrenByParent[issue.ParentID], issue.ID)
		}
	}
	for i := range issues {
		children := childrenByParent[issues[i].ID]
		if len(children) == 0 {
			continue
		}
		issues[i].PlanParent = true
		issues[i].PlanChildrenComplete = true
		for _, childID := range children {
			if stateByID[childID] != doneState {
				issues[i].PlanChildrenComplete = false
				break
			}
		}
	}
}

func (s *State) ensurePlanParentInReview(ctx context.Context, client *linearClient, issue *linearIssue, inReviewStateID *string) error {
	if !issue.PlanParent || !issue.PlanChildrenComplete || !containsString(issue.Labels, s.Config.Linear.ReadyLabel) || containsString(issue.Labels, s.Config.Linear.NeedsAttentionLabel) || issue.StateName == s.Config.Linear.StatusMap["in_review"] || issue.StateName == s.Config.Linear.StatusMap["done"] || issue.StateName == "Canceled" || issue.StateName == "Cancelled" || issue.StateName == "Duplicate" {
		return nil
	}
	if strings.TrimSpace(*inReviewStateID) == "" {
		stateID, err := s.linearStateID(ctx, client, s.Config.Linear.StatusMap["in_review"])
		if err != nil {
			return E(8, "Linear roll-up In Review lookup failed: %v", err)
		}
		*inReviewStateID = stateID
	}
	if err := client.updateState(ctx, issue.ID, *inReviewStateID); err != nil {
		return E(8, "Linear roll-up state promotion failed for %s: %v", issue.Identifier, err)
	}
	issue.StateName = s.Config.Linear.StatusMap["in_review"]
	return nil
}

func rollupParentContract(cfg *Config, parent map[string]any, cards map[string]map[string]any, approvedBy string) map[string]any {
	touches := stringSlice(parent["touches"])
	verification := stringSlice(parent["verification"])
	for _, card := range cards {
		touches = append(touches, stringSlice(card["touches"])...)
		verification = append(verification, stringSlice(card["verification"])...)
	}
	touches = uniqueStrings(touches)
	verification = uniqueStrings(verification)
	if len(touches) == 0 {
		touches = []string{"."}
	}
	if len(verification) == 0 {
		verification = []string{"Run the child acceptance verification on the integrated dev branch"}
	}
	outOfScope := stringSlice(parent["out_of_scope"])
	if len(outOfScope) == 0 {
		outOfScope = []string{"Child issues own implementation; this parent verifies the integrated outcome."}
	}
	tier := strings.TrimSpace(fmt.Sprint(parent["tier"]))
	if tier == "" || tier == "<nil>" {
		tier = "L2"
	}
	rollback := strings.TrimSpace(fmt.Sprint(parent["rollback_notes"]))
	if rollback == "" || rollback == "<nil>" {
		rollback = "Keep completed child reports intact; revert only the roll-up verification change if it introduces a regression."
	}
	risk := parent["risk"]
	if risk == nil {
		risk = map[string]any{"level": "medium"}
	}
	approvedAt := strings.TrimSpace(fmt.Sprint(parent["approved_at"]))
	if approvedAt == "" || approvedAt == "<nil>" {
		approvedAt = Now()
	}
	if strings.TrimSpace(approvedBy) == "" {
		approvedBy = strings.TrimSpace(fmt.Sprint(parent["approved_by"]))
	}
	if approvedBy == "" || approvedBy == "<nil>" {
		approvedBy = "system/rollup"
	}
	return map[string]any{
		"execution_mode":  "rollup",
		"problem":         parent["problem"],
		"desired_outcome": parent["desired_outcome"],
		"out_of_scope":    outOfScope,
		"repo":            cfg.Repo,
		"repo_path":       cfg.RepoPath,
		"base":            "dev",
		"tier":            tier,
		"touches":         touches,
		"acceptance":      stringSlice(parent["acceptance"]),
		"verification":    verification,
		"depends_on":      []string{},
		"risk":            risk,
		"rollback_notes":  rollback,
		"approved_at":     approvedAt,
		"approved_by":     approvedBy,
	}
}

func rollupParentBlock(cfg *Config, parent map[string]any, cards map[string]map[string]any, approvedBy string) (string, error) {
	block, err := json.MarshalIndent(rollupParentContract(cfg, parent, cards, approvedBy), "", "  ")
	if err != nil {
		return "", err
	}
	return string(block), nil
}
