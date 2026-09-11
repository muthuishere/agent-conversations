# Conversational agents over a messenger — reference architecture

How to let an AI agent hold conversations on a messaging channel (Teams, Slack, WhatsApp,
Telegram, email, an in-app inbox) **without a polling loop, without losing messages, and
without paying while nothing happens.**

This is written product-agnostically. Every rule here was learned by building and breaking a
real implementation; everything should survive swapping the channel, the agent runtime, and
the product.

---

## 1. The problem

An agent session is a request/response process. It cannot hold a socket, and nothing can
push into it. To notice an inbound message, **something must invoke the agent again.**

The naive answer is a loop inside the agent:

```
while true: check for messages; sleep 5
```

Three things are wrong with it, and they get worse with scale:

1. **It costs a turn per tick.** Tokens are spent proportional to *time*, not to traffic.
2. **It still misses messages** — anything arriving between ticks waits for the next one.
3. **It cannot survive the session.** Close the terminal and the listener is gone.

Every real design is about **who does the invoking**, and the answer is never "the agent".

---

## 2. The core principle

> **Separate receiving from waking from answering.** They have different lifetimes,
> different costs, and different failure modes. Fusing any two of them produces a system
> that either loses messages or burns money.

| Concern | Owner | Lifetime | Cost |
|---|---|---|---|
| **Receiving** — get messages out of the channel, durably | daemon | forever | ~zero, no model |
| **Waking** — hand the message to an agent | the agent host (Herdr) | per message | zero while idle |
| **Answering** — decide and reply | agent | one turn per batch | the only real cost |

Everything below follows from this split.

---

## 3. Reference architecture

```
        ┌──────────────────────────────────────────────┐
        │  CHANNEL   (Teams / Slack / WhatsApp / SMTP) │
        └───────────────────┬──────────────────────────┘
                            │  transport adapter (poll or push)
                            ▼
   ┌────────────────────────────────────────────────────────┐
   │ DAEMON — always on, no model, outlives every session    │
   │  · discovery: which conversations exist                 │
   │  · fetch: delta/cursor per conversation                 │
   │  · normalize: channel payload → canonical envelope      │
   │  · suppress: drop our own messages                      │
   │  · append: durable journal (append-only)                │
   │  · heartbeat: prove liveness                            │
   │  · hand off: the host delivers this                     │
   └───────┬────────────────────────────────────────────────┘
           │ journal (file/db)
           ▼
   ┌────────────────────────────────────────────────────────┐
   │ HOST — the agent host. REQUIRED. (Herdr)                │
   │  · discovery: which agents are addressable, by name     │
   │  · state: idle / working / blocked / done / unknown     │
   │  · deliver: a real "submit a prompt" primitive          │
   │  · backpressure: never into `blocked`, never into self  │
   └───────────────────────┬────────────────────────────────┘
                           ▼
   ┌────────────────────────────────────────────────────────┐
   │ AGENT + SKILL — one turn per batch                      │
   │  filter → decide → reply → re-arm                       │
   └────────────────────────────────────────────────────────┘
```

The three boxes are the three seams in `go/internal/convo/`: `Channel` at the top, `Store` in
the middle, `Host` at the bottom. Two of them have one implementation each that you are
expected to use — the file store, and Herdr. **The top one is the plugin axis**, and
[`go/internal/channel/README.md`](go/internal/channel/README.md) is how you add to it.

---

## 4. Waking an agent — and why the host is fixed

**Decision: the agent host is a required dependency, and it is Herdr.** Delivery into an agent
is always `Host`/Herdr. What follows is not a menu of co-equal options — it is the measurement
that produced that decision, kept because the comparison *is* the justification.

### 4.1 What was measured

All four were built and run. The marks are observed, not estimated.

| # | Mechanism | Who invokes | Idle cost | Latency | Survives session death | Typed states | Portability |
|---|---|---|---|---|---|---|---|
| **H** | **Agent host — a managed workspace submits the prompt** | the host | zero | **7.6s measured** end to end, including the agent's own turn | ✓ — the host outlives the caller | **✓ `idle`/`working`/`blocked`/`done`/`unknown`** | one dependency |
| W1 | Blocked process exits → runtime re-invokes the session | the agent runtime | zero | instant | ✗ | ✗ | runtime-specific |
| W2 | Spawn a fresh headless agent per batch | the daemon | zero | spawn time | ✓ | ✗ — we started it; there is nothing to ask | universal |
| W3 | Keystroke injection into a live interactive session | external driver | zero | ~1s | ✗ | ✗ — scrape the pane and hope | terminal-specific |

Each of the last three carried a defect, and those defects are the decision:

- **W1** — whether a runtime re-invokes a session when a background task exits has to be
  **verified empirically**; the documentation contradicts itself. And it dies with the session.
- **W2** — correct and universal, but every message pays a cold start into an **empty** context.
  There is no accumulated conversation to answer from, and no backpressure because there is
  nothing to be busy.
- **W3** — you type text, guess at the submit timing, then scrape raw terminal text to find out
  what happened. There is no way to tell *finished* from *parked on a permission prompt nobody
  is watching*, so an injector reports a message as handled while it sits behind a dialog.

### 4.2 What a fixed host buys, unconditionally

`blocked` is the state hand-rolled injection never has, and it is the one that matters. Because
the host is always present, these are guarantees rather than features that degrade when absent:

| | guaranteed by the host |
|---|---|
| **typed states** | `idle` / `working` / `blocked` / `done` / `unknown` — a real backpressure policy instead of a guess (`CROSS-SESSION.md` §4) |
| **stable addressing** | address an agent **by name**, re-resolved on every delivery — no cached id that later resolves to a stranger's pane |
| **pane self-detection** | a pane knows its own address, which is the only reason refusing to prompt *ourselves* is possible at all |
| **supervision** | panes are supervised by something that is not us |

Designs that existed only to work around the absence of those — a hand-rolled injector, a
pane-scraping completion check, spawn-because-we-cannot-ask — are demoted, not deleted.

### 4.3 The trade, stated once

A mandatory host is **a hard third-party dependency and a single point of failure for
delivery**: if it is not installed or not running, nothing reaches any agent.

That was accepted deliberately, in exchange for typed states, stable addressing, pane
self-detection, and not hand-rolling injection — four things that are individually hard to get
right and whose absence fails *silently*, which is the failure mode this whole document is
organised against.

Recorded, not re-argued. If it is revisited, revisit it with new measurements.

### 4.4 What becomes of W1 / W2 / W3

They remain in this document and in the codebase as **recorded findings**, not recommended
paths. Concretely: `go/internal/host/exechost` (W2) is **no longer "the portable floor"**. It is
a test double and a worked reference for what the `Host` interface demands — and it earns its
place, because an interface with a single implementation is not an interface. `Host` fitting
both a workspace manager and a plain spawner is the evidence the seam is in the right place.

**The `Host` interface itself stays and is not dead abstraction.** It is what makes the delivery
path testable with no host server running anywhere: the self-delivery guard is asserted end to
end against a *fake host binary* in a temp directory. Remove the interface and those tests go
with it.

---

## 5. What goes in the daemon

The daemon is **infrastructure**: no model, no judgement, no product policy. If it needs an
LLM to decide something, that belongs in the agent.

### 5.1 Transport adapter (the only channel-specific code)

Isolate everything channel-specific behind one interface. Everything else stays portable.

```
listConversations()                    -> [{id, kind, name}]
fetchSince(conversationId, cursor)     -> {messages[], nextCursor}
send(target, text, opts)               -> {id}
identity()                             -> {id, name}
```

Two fetch styles, same interface:
- **Poll** (delta token / `since` timestamp / message id watermark) — always available.
- **Push** (websocket, SSE, webhook) — a latency optimization where offered.

**Build poll first.** Push is a bonus that not every tenant or plan grants you; a
poll-based daemon works everywhere and is the one you can actually test.

### 5.2 Normalization — the canonical envelope

Every channel gets flattened into one shape. This is what makes the agent portable.

```json
{
  "id": "…",
  "at": "2026-09-09T10:00:00Z",
  "from":   { "id": "…", "name": "…" },
  "text":   "plain text, always present",
  "html":   "original rich body, or null",
  "source": { "kind": "chat|channel|email",
              "conversationId": "…",
              "name": "#engineering | dm",
              "threadId": "…" },
  "replyToId": null,
  "mentionsMe": false,
  "raw": { }
}
```

Two fields carry disproportionate weight:

- **`source.kind` + `conversationId`** — how a reply is routed. Getting this wrong sends
  replies into the void, silently. Make the reply API take a *message id* and route itself.
- **`raw`** — never discard the original. You will need a field you didn't anticipate.

### 5.3 Self-echo suppression — non-negotiable

Drop any message authored by our own identity **before it reaches the journal**.

Without it: agent replies → daemon sees the reply → wakes the agent → it replies again.
An infinite loop that costs real money and spams a real channel.

Suppress by **identity**, not by content matching or id-tracking. Two robust patterns:
- outbound posts under a distinct identity (bot/app) and inbound keeps only human identities;
- or filter on `from.id == self.id`.

**A subtle trap:** if the *handler* runs with a different configuration than the daemon, it
replies as a different identity, suppression doesn't recognize it, and the loop reappears.
Pin the handler to the daemon's identity explicitly.

### 5.4 The journal — durability

An **append-only** log per listener (one file/table, one message per line).

- Written **before** any consumer sees the message, so a crash never loses it.
- Never mutated by reads. Reading is not consuming.
- Retention/compaction is a separate concern; the log is the source of truth.

This is what makes "a message that arrives while the agent is thinking" a non-event.

### 5.5 Cursor vs acknowledgement — keep these distinct

The single most common bug in this class of system.

| | meaning | who advances it | consequence if wrong |
|---|---|---|---|
| **read cursor** | delivery position | advances **whenever a consumer is handed a message** | never advanced → **infinite redelivery** |
| **ack** | processed / handled | set explicitly by the consumer | conflated with cursor → messages redelivered forever or dropped silently |

Persist the cursor immediately *after* a successful hand-off, never before. Reading the
journal to look around must **not** advance the cursor.

### 5.6 Adaptive backoff (poll transports)

Fixed-interval polling is either wasteful or slow. Tier it on recent activity:

| state | interval |
|---|---|
| message on the last poll | `active` (e.g. 15s) |
| quiet > 2 min | 60s |
| quiet > 10 min | `idle` (e.g. 5 min) |
| any message arrives | **snap straight back to `active`** |

**Known trade, state it explicitly:** at the idle tier you cannot notice traffic sooner than
`idle`. The first message after a long silence pays full idle latency — precisely when a
human starts a conversation. Mitigate by lowering `idle` while a consumer is attached, or by
letting an attended consumer poll on its own.

### 5.7 Coalescing

Deliver a *batch*, not a message. After the first message, wait a short window (200–800ms)
and hand over everything that lands.

Five people typing at once become **one** agent invocation instead of five. This is the
single biggest cost lever in the whole design, and it improves answer quality — the agent
sees the whole room at once.

### 5.8 Heartbeat — a dead listener must be detectable

**The defining failure of this architecture is silent deafness:** the listener dies, the
agent believes it is live, and nobody finds out until someone asks why they were ignored.

- Daemon writes `{ts, pollCount, lastMessageAt, intervalMs, scope}` every cycle.
- Health check fails if the heartbeat is older than ~3× the interval.
- **A consumer must refuse to block against a stale heartbeat** — return an error rather than
  waiting forever on a corpse. Silence must never be mistaken for "no messages".
- Never report "listening" from memory. Read the heartbeat.

### 5.9 Single consumer per listener

Two consumers on one cursor race for messages and can tear down each other's state. Enforce
one, with a pid/lock file, a clear error on conflict, and an explicit `--takeover`.

---

## 6. What goes in the agent skill

The skill is **operating discipline and product policy**. It contains no transport code.

### 6.1 Discipline (the rules that keep it loop-free)

1. **Never write a polling loop.** The host hands you the message; be woken.
2. **A timeout is not an error.** Distinguish "nothing arrived" from "something broke" by
   exit code, and don't treat aging out as failure.
3. **Never claim to be listening without checking state.** Health-check first.
4. **Delivery goes through the host.** Never type into a pane, never scrape one to find out
   whether you were heard. If the host says `blocked`, a human is needed — say so; do not
   report the message as handled.
5. **One consumer.** Never arm a second while one is live.
6. **Re-arm once per wake**, after answering — not on a timer.

### 6.2 Policy — filters, not plumbing

The daemon delivers **everything**. The skill decides what matters:

```
--from / --exclude-from / --match <regex> / --in <conversation> / --mentions-me
```

> **Change who the agent answers by changing a filter, never by re-plumbing the transport.**

Filters apply at *delivery*, never to the journal — a filtered-out message is still on disk
and still auditable.

### 6.3 Reply routing

Make the API take a **message id** and route itself from `source.kind`:

```
respond <messageId> "text"     # DM → that conversation; channel → threaded reply
```

Do not make the agent choose between `send --chat` and `reply --thread`. It will choose
wrong, the platform will return success, and the reply will vanish. Ask for a *message id*
and let the tool figure out the target — an unknown id must fail **loudly**.

### 6.4 Answering strategy — rules first, model second

Not every message deserves an LLM.

```
message → filter → rule table (instant, free) → matched?  → reply
                                              → unmatched → escalate to a model
```

Most traffic is `status`, `help`, `ping`, acknowledgements. A regex table answers those in
milliseconds at zero cost. Escalate only what genuinely needs reasoning. This is what makes
"many users, fast" affordable — and it degrades gracefully, since a rule table still works
when the model is down.

---

## 7. The split, at a glance

| Concern | Daemon | Skill / agent |
|---|---|---|
| Channel API, auth, pagination | ✅ | ❌ |
| Cursors, delta tokens, retries | ✅ | ❌ |
| Normalization to the envelope | ✅ | ❌ |
| Self-echo suppression | ✅ | ❌ |
| Durable journal | ✅ | ❌ |
| Coalescing window | ✅ | ❌ |
| Backoff / rate-limit strategy | ✅ | ❌ |
| Heartbeat + liveness | ✅ | reads it |
| Agent discovery, state, backpressure | the **host** | ❌ — it asks, it does not implement |
| Refusing self-delivery / `blocked` targets | the **host** path | ❌ |
| Who to answer (filters) | ❌ | ✅ |
| What to say | ❌ | ✅ |
| Reply routing decision | tool routes | ✅ supplies message id |
| Escalation policy | ❌ | ✅ |
| Re-arming | ❌ | ✅ |

**Test:** if changing a *product* rule forces a daemon change, the split is wrong.

---

## 8. Security — inbound messages are untrusted input

This deserves its own section because the architecture actively invites the problem: it takes
text written by anyone and feeds it to an agent holding real tools.

A message saying *"summarise the CEO's inbox"* or *"run the deploy script"* is **data, not an
instruction**. Rules:

1. **A message can request; it can never authorize.**
2. **Sender identity ≠ authorization.** Many channels let a display name be spoofed; even a
   trustworthy channel only tells you who *posted*, not what they may command.
3. **Give the handler a capability boundary.** A message-driven agent should reach the channel
   and its own product surface — not mail, not credentials, not an unrestricted shell.
4. **Escalation must be narrow.** Route unmatched questions to a model with a *restricted*
   toolset, not to a general-purpose agent with everything mounted.
5. **Audit.** The journal is the record of who asked for what — never let a read destroy it.

A fixed host sharpens this rather than softening it. Delivering into a **long-lived** session
means the message joins a context that already holds someone else's work, and inherits whatever
tools that session was granted — which is why delivery must be **consent-gated** to sessions
that opted in, and why "the pane exists" is not consent (`CROSS-SESSION.md` §2.2). A stateless
handler with a small toolset is still far easier to reason about than a long-lived agent
accumulating capability and context; the host makes that a deployment choice you have to make
on purpose.

---

## 9. Failure modes to design against

The listener staying *visibly healthy* while the wake path is severed is the recurring theme.
All of these were observed in practice, not theorized.

| Failure | Symptom | Defence |
|---|---|---|
| Consumer dies early | silence indistinguishable from no traffic | heartbeat; consumer errors on stale |
| Two consumers on one cursor | they race, one dies, nobody notices | single-consumer lock |
| Tripwire orphaned (detached child) | runs fine, its exit wakes nobody | the watched process must be the tripwire |
| Cursor never advances | same message redelivered forever | cursor advances on delivery, not on ack |
| Handler runs as a different identity | echo suppression misses → infinite self-reply | pin handler identity to the daemon's |
| Reply sent to the wrong target type | platform says 200, message vanishes | route by `source.kind`; fail loudly on unknown id |
| Idle backoff during a new conversation | first message waits a full idle period | lower idle while attended |
| Ad-hoc parsing of tool output | false bugs; swallowed errors | give tools a machine-readable *and* human-readable mode |

---

## 10. Scaling

- **Batch, don't serialize.** Coalesce inbound; send replies concurrently (bounded pool).
- **Group by sender.** One person's messages are one conversation; never interleave two
  people's context. Parallelize *across* senders, serialize *within* one.
- **Bound everything.** Concurrency, batch size, handler runtime, retries.
- **Dedupe by message id** across restarts — at-least-once delivery is the norm; make
  handling idempotent.
- **Partition by listener tag** when one process would otherwise serve unrelated products.

---

## 11. Porting to another channel — the growth axis

The host is fixed, so **the channel is where this product grows.** Only the adapter changes,
and the practical guide to writing one — with the contract per method, cursor handling,
self-echo, reply routing, backoff and a checklist — is
[`go/internal/channel/README.md`](go/internal/channel/README.md). A worked second
implementation lives in [`go/internal/channel/teams`](go/internal/channel/teams).

A rough guide:

| Channel | Fetch | Push available | Notes |
|---|---|---|---|
| Teams / Graph | `delta` + token | change notifications (needs public endpoint) | throttles hard; backoff matters. **Measured:** the channel `messages/delta` stream carries top-level messages only — threaded replies need a separate per-thread scan, so a channel cursor is composite |
| Slack | `conversations.history` + cursor | Events API / Socket Mode | Socket Mode is a genuine push |
| Telegram | `getUpdates` long-poll | webhook | long-poll is already near-push |
| WhatsApp (BSP) | webhook only | webhook | usually *must* accept a public callback |
| Email/IMAP | `UID SEARCH` since | IDLE | threading is by header, not id |

If the envelope and the daemon contract hold, the agent skill needs **no changes at all** —
that is the test of whether this architecture was implemented correctly.

---

## 12. Build order

0. **Install the agent host.** It is a prerequisite, not a step you can defer.
1. Transport adapter + normalization (poll only).
2. Journal + cursor/ack semantics.
3. Self-echo suppression.
4. Hand off to the host: resolve by name, respect `idle`/`working`/`blocked`, refuse
   self-delivery. *You now have a working product.*
5. Heartbeat, single-consumer lock, loud failures.
6. Coalescing + adaptive backoff.
7. Filters and the rules-first answering layer.
8. The **second channel**. This is the growth axis, and it is the step that proves the
   envelope and the daemon contract actually hold —
   [`go/internal/channel/README.md`](go/internal/channel/README.md).
9. Capability boundary on the handler.

Ship 0–4 before optimizing anything. A correct slow listener beats a fast deaf one.
