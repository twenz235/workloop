# Automatic splitting

## Size decision

Keep one executable card only when one worker can deliver one coherent outcome in one PR and QA can verify it as one unit. Split automatically when any of these signals applies:

- two or more outcomes can be accepted independently;
- work crosses separable boundaries such as schema, backend, client, migration, or documentation;
- one part is a prerequisite for another;
- acceptance has more than eight items or verification needs unrelated test suites;
- `touches` spans unrelated ownership areas;
- rollback or risk differs materially between parts;
- the work is unlikely to fit one focused Dev session and one QA cycle.

Do not split only by file or technical layer when that would create unusable fragments. Prefer independently testable vertical slices. Create a foundation card only when later cards genuinely require it.

## Plan rules

- Create one parent issue for the overall outcome. It is human-facing and never receives `loop:ready`.
- Create 2–20 executable sub-issues. Every sub-issue must independently pass the full Definition of Ready.
- If one outcome needs more than 20 executable cards, group it into multiple coherent parent plans and preview all plans for one approval; do not silently truncate work.
- Inherit the parent's work type, Linear project, and priority unless a sub-issue materially differs.
- Use short unique lowercase keys matching `[a-z0-9][a-z0-9-]{0,31}`.
- Express internal dependencies with `depends_on_keys`. Keep existing external Linear UUID dependencies in `depends_on`.
- Reject unknown keys, self-dependencies, and cycles. Show topological execution waves in the preview.
- Avoid a final vague “integrate everything” card. Add one only when it has observable behavior and distinct verification.

## Plan JSON

```json
{
  "operation_id": "00000000-0000-4000-8000-000000000001",
  "parent": {
    "title": "Support configurable package pricing",
    "problem": "Pricing changes require code edits",
    "desired_outcome": "Operators can safely change package pricing modes",
    "acceptance": ["All approved sub-issues are complete"],
    "work_type": "feature",
    "linear_project_id": "linear-project-uuid",
    "linear_project": "Billing",
    "priority": 2
  },
  "cards": [
    {
      "key": "pricing-model",
      "title": "Add the pricing mode model",
      "problem": "The domain has no pricing mode representation",
      "desired_outcome": "The domain represents free, increase, decrease, and per-package modes",
      "out_of_scope": ["Operator UI"],
      "tier": "L1",
      "touches": ["internal/pricing/**"],
      "acceptance": ["All supported modes validate deterministically"],
      "verification": ["go test ./internal/pricing/..."],
      "depends_on": [],
      "depends_on_keys": [],
      "risk": {"level": "medium"},
      "rollback_notes": "Revert the model commit"
    },
    {
      "key": "pricing-api",
      "title": "Expose pricing mode changes through the API",
      "problem": "Operators cannot change the pricing model",
      "desired_outcome": "The API safely applies supported pricing changes",
      "out_of_scope": ["Operator UI"],
      "tier": "L1",
      "touches": ["internal/api/**"],
      "acceptance": ["Valid changes persist", "Invalid changes are rejected"],
      "verification": ["go test ./internal/api/..."],
      "depends_on": [],
      "depends_on_keys": ["pricing-model"],
      "risk": {"level": "medium"},
      "rollback_notes": "Revert the API commit"
    }
  ]
}
```

The CLI fills repo/base/audit fields and lets sub-issues inherit missing `work_type`, `linear_project_id`, `linear_project`, and `priority` from the parent. Use one stable `operation_id` and rerun the same file after partial failure.
