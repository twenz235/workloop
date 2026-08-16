# Linear contract

The configured Linear workspace and team are the source of truth for requirements, priority, approval, labels, and the user-facing board status. The local filesystem is an internal runtime for claims, retries, reservations, workers, and crash recovery; it is not a second board. `loopctl list` and the `counts` field from `loopctl status` use the latest Linear snapshot stored on each imported card. Private execution phases and `runtime_counts` exist only for diagnosis and scheduling; they are never a board or eligibility source. `loopctl init` discovers the token's workspace and selects its sole team automatically; when several teams exist, the user supplies only `--linear-team KEY`. Workloop contains no organization-specific defaults.

An approved issue stays in `Backlog` with `loop:ready`. `loopctl sync` validates its fenced `loop-card` JSON, creates exactly one internal card keyed by Linear UUID, stores the Linear state/labels snapshot, and moves Linear to `Todo`. If a non-terminal issue has a valid approved `loop-card` but is missing `loop:ready`, sync restores that label automatically. A plan parent is a special roll-up card: it starts without `loop:ready`, becomes eligible only when every direct Linear child is `Done`, and is then promoted automatically to `loop:ready` + `In Review`; QA verifies it on the current integrated `origin/dev` without a PR, writes the QA receipt, and `sync-done` performs the guarded `Done` transition. Claims and worker transitions enqueue the corresponding Linear state/label change and flush it immediately when possible. A periodic sync refreshes the snapshot and retries any outbox action that could not be delivered. Transitions into `needs_attention`, `rework`, `blocked`, or `cancelled`, plus explicit QA recovery, also enqueue a structured Linear comment with the reason, what is needed, how to fix it, and a recommendation. A `system/sync` transition never echoes the observed Linear state back to the board, but it still delivers the attention label and diagnostic comment. `needs_attention` retains the last Linear execution state and adds `loop:needs-attention`; the local runtime remains private.

Required labels:

- `loop:ready`: explicit approval and Definition of Ready passed; sync repairs the label from a valid `approved_at`/`approved_by` loop-card when the label is missing
- `loop:needs-attention`: automation paused for a human decision
- exactly one of `type:feature`, `type:bug`, or `type:maintenance`: required work classification

For backwards compatibility, sync also recognizes the legacy Linear labels
`Feature`, `Bug`, and `Maintenance` (case-insensitive); new Groom-created issues
continue to use the explicit `type:*` labels.

Every approved issue must also belong to an available Linear project and use a non-empty priority: Urgent (1), High (2), Medium (3), or Low (4). Acceptance criteria are rendered as Markdown checkboxes for humans and retained as an array in the fenced contract for deterministic validation. QA must return one result per criterion (`passed`, `failed`, `blocked`, or `not_run`) with concrete evidence. Workloop posts an idempotent QA report comment to the issue and checks only the boxes whose result is `passed`; it does not rewrite the fenced contract. A QA merge is rejected until every criterion is present and passed. Optional screenshots and diagrams appear as HTTPS Markdown images and as `visuals` entries containing alt text, URL, and a textual description. Local paths, base64 payloads, and secret-bearing URLs are forbidden.

Large requests use one parent issue plus 2–20 Linear sub-issues. The plan explicitly sets the parent mode to `create` or `reuse`; reuse requires the existing Linear UUID and identifier and validates its title, team, project, Backlog state, archive state, and absence of `loop:ready` before any child is created. The parent contains the overall outcome, roll-up acceptance checklist, and an executable `execution_mode: rollup` contract, but no `loop:ready` until children finish. Each sub-issue must pass Definition of Ready and receives its own `loop:ready`. When all direct children are Done, sync promotes the parent automatically, QA runs every parent criterion against the exact current `origin/dev` snapshot, and only the receipt-backed `sync-done` path can mark the parent Done. Sibling dependencies are resolved from plan keys to Linear UUIDs before issue creation, and Linear sync orders dependencies before dependents so one polling pass can import the complete graph. Plan and issue UUIDs are deterministic, making partial creation safely resumable.

Linear's actual parent relationship is authoritative. Sync refreshes local `linear_parent_id` metadata when a child is moved without treating that administrative move as a contract change.

When Linear is unavailable, active leases continue locally but the supervisor
does not claim new work or treat the cached board as fresh; it retries on the
next interval. `loopctl status` exposes `linear_snapshot_stale: true` until a
successful sync. Set `LINEAR_API_TOKEN` in the process environment or pass a
strict owner-only env file. Tokens must never appear in cards, config, journals,
plist files, or output.
