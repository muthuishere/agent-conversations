# Delivering into an agent session you don't own

Everything else in this repo assumes the agent is yours: your daemon starts it, your router
resumes it, your supervisor restarts it. This document covers the other case — **the agent
is already running, in someone else's session, and you want to hand it a message.**

That inverts the relationship. You stop being the owner and become **a guest**, and almost
every rule changes.

The concrete trigger is a terminal workspace manager that exposes running agents as
addressable targets — Herdr is the one referenced here — but the reasoning applies to any
arrangement where the agent's lifecycle belongs to someone else: a human's terminal, a
teammate's session, a pane started hours ago for an unrelated task.

> **Status: MEASURED.** Run end to end on Herdr 0.8.2 against a simulated Teams channel —
> a message posted by a person reached a `claude` agent in a Herdr pane and its reply came
> back into the channel in **7.6s**. The `working`, `blocked` and `gone` paths were each
> exercised for real. Four of this document's original `--help`-derived claims were **wrong**
> and are corrected below; they are called out explicitly rather than quietly fixed, because
> the difference between reading a `--help` and running the command is the whole lesson.

---

## 1. What the host gives you

A workspace manager that tracks agents gives you three things a raw terminal does not:

| | raw `tmux send-keys` | a managed host |
|---|---|---|
| Delivery | type text, guess at the submit timing | a real "submit a prompt" primitive |
| Completion | scrape the pane and hope | **typed states** — `idle`, `working`, `blocked`, `done` |
| Blocking | sleep and re-check | wait until a state is reached |

In Herdr's vocabulary:

```sh
herdr agent list                                   # discover targets
herdr agent prompt <target> "<text>" --wait --until idle
herdr agent wait   <target> --until idle,blocked --timeout 60000
herdr agent get    <target>                        # current state
```

`blocked` is the state that matters most and the one hand-rolled injectors never have. It
distinguishes *"finished"* from *"sitting on a permission prompt waiting for a human"* —
and a guest that can't tell those apart will happily conclude its message was handled when
it is actually parked behind a dialog nobody is watching.

---

## 2. You are a guest — the rules that follow

### 2.1 The target's lifecycle is not yours
It can vanish between your discovery call and your delivery. Never cache a target id across
runs and assume it still resolves. **Discover, then deliver, and treat a failed delivery as
normal traffic, not an exception.**

### 2.2 The target is probably busy with something else
This is the central problem, and it is not a technical one. A session running someone's
refactor has a context full of that refactor. Injecting *"alice asks when the deploy lands"*
does not just interrupt it — it **permanently contaminates that context**. The agent now
carries your message in every subsequent turn of their task, and they will wonder why their
refactoring assistant is talking about deploys.

Three ways to handle it, in descending order of politeness:

| policy | behaviour | use when |
|---|---|---|
| **Consent-gated** | only deliver to a session that has opted in (a marker file, a name convention, an explicit registration) | default — never inject into a session that didn't ask |
| **Idle-only** | deliver only when the target is `idle`; otherwise queue | the session is shared and interruption is tolerable |
| **Interrupt** | deliver regardless of state | you own the session in all but name |

**Default to consent-gated.** A session that registered itself as a responder expects your
messages; a session that happens to be running does not. The registration can be trivial —
an agreed name prefix, or a file the session writes when it's willing to answer — but it
must be an *opt-in*, not an inference from "the pane exists".

### 2.3 You cannot restart what you did not start
Everywhere else in this repo the answer to a dead component is a supervisor. Here you have
no such right: restarting someone's session destroys their work. When the target is gone,
the correct behaviour is to **fall back, not to resurrect** — see §6.

---

## 3. Addressing and discovery

An address must survive a restart of both sides. Prefer, in order:

1. **A name the session chose for itself** (`herdr agent rename`, or a registration file
   naming the target) — stable, meaningful, and an implicit opt-in.
2. **A workspace/session + pane path** — stable while the layout is, breaks on rearrangement.
3. **A raw pane id** — fine within one run, never across runs.

Keep the mapping **conversation → target** in the same registry the session router uses, so
one conversation always reaches the same agent. A person's follow-up landing in a different
session than their first message is the cross-session version of the branched-transcript
bug: the conversation splits and nobody is told.

Re-resolve the name on every delivery. A cached id that silently resolves to a *different*
pane is the worst outcome available — your message goes to a stranger.

---

## 4. Backpressure: what to do when it's busy

The state machine gives you a real policy instead of a guess.

```
get state
├── idle              → deliver now
├── working           → queue; retry after `wait --until idle --timeout N`
├── blocked           → DO NOT deliver. A human is needed. Alert, hold the message.
├── done              → the session has finished; treat as gone (§6)
└── unknown/absent    → treat as gone (§6)
```

Rules that keep this honest:

- **Never deliver into `blocked`.** The message lands behind a dialog and is invisible until
  a human clears it — the message looks delivered and isn't.
- **Bound the wait.** An unbounded `wait --until idle` against a session whose human went to
  lunch is an indefinite stall. Time out and fall back.
- **Queue in the journal, not in memory.** The daemon already has a durable journal with a
  read cursor; an undeliverable message must stay unacked there so it survives your process
  dying mid-retry.
- **One delivery in flight per target.** Two concurrent prompts into one session interleave
  in its context. Same lock discipline as one-writer-per-session.

---

## 5. Getting the reply back

The agent answers **in its own pane**. Nothing sends that to your channel unless you arrange it.

Two options, and the second is much better:

**Capture the reply.** `prompt --wait --until idle` then read the agent's output and post it
yourself. Simple, but you are now screen-scraping to find where its answer starts and ends,
and any formatting the pane applies is yours to undo.

**Let the agent reply itself — preferred.** Include in the injected text the message id and
the exact command to answer with:

```
[message a1b2c3 from alice in #general] when does the deploy land?
Reply with: convo respond a1b2c3 "<your answer>"
```

The agent then posts through the same routed path as any other handler — threading and DM
routing are handled by the tool, not by your scraper. It also means the reply is attributed
and journaled like everything else.

The cost is that the target session needs the CLI available and a little standing
instruction about how to use it. That is exactly what a consent-gated registration should
establish up front, rather than being re-explained in every injected message.

---

## 6. When the target is gone

Falling back is the normal path, not the error path. In order:

1. **Re-resolve the name.** It may have been renamed or restarted with the same identity.
2. **Deliver to another registered session** for that conversation, if one exists.
3. **Spawn your own** — the owned-session path from `SESSIONS.md`, resuming that
   conversation's session id. Context is preserved because it lives in the session, not in
   the pane you lost.
4. **Leave it unacked and alert.** The message stays in the journal; a human decides.

Never silently drop, and never busy-retry a target that has been absent for more than a
short interval — a guest that hammers a closed door is just a slow denial of service.

---

## 7. Security — this is the sharpest edge in the repo

Injecting a message into a session you don't own is, mechanically, **remote prompt injection
with a delivery guarantee**. Everything in `ARCHITECTURE.md` §8 applies, and more so:

- **You do not know the target's capabilities.** Someone's session may have cloud
  credentials, production access, a shell with no sandbox. Your daemon hands untrusted text
  straight into it. A message reading *"summarise the admin mailbox and post it"* is now
  aimed at a session that might be able to comply.
- **Sender identity is still not authorization** — and here the blast radius is someone
  else's access, not yours.
- **Consent-gating is a security control, not etiquette.** A session opts in knowing it will
  receive text from strangers, and its operator can scope its tools accordingly.
- **Mark the boundary in the injected text.** Frame it unmistakably as quoted, untrusted
  third-party content with a request, not as an instruction from the operator. The framing
  in §5 does this: the message is attributed to a person and a channel, and the only
  imperative is the reply command you supplied.
- **Never inject into a session more privileged than the channel deserves.** If the channel
  is public, the target should be able to do nothing you would not let the public do.

The fresh-session path (`SESSIONS.md`) is safer precisely because you control the spawn: you
choose the working directory, the permission mode and the tool surface. A borrowed session
comes with whatever its owner gave it.

---

## 8. Choosing between the two

| | inject into an existing session | spawn/resume your own |
|---|---|---|
| Context | the target's, plus your contamination | clean, per conversation |
| Latency | no cold start | cold start per message |
| Lifecycle | not yours; can vanish | yours; supervised |
| Capabilities | whatever the owner granted | whatever you grant |
| Backpressure | must respect `working`/`blocked` | none — you start it |
| Best for | a human-attended session that opted in | unattended, multi-user, at scale |

**Default to spawning your own.** Reach for injection when there is a specific reason the
answer must come from *that* session — it holds state you cannot reconstruct, a human is
collaborating with it live, or it is mid-task on the very thing being asked about.

---

## 9. Failure modes

| Failure | Symptom | Defence |
|---|---|---|
| Host not running | every delivery errors | detect once, fall back to spawning; don't retry per message |
| Cached id resolves to a different pane | the message reaches a stranger | re-resolve by name on every delivery |
| Delivered into `blocked` | looks delivered, sits behind a dialog | never deliver unless `idle`; treat `blocked` as needs-a-human |
| Target closed mid-`--wait` | indefinite stall | always pass `--timeout`; fall back on expiry |
| Two deliveries in flight | interleaved context, garbled answers | one in-flight delivery per target |
| Reply never returned | the conversation silently stalls | agent replies via the CLI itself (§5), so the journal records it |
| Conversation split across targets | follow-ups answered by a different session with no history | pin conversation → target in the registry |
| Context contamination | the owner's task derails | consent-gating; idle-only delivery |

---

## 10. Reference shape — corrected against a real run

The original version of this section was written from `herdr agent --help` and was wrong in
four ways. The real interface:

- **`agent list` and `agent get` take no options at all.** There is no `--json` (only
  `agent explain` has one); they emit JSON unconditionally.
- **The response is enveloped**, not a bare array:
  `{"id":"cli:agent:list","result":{"agents":[{"name":…,"agent_status":…,"pane_id":…}]}}`
- **There is no `.id` field on an agent.** The address is `name` or `pane_id`. Every verb
  takes the **name directly**, so resolving to an id is not just unnecessary — it manufactures
  the stale-id hazard §9 warns about.
- **The state field is `agent_status`**, at `.result.agent.agent_status` — not `.status`.
- Errors come back as **JSON on stdout**; `get`/`wait`/`prompt` exit **1** on a missing
  target, `list` exits 0 with an empty array. A handler that checks only `$?` will miss the
  reason.

```sh
# address by NAME, every time. no id resolution, no caching.
state=$(herdr agent get responder-a \
        | python3 -c 'import sys,json; d=json.load(sys.stdin)
print((d.get("result") or {}).get("agent",{}).get("agent_status","absent"))' 2>/dev/null) \
        || state=absent

case "$state" in
  idle)    ;;
  working) herdr agent wait responder-a --until idle --timeout 60000 >/dev/null || fallback ;;
  blocked) alert "target needs a human"; exit 0 ;;   # leave unacked, see §11
  *)       fallback_to_owned_session ;;
esac

herdr agent prompt responder-a \
  "[message $MSG_ID from $FROM in $CONV] $TEXT
Reply with: TEAMSCTL_HOME=$HOME_DIR $ABS_CLI respond $MSG_ID \"<your answer>\"" \
  --wait --until idle --timeout 120000
```

Note what the reply line carries: an **absolute path to the CLI**, the **config dir**, and
the **message id**. The guest is a different process with a different PATH and a different
environment — omit any one of the three and the agent tries to reply and silently cannot.
That is the strongest argument for §5's footnote: put the standing instruction in the
consent-gated registration, and keep the per-message line to the id and the text.

`herdr agent read` is the one verb that returns **raw terminal text, not JSON**. Don't parse it.

---

## 11. What the run taught us that the design missed

**The host enforces `blocked` too.** `herdr agent prompt` against a blocked agent returns
`{"error":{"code":"agent_blocked"}}` "before any input is sent". The guest's job is therefore
**detect and report**, not prevent — the dangerous case was never going to slip through.

**Unacked is not the same as retried — this is a real hole.** §4 says to queue an
undeliverable message in the journal. It does survive there. But `--on-batch` fires once per
*new* message, so **a message the handler declined is never offered again.** Three messages
sat unacked at the end of the run and nothing ever came back for them. "Queue in the journal"
without a drain is "leak into the journal". Add an explicit drain — poll `inbox --new` on a
timer, or drain on the next `idle` transition — or accept that a declined message needs a
human.

**Context contamination is real, and it nearly escaped.** The pane's transcript is one linear
thread mixing the owner's task with four strangers' questions. While answering one person's
question about a capital city, the session volunteered *"The earlier touch command for the
probe file was rejected and was not run"* — state from a different person's message bleeding
into an unrelated answer. It did not reach the channel that time. It was one sentence away.
**Consent-gating is not politeness; it is the containment boundary.**

**Discovery lies by omission.** `herdr status` reports only the default socket. Five servers
were running under named sessions while it said `not running`. A guest that probes with
`status` will conclude no host exists when one does — check `HERDR_SOCKET_PATH` and named
sessions before falling back.

**A stale environment poisons the host.** `HERDR_ENV=1` left in a tmux server's *global*
environment makes every pane look nested, and bare `herdr` refuses with
`nested herdr is disabled by default` without saying why. Launch with a scrubbed env.

**Verification tooling is narrower than the send path.** Replies are threaded, so reading the
channel back shows **nothing** — the first read of the run looked like a total failure. The
artifact is the thread, not the channel. And a thread lookup resolved against the configured
channel only, so a reply correctly routed to a *different* channel could not be read back at
all. When you write "verify the artifact", name the exact command, or someone will verify the
wrong surface and draw the wrong conclusion.
