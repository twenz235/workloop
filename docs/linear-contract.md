# Linear contract

The configured Linear workspace and team are the source of truth for requirements, priority, approval, and status display. The local filesystem is authoritative for claims, retries, reservations, and crash recovery. `loopctl init` discovers the token's workspace and selects its sole team automatically; when several teams exist, the user supplies only `--linear-team KEY`. Workloop contains no organization-specific defaults.

An approved issue stays in `Backlog` with `loop:ready`. `loopctl sync` validates its fenced `loop-card` JSON, creates exactly one local card keyed by Linear UUID, and moves Linear to `Todo`. Local `claimed-dev` maps to `In Progress`; `in_review` and `claimed-qa` map to `In Review`; a GitHub-confirmed QA merge maps to `Done`. `needs_attention` retains the last execution status and adds `loop:needs-attention`.

Required labels:

- `loop:ready`: explicit approval and Definition of Ready passed
- `loop:needs-attention`: automation paused for a human decision
- exactly one of `type:feature`, `type:bug`, or `type:maintenance`: required work classification

Every approved issue must also belong to an available Linear project and use a non-empty priority: Urgent (1), High (2), Medium (3), or Low (4). Acceptance criteria are rendered as Markdown checkboxes for humans and retained as an array in the fenced contract for deterministic validation. Optional screenshots and diagrams appear as HTTPS Markdown images and as `visuals` entries containing alt text, URL, and a textual description. Local paths, base64 payloads, and secret-bearing URLs are forbidden.

Set `LINEAR_API_TOKEN` in the process environment or pass a strict owner-only env file. Tokens must never appear in cards, config, journals, plist files, or output.
