# PATTERNS.md — the deep how-to

Companion to `SKILL.md` / `AGENTS.md`. Those two files teach the discipline; this one shows the
shapes: correct vs. incorrect side by side, worked tables, copy-pasteable snippets. Examples use
`convctl` as a stand-in CLI name — substitute your daemon's actual binary.

---

## 1. Reading from a subagent — fresh per batch, never long-lived

A spawned/forked subagent that needs to answer a batch of messages reads the journal the same way
anything else does: through the daemon's **file interface** (`inbox`, or the batch handed to it by
`--on-batch`), never through the parent session's memory. The subagent has no access to what the
parent has seen — it starts cold and reads state off disk, same as a completely separate process
would.

```bash
# a fresh subagent invoked for ONE batch:
convctl inbox --new --format json > /tmp/batch.json
# ... reason over /tmp/batch.json, decide replies ...
convctl respond <messageId> "reply text"
# ... then the subagent EXITS. It does not loop, does not wait, does not hold anything open.
```

**The trap, stated plainly:** a long-lived forked subagent must **not** be handed the listener
(the blocking `wait`, or the role of "the thing that stays alive between messages"). Its context
is a snapshot taken the moment it was forked. As soon as it drifts even slightly out of date —
new instructions, a filter change, updated credentials — it's acting on stale state, and worse:
**when it eventually ends, nothing re-arms anything.** The daemon keeps running, but the one thing
that was supposed to turn the daemon's output into replies has quietly stopped existing, and
nothing observes that fact. This has been hit for real: a network of listeners each handed their
`wait` off to a fork "to hold the loop," and the majority silently ended up parked with no
answerer — alive-looking, mute.

**Correct pattern:**

```
daemon (forever, never a subagent)
  --on-batch--> fresh subagent/process (one batch, then exits)
  --on-batch--> fresh subagent/process (next batch, then exits)
  ...
```

**Incorrect pattern:**

```
daemon --spawns once--> long-lived forked subagent
                           |
                           +-- holds `wait` in a loop inside its own turn
                           +-- eventually ends (compaction, timeout, told to stop)
                           +-- nothing re-arms it -> listener is now silently dead
```

Rule of thumb: **the daemon is the only thing allowed to be unbounded.** Every handler — forked,
spawned, headless — is disposable and cheap to re-create. If you find yourself asking "how do I
keep this subagent alive to keep listening," the answer is: don't; let the daemon spawn a new one
next time.

---

## 2. The re-arm loop that isn't a loop

The shape is **wake → answer → re-arm exactly once**, not a loop with a re-arm hidden inside it.

**Correct:**

```bash
# Turn N (this happens because the previous wait exited / the daemon invoked you):
BATCH=$(convctl wait --timeout 3600 --window 400 --count 20 --format json)   # already returned
echo "$BATCH" | handle-and-reply.sh
convctl wait --timeout 3600 --window 400 --count 20 --ack &   # ONE new background wait, nothing after it
# Turn N ends here. The next wake is a fresh invocation, not this process continuing.
```

**Incorrect — a health check bundled with the re-arm:**

```bash
# DON'T:
( convctl listen --status && convctl wait --timeout 3600 ) &
```

This backgrounds a *shell*, not `wait`. The runtime watches the shell; the shell's exit tells it
nothing about whether `wait` ever actually ran or unblocked correctly. If `listen --status` hangs
or the shell buffers oddly, the wake path is severed and looks fine from the outside.

**Incorrect — re-arming inside a loop "to be safe":**

```bash
# DON'T:
while true; do
  convctl wait --timeout 60 --ack
done
```

This isn't a re-arm, it's the polling loop from §1 of SKILL.md wearing a `wait` costume — a fresh
turn is still spent every 60 seconds whether or not anything arrived, because the loop body itself
is what's driving re-invocation, not the runtime's wake mechanism.

**Incorrect — re-arming more than once per wake:**

```bash
# DON'T — two waits now race on the same cursor:
convctl wait --timeout 3600 &
convctl wait --timeout 3600 &
```

One consumer per cursor. If your daemon supports a takeover flag, use it deliberately when you
know the old one is dead — never speculatively "just in case."

---

## 3. Rules-first, escalate second

Most inbound traffic is boring — status checks, greetings, acknowledgements, a known command. A
regex table answers those in milliseconds at zero model cost, and it keeps working even if the
model backing your escalation path is down. Escalate only what genuinely needs judgment.

```
message text                        -> rule
------------------------------------------------------
/^\s*(status|ping)\s*$/i            -> "still here, listening"
/^\s*help\s*$/i                     -> <static help text>
/^\s*(thanks|thank you|ty)\s*$/i    -> "👍" (react, no reply needed)
/^\s*(ack|ok|got it)\s*$/i          -> no-op (already acknowledged)
anything else                       -> escalate
```

Worked flow:

```bash
text_lc=$(echo "$msg_text" | tr '[:upper:]' '[:lower:]' | xargs)
case "$text_lc" in
  status|ping)      convctl respond "$id" "still here, listening" ;;
  help)             convctl respond "$id" "$(cat help.txt)" ;;
  thanks|"thank you"|ty) convctl react "$id" "+1" ;;
  ack|ok|"got it")  : ;;  # no-op
  *)                echo "$msg_json" >> escalate-queue.ndjson ;;  # hand to the model
esac
```

**The escalation contract:** anything not matched by the rule table goes to a model call with (a)
just that message's text and minimal surrounding context — not the whole journal, (b) a
**restricted** toolset appropriate to answering a channel message, never the handler's full
capability set, and (c) a reply routed the same way as a rule match — `respond <messageId>`. The
rule table and the escalation path both end at the same reply mechanism; only the source of the
answer differs.

This also degrades gracefully: if the escalation path (the model, or whatever it calls) is down,
the rule table still answers the traffic it covers, and unmatched messages simply queue instead of
being lost — they're still on the durable journal.

---

## 4. Many users at once

Five people typing at the same moment should cost **one** invocation, not five, and it should
never produce a reply that mixes two people's questions.

- **Coalescing window** — the daemon (not the agent) waits a short window after the first message
  in a quiet period (e.g. 200–800ms) before delivering, so a burst becomes one batch. This is
  daemon behavior; the handler just receives the batch already coalesced.
- **Group by sender** — the first thing a handler does with a batch:

  ```bash
  convctl wait --window 400 --count 20 --group-by sender --format json
  # -> {"bySender": {"alice": [...], "bob": [...]}, "count": N, "senders": 2}
  ```

  Iterate `bySender`, never the flat list, once more than one sender is possible in a batch.

- **Bounded concurrency, per-sender serialization** — answer different senders in parallel (a
  small worker pool, e.g. 4–8 in flight), but process one sender's own messages **in order**,
  never in parallel with each other:

  ```bash
  # pseudocode
  for sender, msgs in bySender.items():
      pool.submit(handle_sender, sender, msgs)   # senders run concurrently
  # inside handle_sender: msgs processed strictly in arrival order for THIS sender
  ```

- **Bound everything** — batch size (`--count`), coalescing window, per-handler wall-clock budget,
  and retry attempts. An unbounded batch or an unbounded worker pool turns one noisy sender into
  a denial-of-service against everyone else's replies.

---

## 5. Idempotency

At-least-once delivery is the norm for this class of system — a crash between "handled" and
"acked," a retried daemon poll, a re-processed batch after a restart can all redeliver a message
you already answered. Design the handler so redelivery is harmless:

- **Dedupe by message id.** Keep a small local set/table of ids already replied to (even just a
  file of recent ids, or check the daemon's own ack/journal state) and skip if seen.

  ```bash
  if grep -qxF "$id" replied-ids.log 2>/dev/null; then
    exit 0   # already handled, redelivery is a no-op
  fi
  convctl respond "$id" "..."
  echo "$id" >> replied-ids.log
  ```

- **Make the reply itself idempotent where you can.** If your `respond`/`send` accepts a
  client-supplied dedup key or the platform naturally collapses identical rapid replies, prefer
  that over relying solely on your own log file, which can itself be lost.
- **Never let dedup state substitute for the ack.** Dedup protects *your* handler from acting
  twice; the daemon's own ack/cursor state is what protects the *pipeline* from redelivering
  forever. Keep them distinct — see ARCHITECTURE.md §5.5 for why conflating cursor and ack is the
  single most common bug in this class of system.

---

## 6. Testing your integration

Prove the pipe works **before** blaming the agent for "not responding."

**Inject a message, assert it's journaled:**

```bash
# however your daemon/adapter accepts test input, e.g.:
convctl _test-inject --from "alice" --text "ping" --in "#general"

# then, within a few seconds:
convctl inbox --all --format json | grep -q '"text":"ping"' \
  && echo "OK: journaled" || echo "FAIL: never reached the journal"
```

If this fails, the problem is upstream of the agent entirely — transport, auth, or normalization —
and no amount of prompting the agent will fix it.

**Detect a severed wake path** (the listener is up but nothing is actually waking a consumer):

```bash
convctl listen --status   # 1. is the daemon itself alive and heartbeat fresh?
#   stale/absent heartbeat -> the daemon is dead or wedged. Fix here first.

convctl _test-inject --from "alice" --text "wake-test-$(date +%s)"
#   with a background `wait` parked (W1), does the runtime actually get re-invoked
#   within one poll interval? If the daemon's heartbeat is fresh but the parked
#   `wait` never returns/exits, the wake path — not the daemon — is what's broken:
#   check for the "tripwire is a detached child" mistake (SKILL.md §7 / AGENTS.md §7).
```

**The general test order, cheapest first:**

1. Daemon status/heartbeat fresh? (no → fix the daemon, stop here)
2. Injected test message shows up in `inbox --all`? (no → transport/adapter bug)
3. A parked `wait` unblocks when that message lands? (no → wake-path bug, likely a
   detached-tripwire mistake)
4. Handler replies, and the reply is visible back on the channel? (no → reply-routing bug, check
   `source.kind` handling)

Each step isolates a different layer. Don't skip to "the agent isn't responding" without walking
this list — in practice most "the agent is ignoring messages" reports turn out to be step 1 or 3,
not the agent's reasoning at all.

---

## 7. When there is no zero-cost wake: the subagent-hosted poller

> *Added alongside [`poll-responder.sh`](poll-responder.sh) and
> [`POLLING.md`](POLLING.md), which are the full treatment. This section is the
> pointer and the two rules you must not get wrong.*

§1 and §2 both assume something re-invokes you: a daemon-spawned handler (W2) or
a runtime that wakes a session when a blocked process exits (W1). W1 is
**runtime-specific** — some runtimes never turn a finished background shell into
a prompt — and W2 is a *fresh* agent, so it cannot draw on what an attended
session already knows.

When you need an attended, context-carrying agent on a runtime whose wake path
you cannot rely on, the portable answer is: **a subagent hosts a blocking poll
with an escalating backoff.** The sleeps live in the shell script, so they cost
zero tokens; a turn is spent only when the call returns.

```bash
reference/skill/poll-responder.sh --tag support \
    --tiers 120,240,360 --max-block 540 \
    --max-cycles 20 --max-runtime 1800 --answer-cmd './answer.sh'
```

**Rule one — the backoff tier must live on disk.** A foreground tool call is
capped (~600s in Claude Code, less elsewhere), so one call cannot span a
2+4+6-minute schedule. The tier is carried across invocations in
`poll-responder.<tag>.json`, escalating on exit 64 and snapping back to the
first tier the moment a message lands.

**Rule two — the subagent is replaceable, never immortal.** Every returned poll
grows its context, so it *will* end; and when it does, the daemon is still
healthy and the journal still filling while **nobody is answering**. So bound
the shift (`--max-cycles` / `--max-runtime`), let it exit **75** = "respawn me"
— distinct from 64 "quiet" and 69 "dead" — and supervise it. The handoff is free
because the subagent holds no state: journal, read cursor, ack file and the
poller's tier file are all on disk.

The check that catches a dead responder (daemon health alone will not — the
daemon is fine):

```bash
convo health --tag support                                  # fresh? necessary, not sufficient
convo journal --tag support --new --limit 5 --format compact
#   fresh + nothing new              -> healthy and quiet
#   fresh + unacked messages piling  -> THE RESPONDER IS GONE. Respawn it.
#   stale/absent                     -> the daemon died; different problem
```

Cost, honestly: roughly **6–7 turns per idle hour** at `--max-block 540`, versus
~120 for a naive `check; sleep 30` loop and zero for W1/W2. Cheap, not free —
see the full table in [`POLLING.md`](POLLING.md) §4. Run `node
examples/poll-demo.js` to watch the whole thing offline.

---

## 8. When NO loop may run at all: one agent session per conversation

*Added alongside [`SESSIONS.md`](SESSIONS.md), which is the full spec. §7 above
is the other trade — read both before choosing.*

§7's poller still has a loop; it just moved the sleeps into a shell script and
the hosting into a subagent. When the requirement is **no loop anywhere** — not
in the main session, not in a subagent — the only wake left is **W2**, the
daemon spawning a handler per batch. W2's stated weakness is that the handler is
*fresh*, with no conversational context.

That weakness is a missing lookup table, nothing more:

```
$AGENT_CONVERSATIONS_HOME/sessions.<tag>.json
{ "<conversationId>": { "runtime": "claude", "sessionId": "…",
                        "createdAt": "…", "lastUsedAt": "…", "turns": 12, "cwd": "…" } }
```

The daemon coalesces a batch, a handler groups it **by conversation**, and
`session-router.sh` resumes that conversation's agent session for exactly one
turn:

```bash
convo listen --adapter ./my-adapter.js --tag support --window 800 \
             --on-batch './route-batch.sh'      # route-batch.sh is in SESSIONS.md §5

printf '%s' "$text" | reference/skill/session-router.sh \
    --conversation "$conversationId" --runtime claude --tag support --timeout 240
# stdout is the reply. 0 answered · 65 bad args · 66 that conversation is busy ·
# 69 the runtime failed (a dead session id lands here, loudly).
```

**Rule one — multi-user isolation is not a feature you implement.** It is what a
separate session id per conversation gives you for free. Two people cannot see
each other's context because they were never in the same session. Measured: two
conversations given different codewords, asked to recall them *simultaneously*,
each returned its own and neither mentioned the other's.

**Rule two — serialise within a conversation, or lose turns silently.** Two
processes resuming ONE session id is the single-consumer bug
([`ARCHITECTURE.md`](../../ARCHITECTURE.md) §5.9) one level up. Measured on
Claude Code 2.1.268: two concurrent `-p --resume` calls on one id both returned
success, created no second session file, and **branched the transcript** — one
whole turn stopped existing, with no error anywhere. The per-conversation lock
is mandatory, and the same reasoning forbids a human opening a router-owned
session interactively while the daemon may resume it.

**Rule three — bound the pool.** Twenty users messaging at once must not fork
twenty agents. Different conversations run in parallel; a global semaphore
(default 4) decides how parallel, and the overflow exits 66 rather than piling
up invisibly.

Cost, honestly: **zero turns per idle hour** — nothing is alive between
messages — against ~6–7 for §7's poller. What you give up is interactivity: one
invocation, one answer, no clarifying question mid-turn. Cold start is real
(4–6s measured for a Claude resume) and you pay it per message instead of per
hour. Full spec, the adapter contract, the runtime matrix (Claude / Codex /
Devin, all three measured) and the captured test output are in
[`SESSIONS.md`](SESSIONS.md).
