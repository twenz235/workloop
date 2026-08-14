# Workloop

Workloop is a local, crash-recoverable AI development workflow. It imports approved work from Linear, coordinates isolated Dev and QA workers, and merges verified pull requests into a `dev` branch. Deployment and environment promotion always remain human-controlled.

The command-line interface is `loopctl`. Workloop currently supports macOS, GitHub, Linear, and either Codex or Claude as the worker provider. One state root is permanently bound to one Git repository.

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

Initialize a repository using the workspace name/UUID and team key/UUID shown by Linear:

```bash
loopctl init \
  --project acme-app \
  --repo-path /Users/alice/src/acme-app \
  --state-root /Users/alice/.workloop/acme-app \
  --linear-workspace Acme \
  --linear-workspace-id 00000000-0000-4000-8000-000000000001 \
  --linear-team ENG \
  --linear-team-id 00000000-0000-4000-8000-000000000002 \
  --env-file /Users/alice/.env
```

Start in the foreground:

```bash
loopctl start --state-root /Users/alice/.workloop/acme-app --env-file /Users/alice/.env
```

Or install the idempotent macOS user LaunchAgent:

```bash
loopctl startup enable --state-root /Users/alice/.workloop/acme-app
```

Startup performs an immediate sync and then polls every 300 seconds by default. Change the interval within 60–3600 seconds:

```bash
loopctl config set linear.sync_interval_sec 300 --state-root "$STATE_ROOT"
```

## Grooming

The included `groom` skill clarifies a request, scopes it, previews a complete card, and waits for explicit approval before creating a Linear issue in Backlog with `loop:ready`. Normal intake is Groom → Linear → `loopctl sync`; `loopctl add` is a recovery path for validated card JSON.

## Operations

```bash
loopctl status --role dev --state-root "$STATE_ROOT"
loopctl status --role qa --state-root "$STATE_ROOT"
loopctl list --state-root "$STATE_ROOT"
loopctl doctor --state-root "$STATE_ROOT"
loopctl stop --state-root "$STATE_ROOT"
loopctl restart --state-root "$STATE_ROOT" --env-file ~/.env
```

Cards requiring a human decision remain in `needs_attention` until explicitly resolved:

```bash
loopctl resolve eng-123 --to rework --by human/alice \
  --note "The updated requirement is approved" --state-root "$STATE_ROOT"
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
