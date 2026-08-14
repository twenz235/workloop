---
name: loopctl
description: Inspect, start, stop, diagnose, and safely operate a loopctl local AI workflow. Use when the user invokes /loopctl or $loopctl, asks about queue state, wants backlog synchronization, or needs recovery of a loop card.
---

# Operate loopctl

Use the project state root supplied by the user or discover it from the project's documented setup. Never guess a state root when more than one exists.

- Normal operation: run `loopctl start --state-root <root> --env-file ~/.env`. It synchronizes immediately and then every five minutes, and starts Dev/QA runners automatically.
- Inspect: run `loopctl status --state-root <root> --role dev` and the QA equivalent. Prefer this over reading queue files manually.
- Diagnose: run `loopctl doctor --state-root <root>`, then targeted `list` or `reconcile`. Report evidence before changing state.
- Stop/restart: use `loopctl stop` or `loopctl restart`; never kill workers or edit queue files directly.
- Human resolution: use `loopctl resolve <id> --to todo|rework|cancelled --by human/<id> --note <reason>` only after the user chooses the disposition.
- Treat `runner` as internal/debug-only. Users call `loopctl start`, not `loopctl runner`.

Never move a card directly to Done. Only QA merge plus `sync-done` may complete it. Never merge outside `dev`, deploy, promote environments, expose tokens, or manually modify state files.
