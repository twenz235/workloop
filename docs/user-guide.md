# Workloop User Guide

This guide explains how to set up and operate Workloop. For the exact syntax of every command, see the [CLI Reference](cli-reference.md).

Workloop must be used with Linear, GitHub, and either Codex or Claude Code. Linear defines approved work, the AI provider performs Dev and QA tasks, GitHub holds pull requests, and Workloop coordinates the lifecycle. Workloop merges verified changes into `dev`; a human remains responsible for deployment and promotion to other environments.

## 1. Prepare the tools

You need:

- macOS, Git, Go 1.26 or newer, and an authenticated GitHub CLI (`gh`)
- Codex CLI or Claude Code installed and authenticated
- a GitHub repository with local and remote `dev` branches
- a Linear team with `Backlog`, `Todo`, `In Progress`, `In Review`, `Done`, and `Canceled` statuses
- `loop:ready`, `loop:needs-attention`, `type:feature`, `type:bug`, and `type:maintenance` labels in that Linear workspace
- a Linear API token

Store the token outside the repository and restrict the file to its owner:

```bash
printf 'LINEAR_API_TOKEN=lin_api_xxx\n' > ~/.env
chmod 600 ~/.env
```

Workloop rejects an env file writable or readable by other users.

## 2. Install Workloop and its skills

From the Workloop source repository:

```bash
make check
install -m 0755 bin/loopctl ~/.local/bin/loopctl
```

Install the included skills for the AI tools you use:

```bash
cp -R skills/groom skills/loopctl ~/.codex/skills/
cp -R skills/groom skills/loopctl ~/.claude/skills/
```

The `groom` skill creates approved Linear cards. The `loopctl` skill helps inspect and safely operate the local workflow.

## 3. Initialize a repository

Run `init` from inside the repository Workloop will manage:

```bash
cd /Users/alice/src/acme-app
loopctl init
loopctl doctor
```

Workloop automatically discovers the Git root, uses the repository name as the project name, creates `<repo>/.loopctl`, loads `~/.env`, discovers Linear, and excludes `.loopctl` locally through `.git/info/exclude`.

If the Linear workspace has multiple teams, select the intended team:

```bash
loopctl init --linear-team ENG
```

One `.loopctl` state root is permanently bound to one Git repository. Run commands anywhere below that repository and Workloop will find its state automatically.

Confirm the selected AI provider and change it if necessary:

```bash
loopctl config get runner.provider
loopctl config set runner.provider codex
# or
loopctl config set runner.provider claude
```

## 4. Groom work into Linear

Describe the requested result to Codex or Claude Code and invoke the installed skill:

```text
$groom Make every package temporarily free and support free, increase,
decrease, and per-package price changes.
```

Claude Code may use `/groom` instead. The skill will clarify missing requirements, split oversized work when necessary, and require exactly one work type (`feature`, `bug`, or `maintenance`), one existing Linear project, and a priority from Urgent to Low. Acceptance criteria appear as a checklist in the issue. The preview shows all of these fields and waits for explicit approval before creating a complete Linear issue in `Backlog` with `loop:ready` and the matching `type:*` label.

You may provide screenshots or diagrams while grooming. The skill inspects them, turns relevant observations into textual requirements, and includes safe HTTPS visual references in the issue. When the connected Linear tool supports file upload, it can first upload a local image to Linear's private storage. Local file paths, base64 payloads, and secret-bearing URLs are never written into a card.

For large work, you do not need to split cards yourself. Groom automatically creates one parent issue describing the overall outcome and 2–20 executable sub-issues. If you are splitting an existing Linear issue, Groom reuses that issue explicitly instead of creating a duplicate parent. It assigns dependencies, shows the execution waves and create/reuse choice in one preview, and asks for approval once. The parent remains in Backlog without `loop:ready`; each complete sub-issue receives `loop:ready` and is imported independently by Workloop. If Linear fails partway through creation, rerunning the same approved plan resumes from deterministic IDs without duplicating completed issues.

The normal intake path is:

```text
Groom → approved Linear Backlog issue + loop:ready → loopctl sync → Todo
```

Do not use `loopctl add` for normal intake. It exists to restore a validated card from JSON when recovering a queue.

## 5. Run the workflow

Choose one operating mode.

For a visible foreground process in the current terminal:

```bash
loopctl start
```

For an automatic background process that starts at login:

```bash
loopctl startup enable
```

Both modes perform an immediate Linear sync. While running, the supervisor polls Linear every five minutes by default, moves eligible issues from Backlog to Todo, starts isolated Dev and QA workers, and completes verified merges into `dev`.

Do not run `loopctl start` while the LaunchAgent is already active. Use foreground mode for observation and debugging; use startup mode for everyday unattended operation.

To change the polling interval to a value from 60 to 3600 seconds:

```bash
loopctl config set linear.sync_interval_sec 300
```

Run one manual sync without waiting for the next poll:

```bash
loopctl sync
```

## 6. Monitor day-to-day work

Use these commands without stopping the supervisor:

```bash
loopctl list
loopctl status --role dev
loopctl status --role qa
loopctl doctor
```

Filter cards by queue status or Linear identifier:

```bash
loopctl list --status needs_attention
loopctl list --linear ENG-123
```

The usual card lifecycle is:

```text
todo/rework → claimed-dev → in_review → claimed-qa → done
```

QA may send a card to `rework`. A problem requiring a decision is moved to `needs_attention` and never silently discarded.

## 7. Resolve cards needing human attention

Inspect the card first, then choose a supported destination and leave an audit note:

```bash
loopctl resolve eng-123 --to rework --by human/alice \
  --note "The revised requirement is approved"
```

Valid destinations are `todo`, `rework`, and `cancelled`. Returning to `todo` is allowed only when no PR or dirty worktree remains. Rework requires an existing branch. Cancelling a card with an open PR requires `--close-pr`:

```bash
loopctl resolve eng-123 --to cancelled --by human/alice \
  --note "The request is no longer needed" --close-pr
```

Never edit files under `.loopctl` by hand and never move a card directly to `done`. Only a verified QA merge followed by `sync-done` can finish a card.

## 8. Stop, restart, and recover

Stop a foreground supervisor safely:

```bash
loopctl stop
```

Disable automatic startup and stop new claims:

```bash
loopctl startup disable
loopctl stop
```

Restart in foreground mode after a configuration change:

```bash
loopctl restart
```

For stale claims or an interrupted machine, diagnose first and then reconcile the affected role:

```bash
loopctl doctor
loopctl reconcile --role dev
loopctl reconcile --role qa
loopctl sync-done
loopctl gc-worktrees
```

`doctor` validates and recovers durable transactions. `reconcile` recovers stale worker claims. `sync-done` confirms already merged PRs before marking cards done. `gc-worktrees` removes only verified terminal worktrees.

## 9. Recommended daily flow

1. Groom and approve work in Linear.
2. Let the running supervisor sync automatically, or run `loopctl sync`.
3. Check progress with `loopctl list` and `loopctl status`.
4. Review and resolve only cards in `needs_attention`.
5. After Workloop merges to `dev`, use your normal human-controlled release process for other environments.

For advanced recovery commands, internal worker commands, all flags, and exit codes, continue to the [CLI Reference](cli-reference.md).
