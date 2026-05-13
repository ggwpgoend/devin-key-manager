# PR-3 Test Plan — Quota Detection + Cooldown + Handoff

## What changed (user-visible)

- A running task whose key hits its quota is now **automatically continued
  on the next active key**, with the full chat history seeded as context.
- `/keys` shows a **cooldown badge with deadline + cycles counter** for keys
  in cooldown.
- The chat page has a **"Rotate now" amber button** for manual rotation and
  shows **inbound / outbound handoff banners** when relevant.
- The task detail page has a new **Handoffs table** linking dying sessions
  to their replacements with a markdown view link.

## Primary flow (the one test we run)

We start the manager pointed at a mock Devin server. The mock is configured
to return `HTTP 402 Payment Required` for any session that was created under
key `bad-key`, and `200` for the second key `good-key`. We add both keys via
the UI, create a task, and watch the background poller rotate the task off
the bad key onto the good key with a handoff entry persisted.

### Step 1 — Setup state baseline

- Manager is running on `http://localhost:8765`.
- `DEVIN_API_BASE_URL` points at the mock server (set in env before
  launching).
- Two keys live in the pool: `bad-key` (trial) and `good-key` (trial).

**Assertions on `/keys` before triggering:**
- Both rows show `active` pill (emerald) — no cooldown badges.
- "Last checked" is `—` for both initially, then becomes `✓ valid …` after
  the on-start checker tick (the mock returns 200 for both at this point —
  the bad-key 402 only kicks in on session-level calls, not on
  `GET /sessions?limit=1`).

### Step 2 — Create a task; it lands on the bad key

- Click "+ Add task" on `/tasks`, fill `Title=quota demo`,
  `Prompt=test the quota path`, click Create.
- Wait for HTMX redirect to `/sessions/{session_id}`.

**Assertions:**
- The chat header shows `Key: bad-key · trial` pill.
- The header status pill is `running` (emerald).
- The Devin session id row is filled (e.g. `devin-bad-1`).
- There is **no** inbound or outbound handoff banner.
- The "Rotate now" amber button is **visible** (gated by status=running).

**Why a broken implementation would fail here:**
If `StartTask` were dispatching to the wrong key (e.g. picking `good-key`
first), the test would show the wrong key label. The pill must specifically
say `bad-key`.

### Step 3 — Poller hits 402 → rotates

The manager's background poller fires `SyncSession` every 5s. The mock will
return 402 on `GET /session/{id}`. The expectation: within ~10 seconds the
manager should:
1. Set the bad-key session status to `quota_exhausted`, then `handoff_done`.
2. Cooldown the bad-key for 24h.
3. Open a fresh Devin session under the good key.
4. Persist a handoff row linking the two sessions with markdown.
5. Seed the new session with a system note + the markdown as user text.

**Concrete assertions on `/keys` after rotation:**
- Bad-key state column reads `cooldown 24h` (amber pill).
- A grey `until MM-DD HH:MM` deadline is rendered next to the pill.
- A grey `1/2` cycles counter is rendered.
- Good-key state column still reads `active` (emerald).
- The bad-key row's "Last used" stamp matches the time we created the task.

**Concrete assertions on `/tasks/{task_id}`:**
- The **Sessions** table has **2 rows**.
- Row 1: Key=`bad-key`, Status pill = `handoff_done` (slate).
- Row 2: Key=`good-key`, Status pill = `running` (emerald).
- The **Handoffs** section is present with **1 row**.
- That row shows: When = the time of rotation; From = the bad-key session
  id (clickable); To = the good-key session id (clickable); Markdown =
  a `view` link.

**Concrete assertions on the old (bad-key) chat page:**
- An **outbound handoff banner** (amber border, amber bg) reads
  `This session was rotated out.`
- The banner has an `Open continuation` link pointing at the new session id.
- The banner has a `View handoff markdown` link → `/handoffs/{handoff_id}`.

**Concrete assertions on the new (good-key) chat page:**
- An **inbound handoff banner** (indigo border, indigo bg) reads
  `Resumed from previous session.`
- The banner has a `View previous chat` link pointing at the old session.
- The banner has a `View handoff markdown` link.
- The chat history shows a `system` event at the top with the rotation
  reason, followed by a user message whose body contains both
  `# Handoff from previous session` and the original task prompt
  `test the quota path` in a code-fenced block.

**Concrete assertions on the markdown view (`/handoffs/{id}`):**
- HTTP 200, `Content-Type: text/markdown; charset=utf-8`.
- Body starts with `# Handoff from previous session`.
- Body contains the literal text `test the quota path`.
- Body contains a `## Conversation history` section.
- Body contains a `## What to do next` section.

**Why a broken implementation would fail here:**
- If rotation didn't fire: the old chat would stay on `running` indefinitely,
  no second session would appear in the task's Sessions table, no Handoffs
  section would render.
- If the wrong key were marked cooldown: the good-key row would show the
  cooldown badge, or both rows would stay active.
- If the markdown weren't seeded: the new chat would only show the initial
  bootstrap system note (manager-created) and no `# Handoff` payload.
- If `LinkTo` were broken: the Handoffs row's `To` column would show
  `pending` (grey) instead of a clickable session id.

### Step 4 — Manual force-rotate sanity check

On the new (good-key) session, click the amber "Rotate now" button and
confirm the dialog.

**Assertions:**
- Browser navigates to `/sessions/{new_id}?flash=Rotated+to+...`.
- An **emerald flash banner** is shown with text starting `Rotated to`.
- The good-key state on `/keys` is **still `active`** (manual rotation does
  not cooldown the source key — this is the critical differentiator from
  the quota path).
- A 3rd session appears in the task's Sessions table.
- A 2nd handoff row appears in the Handoffs table with reason `manual` in
  the markdown body.

**Why a broken implementation would fail here:**
If `ForceRotate` were accidentally cooldown-ing the source key (the bug
this test is designed to catch), the good-key row would show
`cooldown 24h`. The whole point of manual rotation is that the user just
wants a fresh start without burning the key.

## Out of scope

- Live testing against `api.devin.ai` (no point — the mock proves the wiring
  end-to-end without burning quota).
- The reactivator timing path (covered by unit tests; observing a 24h timer
  in real time isn't feasible).
- Weekly cooldown escalation (unit test exists; same logic, longer timer).
- Multiple-handoff-chain stress (1 handoff proves the LinkTo wiring).
