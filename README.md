# Workloop

Workloop is a local, crash-recoverable AI development workflow. It imports approved work from Linear, coordinates isolated Dev and QA workers, and merges verified pull requests into a `dev` branch. Deployment and environment promotion always remain human-controlled.

The command-line interface is `loopctl`. Workloop currently supports macOS, GitHub, Linear, and either Codex or Claude as the worker provider. One state root is permanently bound to one Git repository.

## สรุปภาษาไทย

Workloop คือระบบจัดคิวงานพัฒนาด้วย AI บนเครื่อง โดยรับงานที่ groom และอนุมัติแล้วจาก Linear จากนั้นให้ Codex หรือ Claude Code ทำงาน Dev เปิด PR และให้ QA ตรวจสอบก่อน merge เข้า branch `dev` อัตโนมัติ ระบบไม่ deploy หรือเลื่อนงานไป environment อื่น เพราะขั้นตอนนั้นยังเป็นหน้าที่ของคน

## How the pieces fit together

Workloop is an orchestrator; it does not replace an AI coding agent, issue tracker, or Git host. A working setup uses all of these components:

- **Codex or Claude Code** — at least one is required. It performs the Dev and QA work that Workloop schedules. You may install both and choose the active provider in configuration.
- **Linear** — required as the source of truth for requirements, approval, priority, and human-visible status. Workloop imports only approved issues carrying `loop:ready`.
- **GitHub** — required for branches, pull requests, checks, and the verified merge into `dev`.
- **Workloop (`loopctl`)** — connects the three systems, keeps only private runtime/lease data locally, isolates workers in Git worktrees, retries safely, and enforces the Dev → QA → merge lifecycle.

In short: Linear decides **what** is ready and what people see on the board, Codex or Claude Code performs the work, GitHub carries the reviewed change, and Workloop coordinates the lifecycle between them.

### Which system owns which data?

Linear is the only user-facing board. Its state, labels, priority, project,
parent/child links, and acceptance checklist are the values shown to people.
Workloop still keeps a private `.loopctl` runtime for leases, reservations,
retries, worker metadata, outbox actions, and crash recovery; it is not a
second board and must not be edited by hand. `loopctl list` and the `counts`
field from `loopctl status` use the latest Linear snapshot. Local execution
phases are private diagnostics for the orchestrator, not an
alternative workflow board. There is no local `queue/<status>` source of
truth: reopening or moving an issue in Linear is what changes its workflow
eligibility.

## Documentation

- [User Guide](docs/user-guide.md) — install, configure, run, and operate Workloop day to day.
- [CLI Reference](docs/cli-reference.md) — complete command list, syntax, options, examples, and exit codes.
- [Linear contract](docs/linear-contract.md) — required issue format and Linear integration behavior.
- [Design specification](HANDOFF-codex.md) — detailed architecture, invariants, and implementation decisions.

## Workflow

```text
Groom → Linear Backlog + loop:ready → Todo → Dev → PR → QA → merge to dev → Done
```

- Grooming creates a complete, explicitly approved work contract.
- `loopctl sync` imports eligible Linear issues, refreshes the local Linear
  snapshot, and is idempotent. State/label changes caused by a claim or worker
  transition are flushed immediately; the periodic sync repairs missed remote
  updates and refreshes anything changed in Linear.
- Dev works in an isolated `loop/<card-id>` branch and opens a PR to `dev`.
- QA checks the exact head SHA without modifying it.
- Workloop merges only after QA and GitHub checks pass.
- Workloop never deploys or promotes code to another environment.

## Requirements

- macOS
- Go 1.26 or newer
- Git and GitHub CLI (`gh`), authenticated for the target repository
- Codex CLI or Claude Code
- A Linear API token and team with these statuses: `Backlog`, `Todo`, `In Progress`, `In Review`, `Done`, and `Canceled`
- Linear labels `loop:ready`, `loop:needs-attention`, `type:feature`, `type:bug`, and `type:maintenance`
- A local and remote `dev` branch

## Install

```bash
make check
install -m 0755 bin/loopctl ~/.local/bin/loopctl
```

Optionally install the included skills for Codex and Claude Code:

```bash
cp -R skills/groom skills/loopctl ~/.codex/skills/
cp -R skills/groom skills/loopctl ~/.claude/skills/
```

Store the token outside the repository in an owner-only file:

```bash
printf 'LINEAR_API_TOKEN=lin_api_xxx\n' > ~/.env
chmod 600 ~/.env
```

Initialize from inside the target repository:

```bash
cd /Users/alice/src/acme-app
loopctl init
```

Workloop derives the Git root, project name, `<repo>/.loopctl` state root, and Linear workspace automatically. It loads `~/.env` and locally excludes `.loopctl` through `.git/info/exclude`. If the workspace has multiple teams, select one discovered key:

```bash
loopctl init --linear-team ENG
```

All former flags remain available as explicit overrides. Start in the foreground:

```bash
loopctl start
```

Or install the idempotent macOS user LaunchAgent:

```bash
loopctl startup enable
```

Startup performs an immediate sync and then polls every 300 seconds by default. Change the interval within 60–3600 seconds:

```bash
loopctl config set linear.sync_interval_sec 300
```

## Grooming

The included `groom` skill clarifies and scopes a request before asking for explicit approval. It automatically keeps small work as one card or turns large work into one parent plus dependency-ordered executable sub-issues, explicitly reusing an existing Linear issue as the parent when appropriate. Only sub-issues receive `loop:ready`; all cards have one `type:*` label, a Linear project, priority, and acceptance checklist. Screenshots and diagrams can be supplied as grooming context and attached through safe HTTPS references. Normal intake is Groom → Linear → `loopctl sync`; `loopctl add` is a recovery path for validated card JSON.

## Operations

```bash
loopctl status --role dev
loopctl status --role qa
loopctl list
loopctl doctor
loopctl stop
loopctl restart
```

Cards requiring a human decision keep the `loop:needs-attention` label in Linear
until explicitly resolved:

```bash
loopctl resolve eng-123 --to rework --by human/alice \
  --note "The updated requirement is approved"

loopctl qa-retry eng-123 --by human/alice \
  --note "The QA provider is healthy; rerun against the current PR"
```

`qa-retry` is the explicit human path from `needs_attention` back to In
Review. It requires an existing PR, an unchanged contract, and no unresolved
blocking finding; it clears old QA evidence and records the recovery note in
Linear.

Never edit `.loopctl` runtime files manually or move a card directly to Done.
Only QA merge followed by a verified `sync-done` can complete work.

## Development

```bash
make test
make race
make stress
make vet
make build
```

The test suite uses fake Linear, GitHub, and model providers. `tests/stress.sh` runs competing CLI processes and fault-injects crashes at transaction boundaries.

## License

[MIT](LICENSE)
