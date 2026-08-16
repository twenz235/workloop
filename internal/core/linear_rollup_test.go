package core

import "testing"

func TestAnnotatePlanParentsRequiresEveryDirectChildDone(t *testing.T) {
	issues := []linearIssue{
		{ID: "parent", Identifier: "FLO-P", StateName: "Backlog"},
		{ID: "done-child", Identifier: "FLO-C1", ParentID: "parent", StateName: "Done"},
		{ID: "open-child", Identifier: "FLO-C2", ParentID: "parent", StateName: "In Progress"},
	}
	annotatePlanParents(issues, "Done")
	if !issues[0].PlanParent || issues[0].PlanChildrenComplete {
		t.Fatalf("parent=%+v, want incomplete roll-up", issues[0])
	}
	issues[2].StateName = "Done"
	annotatePlanParents(issues, "Done")
	if !issues[0].PlanChildrenComplete {
		t.Fatalf("parent=%+v, want complete roll-up", issues[0])
	}
}

func TestRollupParentContractUnionsChildVerification(t *testing.T) {
	cfg := Config{Repo: "twenz235/arun", RepoPath: "/workspace/arun"}
	parent := map[string]any{
		"problem":         "p",
		"desired_outcome": "o",
		"acceptance":      []string{"integrated behavior works"},
		"touches":         []string{"apps/api"},
		"verification":    []string{"go test ./apps/api"},
		"risk":            map[string]any{"level": "low"},
	}
	cards := map[string]map[string]any{
		"one": {"touches": []string{"apps/api", "packages/db"}, "verification": []string{"go test ./packages/db"}},
	}
	contract := rollupParentContract(&cfg, parent, cards, "human/teerapad")
	if contract["execution_mode"] != "rollup" || contract["approved_by"] != "human/teerapad" {
		t.Fatalf("contract=%v", contract)
	}
	if got := stringSlice(contract["touches"]); len(got) != 2 || got[0] != "apps/api" || got[1] != "packages/db" {
		t.Fatalf("touches=%v", got)
	}
	if got := stringSlice(contract["verification"]); len(got) != 2 {
		t.Fatalf("verification=%v", got)
	}
}
