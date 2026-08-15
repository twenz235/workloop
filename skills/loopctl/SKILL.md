---
name: loopctl
description: Inspect, start, stop, diagnose, and safely operate a loopctl local AI workflow. Use when the user invokes /loopctl or $loopctl, asks about queue state, wants backlog synchronization, or needs recovery of a loop card.
---

# Operate loopctl

Operate from inside the target Git repository. Prefer Workloop's built-in discovery over asking the user for configuration.

- First use: run `loopctl init` from the repository. It derives the Git root, project name, `<repo>/.loopctl` state root, `~/.env`, Linear workspace, and a sole Linear team automatically. It locally excludes `.loopctl` from Git.
- Ask only when discovery has a real ambiguity, such as multiple Linear teams; then show the discovered choices and rerun with only `--linear-team KEY`. An invalid/missing token is a credential blocker, not a reason to ask for paths or workspace IDs.
- Normal operation: run `loopctl start`. It synchronizes immediately and then every five minutes, and starts Dev/QA runners automatically.
- Inspect: run `loopctl status --role dev` and the QA equivalent. Prefer this over reading queue files manually.
- Diagnose: run `loopctl doctor`, then targeted `list` or `reconcile`. Report evidence before changing state.
- Stop/restart: use `loopctl stop` or `loopctl restart`; never kill workers or edit queue files directly.
- Human resolution: use `loopctl resolve <id> --to todo|rework|cancelled --by human/<id> --note <reason>` only after the user chooses the disposition.
- QA recovery: use `loopctl qa-retry <id> --by human/<id> --note <reason>` to return a `needs_attention` card with a reviewable PR to In Review for fresh QA; do not use it for unresolved blocking findings or changed contracts.
- Treat `runner` as internal/debug-only. Users call `loopctl start`, not `loopctl runner`.

Use `--repo-path`, `--state-root`, `--env-file`, or explicit Linear IDs only as overrides. When invoked outside the target repository, change into it first or use an explicit override. If `LOOPCTL_STATE_ROOT` is set, treat it as an explicit selection.

Never move a card directly to Done. Only QA merge plus `sync-done` may complete it. Never merge outside `dev`, deploy, promote environments, expose tokens, or manually modify state files.
