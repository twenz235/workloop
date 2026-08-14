# HANDOFF — Orchestrated Loop System (`loopctl`)

> เอกสารส่งต่อสำหรับผู้ implement (Codex). อ่านจบแล้วต้องลงมือได้โดยไม่ต้องเดา
> ถ้ามีอะไรในสเปคนี้ขัดกันเอง หรือต้องตัดสินใจเชิงออกแบบที่สเปคไม่ได้ระบุ — **หยุดแล้วถาม อย่าเดาแล้วเดินต่อ**

**Version:** 1.2 · **วันที่:** 2026-08-14 · **สถานะ:** implementation-ready สำหรับ M0–M8

---

## 0. TL;DR สำหรับคนที่จะลงมือ

สร้างระบบรับงานจาก Linear และคิว execution บน filesystem ให้ AI agent 2 กลุ่ม (dev / qa) ซึ่งอยู่**คนละ process และมองไม่เห็นกัน** ทำงานร่วมกันโดยไม่ชนกัน. Linear เป็น source of truth ของ requirement/priority/การอนุมัติ ส่วน filesystem เป็น source of truth ของ claim/retry/recovery

**สิ่งที่ต้องส่งมอบคือ CLI ชื่อ `loopctl` และ skill `/groom`**. `/groom` เปลี่ยนโจทย์ภาษาคนเป็น Linear issue ที่ผ่าน Definition of Ready; `loopctl` sync issue ที่อนุมัติแล้วเข้าคิวและห่อ operation ที่ต้องแม่นยำ (claim / move / conflict detection / reconcile / Linear sync) พร้อมเทส concurrency และ failure recovery

**ทำไมต้องเป็น CLI ไม่ปล่อยให้ agent ทำเอง:** agent ที่พิมพ์ `mv` เองจะพลาดเรื่อง race และเปลืองรอบอ่านไฟล์มหาศาล. ตรรกะที่ผลลัพธ์ต้องเหมือนเดิมทุกครั้ง (deterministic) ต้องอยู่ในโค้ด ส่วน agent เก็บไว้ทำเฉพาะงานที่ต้องใช้วิจารณญาณ (เขียนโค้ด, ตัดสินว่างานผ่านไหม)

---

## 1. เป้าหมาย / ไม่ใช่เป้าหมาย

### เป้าหมาย
1. คิวงานที่ **2 process เขียนพร้อมกันได้โดยไม่มีทางเกิด lost update** — พิสูจน์ด้วยเทส ไม่ใช่ด้วยการอธิบาย
2. Dev orchestrator ปล่อย worker หลายตัวขนานกันได้ โดยการ์ดที่แตะไฟล์ทับกัน**จะไม่ถูกรันพร้อมกันเด็ดขาด**
3. QA orchestrator ตรวจงานจาก context สะอาด แล้วตีกลับพร้อม finding ได้
4. เพิ่มการ์ดเข้าคิวระหว่างที่ระบบกำลังวิ่งได้ทันที โดยไม่ต้องรีสตาร์ทอะไร
5. ทุก state ฟื้นได้หลัง crash — ไม่มีสถานะที่อยู่แต่ในหน่วยความจำของ process
6. ผู้ใช้คุย requirement ผ่าน `/groom`, ยืนยัน preview แล้วได้ Linear issue ที่ `loopctl` นำไปทำต่อได้โดยไม่ต้องคัดลอกข้อมูลเอง
7. QA merge PR เข้า `dev` ได้เมื่อ acceptance และ required checks ผ่าน; การ promoteขึ้น environment อื่นเป็นสิทธิ์ของคนเท่านั้น

### ไม่ใช่เป้าหมาย (อย่าทำ)
- ไม่ต้องมี web UI / dashboard / public server; `loopctl start` เป็น local background supervisor ได้
- ไม่ต้องมี database — filesystem เท่านั้น
- ไม่ต้องรองรับหลายเครื่อง (single machine, single filesystem)
- ห้าม merge เข้า `main`, `master`, release, staging หรือ production และห้าม deploy/promote environment ทุกกรณี
- ไม่ต้อง implement ตัว AI agent — CLI แค่ต้องถูกเรียกจาก agent ได้

---

## 2. คำศัพท์

| คำ | ความหมาย |
|---|---|
| **card** | ใบงาน 1 ใบ = ไฟล์ JSON 1 ไฟล์ |
| **status** | สถานะของการ์ด = **ชื่อโฟลเดอร์ที่ไฟล์นั้นอยู่** |
| **role** | `dev` หรือ `qa` — สอง orchestrator ที่รันคนละ process |
| **worker** | agent ลูกที่ทำการ์ด 1 ใบ (dev worker เขียนโค้ด, qa worker ตรวจ) |
| **claim** | การจองการ์ดภายใน queue transaction แล้วเปลี่ยน authoritative location เข้าโฟลเดอร์ claim |
| **touches** | รายการ glob ของไฟล์ที่การ์ดจะแก้ — ใช้เป็น**กุญแจล็อก**ของ scheduler |
| **conflict-free** | การ์ดที่ `touches` ไม่ตัดกับการ์ดที่กำลังรันอยู่ทุกใบ |
| **in-flight** | การ์ดที่อยู่ระหว่าง `claimed-dev` / `in_review` / `claimed-qa` |
| **Linear issue** | backlog/source of truth ฝั่งคน; ใช้ UUID เป็น identity ถาวร |
| **Definition of Ready (DoR)** | เงื่อนไขขั้นต่ำก่อน `/groom` ติด `loop:ready` และอนุญาตให้ sync เข้า `todo` |
| **spec revision** | `updatedAt`/revision ของ Linear issue ตอนสร้าง local card ใช้ตรวจ requirement เปลี่ยนกลางงาน |

---

## 3. หลักการที่ทั้งระบบตั้งอยู่บนมัน

อ่านสองข้อนี้ให้เข้าใจก่อนเขียนโค้ดบรรทัดแรก — ที่เหลือทั้งหมดเป็นแค่รายละเอียดของสองข้อนี้

### 3.1 Ownership by location
แต่ละโฟลเดอร์สถานะมี **เจ้าของ role เดียว** ที่มีสิทธิ์หยิบการ์ดออกและแก้เนื้อใน. location ระบุ ownership ส่วน queue transaction lock ป้องกัน writer หลาย process ภายใน role เดียวกันและ operation ข้าม role

### 3.1.1 One state = one repo
หนึ่ง `STATE_ROOT` ผูกกับ repository เดียวแบบ immutable ตั้งแต่ `init` โดยเก็บ canonical `repo_path`, Git remote identity และ Linear team binding. ทุก card ใน state ต้องมี repo/base ตรง config; mismatch ให้ exit 2. ถ้าจะทำงานอีก repo ต้อง `init` state root ใหม่ ห้ามแชร์ queue/reservation/worktree ข้าม repo

### 3.2 Queue transaction + claim
การตรวจ conflict/hot/backpressure และการ claim ต้องอยู่ใน **critical section เดียวกัน** ต่อ `STATE_ROOT`; การใช้ `os.Link()` อย่างเดียวป้องกันได้เฉพาะการแย่ง card id เดียว แต่ป้องกันสอง process หยิบคนละ card ที่ `touches` ทับกันไม่ได้

ใช้ advisory file lock อายุสั้นด้วย `syscall.Flock(LOCK_EX)` บน file descriptor ของ `runtime/queue.lock` ครอบเฉพาะ `read state → validate → select → transition → journal`. ห้ามถือ lock ระหว่าง network/Git/worker ทำงาน; เตรียม external facts ก่อนเข้า lock แล้ว revalidate state ภายใน lock. process ตายแล้ว kernel ปล่อย lock ให้เอง และทุกคำสั่งที่เปลี่ยน queue ต้องใช้ protocol เดียวกัน

ทุก transition ต้องมี durable intent ที่ `runtime/transactions/<tx-id>.json` ก่อนแตะ queue. Intent เก็บ `card_id`, source, destination, hash ของเนื้อหาใหม่, operation, actor และ phase (`prepared|destination-created|source-removed|committed`). เขียน temp/intent แล้ว `fsync` ไฟล์และ parent directory ก่อนเดิน phase ถัดไป

ภายใน transaction ใช้ `Root.Link()` + `Root.Remove()` เพื่อสร้าง destination แบบไม่ overwrite:
```go
err := root.Link(src, dst)      // ปลายทางมีอยู่แล้ว → error ที่ os.IsExist(err) == true
if err != nil { /* แพ้การแย่ง */ }
err = root.Remove(src)          // link สำเร็จแล้วค่อยลบต้นทาง
```
เหตุผล: `os.Rename()` จะ **ทับปลายทางเงียบ ๆ** ถ้าบังเอิญมีไฟล์ชื่อเดียวกันค้างอยู่ (เกิดได้จาก crash รอบก่อน) = การ์ดหายโดยไม่มีใครรู้. `os.Link()` คืน error แทน ซึ่งเป็นสิ่งที่เราต้องการ

การแย่งกันจบลงแบบนี้: ตัวแรก link สำเร็จแล้ว remove ต้นทาง · ตัวที่สองได้ error ที่ `os.IsNotExist` (ต้นทางหายแล้ว) หรือ `os.IsExist` (ปลายทางมีแล้ว) → รู้ว่าแพ้ ไปหยิบใบอื่น

`link + unlink` ไม่ใช่ syscall เดียว: crash หลัง link อาจเหลือชื่อสองตำแหน่ง. `doctor` ต้อง replay intent ตาม phase: ก่อน `destination-created` ให้ source เป็นจริง; ตั้งแต่ `destination-created` ให้ verify destination hash แล้ว finish ลบ source; hash/path ไม่ตรงให้หยุด exit 7 ห้ามเดา. หลัง committed จึง append journal และเก็บ receipt/ลบ intentตาม retention policy. ดังนั้น invariant คือ **หลัง transaction สำเร็จหรือหลัง doctor/reconcile การ์ดมี authoritative location เดียว**; ห้ามอ้างว่าไม่มี duplicate window ระหว่างสอง syscall

> **แยกสอง error นี้ให้ถูกเสมอ** — ทั้งระบบตั้งอยู่บนการแยก `IsExist` ออกจาก `IsNotExist`. ห้ามจับ error แบบรวม ๆ แล้วเดา
> **ห้ามใช้ `exec.Command("mv", …)` หรือ copy+delete** — ไม่ atomic
> ควรใช้ `os.OpenRoot(stateRoot)` (มีใน go1.26) ครอบทุก FS operation เพื่อกัน path escape จาก card id ที่ผิดรูป

> **ข้อบังคับ:** ทุกโฟลเดอร์ต้องอยู่ filesystem เดียวกัน. ถ้าข้าม volume `rename()` จะ fail ด้วย `EXDEV` และถ้าไป fallback เป็น copy+delete จะไม่ atomic อีกต่อไป
> **`loopctl init` ต้องเช็คเรื่องนี้แล้ว fail ทันทีถ้าไม่ผ่าน** — เทียบ `st_dev` ของทุกโฟลเดอร์

---

## 4. โครงไฟล์

```
<STATE_ROOT>/                      # default: ~/.claude/loops/<project>/
├── config.json
├── STOP                           # มีไฟล์นี้ = ทุก role หยุดใน tick ถัดไป
├── queue/
│   ├── .tmp/                      # เขียนที่นี่ก่อน rename เสมอ
│   ├── todo/                      # Linear sync/add สร้าง · dev หยิบ
│   ├── rework/                    # qa ใส่ · dev หยิบ (ก่อน todo เสมอ)
│   ├── claimed-dev/               # dev เจ้าของเต็ม
│   ├── in_review/                 # dev ใส่ · qa หยิบ
│   ├── claimed-qa/                # qa เจ้าของเต็ม
│   ├── needs_attention/           # ทั้งคู่ใส่ได้ · คนมาดู
│   ├── blocked/                   # รอ depends_on (L3 ถูกปฏิเสธก่อนเข้าคิว)
│   ├── cancelled/                 # คนยกเลิก; terminal แต่ไม่ใช่ done
│   └── done/                      # merged แล้ว
├── journal/
│   ├── dev.md                     # append-only
│   ├── qa.md                      # append-only
│   └── workers/<card-id>/         # attempt logs + atomic result JSON
└── runtime/
    ├── queue.lock                 # advisory lock อายุสั้น
    ├── transactions/              # durable transition intents/receipts
    ├── reservations/              # touches reservations จน PR merge/close
    ├── supervisor.json            # pid/start/config ของ loopctl start
    ├── linear.json                # cursor/last sync; ไม่มี token
    ├── dev.json                   # dev เจ้าของเต็ม
    └── qa.json                    # qa เจ้าของเต็ม
```

### ตารางสิทธิ์ — ต้องบังคับใน CLI

| path | dev | qa | groom | คน |
|---|---|---|---|---|
| `todo/` | อ่าน, หยิบออก | — | — | อ่าน |
| `rework/` | อ่าน, หยิบออก | ใส่เข้า | — | อ่าน |
| `claimed-dev/` | **เจ้าของเต็ม** | — | — | อ่าน |
| `in_review/` | ใส่เข้า | อ่าน, หยิบออก | — | อ่าน |
| `claimed-qa/` | — | **เจ้าของเต็ม** | — | อ่าน |
| `needs_attention/` | ใส่เข้า | ใส่เข้า | — | หยิบออก |
| `blocked/` | ใส่/auto-unblock | — | — | resolve/cancel |
| `cancelled/` | — | — | — | ใส่ผ่าน `resolve` |
| `done/` | — | ใส่ผ่าน `sync-done` | — | อ่าน |
| `journal/dev.md` | **append** | — | — | อ่าน |
| `journal/qa.md` | — | **append** | — | อ่าน |
| `runtime/dev.json` | **เจ้าของเต็ม** | อ่านอย่างเดียว | — | อ่าน |
| `runtime/qa.json` | อ่านอย่างเดียว | **เจ้าของเต็ม** | — | อ่าน |

**ไม่มีแถวไหนที่มี "เจ้าของเต็ม" หรือ "append" ซ้ำสองคอลัมน์ — นี่คือหลักฐานว่าไม่มี write conflict ที่เป็นไปได้ในระบบ. ทุก transition ที่ CLI ทำต้องไม่ละเมิดตารางนี้ และต้องมีเทสยืนยัน**

---

## 5. Card schema

```jsonc
{
  // --- ระบุตอนสร้าง (groom) — required ทั้งหมด ---
  "id": "aru-123",                     // immutable local id; ^[a-z0-9][a-z0-9-]{0,31}$
  "title": "override transitive dep GHSA-r28c",
  "problem": "dependency tree resolve ไปเวอร์ชันที่มีช่องโหว่",
  "desired_outcome": "ทุก install resolve ไปเวอร์ชันที่แก้แล้วโดยไม่ทำให้ build พัง",
  "out_of_scope": ["upgrade dependency อื่นที่ไม่เกี่ยวข้อง"],
  "repo": "acme/acme-app",
  "repo_path": "/Users/alice/src/acme-app",
  "base": "dev",                       // branch ที่จะแตกออกและเปิด PR เข้า
  "tier": "L1",                        // L0|L1|L2 เท่านั้น — L3 ห้ามเข้าคิว (ดู §10)
  "touches": ["package.json", "pnpm-lock.yaml"],   // glob ได้ ต้องไม่ว่าง
  "acceptance": ["lockfile ไม่มีเวอร์ชันที่ได้รับผลกระทบ", "typecheck และ build ผ่าน"],
  "verification": ["pnpm install", "pnpm typecheck", "pnpm build"],
  "depends_on": [],                    // Linear UUID; ต้อง done ก่อน
  "priority": 2,
  "risk": {"level": "medium", "reason": "แตะ dependency กลาง"},
  "rollback_notes": "revert merge commit ของ PR นี้",
  "linear_issue_id": "ARU-123",
  "linear_issue_uuid": "00000000-0000-0000-0000-000000000000",
  "linear_url": "https://linear.app/...",
  "source_revision": "2026-08-13T10:00:00.000Z",
  "contract_hash": "sha256:...",       // canonical hash ของ contract fields เท่านั้น
  "approved_at": "2026-08-13T10:01:00+07:00",
  "approved_by": "linear-user-id",

  // --- ระบบจัดการเอง ---
  "status": "todo",                    // mirror ของโฟลเดอร์ (ดูกฎด้านล่าง)
  "hot": false,                        // คำนวณจาก config.hot_paths ตอน add
  "attempts": 0,
  "max_attempts": 2,
  "rework_count": 0,
  "max_rework": 2,
  "conflict_skips": 0,                 // starvation guard
  "claimed_at": null,                  // ISO8601 +07:00
  "claimed_by": null,                  // worker id
  "worktree": null,
  "branch": null,
  "pr": null,
  "base_sha": null,
  "tested_head_sha": null,
  "stale": false,                      // true = base ขยับ ต้อง verify ซ้ำ
  "spec_changed": false,               // Linear requirement เปลี่ยนหลัง import
  "qa_findings": [],
  "proposed": [],
  "history": [
    {"at": "...", "from": "todo", "to": "claimed-dev", "by": "dev/w1", "note": ""}
  ]
}
```

### 5.1 Definition of Ready — `/groom` ต้องบังคับ

ก่อนติด `loop:ready` ต้องผ่านครบ:
1. `problem`, `desired_outcome`, `out_of_scope` ชัดเจนและไม่ขัดกัน
2. acceptance ทุกข้อเป็นผลที่ตรวจได้; `verification` ระบุวิธี/คำสั่งแยกต่างหาก
3. ระบุ repo, base=`dev`, tier L0–L2, risk และ rollback ตามระดับความเสี่ยง
4. `touches` ไม่ว่างและ fail-safe; ถ้าไม่มั่นใจให้กว้างขึ้น ไม่เดาให้แคบ
5. dependency อ้าง Linear UUID ที่มีอยู่จริงและไม่มี cycle
6. งานพอดีกับ worker หนึ่งตัว; ถ้าไม่พอดีต้อง preview การแตก sub-issues และ dependency ก่อนสร้าง
7. ไม่มีคำถาม blocking ค้างอยู่ และผู้ใช้ยืนยัน preview แบบ explicit

ถ้ายังไม่ผ่าน ให้ `/groom` สร้าง/อัปเดต issue ใน Backlog โดย **ไม่ติด `loop:ready`**. ห้าม `loopctl sync` นำเข้า issue ที่ไม่ผ่าน DoR แม้มี label จากการติดผิด

### 5.2 Linear issue contract

Linear description ต้องมี human-readable summary และ fenced block `loop-card` ที่ parse ได้ deterministic. Linear UUID เป็น idempotency key; identifier เช่น `ARU-123` ใช้แสดงผลเท่านั้นเพราะเปลี่ยนได้เมื่อย้ายทีม. `/groom` ต้อง preview จำนวนการ์ด, fields สำคัญ, dependency graph และ URLs ก่อน/หลังสร้าง

ตอน import ครั้งแรกให้ derive `id` จาก Linear identifier เป็น lowercase และเก็บค่านั้น immutable; ถ้าชนกับ id เดิมแต่ UUID ต่างกัน ให้เติม suffix hash จาก UUID ภายในเพดาน 32 ตัวอักษร. การ sync/retry lookup ด้วย `linear_issue_uuid` เสมอ ไม่ derive id ใหม่

`contract_hash` คำนวณ SHA-256 จาก canonical JSON ของ `problem`, `desired_outcome`, `out_of_scope`, `repo`, `repo_path`, `base`, `tier`, `touches`, `acceptance`, `verification`, `depends_on`, `risk`, `rollback_notes` เท่านั้น. ต้อง sort object keys แต่รักษาลำดับ array. `source_revision` ใช้สังเกตการณ์/polling เท่านั้น; status, label, comment, assignee หรือ `updatedAt` ที่เปลี่ยนโดยระบบต้องไม่ทำให้ `contract_hash` เปลี่ยน

**กฎของ `status`:** เป็น mirror ไว้ให้คนอ่านสบาย. **location คือความจริง** — ถ้าสองอย่างไม่ตรงกัน ให้เชื่อโฟลเดอร์แล้วแก้ `status` ให้ตรง พร้อม log warning. `loopctl doctor` ต้องตรวจข้อนี้ทั้งระบบ

### หมายเหตุสำหรับ Go — `--patch` แบบ partial
`move --patch '{"pr":202}'` ต้องแก้เฉพาะฟิลด์ที่ส่งมา **ห้ามรีเซ็ตฟิลด์ที่ไม่ได้ส่งเป็น zero value** ซึ่งเป็นกับดักคลาสสิกของ `encoding/json` ใน Go

วิธีที่ต้องใช้: unmarshal การ์ดเดิมเป็น `map[string]any` → merge patch ทับทีละ key → validate → marshal กลับ
- **ห้าม** unmarshal เข้า struct แล้ว marshal ออก เพราะฟิลด์ที่ struct ไม่รู้จักจะหายไปเงียบ ๆ
- ใช้ struct เฉพาะตอน **validate** (unmarshal เข้า struct เพื่อตรวจ แล้วทิ้ง) ไม่ใช่ตอนเขียนกลับ
- marshal ด้วย indent 2 spaces + key เรียงคงที่ เพื่อให้ diff ของการ์ดอ่านรู้เรื่องตอนไล่ปัญหา

### `qa_findings[]`
```jsonc
{
  "file": "src/utils/date.test.ts",
  "line": 12,
  "issue": "assert เช็คแค่ไม่ throw ไม่ได้เช็คค่าที่ได้",
  "violates": "acceptance ข้อ 2",      // required — ถ้าอ้างไม่ได้ ห้าม blocking
  "evidence": "vitest run … → เขียวทั้งที่ input ผิด",
  "severity": "blocking"                // blocking | non-blocking
}
```
**CLI ต้อง validate:** finding ที่ `severity=blocking` แต่ไม่มี `violates` → **reject** (exit 2). นี่คือกลไกที่กัน QA ตีกลับด้วยรสนิยมจนงานไม่เดิน

---

## 6. Config schema

```jsonc
{
  "project": "acme-app",
  "state_root": "/Users/alice/.workloop/acme-app",
  "worktree_root": "/Users/alice/.workloop/acme-app/worktrees",
  "dev":  { "max_workers": 3, "claim_stale_min": 30 },
  "qa":   { "max_workers": 2, "claim_stale_min": 30 },
  "runner": {
    "adapter": "builtin",
    "provider": "codex",
    "provider_path": "/usr/local/bin/codex",
    "stop_grace_sec": 30
  },
  "limits": {
    "max_in_flight": 5,
    "max_open_prs": 3,
    "conflict_skip_boost": 5           // ข้ามกี่รอบแล้วเลื่อนขึ้นหน้าสุด
  },
  "hot_paths": ["package.json", "pnpm-lock.yaml", "prisma/schema.prisma", "*.config.*"],
  "deadline_at": null,                 // ISO8601 หรือ null
  "heartbeat_stale_sec": 7200,
  "linear": {
    "enabled": true,
    "token_env": "LINEAR_API_TOKEN",
    "workspace": "Acme",
    "team": "ENG",
    "workspace_id": "00000000-0000-4000-8000-000000000001",
    "team_id": "00000000-0000-4000-8000-000000000002",
    "ready_label": "loop:ready",
    "needs_attention_label": "loop:needs-attention",
    "sync_interval_sec": 300,
    "status_map": {
      "backlog": "Backlog",
      "todo": "Todo",
      "in_progress": "In Progress",
      "in_review": "In Review",
      "done": "Done"
    }
  },
  "github": {
    "open_pr_cache_max_age_sec": 300
  }
}
```

Linear API token อ่านจาก environment variable `LINEAR_API_TOKEN` เท่านั้น (`linear.token_env` ล็อกค่า default/production นี้). ห้ามเก็บ token value ใน `config.json`, card, journal, plist, stdout/stderr หรือ log. `init` ต้องตรวจว่าตัวแปรมีค่าและ validate team/status/labels แต่ห้ามพิมพ์ค่า tokenหรือสร้าง/เปลี่ยน Linear workflow โดยไม่ขออนุมัติ. test ใช้ process-local fake token กับ fake server ห้ามใช้ token จริง

---

## 7. CLI spec — `loopctl`

### ข้อกำหนดทางเทคนิค
- **Go 1.26 · stdlib เท่านั้น · `go.mod` ต้องไม่มี `require` แม้แต่บรรทัดเดียว** — ข้อนี้ต่อรองไม่ได้
  เครื่องเป้าหมาย: `go1.26.5 darwin/arm64`
- build เป็น binary เดียวชื่อ `loopctl` (`go build -o loopctl ./cmd/loopctl`)
- เทสใช้ `go test ./...` ที่มีมาให้ · ห้ามลง testify หรือ assertion library ใด ๆ
- ใช้ `os.OpenRoot` ครอบ FS operation ทั้งหมด
- **ข้อควรรู้:** `filepath.Match` ของ stdlib **ไม่รองรับ `**` แบบข้าม separator** (ยืนยันแล้ว: `src/**/*.ts` ไม่ match `src/a/b/c.ts`) → ต้อง implement matcher เองตาม §7.1 · **ห้ามลง `doublestar` หรือ glob library ใด ๆ**

### 7.1 Glob matcher — ต้องเขียนเอง

`touches` ใช้ pattern พวกนี้ ต้อง implement ให้ครบพร้อมเทส:

| pattern | ความหมาย | ตัวอย่าง match | ไม่ match |
|---|---|---|---|
| `package.json` | ตรงตัว | `package.json` | `src/package.json` |
| `src/api/*` | 1 segment ใน `src/api/` | `src/api/user.ts` | `src/api/v1/user.ts` |
| `src/api/**` | ทุกอย่างใต้ `src/api/` ทุกความลึก | `src/api/v1/user.ts` | `src/lib/x.ts` |
| `src/**/*.ts` | `.ts` ใต้ `src/` ทุกความลึก | `src/a/b/c.ts`, `src/x.ts` | `src/a/b.js` |
| `*.config.*` | ที่ root เท่านั้น | `vite.config.ts` | `app/vite.config.ts` |

**กฎการ implement:**
- normalize เป็น `/`-separated segments แล้ว match ทีละ segment
- `*` match ภายใน 1 segment เท่านั้น (ไม่ข้าม `/`)
- `**` match ได้ 0 segment ขึ้นไป — **`src/**` ต้อง match `src/x.ts` ด้วย ไม่ใช่แค่ที่ลึกกว่า 1 ชั้น**
- ทุก path เทียบแบบ relative ต่อ repo root, ไม่มี `./` นำหน้า, ไม่สนใจ case-insensitivity ของ macOS (เทียบแบบ case-sensitive)

**ฟังก์ชันที่ต้องมี — นี่คือหัวใจของ scheduler:**
```go
// PatternsOverlap คืน true ถ้า "มีความเป็นไปได้" ที่สองชุด pattern จะแตะไฟล์เดียวกัน
func PatternsOverlap(a, b []string) bool
```
ตรงนี้ต้องระวัง: มันไม่ใช่แค่เทียบ string เท่ากัน — `src/api/**` กับ `src/api/user.ts` **ทับกัน** ทั้งที่เขียนคนละแบบ

**เมื่อไม่แน่ใจให้ตอบว่าทับกัน (fail-safe)** เพราะผลของการตอบผิดสองทางไม่เท่ากัน:
- ตอบว่าทับทั้งที่ไม่ทับ → เสียความขนานไปนิดหน่อย
- ตอบว่าไม่ทับทั้งที่ทับ → **สอง worker แก้ไฟล์เดียวกัน = PR conflict ที่ต้องแก้มือ**
- ทุกคำสั่งรองรับ `--json` (default) และ `--state-root PATH`
- ทุกคำสั่ง **idempotent เท่าที่เป็นไปได้** — เรียกซ้ำต้องไม่พังและไม่ทำผลข้างเคียงซ้ำ
- ห้ามต่อเน็ต/เรียก `gh`/`git` ยกเว้นคำสั่งที่ระบุ: Linear API (`init`, `sync`, `status`), Git/GitHub (`reconcile`, `gc-worktrees`, `qa-merge`, `sync-done`). ห้ามส่ง source, diff, secret หรือ environment content ไป Linear

### Exit codes (ตายตัว — agent จะเช็คค่านี้)
| code | ความหมาย |
|---|---|
| 0 | สำเร็จ |
| 2 | input ไม่ถูกต้อง / validation ไม่ผ่าน |
| 3 | ไม่มีอะไรให้ทำ (ไม่มีการ์ดที่ claim ได้) — **ไม่ใช่ error** |
| 4 | แพ้การแย่ง claim (การ์ดถูกคนอื่นหยิบไปก่อน) — **ไม่ใช่ error** |
| 5 | ชนเพดาน backpressure |
| 6 | เจอ STOP / เลย deadline |
| 7 | invariant พัง (state เสียหาย ต้องมีคนดู) |
| 8 | external integration ใช้งานไม่ได้ชั่วคราว; state local ยังปลอดภัยและ retry ได้ |

### คำสั่ง

#### `loopctl init --project NAME --repo-path PATH [--state-root PATH] --linear-workspace NAME --linear-workspace-id UUID --linear-team KEY --linear-team-id UUID`
สร้างโครงโฟลเดอร์ + `config.json` ค่าเริ่มต้น
- ต้องเช็คว่าทุกโฟลเดอร์ย่อยอยู่ `st_dev` เดียวกัน ไม่ผ่าน → exit 7 พร้อมบอกว่าปัญหาคืออะไร
- idempotent: รันซ้ำบนของที่มีอยู่แล้วต้องไม่ทับ config
- รับ Linear workspace/team binding แบบ explicit (ไม่มี organization-specific default), validate mapping และตั้งค่า sync เริ่มต้น 300 วินาที; secret เก็บนอก state root
- canonicalize repo path/remote แล้ว bind state กับ repo นี้แบบ immutable; รัน `init` ซ้ำด้วย repo อื่นให้ exit 2

#### `loopctl start [--env-file PATH]` / `stop` / `restart`
- `start`: single-instance local supervisor; sync ทันที, reconcile, แล้วเริ่ม Linear sync + dev/qa ticks. เรียกซ้ำคืนสถานะเดิมโดยไม่ spawn ซ้ำ
- `stop`: สร้าง STOP, หยุดรับ claim ใหม่, รอเฉพาะช่วง grace ที่ config กำหนด แล้วเก็บ state worker ที่ยังทำอยู่ไว้ให้ resume
- `restart`: เทียบเท่า safe stop แล้ว start; ต้องไม่ duplicate worker/card
- supervisor crash แล้ว start ใหม่ต้อง reconcile state จาก filesystem/Git/Linear ไม่เชื่อ memory เดิม
- supervisor ไม่ embed AI model; ใช้ executable ปัจจุบันจาก `os.Executable()` เรียก internal runner โดยไม่ผ่าน shell: `loopctl runner --provider <provider> --role <dev|qa>`. ผู้ใช้ไม่ต้องเรียกคำสั่งนี้เอง. หาก `provider_path` ใช้งานไม่ได้ `start` ต้อง fail validation ห้ามเดาคำสั่งเอง
- `--env-file` parser อนุญาตเฉพาะบรรทัด `KEY=VALUE`/comment แบบ strict, อ่านเฉพาะ key allowlist (`LINEAR_API_TOKEN`), ไม่ execute/source shell syntax และไม่ override environment ที่ process ได้รับมาแล้ว. file ต้อง owner=current user และ mode ไม่เปิดกว้างกว่า `0600`

##### Runner + worktree contract

- internal subcommand `loopctl runner` เป็น unified adapter ตัวเดียวรองรับ provider `codex` และ `claude`; ห้ามสร้าง adapter executable แยก. เลือก provider ด้วย config/argv ไม่สร้าง protocol แยก. Envelope, result schema, exit code, security และ retry semantics ต้องเหมือนกันทั้งสอง provider
- รูปแบบ argv ตายตัวคือ `[<loopctl executable>, "runner", "--provider", <codex|claude>, "--role", <dev|qa>]`; `runner` เป็น internal/debug command จึงไม่แสดงใน top-level help แต่ `loopctl runner --help` ต้องใช้งานได้
- `runner.provider` อนุญาต `codex|claude`; `provider_path` เป็น absolute path ของ CLI ที่ `init` resolve ด้วย `exec.LookPath` แล้ว canonicalize. เปลี่ยน provider มีผลเฉพาะ attempt ใหม่ ห้ามสลับ provider กลาง attempt. `init` และ `start` ต้องตรวจว่า provider CLI ที่เลือกมีอยู่จริง
- Supervisor สร้าง worktree/branch ก่อน spawn Dev: worktree อยู่ใต้ `worktree_root/<card-id>`, branch ชื่อ deterministic `loop/<card-id>` จาก `base_sha` ของ `dev`. เรียกซ้ำต้อง reuse เฉพาะเมื่อ branch/worktree ตรง card เดิม
- Runner command เป็น argv array ไม่ผ่าน shell. `loopctl` ส่ง envelope JSON versioned ทาง stdin และตั้ง environment เฉพาะ `LOOPCTL_STATE_ROOT`, `LOOPCTL_CARD_ID`, `LOOPCTL_ROLE`; ห้ามส่ง token
- Dev envelope มี card snapshot, contract hash, repo/worktree, branch/base SHA และ output path. Dev แก้ได้เฉพาะ worktree นี้ รัน verification, commit/push และเปิด PR เข้า `dev`
- QA ใช้ review worktree detached ที่ tested PR head SHA; read-only ต่อ source. หากต้องแก้โค้ดต้องคืน rework ห้าม commit/push
- Runner เขียนผลแบบ atomic ไป `journal/workers/<card-id>/<attempt>.result.json` ตาม schema `{version,card_id,role,outcome,evidence,branch,pr,head_sha,error}` แล้ว exit `0=completed`, `2=invalid task`, `10=retryable`, `11=needs attention`. stdout/stderr เป็น log เท่านั้น ไม่ใช่ state
- Supervisor เชื่อผลลัพธ์ต่อเมื่อ card id, role, attempt และ SHA ตรงกับ runtime record; จากนั้นจึงเรียก transition CLI. ผลลัพธ์ซ้ำต้อง idempotent
- `stop` ส่ง SIGTERM ให้ runner หลังหยุด claim ใหม่, รอ `stop_grace_sec`, แล้วปล่อย process ที่ยังไม่จบให้ reconcile จาก result/Git; ห้าม SIGKILL อัตโนมัติ

#### `loopctl startup enable|disable`
ติดตั้ง/ถอด macOS user LaunchAgent สำหรับ project นี้. ต้อง `init` ก่อนและ `enable` ซ้ำต้อง idempotent. LaunchAgent ห้ามฝัง token ใน plist; ProgramArguments เรียก `loopctl start --env-file /Users/<user>/.env`. `loopctl` parse แบบ data ตามกฎข้างบน—ห้าม shell/source file—และ reject `.env` ที่ owner/mode ไม่ปลอดภัย. ทางเลือกในอนาคตคือ Keychain แต่ไม่ใช่ contract รอบนี้

#### `loopctl config get [KEY]` / `config set KEY VALUE`
อ่าน/แก้เฉพาะ key ที่ schema อนุญาตด้วย atomic write. validate ก่อน commit และห้ามใช้แก้ secret. ค่า `linear.sync_interval_sec` default 300 และต้องอยู่ในช่วงที่กำหนดใน schema

#### `loopctl sync`
sync Linear แบบมีขอบเขต team/project ที่ config อนุญาต:
1. อ่านเฉพาะ issue ใน Backlog ที่มี `loop:ready`
2. parse `loop-card`, validate DoR/schema/dependency และตรวจ UUID ซ้ำทั้งระบบ
3. สร้าง local card ใน `todo` ก่อน แล้วจึงย้าย Linear เป็น Todo; ถ้าขั้นหลัง fail ให้บันทึก pending outbound action และ retry โดยไม่สร้าง card ซ้ำ
4. อัปเดตสถานะ/PR/finding summary จาก local ไป Linear โดยไม่ทับ requirement ฝั่งคน
5. คำนวณ `contract_hash`: ถ้าเปลี่ยนก่อน claimให้อัปเดต local cardหลัง validate; ถ้าเปลี่ยนตั้งแต่ `claimed-dev` เป็นต้นไป ให้ `spec_changed=true`, หยุด transition อัตโนมัติ และส่ง `needs_attention`. `updatedAt` เปลี่ยนอย่างเดียวไม่ถือว่า spec เปลี่ยน
6. ถ้า issue ถูก cancel/archive หรือลบ `loop:ready`: ก่อน claim ให้ย้าย local card ไป `cancelled` เพื่อคง audit trail; หลัง claim ให้ mark `needs_attention` และหยุดที่ safe point
7. Linear ล่มให้ exit 8, ทำงาน local ที่ claim ไปแล้วต่อได้ แต่ห้าม claim งานใหม่ที่ต้องพึ่ง state/approval จาก Linear

sync ต้อง idempotent ด้วย `linear_issue_uuid` และ durable outbox ใน `runtime/linear.json`; polling ทันทีตอน start และทุก `linear.sync_interval_sec` (default 300)

#### `loopctl add --file CARD.json` / `--stdin`
- เป็น low-level/manual recovery path ไม่ใช่ workflow ปกติ; card จาก Linear ใช้ `sync`
- validate ครบทุก required field + `id` ต้องไม่ซ้ำกับการ์ดใดในระบบ (สแกนทุกโฟลเดอร์)
- **ปฏิเสธ `tier: "L3"`** → exit 2 พร้อมข้อความว่าให้ไป escalate ที่คน
- คำนวณ `hot` จาก `config.hot_paths`
- ใช้ queue lock + durable transaction intent: เขียน `.tmp/`, fsync, สร้าง destination แบบ no-overwrite แล้ว commit เข้า `todo/` — **ห้ามเขียนตรงเข้า `todo/`**

#### `loopctl list [--status STATUS] [--linear ID]`
คืน card summary แบบ deterministic (id, title, status, Linear URL, dependency, worker, PR, flags) เรียง priority แล้ว created time; ไม่แก้ state และห้ามดึง comment/source ที่ไม่จำเป็น

#### `loopctl claim --role dev|qa --worker ID`
หัวใจของระบบ. ทำตามลำดับนี้เป๊ะ ๆ:
1. เช็ค `STOP` / `deadline_at` → exit 6
2. เช็ค backpressure (เฉพาะ role `dev`): นับ in-flight ≥ `max_in_flight` หรือ open PR ≥ `max_open_prs` → exit 5
   - open PR ใช้ GitHub API เป็น authoritative source โดย query repo+base=`dev` และผูก PR กับ active cards
   - เก็บผลสำเร็จล่าสุดเป็น local cache พร้อม `fetched_at`, repo, base, PR IDs/head SHAs และ count
   - ถ้า GitHub ใช้ไม่ได้ ใช้ cache ได้เฉพาะเมื่ออายุไม่เกิน `github.open_pr_cache_max_age_sec`; เพื่อลดความเสี่ยง undercount ให้ใช้ `max(cached_count, local active cards ที่มี open/unknown PR)`
   - cache หมดอายุหรือไม่เคยมี → fail closed ด้วย exit 8 ห้าม claim Dev ใหม่; Dev/QA ที่ claim แล้วทำต่อได้
3. รวบรวมการ์ดผู้สมัคร:
   - `dev`: `rework/` ก่อน แล้วค่อย `todo/` — **ลำดับนี้ห้ามสลับ**
   - `qa`: `in_review/`
4. ก่อนเลือก ให้ย้ายการ์ด `blocked/` ที่ dependency อยู่ `done/` ครบกลับ `todo/` อัตโนมัติใน transaction เดียว; dependency ที่ยังไม่ครบให้คง/ย้าย `blocked` พร้อมเหตุผล
5. **เฉพาะ role `dev`** — กรอง conflict:
   - อ่าน `touches` จาก reservation ทั้งหมดที่ยังไม่ released ไม่ใช่เฉพาะ `claimed-dev/`; ตอน claim rework ให้ไม่นับ reservation ของ card ตัวเอง แต่ยังต้องชนกับ card อื่นตามปกติ
   - การ์ดที่ `touches` ตัดกัน (glob match ทั้งสองทาง) → ข้าม + `conflict_skips++`
   - การ์ดที่ `hot=true` → dispatch ได้เฉพาะเมื่อไม่มี active reservation ของ card อื่น; เมื่อ hot reservation active ห้าม dispatch การ์ดอื่น แต่ owner card กลับเข้า rework ได้
   - การ์ดที่ `conflict_skips ≥ conflict_skip_boost` → เลื่อนขึ้นหน้าสุดของรอบถัดไป
6. Dev claim ต้องสร้าง durable reservation ก่อนเปลี่ยน location. Reservation คงอยู่ผ่าน `claimed-dev`, `in_review`, `claimed-qa`, `rework` และ `needs_attention` ที่ยังมี PR/branch เปิด; release เฉพาะ merge สำเร็จ, PR/branch ปิดหรือยกเลิกโดยคน, หรือคืน `todo` หลังพิสูจน์ว่าไม่มี diff/PR. จากนั้นย้ายเข้า `claimed-{role}/` ตาม §3.2
   - `os.IsExist` / `os.IsNotExist` = มีคนหยิบไปก่อน → **ลองใบถัดไป** (อย่ารีบ exit 4)
   - ลองครบทุกใบแล้วไม่ได้เลย → exit 4
7. อัปเดต `claimed_at` / `claimed_by` / `status` + append history + journal
8. พิมพ์การ์ดทั้งใบเป็น JSON ออก stdout

**ไม่มีการ์ดให้ claim → exit 3 พร้อม `{"reason": "empty"|"all_conflicted"|"all_blocked"}`** — เหตุผลต่างกัน orchestrator ตัดสินใจต่างกัน

#### `loopctl move ID --to STATUS --by ROLE/WORKER [--patch JSON] [--note TEXT]`
ย้ายการ์ด + แก้ฟิลด์ในคราวเดียว
- ตรวจว่า transition ถูกต้องตาม §8 ไม่ถูก → exit 2
- ตรวจสิทธิ์ตามตาราง §4 ไม่ผ่าน → exit 2
- ใช้ protocol §3.2: queue lock → temp+fsync → durable intent → destination no-overwrite → verify hash → ลบต้นทาง → commit receipt. ห้ามลบต้นทางก่อน destination ถูกยืนยัน
- append history + journal ของ role นั้น

#### `loopctl reconcile --role dev|qa`
เก็บกวาดการ์ดค้างใน `claimed-{role}/`:
- `dev`: `claimed_at` เก่ากว่า `claim_stale_min` → ไปดูความจริงที่ runner result/Git:
  - ไม่มี branch และไม่มี diff → คืน `todo/` (`attempts++`)
  - มี branch + diff ครบ → คืน `in_review/` ถ้ามี PR แล้ว ไม่งั้น `needs_attention/`
  - diff ครึ่ง ๆ กลาง ๆ → `needs_attention/` **อย่าเดาว่า worker ตั้งใจอะไร**
- `qa`: stale claim แล้ว PR ยังเปิด/head เดิม → คืน `in_review` (`attempts++`); head เปลี่ยน → `in_review(stale)`; PR merged → `sync-done`; PR closed/unresolvable → `needs_attention`
- role ใดที่ `attempts ≥ max_attempts` → `needs_attention/`; ทุกทางต้องคง reservation หาก PR/branch ยัง active

#### `loopctl status --role dev|qa`
คืนทุกอย่างที่ orchestrator ต้องใช้ตัดสินใจใน**คำสั่งเดียว** (ออกแบบมาเพื่อไม่ให้ agent ต้องไปไล่ `ls` เอง — ประหยัด token):
```json
{
  "counts": {"todo":4,"rework":1,"claimed-dev":2,"in_review":1,"claimed-qa":0,
             "needs_attention":1,"blocked":1,"cancelled":1,"done":7},
  "in_flight": 5,
  "open_prs": 2,
  "slots_free": 1,
  "backpressure": false,
  "claimable_now": ["q5"],
  "blocked_by_conflict": ["q3","q7"],
  "stop": false,
  "deadline_passed": false,
  "peer": {"role":"qa","last_tick_at":"...","stale":false}
}
```

#### `loopctl heartbeat --role dev|qa [--patch JSON]`
เขียน `last_tick_at` + สถิติลง `runtime/{role}.json` (เจ้าของเขียนไฟล์ตัวเองเท่านั้น)

#### `loopctl peer-check --role dev|qa`
อ่าน `runtime/` ของอีกฝั่ง — เก่ากว่า `heartbeat_stale_sec` → `{"peer_dead": true}` + exit 0
(orchestrator เป็นคนตัดสินใจว่าจะหยุดรับงานใหม่ — CLI แค่รายงาน)

#### `loopctl findings ID --file FINDINGS.json`
แนบ findings ทุก severity โดย dedupe จาก fingerprint ของ `file,line,issue,violates,evidence,severity`:
- validate ตาม §5 (blocking ต้องมี `violates`)
- มี blocking → `rework_count++` และย้าย `rework/` หรือ `needs_attention/` เมื่อครบเพดาน
- มีเฉพาะ non-blocking → บันทึกไว้โดยคง `claimed-qa`; QA ใช้ `qa-merge` ต่อได้ และ retry ห้ามเพิ่ม finding/rework ซ้ำ

#### `loopctl resolve ID --to todo|rework|cancelled --by human/ID --note TEXT [--close-pr]`
ทางเดียวที่คนแก้ `needs_attention`/ยกเลิกงาน:
- note และ human identity required; append audit/history และ sync Linear
- `todo`: อนุญาตเมื่อไม่มี open PR/diff; clear claim fields แต่ห้าม reset attempts/rework_count/findings
- `rework`: อนุญาตเมื่อมี branch/PR ที่ Dev แก้ต่อได้; คง reservation
- `cancelled`: terminal และ sync Linear Canceled. ถ้ามี open PR ต้องระบุ `--close-pr`; CLI ตรวจว่า PR เป็นของ card และ base=`dev` ก่อนปิด แล้วจึง release reservation; worktree มีสิทธิ์ถูกลบใน `gc-worktrees`
- ห้าม resolve ไป `done`; ถ้าเงื่อนไขไม่ชัดให้คง `needs_attention`

#### `loopctl qa-merge ID --by qa/WORKER`
happy path หลัง QA ผ่าน:
- ตรวจ card อยู่ `claimed-qa`, base เท่ากับ `dev`, PR head SHA ตรง `tested_head_sha` ที่ QA ตรวจ, ไม่ stale/spec_changed, acceptance evidence ครบ, required CI checks ผ่าน และไม่มี blocking finding
- merge ด้วย merge commit เท่านั้น; ห้าม squash/rebase merge
- ปฏิเสธ base `main`, `master`, release/prod/staging หรือชื่ออื่นที่ไม่ใช่ `dev` แบบ hard fail
- merge สำเร็จแล้วเขียน durable receipt ก่อน sync Linear; retry ต้องตรวจ merged commit เดิมและห้าม merge ซ้ำ
- QA worker ที่ตรวจห้ามแก้ source ใน PR; หากต้องแก้ให้ส่ง `rework`

#### `loopctl mark-stale --base-moved --merged-card ID --base-sha SHA`
เรียกอัตโนมัติหลัง merge เข้า `dev` พร้อม merged card/touches/base SHA ใหม่:
- mark stale เฉพาะการ์ด `in_review`/`claimed-qa` ที่ reservation `touches` overlap กับ merged card
- claimed QA ที่ overlap ให้ยุติ attempt ปัจจุบันอย่างปลอดภัยและกลับ `in_review`; ต้อง verify ใหม่กับ base/head SHA ล่าสุด
- การ์ดไม่ overlap ทำต่อได้ แต่ `qa-merge` ยังต้องยืนยัน PR mergeable และ required checks บน head SHA ล่าสุด
- ถ้าไม่ทราบ provenance/touches ของ base movement ให้ fail-safe mark ทุกการ์ด review เป็น stale

#### `loopctl sync-done`
ยืนยันจาก GitHub ว่า `qa-merge` merge PR เข้า `dev` สำเร็จ → ย้ายการ์ดไป `done/` และ sync Linear เป็น Done. ถ้า merge สำเร็จแต่ Linear ล่ม ให้ local เป็น `done` พร้อม pending sync; retry ห้าม merge ซ้ำ
**`qa-merge` + `sync-done` เป็นทางเดียวที่การ์ดจะเข้า `done/` ได้** — ไม่มีคำสั่ง generic ให้ agent ตั้ง done เอง
- หลัง local done ให้ release reservation และเรียก selective stale evaluation ด้วย merged card/touches/base SHA; worktree รอ `gc-worktrees`

#### `loopctl gc-worktrees`
ลบเฉพาะ worktree ที่ card เป็น `done`/`cancelled`, reservation released, PR merged/closed และไม่มี runner ใช้งาน; เก็บ worktree ตลอด `claimed-dev`, `in_review`, `claimed-qa`, `rework`, `needs_attention` ที่มี branch/PR เปิด แล้วค่อย `git worktree prune`

#### `loopctl doctor`
ตรวจสุขภาพทั้งระบบ: replay transaction intents, status/location, id/Linear UUID ซ้ำ, reservation-card-PR consistency, active card ที่ไม่มี worktree, orphan worktree, `.tmp/`, filesystem/hard-link capability. ซ่อมเฉพาะกรณีที่ intent/receipt/Git fact พิสูจน์ได้; กรณีคลุมเครือรายงานและ exit 7

---

## 7.2 Source of truth และ sync ownership

| ข้อมูล | เจ้าของความจริง | อีกฝั่งทำอะไรได้ |
|---|---|---|
| requirement, acceptance, out-of-scope, priority, approval | Linear | local เก็บ snapshot + revision; ห้ามแก้กลับอัตโนมัติ |
| claim, worker, retry, rework, conflict, recovery | filesystem | Linear แสดง summary เท่านั้น |
| branch, PR head SHA, CI/merge receipt | Git/GitHub | card และ Linear เก็บ reference |
| Done | GitHub merged-to-`dev` fact | local/Linear สะท้อนผล |

Linear status mapping ตายตัวตาม category/name ที่ resolve ตอน `init`:

| local | Linear |
|---|---|
| ยังไม่ import | Backlog (+ `loop:ready` เมื่ออนุมัติ) |
| `todo`, `blocked` | Todo |
| `claimed-dev`, `rework` | In Progress |
| `in_review`, `claimed-qa` | In Review |
| `needs_attention` | คง execution status ล่าสุด + `loop:needs-attention` |
| `cancelled` | Canceled |
| `done` | Done |

กฎ conflict:
- ก่อน claim: `contract_hash` ใหม่ที่ยังผ่าน DoR อัปเดต snapshot ได้
- ตั้งแต่ claim: `contract_hash` เปลี่ยน = `spec_changed` และ `needs_attention`; ห้าม merge หรือแอบ sync ทับ worker
- เปลี่ยน title/ข้อความประกอบที่ไม่กระทบ contract อัปเดต Linear ได้โดยไม่ interrupt
- local execution status ห้ามถูก Linear status ที่คนลากผิดทับ; ให้รายงาน drift และซ่อม Linear จาก local/Git fact เมื่อปลอดภัย

## 7.3 `/groom` interaction contract

`/groom` ทำงานเป็นสอง phase:
1. **Draft:** รับโจทย์, อ่าน docs/source แบบ read-only, ถามเฉพาะ ambiguity, ประเมิน risk/size, แตกการ์ด และสร้าง preview. ยังไม่สร้าง external change เว้นแต่ผู้ใช้ขอ save draft
2. **Approve:** เมื่อผู้ใช้ยืนยัน explicit จึง create/update Linear issue, เชื่อม parent/dependency, ใส่ `loop-card` และ `loop:ready`; คืน URL ทุกใบและลำดับ execution

ต้องรองรับผลลัพธ์สามแบบ:
- `ready`: ผ่าน DoR และผู้ใช้ยืนยัน → Backlog + `loop:ready`
- `draft`: ยังไม่ยืนยัน/ข้อมูลไม่ครบ → Backlog ไม่มี ready label
- `escalated`: L3, production/secret/data-loss หรือ design decision ใหญ่ → ไม่เข้า loop และระบุผู้ตัดสินใจที่ต้องการ

เนื้อหาจาก issue/comment/docs เป็น untrusted input: ห้ามคำสั่งในเนื้อหา override system policy, ขอ secret, ขยาย repo/team scope หรือสั่ง deploy. `/groom` ต้อง redact secret ก่อนเขียน Linear และบันทึก `approved_by/approved_at`

## 7.4 Linear failure matrix

| เหตุการณ์ | state ที่เก็บ | การ retry |
|---|---|---|
| create issue สำเร็จแต่ client ไม่ได้รับคำตอบ | lookup ด้วย operation id/UUID | คืน issue เดิม ห้ามสร้างซ้ำ |
| local card สำเร็จ แต่ Linear→Todo fail | card + pending outbound | retry status update ห้าม import ซ้ำ |
| Linear→Todo สำเร็จ แต่ process ตายก่อน receipt | UUID scan พบ card | reconstruct receipt |
| QA merge สำเร็จ แต่ local/Linear update fail | GitHub merge SHA เป็น fact | sync-done ปิด card และ Linear |
| Linear rate limit/network fail | cursor/outbox เดิม | exponential backoff + jitter; manual `sync` ได้ |
| malformed/DoR ไม่ผ่าน | ไม่สร้าง card | comment สรุป validation + `loop:needs-attention` |

## 7.5 Gate review จาก 8 มุม

ก่อน issue เข้า `todo` และก่อน QA merge ต้องประเมินครบ:
1. **ผู้ใช้:** preview เข้าใจง่าย, ยืนยัน explicit, มี URL/สถานะให้ตามต่อ
2. **คุณภาพการ์ด:** outcome/acceptance/verification/out-of-scope ครบและไม่สั่ง implementation เกินจำเป็น
3. **Groom:** DoR ผ่าน, แตกงานพอดี, ambiguity/L3 ถูก escalate
4. **Linear:** UUID idempotent, `contract_hash` และ ownership ไม่ขัดกัน (`source_revision` ใช้ polling เท่านั้น)
5. **Dependency:** graph ไม่มี cycle, dependency ใช้ UUID และ completion fact ถูกต้อง
6. **Dev/QA:** แยก context/หน้าที่, tested SHA ตรง, QA ไม่แก้ PR ที่ตนตรวจ
7. **Security:** scope จำกัด, input เป็น untrusted, secret ไม่รั่ว, ห้าม deploy/promote
8. **Recovery:** ทุก external side effect มี durable receipt/outbox และ retry ไม่ทำซ้ำ

หากข้อใดเป็น blocking และแก้อัตโนมัติไม่ได้ ให้ `needs_attention`; ห้ามลดข้อกำหนดเพื่อให้งานเดินต่อ

---

## 8. Transition ที่อนุญาต

```
(groom approved)→ Linear Backlog + loop:ready
(sync)         → todo
todo           → claimed-dev | blocked          [dev]
rework         → claimed-dev                    [dev]
claimed-dev    → in_review | needs_attention | todo(คืน) | blocked   [dev]
in_review      → claimed-qa                     [qa]
claimed-qa     → rework | in_review(stale) | needs_attention | done(qa-merge + sync-done) [qa]
needs_attention→ todo | rework | cancelled      [resolve โดยคนเท่านั้น]
blocked        → todo                           [ระบบอัตโนมัติเมื่อ dependency done]
todo|blocked   → cancelled                      [resolve หรือ sync เมื่อคนยกเลิกใน Linear]
```
**transition อื่นทั้งหมด = exit 2.** โดยเฉพาะ:
- `claimed-dev → done` (ข้าม QA) — **ห้าม**
- อะไรก็ตาม `→ done` ที่ไม่ได้ยืนยัน merge receipt จาก `qa-merge` ผ่าน `sync-done` — **ห้าม**
- `rework → todo` (ล้าง finding ทิ้ง) — **ห้าม**
- การแก้ `needs_attention`/ยกเลิกโดยไม่ผ่าน `resolve` — **ห้าม**

---

## 9. Orchestrator tick — pseudo-code

CLI ต้องรองรับให้เขียน orchestrator ได้ประมาณนี้ (ตัว orchestrator เป็น prompt/skill ไม่ใช่หน้าที่ของ Codex ที่จะ implement แต่ **CLI ต้องพอสำหรับมัน**):

```
# --- dev orchestrator, 1 tick ---
s = loopctl status --role dev
if s.stop or s.deadline_passed: จบ
if s.peer.stale: รายงาน + หยุดรับงานใหม่

loopctl reconcile --role dev

for w in workers ที่จบแล้ว:
    ผ่าน  → loopctl move <id> --to in_review --patch '{"branch":…,"pr":…}'
    ไม่ผ่าน → loopctl move <id> --to needs_attention --note "<เหตุผล>"

if not s.backpressure:
    while slots_free > 0:
        card = loopctl claim --role dev --worker w<N>
        exit 3 → break            # ไม่มีอะไรให้ทำ
        exit 4 → continue         # แพ้แย่ง ลองต่อ
        exit 5 → break            # backpressure
        spawn dev worker (background, worktree แยก) ด้วย card

loopctl heartbeat --role dev
ScheduleWakeup ตามตารางใน §11
```

```
# --- qa orchestrator, 1 tick ---
loopctl reconcile --role qa
card = loopctl claim --role qa --worker qa-w<N>
spawn QA runner ด้วย detached review worktree ที่ PR head SHA

ถ้ามี blocking finding:
    loopctl findings <id> --file findings.json
ถ้ามีเฉพาะ non-blocking:
    loopctl findings <id> --file findings.json
    loopctl qa-merge <id> --by qa/qa-w<N>
ถ้าไม่มี finding และ policy ผ่าน:
    loopctl qa-merge <id> --by qa/qa-w<N>

loopctl sync-done
loopctl heartbeat --role qa
```

---

## 10. Invariant ที่ห้ามละเมิด

เขียนเทสให้ครบทุกข้อ:

1. **หลัง transaction สำเร็จหรือหลัง recovery การ์ดมี authoritative location เดียว**; duplicate hard-link window จาก crash ต้องตรวจพบและซ่อม deterministic
2. **การ์ดที่ `touches` ตัดกันมี active reservation พร้อมกันไม่ได้** ตั้งแต่ Dev claim จน PR merge/close
3. **การ์ด `hot=true` ต้องเป็น active reservation เดียวเท่านั้น**
4. **ไม่มีคำสั่งไหนที่ให้ role หนึ่งเขียนไฟล์ที่อีก role เป็นเจ้าของ** (ตาราง §4)
5. **`done/` เข้าถึงได้เมื่อมี GitHub receipt ว่า QA merge เข้า `dev` ผ่าน `qa-merge` และ `sync-done` เท่านั้น**
6. **`tier: L3` ไม่มีทางอยู่ในคิวได้**
7. **finding ที่ blocking ต้องมี `violates` เสมอ**
8. **`rework_count` ไปได้ทางเดียว** (เพิ่มอย่างเดียว ไม่มีคำสั่งไหนรีเซ็ตได้)
9. **ทุกการเขียนเนื้อการ์ดผ่าน `.tmp/` + durable intent + no-overwrite publish** — ห้ามเขียนทับที่เดิม
10. **crash ตรงไหนก็ได้ต้องไม่ทำให้การ์ดหาย** — อย่างแย่ที่สุดคือค้างใน `claimed-*` ซึ่ง `reconcile` เก็บได้
11. Linear issue UUID หนึ่งค่ามี local card ได้ไม่เกินหนึ่งใบ แม้ sync/create/retry พร้อมกัน
12. หลัง claim แล้ว `contract_hash` เปลี่ยนต้องหยุดที่ `needs_attention`; ห้าม QA merge contract เก่า
13. token/secret ไม่มีทางปรากฏใน config/card/journal/stdout
14. QA merge ได้เฉพาะ base `dev`, merge commit, tested head SHA เดิม และ required checks ผ่าน
15. queue mutation ทุกคำสั่งใช้ critical-section protocol เดียวกัน; ห้าม check-then-act นอก lock
16. ทุก transition มี durable intent ที่ replay ได้; crash ระหว่าง phase ห้ามบังคับ doctor เดาสถานะ
17. worktree/reservation ห้ามถูกลบหรือ release ขณะ PR/runner/card ยัง active
18. `blocked` ต้องกลับ `todo` อัตโนมัติเมื่อ dependency ครบ และ dependency graph ต้องไม่มี cycle

---

## 11. Acceptance criteria

ระบบถือว่าเสร็จเมื่อคำสั่งเหล่านี้รันแล้วผ่านทั้งหมด **ผลเทสต้องรายงานตามจริง — เทสที่แดงหรือ skip ต้องบอก อย่ากลบ**

### 11.1 Concurrency (สำคัญที่สุด — ถ้าข้อนี้ไม่ผ่าน อย่างอื่นไม่มีความหมาย)
```
T1  ยิง `loopctl claim --role dev` พร้อมกัน 20 process บนคิวที่มีการ์ด 1 ใบ
    → สำเร็จ (exit 0) พอดี 1 ตัว · ที่เหลือได้ exit 3 หรือ 4 · ไม่มีตัวไหน crash
    → การ์ดอยู่ใน claimed-dev/ ใบเดียว ไม่มีสำเนาที่อื่น

T2  ยิง claim พร้อมกัน 20 process บนคิวที่มีการ์ด 5 ใบที่ conflict-free กันหมด
    → สำเร็จพอดี 5 ตัว · ไม่มีการ์ดใบไหนถูก claim ซ้ำ

T3  dev claim กับ qa claim ยิงพร้อมกัน 50 รอบบนคิวที่มีทั้ง todo และ in_review
    → ไม่มีการ์ดหาย · doctor ผ่านสะอาด

T4  `sync`/manual `add` ยิงพร้อมกับ `dev claim` 50 รอบ
    → ไม่มีการ์ดที่ถูกอ่านตอนเขียนค้าง (JSON เสีย) แม้แต่ครั้งเดียว

T5  kill -9 ระหว่าง move (จำลองด้วย fault injection)
    → หลัง reconcile: การ์ดยังอยู่ 1 ที่ · JSON ไม่เสีย · doctor ซ่อมได้
```

### 11.2 Scheduling
```
T6  การ์ด A{src/api/**} B{src/api/user.ts} → claim ได้ทีละใบเท่านั้น
T7  การ์ด hot=true → claim ไม่ได้เมื่อมี active reservation และเมื่อ hot active ห้ามการ์ดอื่น claim
T8  การ์ดถูกข้ามครบ conflict_skip_boost รอบ → รอบถัดไปได้คิวหน้าสุด
T9  rework/ ถูกหยิบก่อน todo/ เสมอ แม้ todo จะมีใบที่เก่ากว่า
T10 depends_on ที่ยังไม่ done → ไม่ถูก claim
T10a dependency ใบสุดท้าย done → blocked กลับ todo อัตโนมัติ; graph cycle ถูก reject
```

### 11.3 Rule enforcement
```
T11 transition นอกตาราง §8 → exit 2 ทุกกรณี
T12 add การ์ด tier=L3 → exit 2
T13 findings blocking ที่ไม่มี violates → exit 2
T14 พยายาม move --to done ตรง ๆ → exit 2
T15 backpressure: in_flight เต็ม → claim ได้ exit 5
```

### 11.4 Glob matcher (§7.1)
```
T16 ทุกแถวในตาราง §7.1 match/ไม่ match ตามที่ระบุ
T17 PatternsOverlap: {"src/api/**"} vs {"src/api/user.ts"} → true
    {"src/api/**"} vs {"src/lib/**"} → false
    {"*.config.*"} vs {"app/vite.config.ts"} → false
    {"src/**"} vs {"src/x.ts"} → true
T18 ไม่มีเคสไหนที่ overlap จริงแล้วตอบ false (ทดสอบด้วยชุด path จริงจาก repo)
```

### 11.5 คุณภาพโค้ด
```
T19 go.mod ไม่มี require แม้แต่บรรทัดเดียว (ตรวจด้วย script)
T20 go vet ./... สะอาด · go build สำเร็จบน go1.26 darwin/arm64
T21 loopctl --help อธิบายทุก user-facing command + exit code ครบ; `loopctl runner --help` อธิบาย internal runner contract
```

### 11.6 Groom + Linear + lifecycle
```
T22 /groom ที่ยังไม่ยืนยันหรือ DoR ไม่ครบ → draft ไม่มี loop:ready และไม่มี local card
T23 ผู้ใช้ยืนยัน → Linear issue มี loop-card schema ถูกต้อง, approval audit และ loop:ready
T24 sync issue เดิมพร้อมกัน/ซ้ำ 20 process → local card พอดี 1 ใบด้วย Linear UUID
T25 local add สำเร็จแต่ Linear update fault → retry แล้วไม่ duplicate และจบที่ Todo ทั้งสองฝั่ง
T26 contract_hash เปลี่ยนก่อน claim → snapshot อัปเดต; เปลี่ยนหลัง claim → needs_attention;
    เปลี่ยนเฉพาะ status/comment/updatedAt → ไม่ interrupt
T27 cancel/archive/remove ready ก่อน claim → ถอน local todo; หลัง claim → safe stop + needs_attention
T28 Linear API ล่ม → exit 8, outbox/cursor ไม่หาย, งานที่ claim แล้วทำต่อได้ แต่ไม่รับ intake ใหม่
T29 qa-merge ปฏิเสธ base อื่นนอกจาก dev, stale/spec_changed, head SHA เปลี่ยน, CI ไม่ผ่าน หรือ blocking finding
T30 qa-merge ผ่าน → merge commit เข้า dev พอดีครั้งเดียว; crash หลัง merge recover ด้วย merge SHA และ Linear→Done
T31 init/start/startup enable ซ้ำไม่สร้าง supervisor/LaunchAgent ซ้ำ; restart resume card เดิม
T32 scan config/card/journal/stdout หลังทุก flow → ไม่มี token/secret
T33 Linear status ถูกลากผิดระหว่าง execution → ไม่ทับ local claim; sync รายงานและซ่อม drift ตาม ownership
T34 card A เข้า in_review แล้ว card B ที่ touches overlap ยัง claim ไม่ได้จน A merged/PR closed
T35 non-blocking findings ถูกเก็บและ QA merge ต่อได้; retry findings ไม่เพิ่มซ้ำ
T36 resolve บังคับ human/note และเงื่อนไข todo/rework/cancelled; ห้าม resolve เป็น done
T37 base ขยับจาก card A → stale เฉพาะ review cards ที่ touches overlap; provenance หาย → stale ทั้งหมด
T38 gc-worktrees ไม่ลบ active/review/rework/needs_attention worktree และลบได้หลัง merged/closed+released
T39 kill -9 ทุก transaction phase → doctor replay intent ได้ผลเดียว ไม่มีการ์ดหาย/เดาสถานะ
T40 fake runner รับ envelope versioned, result ซ้ำ idempotent, SHA/attempt/card mismatch ถูก reject
T41 adapter เดียวรัน fake Codex/Claude providers ด้วย envelope/result contract เดียว; เปลี่ยน provider กลาง attempt ถูก reject
T42 GitHub authoritative count ถูกใช้; API ล่ม+cache สดใช้ max(cache,local); cache เก่าหรือไม่มี cache → exit 8 และไม่ claim ใหม่
T43 LINEAR_API_TOKEN อ่านจาก process env หรือ strict --env-file เท่านั้น; shell syntax ไม่ execute, unsafe owner/mode ถูก reject และ token ไม่รั่วใน plist/log/output
```

---

## 12. Deliverables

| # | ไฟล์ | หมายเหตุ |
|---|---|---|
| 1 | `cmd/loopctl/main.go` + package ย่อยตามเหมาะสม | stdlib only, `go.mod` ไม่มี require |
| 2 | `internal/glob/` | matcher + `PatternsOverlap` ตาม §7.1 แยกเป็น package ของตัวเองเพราะเป็นหัวใจของ scheduler |
| 3 | `*_test.go` | unit/CLI tests ที่ทำ offline ได้, รันด้วย `go test ./...` |
| 4 | `tests/stress.sh` | ยิง concurrent จริงหลาย **process** (T1–T5) — goroutine ไม่นับ ดูหมายเหตุด้านล่าง |
| 5 | `README.md` | build, ทุกคำสั่ง, exit code, ตัวอย่าง flow เต็ม 1 รอบ |
| 6 | `examples/card.json` + `examples/config.json` | ตัวอย่างที่ใช้ได้จริง |
| 7 | `Makefile` | `make build` `make test` `make stress` |
| 8 | `/groom` skill | draft/preview/approve, DoR validation, Linear create/update, prompt-injection guard |
| 9 | `docs/linear-contract.md` | issue template, status mapping, ownership, failure matrix, token setup |
| 10 | `tests/integration/` | fake Linear/GitHub server + unified runner adapter; ครอบคลุม T22–T43 โดยไม่แตะ production services |

> **T1–T5 ต้องยิงเป็น process จริง ไม่ใช่ goroutine.** race ที่เราต้องพิสูจน์อยู่ **ข้าม process บน filesystem** — goroutine ใน process เดียวใช้ page cache และ fd table ร่วมกัน จึงไม่ได้ทดสอบสิ่งที่เราต้องการ. `go test -race` ก็ไม่ครอบคลุมเคสนี้เช่นกัน (มันตรวจ data race ใน memory) — จะรันเพิ่มก็ดี แต่**ห้ามนับว่าเป็นหลักฐานของ T1–T5**

**ยังไม่ต้องทำในรอบนี้:** deployment/promotion automation และ web dashboard. Dev/QA orchestrator, `/groom`, Linear sync และ local supervisor อยู่ใน scope ใหม่

User-facing entry points ต้องรองรับทั้ง slash command `/loopctl` และ skill invocation `$loopctl`; ทั้งคู่เรียก workflow/CLI contract เดียวกัน ห้ามมี state machine แยก

---

## 13. Context ที่ต้องรู้ (เดาเองไม่ได้)

- **สภาพแวดล้อม:** macOS-only (Darwin 25.x), Apple Silicon, zsh, **go1.26.5 darwin/arm64**; ไม่ต้องรองรับ Linux/Windows ใน scope นี้
  ทางเลือกอื่นที่พิจารณาแล้วตัดออก: Python (3.9.6 ที่ติดมากับ macOS เก่าเกินไป), Node/Bun (ต้องมี runtime ติดเครื่อง), Rust (ไม่มี toolchain). Go ถูกเลือกเพราะได้ **binary เดียวที่ไม่ต้องพึ่ง runtime ใด ๆ** + error type ของ syscall แม่น (`os.IsExist` / `os.IsNotExist`) ซึ่งทั้งระบบตั้งอยู่บนการแยกสองอันนี้
- **Git flow ของเจ้าของงาน:** feature → เปิด PR เข้า `dev`; QA merge เข้า `dev` ได้เมื่อผ่าน policy · **`main`/`master`/release/staging/prod ห้ามระบบ merge/deploy/promote** · ใช้ merge commit เสมอ ห้าม squash/rebase-merge
- **ห้ามใส่ AI attribution** ใน commit message / PR body ทุกกรณี (ห้ามทั้งบรรทัด `Generated with…` และ trailer `Co-Authored-By`)
- **ห้ามแตะ production, secret, `.env`** ทุกกรณี
- ระบบนี้ต้องรันควบคู่กับ repo จริงของเจ้าของงาน: dev worker แก้เฉพาะ worktree ของการ์ด; `reconcile`/`sync-done` อ่าน Git/GitHub, `gc-worktrees` จัดการเฉพาะ worktree ที่พิสูจน์ว่า orphan และ `qa-merge` เปลี่ยนสถานะ PR เฉพาะ base `dev`. คำสั่งอื่นห้ามแก้ repo

---

## 14. ลำดับที่แนะให้ทำ

| Milestone | ขอบเขต | เสร็จเมื่อ |
|---|---|---|
| **M0** | `internal/glob` — matcher + `PatternsOverlap` | T16–T18 ผ่าน · **ทำก่อนอย่างอื่น** เพราะ M3 ทั้งก้อนตั้งอยู่บนมัน และเป็นส่วนที่ผิดง่ายที่สุด |
| **M1** | โครง + `init` `add` `list` `status` + card/config validation | T12, T19–T21 ผ่าน |
| **M2** | `claim` `move` + transition rules + ตารางสิทธิ์ | T1, T2, T9, T11, T14 ผ่าน |
| **M3** | durable reservation + conflict/hot/starvation + depends_on/auto-unblock | T6–T10a, T34 ผ่าน |
| **M4** | transaction intent + `reconcile` `doctor` + crash safety | T3–T5, T39 ผ่าน |
| **M5** | `findings` `resolve` `heartbeat` `peer-check` selective `mark-stale` `sync-done` `gc-worktrees` | T13, T15, T35–T38 ผ่าน + README เสร็จ |
| **M6** | Linear client + durable sync/outbox + contract hash + `LINEAR_API_TOKEN` env-file safety | T24–T28, T32–T33, T43 ผ่าน |
| **M7** | `/groom` skill + Linear issue contract | T22–T23 ผ่าน |
| **M8** | unified Codex/Claude adapter + runner/worktree + supervisor/startup + GitHub backpressure + `qa-merge` recovery | T29–T31, T40–T42 ผ่าน + end-to-end flow |

**ส่งงานทีละ milestone** พร้อมผลเทสตามจริง อย่ารวบส่งทีเดียวตอนจบ

---

## 15. คำถามที่ต้องถามกลับ ไม่ใช่เดา

ถ้าเจอสถานการณ์เหล่านี้ **หยุดแล้วถาม**:
- อะไรก็ตามที่ทำให้ต้องเพิ่ม dependency

Runner binding ที่ยืนยันแล้ว:
- unified adapter คือ internal subcommand ของ binary เดียว: `loopctl runner --provider <codex|claude> --role <dev|qa>`
- `loopctl start` เป็นผู้เรียก runner อัตโนมัติ; ผู้ใช้ไม่ต้องเรียกเอง
- provider เริ่มต้นคือ `codex`; path จริง resolve และ validate ตอน `init` โดยไม่ผ่าน shell

ตัวอย่าง Linear binding (ผู้ใช้ต้องแทนค่าจาก workspace จริง):
- workspace `Acme` (`00000000-0000-4000-8000-000000000001`)
- team `ENG` (`00000000-0000-4000-8000-000000000002`)
- statuses: `Backlog`, `Todo`, `In Progress`, `In Review`, `Done`, `Canceled`, `Duplicate`
- labels: `loop:ready` และ `loop:needs-attention`

---

## ภาคผนวก — เอกสารออกแบบเบื้องหลัง

อ่านเพื่อเข้าใจ *ทำไม* ไม่จำเป็นต่อการ implement แต่ช่วยตัดสินใจตอนเจอเคสที่สเปคไม่ครอบคลุม:

- `plan.html` — ภาพรวม 3 เลน, ข้อจำกัดของ `/loop` (ช่วงพัก 60–3600s)
- `dev-queue-loop.html` — ทำไมต้องเป็น queue ไม่ใช่ open-ended loop, ticket contract 4 ข้อ
- `qa-loop-and-grooming.html` — ทางกลับของ QA 3 ชั้น, กับดักของ finding ที่ละเอียดเกินจนกลายเป็นสั่งวิธีแก้
- `orchestrated-loops.html` — concurrency model, conflict-set, backpressure, ปัญหาที่การขนานสร้างขึ้น
