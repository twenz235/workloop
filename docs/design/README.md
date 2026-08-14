# Design documents (pre-implementation)

These four pages are the design rationale written **before** Workloop was implemented. They explain *why* the system is shaped the way it is — the reasoning behind the queue model, the concurrency scheme, and the Dev → QA lifecycle.

They are **historical**. For how Workloop actually behaves today, read [`docs/user-guide.md`](../user-guide.md), [`docs/cli-reference.md`](../cli-reference.md), and [`HANDOFF-codex.md`](../../HANDOFF-codex.md) instead.

Open any file in a browser — the set is cross-linked, so you can start anywhere.

| File | Contents |
|---|---|
| [`plan.html`](plan.html) | Three lanes (watcher / dev queue / digest), comparison of four scheduling mechanisms, shared tick protocol |
| [`dev-queue-loop.html`](dev-queue-loop.html) | Why a countable queue beats an open-ended loop, the four-point ticket contract, card lifecycle, git model |
| [`qa-loop-and-grooming.html`](qa-loop-and-grooming.html) | Why QA needs a clean context, the three-layer rejection path, what a useful finding looks like |
| [`orchestrated-loops.html`](orchestrated-loops.html) | Ownership by location, atomic claim, conflict-set scheduling, backpressure, parallelism failure modes |

## What changed after implementation

The core ideas survived — durable on-disk state, one card per file, atomic claim, conflict-set scheduling, `rework` with a bounded retry count, and human control over anything irreversible. These parts diverged:

| Design documents say | Workloop does |
|---|---|
| Cards are authored by a `/groom` skill writing directly into `todo/` | Linear is the source of truth; `loopctl sync` imports approved `loop:ready` issues |
| Runs as two Claude Code `/loop` sessions | Runs as `loopctl start` / a macOS LaunchAgent polling on an interval |
| A loop may never merge; a human merges every PR | Workloop merges into `dev` after QA and GitHub checks pass; deployment and environment promotion stay human-controlled |

## สรุปภาษาไทย

เอกสาร 4 ฉบับนี้คือเหตุผลเบื้องหลังการออกแบบที่เขียน**ก่อน**ลงมือ implement — อธิบายว่าทำไมระบบถึงมีหน้าตาแบบนี้ ไม่ใช่คู่มือการใช้งาน

**ถ้าอยากรู้ว่าระบบทำงานยังไงตอนนี้ ให้อ่าน `docs/user-guide.md` และ `HANDOFF-codex.md` แทน** เพราะบางส่วนในเอกสารชุดนี้ถูกแทนที่ไปแล้ว (ดูตารางด้านบน) โดยเฉพาะเรื่อง intake ที่ย้ายไป Linear และเรื่องการ merge ที่ระบบทำเองได้แล้วหลัง QA + checks ผ่าน
