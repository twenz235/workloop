package core

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type groomPlanInput struct {
	OperationID string           `json:"operation_id"`
	Parent      map[string]any   `json:"parent"`
	Cards       []map[string]any `json:"cards"`
}

func (s *State) GroomPlanCreate(ctx context.Context, data []byte, approvedBy string) (map[string]any, error) {
	if strings.TrimSpace(approvedBy) == "" {
		return nil, E(2, "--approved-by is required")
	}
	var plan groomPlanInput
	if err := json.Unmarshal(data, &plan); err != nil {
		return nil, E(2, "invalid plan JSON: %v", err)
	}
	if len(plan.Parent) == 0 || len(plan.Cards) < 2 || len(plan.Cards) > 20 {
		return nil, E(2, "a groom plan requires one parent and 2..20 executable cards")
	}
	planID := plan.OperationID
	if planID == "" {
		planID = deterministicUUID(append(append([]byte(nil), data...), []byte("|"+approvedBy+"|plan")...))
	}
	if !uuidPattern.MatchString(planID) {
		return nil, E(2, "operation_id must be a UUID")
	}
	parent, err := normalizePlanParent(plan.Parent)
	if err != nil {
		return nil, err
	}
	order, cards, err := s.preparePlanCards(planID, parent, plan.Cards, approvedBy)
	if err != nil {
		return nil, err
	}
	if err := s.preflightPlanLinear(ctx, parent, cards); err != nil {
		return nil, err
	}

	waves := planExecutionWaves(order, cards)
	parentResult, err := s.createPlanParent(ctx, planID, parent, order, cards)
	if err != nil {
		return nil, err
	}
	parentID, _ := parentResult["id"].(string)
	results := []map[string]any{}
	createdIDs := map[string]string{}
	for _, key := range order {
		card := cloneMap(cards[key])
		deps := stringSlice(card["depends_on"])
		for _, dependencyKey := range stringSlice(card["depends_on_keys"]) {
			dependencyID := createdIDs[dependencyKey]
			if dependencyID == "" {
				return planPartialResult(planID, parentResult, results, key, order, waves), E(7, "plan dependency %s has no created Linear issue", dependencyKey)
			}
			deps = append(deps, dependencyID)
		}
		card["depends_on"] = uniqueStrings(deps)
		card["linear_parent_id"] = parentID
		delete(card, "key")
		delete(card, "depends_on_keys")
		encoded, _ := json.Marshal(card)
		created, createErr := s.GroomCreate(ctx, encoded, approvedBy)
		if createErr != nil {
			return planPartialResult(planID, parentResult, results, key, order, waves), createErr
		}
		created["key"] = key
		results = append(results, created)
		createdIDs[key], _ = created["id"].(string)
	}
	return map[string]any{"plan_id": planID, "parent": parentResult, "cards": results, "execution_order": order, "execution_waves": waves, "resumable": true, "complete": true}, nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func normalizePlanParent(input map[string]any) (map[string]any, error) {
	parent := cloneMap(input)
	for _, key := range []string{"title", "problem", "desired_outcome", "work_type", "linear_project_id", "linear_project"} {
		if strings.TrimSpace(fmt.Sprint(parent[key])) == "" || fmt.Sprint(parent[key]) == "<nil>" {
			return nil, E(2, "parent.%s is required", key)
		}
	}
	workType := fmt.Sprint(parent["work_type"])
	if workType != "feature" && workType != "bug" && workType != "maintenance" {
		return nil, E(2, "parent.work_type must be feature, bug or maintenance")
	}
	priority, ok := integerValue(parent["priority"])
	if !ok || priority < 1 || priority > 4 {
		return nil, E(2, "parent.priority must be 1..4")
	}
	parent["priority"] = priority
	acceptance := stringSlice(parent["acceptance"])
	if len(acceptance) == 0 {
		return nil, E(2, "parent.acceptance must be non-empty")
	}
	parent["acceptance"] = acceptance
	if visuals, exists := parent["visuals"]; exists {
		b, _ := json.Marshal(visuals)
		var decoded []Visual
		if json.Unmarshal(b, &decoded) != nil {
			return nil, E(2, "parent.visuals is invalid")
		}
		for _, visual := range decoded {
			if err := validateVisual(visual); err != nil {
				return nil, err
			}
		}
	}
	return parent, nil
}

func (s *State) preparePlanCards(planID string, parent map[string]any, inputs []map[string]any, approvedBy string) ([]string, map[string]map[string]any, error) {
	cards := map[string]map[string]any{}
	inputOrder := []string{}
	for _, input := range inputs {
		card := cloneMap(input)
		key := strings.TrimSpace(fmt.Sprint(card["key"]))
		if !cardIDPattern.MatchString(key) || cards[key] != nil {
			return nil, nil, E(2, "card key must be unique and match %s", cardIDPattern.String())
		}
		for _, field := range []string{"work_type", "linear_project_id", "linear_project", "priority"} {
			if _, exists := card[field]; !exists {
				card[field] = parent[field]
			}
		}
		card["operation_id"] = deterministicUUID([]byte(planID + "|" + key))
		card["depends_on_keys"] = stringSlice(card["depends_on_keys"])
		if _, exists := card["depends_on"]; !exists {
			card["depends_on"] = []string{}
		}
		if err := s.validateGroomDraft(card, approvedBy); err != nil {
			return nil, nil, E(2, "card %s: %v", key, err)
		}
		cards[key] = card
		inputOrder = append(inputOrder, key)
	}
	for key, card := range cards {
		for _, dependencyKey := range stringSlice(card["depends_on_keys"]) {
			if dependencyKey == key || cards[dependencyKey] == nil {
				return nil, nil, E(2, "card %s has invalid dependency key %s", key, dependencyKey)
			}
		}
	}
	order, err := topoPlanOrder(inputOrder, cards)
	if err != nil {
		return nil, nil, err
	}
	return order, cards, nil
}

func (s *State) validateGroomDraft(input map[string]any, approvedBy string) error {
	raw := cloneMap(input)
	delete(raw, "key")
	delete(raw, "depends_on_keys")
	raw["id"] = "preview"
	raw["repo"] = s.Config.Repo
	raw["repo_path"] = s.Config.RepoPath
	raw["base"] = "dev"
	raw["linear_issue_id"] = "pending"
	raw["linear_issue_uuid"] = "pending"
	raw["linear_url"] = "pending"
	raw["source_revision"] = Now()
	raw["approved_at"] = Now()
	raw["approved_by"] = approvedBy
	b, _ := json.Marshal(raw)
	_, _, err := DecodeCard(b, &s.Config)
	return err
}

func (s *State) preflightPlanLinear(ctx context.Context, parent map[string]any, cards map[string]map[string]any) error {
	projects, err := s.LinearProjects(ctx)
	if err != nil {
		return err
	}
	availableProjects := map[string]string{}
	for _, project := range projects {
		availableProjects[project.ID] = project.Name
	}
	all := []map[string]any{parent}
	for _, card := range cards {
		all = append(all, card)
	}
	client, err := s.linearClient()
	if err != nil {
		return err
	}
	seenTypes := map[string]bool{}
	externalDependencies := map[string]bool{}
	for _, item := range all {
		projectID := fmt.Sprint(item["linear_project_id"])
		if availableProjects[projectID] != fmt.Sprint(item["linear_project"]) {
			return E(2, "Linear project %q (%s) is not available to team %s", item["linear_project"], projectID, s.Config.Linear.Team)
		}
		workType := fmt.Sprint(item["work_type"])
		if !seenTypes[workType] {
			if _, err := s.linearLabelID(ctx, client, "type:"+workType); err != nil {
				return E(8, "Linear work type label lookup failed: %v", err)
			}
			seenTypes[workType] = true
		}
		for _, dependency := range stringSlice(item["depends_on"]) {
			externalDependencies[dependency] = true
		}
	}
	for dependency := range externalDependencies {
		if _, ok, err := client.findIssue(ctx, dependency); err != nil {
			return E(8, "Linear dependency lookup failed: %v", err)
		} else if !ok {
			return E(2, "Linear dependency %s does not exist", dependency)
		}
	}
	if _, err := s.linearLabelID(ctx, client, s.Config.Linear.ReadyLabel); err != nil {
		return E(8, "Linear label lookup failed: %v", err)
	}
	if _, err := s.linearStateID(ctx, client, s.Config.Linear.StatusMap["backlog"]); err != nil {
		return E(8, "Linear backlog lookup failed: %v", err)
	}
	return nil
}

func (s *State) createPlanParent(ctx context.Context, planID string, parent map[string]any, order []string, cards map[string]map[string]any) (map[string]any, error) {
	client, err := s.linearClient()
	if err != nil {
		return nil, err
	}
	if existing, ok, e := client.findIssue(ctx, planID); e == nil && ok {
		existing["plan_id"] = planID
		return existing, nil
	}
	backlogID, err := s.linearStateID(ctx, client, s.Config.Linear.StatusMap["backlog"])
	if err != nil {
		return nil, E(8, "Linear backlog lookup failed: %v", err)
	}
	workType := fmt.Sprint(parent["work_type"])
	typeLabelID, err := s.linearLabelID(ctx, client, "type:"+workType)
	if err != nil {
		return nil, E(8, "Linear work type label lookup failed: %v", err)
	}
	acceptance := interfaceSlice(parent["acceptance"])
	description := fmt.Sprintf("## Problem\n%s\n\n## Desired outcome\n%s\n\n## Acceptance criteria\n%s%s\n\n## Execution plan\n%s\n\n<!-- workloop-plan:%s -->", safeHumanMarkdown(fmt.Sprint(parent["problem"])), safeHumanMarkdown(fmt.Sprint(parent["desired_outcome"])), markdownChecklist(acceptance), markdownVisuals(parent["visuals"]), planCardChecklist(order, cards), planID)
	priority, _ := integerValue(parent["priority"])
	input := map[string]any{"id": planID, "teamId": s.Config.Linear.TeamID, "title": parent["title"], "description": description, "stateId": backlogID, "labelIds": []string{typeLabelID}, "projectId": parent["linear_project_id"], "priority": priority}
	const q = `mutation CreateGroomParent($input: IssueCreateInput!) { issueCreate(input: $input) { success issue { id identifier title url state { name } } } }`
	var response struct {
		IssueCreate struct {
			Success bool
			Issue   struct {
				ID, Identifier, Title, URL string
				State                      struct{ Name string }
			}
		}
	}
	if err := client.graphql(ctx, q, map[string]any{"input": input}, &response); err != nil {
		if existing, ok, _ := client.findIssue(ctx, planID); ok {
			existing["plan_id"] = planID
			return existing, nil
		}
		return nil, E(8, "Linear parent create failed: %v", err)
	}
	if !response.IssueCreate.Success {
		return nil, E(8, "Linear parent create failed")
	}
	i := response.IssueCreate.Issue
	return map[string]any{"id": i.ID, "identifier": i.Identifier, "title": i.Title, "url": i.URL, "status": i.State.Name, "plan_id": planID, "label": "type:" + workType}, nil
}

func topoPlanOrder(inputOrder []string, cards map[string]map[string]any) ([]string, error) {
	remaining := map[string]int{}
	for key, card := range cards {
		remaining[key] = len(uniqueStrings(stringSlice(card["depends_on_keys"])))
	}
	order := []string{}
	done := map[string]bool{}
	for len(order) < len(cards) {
		progress := false
		for _, key := range inputOrder {
			if done[key] || remaining[key] != 0 {
				continue
			}
			done[key] = true
			order = append(order, key)
			progress = true
			for other, card := range cards {
				if !done[other] && containsString(stringSlice(card["depends_on_keys"]), key) {
					remaining[other]--
				}
			}
		}
		if !progress {
			return nil, E(2, "groom plan dependency graph contains a cycle")
		}
	}
	return order, nil
}

func planCardChecklist(order []string, cards map[string]map[string]any) string {
	lines := make([]string, 0, len(order))
	for _, key := range order {
		dependencyText := ""
		if dependencies := stringSlice(cards[key]["depends_on_keys"]); len(dependencies) > 0 {
			dependencyText = " (after " + strings.Join(dependencies, ", ") + ")"
		}
		lines = append(lines, fmt.Sprintf("- [ ] **%s** — %s%s", key, cards[key]["title"], dependencyText))
	}
	return strings.Join(lines, "\n")
}

func planPartialResult(planID string, parent map[string]any, cards []map[string]any, failedKey string, order []string, waves [][]string) map[string]any {
	return map[string]any{"plan_id": planID, "parent": parent, "cards": cards, "failed_key": failedKey, "execution_order": order, "execution_waves": waves, "resumable": true, "complete": false}
}

func cloneMap(input map[string]any) map[string]any {
	b, _ := json.Marshal(input)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func stringSlice(value any) []string {
	b, _ := json.Marshal(value)
	var out []string
	_ = json.Unmarshal(b, &out)
	return out
}

func interfaceSlice(value any) []any {
	b, _ := json.Marshal(value)
	var out []any
	_ = json.Unmarshal(b, &out)
	return out
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func integerValue(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	}
	return 0, false
}

func planExecutionWaves(order []string, cards map[string]map[string]any) [][]string {
	level := map[string]int{}
	waves := [][]string{}
	for _, key := range order {
		cardLevel := 0
		for _, dependency := range stringSlice(cards[key]["depends_on_keys"]) {
			if level[dependency]+1 > cardLevel {
				cardLevel = level[dependency] + 1
			}
		}
		level[key] = cardLevel
		for len(waves) <= cardLevel {
			waves = append(waves, []string{})
		}
		waves[cardLevel] = append(waves[cardLevel], key)
	}
	return waves
}

func orderLinearIssuesByDependencies(issues []linearIssue) []linearIssue {
	byID := map[string]bool{}
	for _, issue := range issues {
		byID[issue.ID] = true
	}
	dependencies := map[string][]string{}
	for _, issue := range issues {
		match := loopCardRE.FindStringSubmatch(issue.Description)
		if len(match) != 2 {
			continue
		}
		var raw map[string]any
		if json.Unmarshal([]byte(match[1]), &raw) == nil {
			for _, dependency := range stringSlice(raw["depends_on"]) {
				if byID[dependency] {
					dependencies[issue.ID] = append(dependencies[issue.ID], dependency)
				}
			}
		}
	}
	ordered := make([]linearIssue, 0, len(issues))
	done := map[string]bool{}
	for len(ordered) < len(issues) {
		progress := false
		for _, issue := range issues {
			if done[issue.ID] {
				continue
			}
			ready := true
			for _, dependency := range dependencies[issue.ID] {
				if !done[dependency] {
					ready = false
					break
				}
			}
			if ready {
				done[issue.ID] = true
				ordered = append(ordered, issue)
				progress = true
			}
		}
		if !progress {
			for _, issue := range issues {
				if !done[issue.ID] {
					ordered = append(ordered, issue)
				}
			}
			break
		}
	}
	return ordered
}
