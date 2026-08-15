# Workloop CLI Reference

This is the complete reference for the `loopctl` command-line interface. Start with the [User Guide](user-guide.md) if you are setting up Workloop for the first time.

## Conventions

Run `loopctl` inside the managed Git repository. State-aware commands search upward from the current directory for `.loopctl/config.json`. `LOOPCTL_STATE_ROOT` overrides discovery, and `--state-root PATH` explicitly selects a state root:

```bash
loopctl --help
loopctl list --state-root /Users/alice/src/acme-app/.loopctl
```

Output is JSON except for help text and the long-running supervisor. Commands marked **internal** are called by the supervisor or worker adapters; use them manually only for diagnosis or a documented recovery procedure.

## Complete command list

| Command | Audience | Purpose |
| --- | --- | --- |
| `init` | user | Initialize one repository-bound state root. |
| `add` | recovery | Import a validated card JSON snapshot into the private runtime. |
| `list` | user | List and filter cards. |
| `status` | user | Report Linear board counts plus runtime capacity diagnostics. |
| `claim` | internal | Atomically claim the next eligible card. |
| `move` | advanced | Apply a role-authorized execution-phase transition. |
| `findings` | internal | Record structured QA findings. |
| `resolve` | user | Resolve a card requiring human attention or cancellation. |
| `qa-retry` | user | Return an attention card to In Review for a fresh QA attempt. |
| `reconcile` | recovery | Recover stale Dev or QA claims. |
| `sync` | user | Import approved Linear backlog issues. |
| `qa-merge` | internal | Merge a QA-verified PR into `dev`. |
| `sync-done` | recovery | Confirm merged PRs and complete their cards. |
| `mark-stale` | internal | Invalidate overlapping reviews after `dev` moves. |
| `groom` | skill/internal | Create an explicitly approved Linear backlog issue. |
| `config` | user | Read configuration or update safe keys. |
| `startup` | user | Manage the macOS LaunchAgent. |
| `gc-worktrees` | recovery | Remove verified terminal worktrees. |
| `heartbeat` | internal | Update a worker role heartbeat. |
| `peer-check` | diagnostic | Inspect peer-role liveness. |
| `doctor` | user | Validate and recover durable local state. |
| `start` | user | Run the local supervisor in the foreground. |
| `stop` | user | Safely prevent new claims and stop the supervisor. |
| `restart` | user | Stop and start the foreground supervisor. |
| `runner` | internal | Invoke the unified Codex or Claude adapter. |
| `version` | user | Print the Workloop version. |

## Setup and intake

### `loopctl init`

```text
loopctl init [--project NAME] [--repo-path PATH] [--state-root PATH]
             [--env-file PATH] [--linear-team KEY]
             [--linear-workspace NAME --linear-workspace-id UUID
              --linear-team KEY --linear-team-id UUID]
```

Initializes Workloop for one Git repository. With no options it discovers the current Git root, names the project after the repository, creates `<repo>/.loopctl`, reads `LINEAR_API_TOKEN` from `~/.env`, discovers the Linear workspace, and selects its only team.

Use `--linear-team KEY` when the workspace has multiple teams. The four workspace/team name and UUID options form one explicit binding and must be supplied together. `--offline` skips live Linear validation and is intended only for tests.

```bash
cd /Users/alice/src/acme-app
loopctl init --linear-team ENG
```

### `loopctl groom`

```text
loopctl groom --list-projects
loopctl groom --file APPROVED_CARD.json --approved-by ID
loopctl groom --plan-file APPROVED_PLAN.json --approved-by ID
```

`--list-projects` returns projects available to the configured Linear team. Card creation requires exactly one `work_type` (`feature`, `bug`, or `maintenance`), `linear_project_id`, `linear_project`, and priority 1–4. It creates an approved Linear issue in Backlog with `loop:ready`, the matching `type:*` label, project, and priority. Acceptance criteria also appear as a clickable checklist.

Normal users should invoke `$groom` in Codex or `/groom` in Claude Code; the skill prepares the JSON and requests explicit approval before it calls this command. Optional `visuals` entries use `{alt, url, description}` with a safe HTTPS URL and appear inline in the Linear description.

`--plan-file` creates or explicitly reuses one non-executable parent and creates 2–20 executable Linear sub-issues. Set `parent.mode` to `create`, or use `reuse` with `linear_issue_uuid` and `linear_issue_id` for an existing umbrella issue. Reuse validates the approved title, team, project, Backlog/archive state, and absence of `loop:ready`. Sub-issues may inherit type, project, and priority from the parent and use `depends_on_keys` to reference sibling keys. Workloop validates the entire graph before the first write, creates issues in topological order, gives only sub-issues `loop:ready`, and returns partial progress if Linear fails. Returned execution waves also account for overlapping `touches` and configured hot paths, so cards shown in one wave are safe for the scheduler to run concurrently. Rerun the exact file to resume idempotently.

```bash
loopctl groom --list-projects
loopctl groom --file /tmp/approved-card.json --approved-by alice
loopctl groom --plan-file /tmp/approved-plan.json --approved-by alice
```

### `loopctl sync`

```text
loopctl sync
```

Imports eligible `loop:ready` issues from the configured Linear Backlog, creates idempotent internal runtime cards, stores the Linear state and labels used by the board view, and moves accepted issues to Todo. For a non-terminal issue with a valid approved `loop-card` but no `loop:ready`, it restores the label before importing; parent plan issues without an executable `loop-card` are not promoted. It also refreshes those snapshots for cards already known and retries pending outbound state/label changes. It automatically reads `LINEAR_API_TOKEN` from `~/.env`.

```bash
loopctl sync
```

### `loopctl add`

```text
loopctl add (--file CARD.json | --stdin)
```

Imports a validated card snapshot into `.loopctl/cards/` and records its private
execution phase. It never creates or edits a Linear issue, and is a recovery
path—not the normal replacement for Groom → Linear → Sync.

```bash
loopctl add --file restored-card.json
loopctl add --stdin < restored-card.json
```

## Running and observing

### `loopctl start`

```text
loopctl start [--env-file PATH] [--once]
```

Runs the supervisor in the current terminal. It loads `~/.env` by default, performs an immediate sync, polls at the configured interval, and schedules Dev and QA workers. `--once` runs until currently available work finishes instead of remaining active.

```bash
loopctl start
loopctl start --env-file /Users/alice/.config/workloop.env
loopctl start --once
```

### `loopctl stop`

```text
loopctl stop
```

Sets the durable stop signal so the supervisor stops taking new claims and exits safely. Existing work follows the supervisor's shutdown policy.

### `loopctl restart`

```text
loopctl restart [--env-file PATH] [--once]
```

Runs `stop` and then starts a foreground supervisor with the same options as `start`.

### `loopctl startup`

```text
loopctl startup enable
loopctl startup disable
```

Enables or disables the macOS user LaunchAgent. `enable` is idempotent and launches Workloop automatically at login using `~/.env`. Use `startup` for background operation and `start` for a foreground terminal session; do not run both supervisors at once.

### `loopctl list`

```text
loopctl list [--status STATUS] [--linear LINEAR_ID]
```

Lists cards, optionally filtering by Linear state, Linear label, or Linear
identifier. The `status` field is the latest Linear board snapshot; local
execution phases are deliberately not returned as a second board.
When a card has a transition note, `last_note` contains the latest reason.
Runner failures use a structured diagnostic with a code, phase, cause, required
evidence, fix, recommendation, and a worker log path relative to the state
root; this is especially useful for `loop:needs-attention` cards.

```bash
loopctl list
loopctl list --status "In Review"
loopctl list --status loop:needs-attention
loopctl list --linear ENG-123
```

The private execution phases are `todo`, `rework`, `claimed-dev`, `in_review`,
`claimed-qa`, `needs_attention`, `blocked`, `cancelled`, and `done`. They are
lease/retry/recovery bookkeeping only. Linear states (`Backlog`, `Todo`, `In
Progress`, `In Review`, `Done`, `Canceled`) and labels are the user-facing
workflow and the only eligibility source.

### `loopctl status`

```text
loopctl status --role dev|qa
```

Reports orchestrator state for the selected role. `counts` is grouped by the
latest Linear board snapshot; `runtime_counts` is a private execution-phase
diagnostic used for leases and recovery, not a local board. A stale snapshot is
possible while Linear is unavailable; active claimed work can continue, but new
Linear intake is blocked until sync recovers. `linear_snapshot_stale: true`
means both `claimable_now` and new claims are fail-closed.

```bash
loopctl status --role dev
loopctl status --role qa
```

### `loopctl doctor`

```text
loopctl doctor
```

Recovers incomplete durable transactions, checks card/runtime invariants, and
reports state issues. Run it after a crash, manual interruption, or unexpected
result.

### `loopctl version`

```text
loopctl version
```

Prints the installed version as JSON.

## Configuration

### `loopctl config get`

```text
loopctl config get [KEY]
```

Prints all configuration or one dotted key.

```bash
loopctl config get
loopctl config get runner.provider
loopctl config get linear.sync_interval_sec
```

### `loopctl config set`

```text
loopctl config set KEY VALUE
```

Updates one explicitly mutable setting:

| Key | Accepted value |
| --- | --- |
| `linear.sync_interval_sec` | Integer from 60 to 3600. Default: 300. |
| `runner.provider` | `codex` or `claude`; also discovers and records its executable. |
| `runner.provider_path` | Absolute path to an executable provider CLI. |
| `dev.max_workers` | Positive integer. Default: 3. |
| `qa.max_workers` | Positive integer. Default: 2. |
| `limits.max_in_flight` | Positive integer. |
| `limits.max_open_prs` | Positive integer. |

```bash
loopctl config set runner.provider codex
loopctl config set linear.sync_interval_sec 300
loopctl config set dev.max_workers 2
```

## Human resolution and recovery

### `loopctl resolve`

```text
loopctl resolve CARD_ID --to todo|rework|cancelled
                --by human/ID --note TEXT [--close-pr]
```

Resolves a card whose Linear issue needs attention. A human identity and
non-empty audit note are mandatory. The command updates private recovery data
and queues the corresponding Linear state/label mutation. `todo` requires no PR
or dirty worktree; `rework` requires an existing branch; `cancelled` requires
`--close-pr` when its PR is open. Cancellation is also allowed from `todo` or
`blocked`.

```bash
loopctl resolve eng-123 --to rework --by human/alice \
  --note "Apply the newly approved acceptance criteria"
```

### `loopctl qa-retry`

```text
loopctl qa-retry CARD_ID --by human/ID --note TEXT
```

Returns a `needs_attention` card with an existing PR to `In Review` so QA can
claim it again. The human note is required and is mirrored to Linear as an
audit comment. The command refuses changed contracts and unresolved blocking
QA findings; use `resolve --to rework` when Dev must fix the PR first. Previous
QA evidence is cleared, while stale facts remain until the next QA run proves
the current head and base again.

```bash
loopctl qa-retry eng-123 --by human/alice \
  --note "The provider outage is resolved; rerun QA on the current PR"
```

### `loopctl reconcile`

```text
loopctl reconcile --role dev|qa
```

Recovers stale claims for one role according to recorded heartbeats, claim age,
execution phase, Git state, and PR state. Use it after `doctor` when a worker
died or a machine was interrupted.

### `loopctl sync-done`

```text
loopctl sync-done
```

Checks cards in the merge phase against GitHub, marks only confirmed merges done, performs one immediate full Linear sync when a card is completed, and completes related cleanup. It is safe to use when a QA merge succeeded but local completion was interrupted.

### `loopctl gc-worktrees`

```text
loopctl gc-worktrees
```

Removes worktrees only for terminal cards after verifying they are safe to collect. It does not delete active worktrees.

## Advanced and internal commands

### `loopctl claim`

```text
loopctl claim --role dev|qa --worker WORKER_ID
```

Atomically claims the next eligible card for a supervisor worker. Exit code 3 means no card is available; code 4 means all candidates conflicted with another claim.

### `loopctl move`

```text
loopctl move CARD_ID --to STATUS --by ACTOR [--patch JSON] [--note TEXT]
```

Applies an allowed state transition and optional JSON merge patch under Workloop's role and invariant checks. It cannot bypass the transition policy. Do not use it to force `done`; use the verified QA merge lifecycle.

```bash
loopctl move eng-123 --to in_review --by dev/dev-1 \
  --note "PR opened" --patch '{"branch":"loop/eng-123"}'
```

### `loopctl findings`

```text
loopctl findings CARD_ID --file FINDINGS.json
```

Records QA findings and applies the corresponding QA transition. The file is a JSON array:

```json
[
  {
    "file": "internal/pricing.go",
    "line": 42,
    "issue": "Free mode still charges the setup fee",
    "violates": "AC-2",
    "evidence": "go test ./internal/pricing -run TestFreeMode",
    "severity": "blocking"
  }
]
```

### `loopctl heartbeat`

```text
loopctl heartbeat --role dev|qa [--patch JSON]
```

Updates the selected role heartbeat with optional structured worker metadata.

### `loopctl peer-check`

```text
loopctl peer-check --role dev|qa
```

Reports the other role's liveness from durable heartbeat data. This is primarily a supervisor diagnostic.

### `loopctl qa-merge`

```text
loopctl qa-merge CARD_ID --by QA_WORKER
```

Merges the exact QA-verified PR into `dev` only after Workloop fetches the
latest `origin/dev`, rechecks that base against the tested card and PR head,
confirms the PR head includes that base, and verifies every check returned by
`gh pr checks --json name,state,bucket,link`. Each reported check must have
`bucket: pass`; the actual passed check names are stored in the durable merge
receipt.
The supervisor normally calls it.

### `loopctl mark-stale`

```text
loopctl mark-stale --base-moved --merged-card CARD_ID --base-sha SHA
```

After `dev` advances, marks review cards stale when their recorded base SHA is
no longer current; touch overlap remains an additional conservative trigger.
Claimed QA cards return to In Review so they cannot approve results tested
against an invalid base. The supervisor normally calls it after a merge.

### `loopctl runner`

```text
loopctl runner --provider codex|claude --role dev|qa
```

Runs the unified provider adapter. It reads a Workloop runner-envelope JSON document from standard input, verifies that its provider and role match the command arguments, invokes the selected CLI, and prints the structured result. This is an internal integration boundary, not an interactive way to start a worker.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success. |
| 2 | Invalid input, policy violation, or validation error. |
| 3 | No eligible card to claim. |
| 4 | Claim contention or all candidates conflict. |
| 5 | Backpressure limit reached. |
| 6 | Stop signal or deadline reached. |
| 7 | State invariant failure or corruption. |
| 8 | External integration unavailable. |
| 10 | Retryable runner failure. |
| 11 | Runner result requires human attention. |

## Safety rules

- Do not edit `.loopctl` card snapshots or runtime files manually. There is no
  local status-folder queue; Linear is the workflow source of truth.
- Do not run multiple supervisors for the same state root.
- Do not force a card directly to `done`.
- Do not deploy from Workloop; promotion beyond `dev` remains human-controlled.
- Use `doctor` before manual recovery and keep an audit note on human resolutions.
