# The polling responder — the portable wake path

> *When no runtime-specific wake exists, a **subagent hosts a blocking poll with
> an escalating backoff**. The main session stays free, the sleeps happen inside
> a shell script, and a turn is spent only when the call returns.*

[`ARCHITECTURE.md`](../../ARCHITECTURE.md) §4 lists three wake mechanisms. Two of
them are effectively free, and neither is portable:

- **W1** — the runtime re-invokes a session when a background task exits. Some
  agent runtimes do this; others track the completion internally and never turn
  it into a prompt, so a background shell finishing wakes nobody. A stop-hook
  that long-polls is the nearest equivalent where one exists. **Verify
  empirically; documentation lies about this constantly.**
- **W2** — the daemon spawns a fresh handler per batch. Universal, survives
  everything, and needs no session at all — but it is *not* the session you are
  talking to, so it cannot ask a follow-up question or use what that session
  already knows.

This document is about the gap between them: **you want a live, attended,
context-carrying agent to answer, on a runtime whose wake path you cannot rely
on.** That is what [`poll-responder.sh`](poll-responder.sh) is for.

It is **cheap, not free.** Say so out loud. Anyone who claims otherwise has not
counted the turns.

---

## 1. The shape

```
main session ──spawns──> SUBAGENT
                            │
                            │  poll-responder.sh          (one tool call)
                            │    ├─ check now ─────────── message? → print/answer, exit 0
                            │    ├─ heartbeat alive? ──── no → exit 69
                            │    ├─ sleep <tier>          ← zero tokens, no model, no turn
                            │    ├─ check again
                            │    └─ … until --max-block → exit 64, tier escalates
                            │
                            └─ loop: call it again (a turn), until the shift ends → exit 75
```

The main session never blocks and never polls. The subagent spends one turn per
*returned call*, not per tick — and an idle channel makes the calls longer and
longer, so an idle hour costs a handful of turns instead of hundreds.

---

## 2. Why the tier lives in a state file

**A foreground shell/tool call is capped.** ~600 seconds in Claude Code; other
runtimes have their own limits, some much shorter. So a 2 + 4 + 6-minute backoff
schedule **cannot** live inside one call — the call would be killed mid-sleep,
and a killed call is indistinguishable from a hung one.

So the tier is carried **across invocations**, in
`$AGENT_CONVERSATIONS_HOME/poll-responder.<tag>.json`:

```json
{ "tier_index": 1, "last_message_at": "2026-09-09T10:00:00Z",
  "consecutive_empty": 3, "cycles": 4, "shift_started_at": 1789013734 }
```

- **empty call** → exit 64, `tier_index` moves one step down the list, clamped at
  the last tier.
- **messages** → exit 0, `tier_index` **snaps back to the first tier** — a
  conversation has started, and the first message after a long silence is
  exactly when latency is most visible (ARCHITECTURE §5.6).
- **missing or corrupt file** → reset to tier 0 and carry on. A truncated JSON
  file must never take the listener down.

`--max-block` bounds one call (default 540s, safely under the cap). The tier
bounds the *sleep between checks*; `--max-block` bounds *the call*. They are
different numbers on purpose, and a tier longer than `--max-block` is simply
clamped for that call.

---

## 3. Exit codes

The daemon's codes are reused unchanged (INTERFACES.md §1) — the poller invents
exactly one, and only because none of the existing ones mean it.

| code | meaning | what the host agent should do |
|---|---|---|
| **0** | messages delivered on stdout (or piped to `--answer-cmd`) | answer them, then call again |
| **64** | aged out, nothing arrived. **Not an error.** | call again — say nothing to anyone |
| **65** | bad arguments | fix the invocation |
| **66** | another consumer already holds this tag | **stop.** Do not start a second listener |
| **69** | the daemon is dead or stale | **stop and report.** Restart the daemon; never silently retry |
| **75** | **shift over** — `--max-cycles` / `--max-runtime` reached | exit cleanly so a supervisor can spawn a replacement |

75 exists because "my shift ended normally", "nothing arrived" and "the listener
is dead" require three different reactions, and collapsing any two of them
reproduces the exact bug INTERFACES.md §1 spends a page warning about. A
supervisor that cannot tell 75 from 69 either respawns forever against a dead
daemon, or treats a healthy shift change as an outage.

---

## 4. The honest cost table

Assume an idle channel and a one-hour window.

| approach | idle turns/hour | portable | async from the main session | notes |
|---|---|---|---|---|
| **naive in-agent loop** (`check; sleep 30`) | **~120** | ✅ | ❌ | a turn per tick, forever. Cost scales with *time*, not traffic |
| **this poller**, tiers `120,240,360`, `--max-block 540` | **~6–7**, dropping toward **~6** as tiers escalate | ✅ | ✅ (in a subagent) | cheap, *not* free. Each returned call is a real turn |
| **W1 zero-cost wake** (runtime re-invokes on background-task exit) | **0** | ❌ runtime-specific | ❌ it *is* the main session | the best option when your runtime actually does it |
| **stop-hook long-poll** (where the runtime offers one) | **0** | ❌ runtime-specific | ❌ | same idea, different hook |
| **W2 daemon-spawns-handler** | **0** | ✅ | ✅ (no session at all) | a *fresh* agent, so no conversational context to draw on |

Where "~6–7" comes from: at `--max-block 540` an idle hour is `3600/540 ≈ 6.7`
returned calls, each one turn. Add a turn whenever a message actually arrives.
That is roughly **one-twentieth** of the naive loop and roughly **six turns more
per hour than free** — a real, small, permanent cost you should decide to pay on
purpose.

Latency is the trade: at the 360s tier, a message can wait up to six minutes.
Shorten the tiers if that matters and pay for it in turns; there is no
configuration in which both are free.

---

## 5. Shift change — the subagent is replaceable, not immortal

**This is the part that fails in practice, so read it before deploying.**

The reflex is to spawn one subagent and think of it as "always there". It is
not. Every poll that returns adds to its context; it has token and wall-clock
limits; a runtime restart takes it with it. **It will end.** And when it does,
the symptom is the worst one this architecture has:

> the daemon is healthy, the heartbeat is fresh, the journal is filling —
> **and nobody is answering.**

Everything you would check looks fine, because everything you would check is the
*daemon*, and the daemon was never the thing that died.

### 5.1 The fix: end the shift on purpose

Do not try to make the listener immortal. Make it **replaceable**, and let it
retire *deliberately* rather than dying mid-sleep:

```bash
poll-responder.sh --tag support --tiers 120,240,360 --max-block 540 \
                  --max-cycles 20 --max-runtime 1800
```

- `--max-cycles N` — end the shift after N checks (counted across invocations,
  because that is what grows the subagent's context).
- `--max-runtime S` — end the shift after S seconds of wall clock.
- Either budget → **exit 75**, cleanly, with the shift counters reset for the
  successor. It is not a signal death and not a timeout; a supervisor can tell
  it apart from both.

Sensible default for a hosted listener: **`--max-cycles 20 --max-runtime 1800`**
— roughly a half-hour shift. Shorten it if each poll returns a lot of messages
(more context per cycle), lengthen it if the channel is nearly silent.

### 5.2 Why the handoff costs nothing

Because **the subagent holds no state at all.** Everything a replacement needs
is already on disk, owned by the daemon or by the poller's own state file:

| file | what it remembers | INTERFACES.md |
|---|---|---|
| `journal/<tag>.ndjson` | every message, durably, append-only | §2.1 |
| `cursor/<tag>.read.json` | delivery position — what has been handed over | §2.3 |
| `cursor/<tag>.ack.json` | what has actually been handled | §2.4 |
| `poll-responder.<tag>.json` | the current backoff tier and shift counters | this doc, §2 |

A fresh subagent runs the same command and resumes exactly where the dead one
stopped: no handoff message, no coordination, no replay, nothing lost. The tier
is deliberately **not** reset by a shift change — the backoff belongs to the
conversation, not to whoever is holding the pager.

Treat that as the headline property, not a footnote: **statelessness by
construction is what makes the listener disposable, and disposability is what
makes it reliable.**

### 5.3 How to supervise

Pick one. Any of them beats hoping.

**(a) A wrapper loop** — simplest, and the one to use if you have anywhere to
run it:

```bash
while true; do
  poll-responder.sh --tag support --max-cycles 20 --max-runtime 1800 \
                    --answer-cmd './answer.sh'
  case $? in
    0|64|75) : ;;                                   # answered / quiet / shift over → respawn
    66) echo "another consumer holds this tag" >&2; exit 1 ;;
    69) echo "DAEMON DEAD — restart it" >&2; exit 1 ;;
    *)  echo "unexpected failure" >&2; exit 1 ;;
  esac
done
```

**(b) A cron / timer** — run one bounded shift every few minutes. It cannot
"stay dead", because nothing is long-lived to begin with.

**(c) A main-session checkpoint** — cheapest to add to an existing agent: on
every wake, before anything else, run one `--once` check and look at the health
of the pair. If the poller is gone, spawn a new subagent.

### 5.4 The one health check that catches a dead responder

Daemon health alone will not do it — the daemon is *fine*. Compare **what has
arrived** against **what has been handled**:

```bash
convo health --tag support            # exit 0 = daemon fresh (necessary, not sufficient)
convo journal --tag support --new --limit 5 --format compact
```

- daemon fresh **and** `--new` empty → healthy and quiet. Nothing to do.
- daemon fresh **and** `--new` growing, oldest entries minutes old → **the
  responder is gone.** Respawn it.
- daemon stale/absent → the daemon died; the responder is a separate problem.

`--new` is the right signal because it lists what nobody has *acked* — so it
answers "is anyone still working?" rather than "did anything arrive?". Make the
answerer ack what it handles (`convo ack <id>`, or `--ack`) and this check stays
honest.

---

## 6. When to prefer what

1. **A runtime-specific zero-cost wake, if your runtime really has one.** Verify
   it: arm the tripwire, inject a message, confirm the session is actually
   re-invoked. Free beats cheap.
2. **This poller** when the wake path is unreliable, unavailable, or you need
   the listener to be *asynchronous from the main session*.
3. **W2, daemon-spawns-handler** when nothing needs to be interactive. It is the
   floor of the whole design: no session, no polling, no cost, survives
   everything. If a rule table can answer most of your traffic, prefer this and
   skip the agent entirely.

These compose. Running W2 for unattended coverage *and* this poller for an
attended session is normal — the read cursor and the single-consumer lock keep
them from fighting, and the journal means neither can lose a message.

---

## 7. Copy-pasteable: a subagent that listens and answers

Give a subagent this, verbatim.

```
You are the listener for tag `support`. You do not write a polling loop and you
do not sleep yourself — the script does both, for free.

Repeat until told otherwise:

  reference/skill/poll-responder.sh --tag support \
      --tiers 120,240,360 --max-block 540 \
      --max-cycles 20 --max-runtime 1800 \
      --filter-args '--mentions-me'

  exit 0  -> messages are on stdout, one per line, id first. Answer each with
             `convo respond <id> "text"`, then `convo ack <id>`, then run the
             script again.
  exit 64 -> nothing arrived. Say nothing. Run it again.
  exit 75 -> your shift is over. Exit cleanly and report that a replacement
             listener is needed. Do NOT keep going.
  exit 69 -> the daemon is dead. STOP and report it. Never retry silently and
             never describe this as "quiet".
  exit 66 -> another consumer holds this tag. STOP; do not start a second one.

Never report "I am listening" from memory — that claim requires `convo health`.
```

### With a rules-first answerer

Most traffic is `status`, `help`, `ping`, "thanks" (ARCHITECTURE §6.4). Answer
those in the shell for nothing, and let the agent's turn be spent only on what
actually needs reasoning:

```bash
#!/usr/bin/env bash
# answer.sh — stdin is NDJSON, one envelope per line (the handler contract,
# INTERFACES.md §4), so the SAME script works under `--on-batch` too.
set -euo pipefail
while IFS= read -r line; do
  id=$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
  text=$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.load(sys.stdin)["text"])')
  case "$(printf '%s' "$text" | tr '[:upper:]' '[:lower:]')" in
    ping|status|health*) convo respond "$id" "up"; convo ack "$id" ;;
    help|"?")            convo respond "$id" "ask me anything, or say status"; convo ack "$id" ;;
    *)                   : ;;   # leave it UNACKED — the agent will see it in `--new`
  esac
done
```

```bash
poll-responder.sh --tag support --answer-cmd './answer.sh' --max-cycles 20
```

With `--answer-cmd` the messages are piped to the command as NDJSON instead of
printed, so one call both **polls and answers**. The command is bounded
(`--answer-timeout`, default 120s) and a failure is logged, never fatal: the
batch is still in the journal, so nothing is lost — and leaving a message
unacked is a deliberate, visible way to hand it up to the model.

---

## 8. Things this pattern does not fix

- **It is not free.** ~6–7 turns an idle hour. Budget it.
- **It is not instant.** At the last tier a message can wait a full tier.
- **It does not survive the machine.** Nothing here restarts the daemon; that is
  a service manager's job.
- **It does not make one consumer into two.** The single-consumer lock is real:
  a second poller on the same tag exits 66, on purpose (ARCHITECTURE §5.9).
- **It does not decide who to answer.** `--filter-args` is passed straight
  through to the daemon — change a filter, never the transport (§6.2).

See it work offline, in about a minute:

```bash
node examples/poll-demo.js
```

---

## Restarting the responder when it goes away

The responder will go away. It ends its shift, or it crashes, or the box reboots.
Plan for that rather than hoping.

**The supervisor must not be an agent.** An agent supervising an agent moves the liveness
problem up a level: now two things can die quietly, and the outer one has to *remember* to
check. A shell loop blocked in `wait(2)` costs nothing and has nothing to forget.

`supervise.sh` is that loop. It reads the responder's exit code and acts:

| exit | meaning | supervisor does |
|---|---|---|
| `0` | answered a batch | respawn now — more may be queued |
| `64` | nothing arrived | respawn now — the backoff tier does the waiting |
| `75` | shift ended cleanly | respawn now with fresh context |
| `69` | **the daemon** is dead | back off exponentially and alert; respawning cannot fix it |
| other | unexpected | back off and alert |

The `69` case is the one worth getting right. A dead daemon is not something the responder
can repair, so hot-looping against it just burns CPU and buries the real error. Back off,
make it loud, and let a human or the daemon's own supervisor deal with it.

### Three layers, each supervising the one below

```
launchd / systemd   restarts the supervisor if it dies or the box reboots
   └── supervise.sh restarts the responder every shift or crash
        └── poll-responder.sh  answers, then ends its shift deliberately
```

Templates for the top layer are in `service/` — `KeepAlive` on macOS, `Restart=always` on
Linux. Both should be installed with lingering enabled so they start at boot rather than at
login; a responder that only runs while someone is logged in is a responder that is missing
overnight.

### Verifying it actually recovered

Restarting is not the same as answering. The check that matters compares two things the
daemon owns:

```sh
convo health                 # is the daemon fresh?
convo journal --new          # are messages piling up unanswered?
```

**Daemon fresh + journal growing = the responder is gone**, whatever the process table says.
That single comparison catches every silent-deafness variant, because it measures the
outcome (messages being answered) instead of the mechanism (a process appearing to exist).
