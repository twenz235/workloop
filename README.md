# Workloop

Workloop is a local, crash-recoverable AI development workflow. It imports approved work from Linear, coordinates isolated Dev and QA workers, and merges verified pull requests into a `dev` branch. Deployment and environment promotion always remain human-controlled.

The command-line interface is `loopctl`. Workloop currently supports macOS, GitHub, Linear, and either Codex or Claude as the worker provider. One state root is permanently bound to one Git repository.

## How the pieces fit together

Workloop is an orchestrator; it does not replace an AI coding agent, issue tracker, or Git host. A working setup uses all of these components:

- **Codex or Claude Code** — at least one is required. It performs the Dev and QA work that Workloop schedules. You may install both and choose the active provider in configuration.
- **Linear** — required as the source of truth for requirements, approval, priority, and human-visible status. Workloop imports only approved issues carrying `loop:ready`.
- **GitHub** — required for branches, pull requests, checks, and the verified merge into `dev`.
- **Workloop (`loopctl`)** — connects the three systems, owns the durable local queue, isolates workers in Git worktrees, retries safely, and enforces the Dev → QA → merge lifecycle.

In short: Linear decides **what** is ready, Codex or Claude Code performs the work, GitHub carries the reviewed change, and Workloop coordinates the lifecycle between them.

## Workflow

```text
Groom → Linear Backlog + loop:ready → Todo → Dev → PR → QA → merge to dev → Done
```

- Grooming creates a complete, explicitly approved work contract.
- `loopctl sync` imports eligible Linear issues and is idempotent.
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
- Linear labels `loop:ready` and `loop:needs-attention`
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

The included `groom` skill clarifies a request, scopes it, previews a complete card, and waits for explicit approval before creating a Linear issue in Backlog with `loop:ready`. Normal intake is Groom → Linear → `loopctl sync`; `loopctl add` is a recovery path for validated card JSON.

## Operations

```bash
loopctl status --role dev
loopctl status --role qa
loopctl list
loopctl doctor
loopctl stop
loopctl restart
```

Cards requiring a human decision remain in `needs_attention` until explicitly resolved:

```bash
loopctl resolve eng-123 --to rework --by human/alice \
  --note "The updated requirement is approved"
```

Never edit queue files manually or move a card directly to Done. Only QA merge followed by a verified `sync-done` can complete work.

## Development

```bash
make test
make race
make stress
make vet
make build
```

The test suite uses fake Linear, GitHub, and model providers. `tests/stress.sh` runs competing CLI processes and fault-injects crashes at transaction boundaries.

See [docs/linear-contract.md](docs/linear-contract.md) for the integration contract and [HANDOFF-codex.md](HANDOFF-codex.md) for the complete design specification.

## License

[MIT](LICENSE)
