# Session routing — a multi-turn agent with no loop anywhere

> *One persistent agent session per conversation, resumed on every inbound
> message. The agent is **invoked**, answers in one turn, and exits. Nothing
> polls, nothing sleeps, nothing is resident between messages — and the
> conversation is still multi-turn, for many users at once, with no cross-talk.*

[`POLLING.md`](POLLING.md) solves "how does an attended agent notice a message"
by having a subagent host a blocking poll. It works, and it costs ~6–7 turns an
idle hour, and the host is mortal.

This document solves a different problem: **no loop may run inside the agent
session at all** — not in the main session, not in a subagent. That rules the
poller out entirely. What is left is wake mechanism **W2**
([`ARCHITECTURE.md`](../../ARCHITECTURE.md) §4): the daemon spawns a handler per
batch. W2's stated weakness is that the handler is *fresh* — "no conversational
context to draw on".

**That weakness is not inherent. It is just a missing lookup table.**

```
  daemon coalesces a batch  ──►  group by conversationId  ──►  session-router.sh
                                                                   │
                                                 conversationId ────┤ registry
                                                                   │   (on disk)
                                                          sessionId ▼
                                                        runtime adapter
                                                   claude -p --resume <id>
                                                                   │
                                                              one reply
                                                                   ▼
                                                          convo respond <id>
```

The agent process is born, resumes a transcript it has never seen in memory,
answers, and dies. Multi-user isolation is not something this code implements —
it is what a **separate session id per conversation** gives you for free. Two
users cannot see each other's context because they were never in the same
session.

Files:

| file | what it is |
|---|---|
| [`session-router.sh`](session-router.sh) | the router — registry, locking, concurrency, one invocation |
| [`runtimes/claude.sh`](runtimes/claude.sh) | adapter: Claude Code |
| [`runtimes/devin.sh`](runtimes/devin.sh) | adapter: Devin |
| [`runtimes/codex.sh`](runtimes/codex.sh) | adapter: Codex |
| [`../../tests/session-router.test.sh`](../../tests/session-router.test.sh) | the proof (§9) |

---

## 1. The session registry

The whole product is one persisted map: **`conversationId -> agent session`**.

### Location

```
$AGENT_CONVERSATIONS_HOME/sessions.<tag>.json
```

Same runtime directory and the same `<tag>` partitioning as every other daemon
file ([`INTERFACES.md`](../../INTERFACES.md) §2), so one tag's listener, journal,
cursors, heartbeat, lock **and sessions** move together. Override with
`--registry PATH` or `$SESSION_ROUTER_REGISTRY`.

### Shape

```json
{
  "c-general": {
    "runtime": "claude",
    "sessionId": "2cf2d7b8-3215-4412-b6bb-5729e10f7c8b",
    "createdAt": "2026-09-11T03:30:07Z",
    "lastUsedAt": "2026-09-11T03:31:52Z",
    "turns": 3,
    "cwd": "/srv/support-agent"
  },
  "c-dm-alice": {
    "runtime": "codex",
    "sessionId": "01a08e8a-f540-7341-9054-929dd31d620b",
    "createdAt": "2026-09-11T03:37:54Z",
    "lastUsedAt": "2026-09-11T03:38:21Z",
    "turns": 2,
    "cwd": "/srv/support-agent"
  }
}
```

| field | meaning |
|---|---|
| `runtime` | which adapter owns this conversation. The registry, not the caller, is the authority — see §7 |
| `sessionId` | the runtime's own id. A uuid for Claude and Codex, a word-pair for Devin |
| `createdAt` / `lastUsedAt` | ISO-8601 UTC, `%Y-%m-%dT%H:%M:%SZ`. `lastUsedAt` drives eviction |
| `turns` | messages routed into this session. A cheap, honest proxy for context growth |
| `cwd` | the directory the agent runs in. Part of the record because Devin scopes `list` by directory |

**There is deliberately no envelope** — no `{"version":…, "conversations":{…}}`.
Every top-level key *is* a conversation id, and a conversation id is an opaque
string chosen by a channel nobody controls. An envelope would create reserved
names, and a channel that names a room `version` would corrupt the file. The
test suite asserts exactly that case.

### Writing

`tmp` file in the same directory → `fsync` → `rename(2)`. Rename is atomic
within a filesystem, so a concurrent reader sees the old file or the new one and
never a half-written one. Read-modify-write is serialised by the
per-conversation lock (§4): two conversations updating *their own* records are
still two writers of **one file**.

### Corrupt file

A truncated or unparseable registry is **moved aside** to
`sessions.<tag>.json.corrupt.<epoch>`, a `{"warning":{"code":"registryCorrupt"}}`
goes to stderr, and the router carries on with an empty map.

Why not refuse to start? Because one bad byte would become a total outage for
every user, and the failure this repo exists to prevent is exactly "the system
looks up but nobody is being answered". Why not delete it? Because it is
evidence, and the ids in it are recoverable by hand. The cost is that every
conversation loses its memory **once** — the next message opens a fresh session.
That is the same graceful degradation as eviction, and it is loud.

### Eviction policy — age-based, 14 days, lazy

Sessions accumulate forever otherwise. `--max-age-days` (default 14,
`$SESSION_ROUTER_MAX_AGE_DAYS`) drops any record whose `lastUsedAt` is older
than the cutoff. The sweep runs **on every write**, so there is no cron, no
timer and nothing extra to keep alive — the eviction path cannot itself go
silently dead. `session-router.sh --evict` forces a sweep with no message.

**Why age and not an LRU count cap.** The cost being bounded is the *runtime's*
transcript storage, which is itself age-shaped; and a count cap evicts a
quiet-but-alive conversation precisely when a crowd arrives — the user who has
been waiting politely is the one whose memory you throw away. Age is also the
only policy a user can predict: "the bot forgets you after two weeks" is a
sentence you can put in a help message.

**Eviction drops the mapping, never the transcript.** The runtime's own session
still exists on disk; we have only forgotten which conversation it belonged to.
Recovery is possible from the runtime's own session list if it ever matters.

---

## 2. `session-router.sh`

```
session-router.sh --conversation <id> [--runtime claude|devin|codex] [--cwd DIR]
                  [--new] [--print-id] [--registry PATH] [--timeout N] [--dry-run]
                  [--tag NAME] [--concurrency N] [--lock-timeout N]
                  [--max-age-days N] [--runtimes-dir DIR] [--list] [--evict]
```

**The message text arrives on stdin. The agent's reply leaves on stdout,
verbatim.** stderr is always one JSON object with a stable `code`, exactly like
the daemon (`INTERFACES.md` §1) — a router that fails in prose forces its caller
to parse English to decide what to do, and it will get it wrong.

| flag | meaning |
|---|---|
| `--conversation <id>` | required. The channel's conversation id, straight from `source.conversationId` |
| `--runtime <name>` | which adapter. Default `claude`, or `$SESSION_ROUTER_RUNTIME`. Ignored in favour of the registry for a conversation that already has one (§7) |
| `--cwd DIR` | the directory the agent runs in |
| `--new` | force a fresh session, abandoning the recorded one. Also the way to rebind a conversation to a different runtime |
| `--print-id` | print the session id and exit. **Invokes no agent.** For Claude (caller-chosen uuid) this mints and records an id if there is none, so asking twice gives the same answer; for a runtime that mints its own, it exits 65 until the first message has been sent |
| `--timeout N` | seconds before the agent is SIGTERMed then SIGKILLed. Default 300 |
| `--concurrency N` | how many agents may run at once, across all conversations. Default 4 |
| `--lock-timeout N` | how long to wait for a busy conversation or a free slot. Default 120 |
| `--dry-run` | print the resolved routing decision as JSON; spawn nothing |
| `--list` / `--evict` | dump the registry / force an eviction sweep |

### Exit codes — the daemon's own, nothing invented

| code | meaning |
|---|---|
| **0** | the agent answered; the reply is on stdout |
| **1** | unexpected error |
| **65** | bad arguments, unknown runtime, empty message, no session id yet, runtime mismatch |
| **66** | another message for **this** conversation is still being processed, or the concurrency pool stayed full past `--lock-timeout` |
| **69** | the runtime adapter failed — an invalid or purged session id lands here, **loudly** |

66 and 69 are the daemon's codes for "someone else holds this" and "the thing I
depend on is not working", and they mean the same things here. A caller can
branch on them without reading a byte of output.

---

## 3. The runtime adapter contract

`runtimes/<name>.sh` is the seam. **Adding a runtime must not touch the router.**

| | |
|---|---|
| **invocation** | `runtimes/<name>.sh` with the message text on **stdin** |
| | `runtimes/<name>.sh --mint-id` — print a caller-chosen session id, or **exit 3** if this runtime only reveals its id after the first run |
| **stdout** | the reply, verbatim. Nothing else may be written there |
| **stderr** | diagnostics; the router passes them through untouched on failure |
| **exit** | 0 ok, non-zero failure (the router turns any non-zero into exit 69) |

Environment in:

| variable | meaning |
|---|---|
| `SR_SESSION_ID` | the id to resume, or empty when a new session must be created |
| `SR_NEW` | `1` when a brand-new session is required |
| `SR_CWD` | working directory |
| `SR_TIMEOUT` | seconds; the router also enforces this from outside |
| `SR_SESSION_ID_OUT` | a path the adapter **must** write the authoritative session id to |
| `SR_CONVERSATION` | the conversation id, for logging |

`SR_SESSION_ID_OUT` is the whole reason the contract is shaped this way. Claude
lets the *caller* choose a uuid, so the id exists before the agent does. Devin
and Codex mint their own, and the only moment the id is knowable is *after* the
first run. One file path covers both, and the router reads it unconditionally —
so every adapter answers the same question the same way, and an adapter that
answers but reports no id is treated as a **failure**, because the next message
could not resume it.

---

## 4. Concurrency and locking

Three separate rules, three separate reasons.

### 4.1 One message at a time per conversation — **mandatory, not tidy**

`$AGENT_CONVERSATIONS_HOME/locks/conv.<tag>.<hash>.lock`, a `mkdir(2)` lock
directory holding the owner's pid. `mkdir` is the portable atomic test-and-set;
`flock(1)` is not installed on macOS by default.

Two messages from one person must never run concurrently, because they would be
two processes resuming **one** session id, and the runtime's transcript is a
single file. This is the daemon's single-consumer rule (`ARCHITECTURE.md` §5.9)
one level up, and the consequence of skipping it was measured, not imagined —
see §6.

A lock whose pid is no longer alive is **stale** and is reclaimed silently. The
alternative is one crashed router wedging a user forever.

> **Operational rule for humans: do not open a router-owned session
> interactively** (`claude --resume <id>`, `devin --resume <id>`) while the
> daemon may resume it. One writer per session, always. It is the same class of
> error as running two `convo wait` consumers on one tag — and it is quieter,
> because nothing refuses.

### 4.2 Different conversations run in parallel

They are different session ids and different files; there is nothing to
serialise. Twenty people messaging at once is twenty independent conversations.

### 4.3 Bounded total concurrency — default 4

`locks/slots.<tag>/<1..N>`, the same lock primitive used as a semaphore. Twenty
simultaneous users must not fork twenty agents: that is a fork bomb made of
LLMs, and it is the **box**, not the queue, that fails. The (N+1)th message
waits up to `--lock-timeout` and then exits 66 rather than piling up invisibly.

Lock order is always conversation → slot, and the registry's own write lock is a
leaf that is never held across a spawn, so there is no deadlock.

---

## 5. Wire-up — where the loop would have been

The daemon already coalesces and already spawns. All that is missing is
*grouping by conversation* — because one batch can contain several
conversations, and one router call handles exactly one.

```bash
convo listen --adapter ./my-adapter.js --tag support \
             --window 800 \
             --on-batch './route-batch.sh'
```

That is the entire wire-up. `route-batch.sh`:

```bash
#!/usr/bin/env bash
# stdin: the coalesced batch as NDJSON (INTERFACES.md §4).
# One session-router.sh call per CONVERSATION; conversations run in parallel and
# the router's own --concurrency bounds how many agents that really is.
set -euo pipefail
ROUTER="${ROUTER:-reference/skill/session-router.sh}"
BATCH="$(mktemp)"; trap 'rm -f "$BATCH"' EXIT
cat > "$BATCH"

# Group the batch: {conversationId, replyTo, text}, one job per line.
python3 - "$BATCH" > "$BATCH.jobs" <<'PY'
import json, sys, collections
groups = collections.OrderedDict()
for line in open(sys.argv[1]):
    m = json.loads(line)
    groups.setdefault(m["source"]["conversationId"], []).append(m)
for cid, ms in groups.items():
    # the whole room at once, ids included so the agent can be asked to cite one
    text = "\n".join("[%s] %s: %s" % (m["id"], m["from"]["name"], m["text"]) for m in ms)
    print(json.dumps({"conversationId": cid, "replyTo": ms[-1]["id"], "text": text}))
PY

while IFS= read -r job; do
  (
    cid=$(printf '%s' "$job" | python3 -c 'import json,sys;print(json.load(sys.stdin)["conversationId"])')
    rid=$(printf '%s' "$job" | python3 -c 'import json,sys;print(json.load(sys.stdin)["replyTo"])')
    txt=$(printf '%s' "$job" | python3 -c 'import json,sys;print(json.load(sys.stdin)["text"])')
    if reply=$(printf '%s' "$txt" | "$ROUTER" --conversation "$cid" \
                 --tag "${CONVO_TAG:-default}" \
                 --runtime "${ROUTER_RUNTIME:-claude}" --timeout 240); then
      convo respond "$rid" "$reply"        # routes dm vs channel itself
      convo ack "$rid"
    else
      rc=$?
      # 66 = that conversation is already being answered; the batch is still in
      # the journal and still UNACKED, so nothing is lost. 69 = the runtime is
      # broken: leave it unacked and let `convo journal --new` make it visible.
      printf 'route-batch: conversation %s -> router exit %s\n' "$cid" "$rc" >&2
    fi
  ) &
done < "$BATCH.jobs"
wait
```

Count the loops in the agent session: **zero**. The `while` above is a shell
iterating a finite list and exiting; the agent processes it spawns each answer
once and die. Nothing waits for a message anywhere — the daemon's poll is the
only thing that ever waits, and it has no model attached.

Two rules carried over unchanged from `INTERFACES.md` §4:

- **Pin the handler to the daemon's identity.** `convo respond` inside the
  handler uses `CONVO_ADAPTER` / `CONVO_SELF_ID`, which the daemon exports. A
  handler replying as somebody else is a self-reply loop that echo suppression
  cannot see.
- **Idempotence.** Delivery is at-least-once. Dedupe on message id before doing
  anything with a side effect; `turns` in the registry will tell you if a batch
  was routed twice.

And one that is specific to this pattern: an inbound message is **untrusted
input** (`ARCHITECTURE.md` §8), and here it is fed to an agent that *remembers*.
A session accumulates whatever anyone has said to it, so a narrow toolset
matters more, not less, than in a stateless handler. `--permission-mode
bypassPermissions` exists because a non-interactive run has nobody to answer a
prompt — it is not a licence to mount everything.

---

## 6. Measured: what happens without the per-conversation lock

Claude Code's help text documents a fork for background resumes:

> `--bg, --background` … With `--resume <session-id>`, continues that session in
> the background under the same ID, **or starts a copy and says so when the
> session is already running**

We never pass `--bg`. The interesting question was what plain `-p --resume` does
under concurrency, so it was measured (2026-09-11, Claude Code 2.1.268). Two
`claude -p --resume <same id>` processes were started at the same instant, one
told to remember `WALRUS` and one told to remember `BADGER`:

```
--- before:   2 transcripts; lines in ours: 34
A=OK
B=OK
--- after:    2 transcripts; lines in ours: 46      # no new session file
```

Both succeeded. No new session id. Then, sequentially:

```
$ claude -p --resume <id> "List every codeword I have given you ..."
WALRUS
```

And the transcript's parent-uuid chain:

```
rows: 52  branch points: 2
  parent 3d4a1e5d-af6a-4751-88b9-8a49696ec4e8 -> 2 children
```

**The conversation branched inside the single transcript, and one whole turn
stopped existing.** `BADGER` was accepted, answered `OK`, written to disk — and
is not in the conversation. There is no error, no warning, no second session
file: in `-p` mode the CLI does not even print the "started a copy" notice its
`--bg` documentation promises. This is the exact shape of failure this repo is
about: the system reports success and quietly stops working.

So the per-conversation lock is **mandatory**. The test suite proves it twice —
offline against the stub (§9 D) and live against Claude (§9 G), where two
concurrent messages to one conversation must both survive into one branch. Remove
the lock and both tests fail.

Defensively, `runtimes/claude.sh` also greps its own stdout for a "started a
copy" / "session is already running" notice and exits non-zero if it ever
appears. A forked conversation must never be handed to a user as if it were
intact.

---

## 7. Failure modes

| failure | symptom without a defence | what the router does |
|---|---|---|
| two messages, one conversation, at once | transcript branches; a turn silently vanishes (§6) | per-conversation lock; the second waits, or exits **66** |
| twenty users at once | twenty agents forked; the box falls over | `--concurrency` semaphore; the overflow exits **66** |
| session id purged / invalid | a runtime "starts over" and the user is answered by an amnesiac | adapter exits non-zero → router exits **69** with the runtime's own stderr passed through |
| registry truncated mid-write | unparseable JSON takes every conversation down | moved aside, warned about, empty map, keep serving |
| caller passes the wrong `--runtime` | a Claude uuid is handed to Devin; unreadable failure | the registry is the authority; **65** `runtimeMismatch` unless `--new` |
| adapter answers but reports no id | the next message opens a fresh session and memory is lost invisibly | treated as a **69** failure, not a success |
| a router crashes holding a lock | that user is wedged forever | a lock whose pid is dead is reclaimed silently |
| sessions accumulate forever | unbounded disk, unbounded transcripts | age-based eviction on every write |
| a human opens the session interactively | same branch-and-lose as two routers | documented rule, §4.1. Nothing can enforce it from here |

---

## 8. Runtime support — measured 2026-09-11

| | Claude Code 2.1.268 | Codex CLI 0.153.4 | Devin 2026.8.18 |
|---|---|---|---|
| caller-chosen session id | **yes** — `--session-id <uuid>` | no | no |
| resume in a separate process | `-p --resume <id>` | `exec resume <id>` | `--resume <id> -p` |
| how the id is learned | we chose it | `{"type":"thread.started","thread_id":…}` on the first `--json` event | diff `devin list --format json` around the first run |
| reply extraction | stdout of `-p` | `-o FILE` (last message) | stdout of `-p` |
| id shape | uuid | uuid | word-pair (`wary-cemetery`) |

Three findings worth stating plainly:

- **Codex is no longer single-shot.** `codex exec resume <id>` exists and
  carries context — verified with a codeword. `codex exec` and `codex exec
  resume` do **not** share a flag set, though: `exec` accepts `-C/--cd`, `exec
  resume` rejects it (`error: unexpected argument '-C' found`), so the adapter
  `cd`s instead. Also, with a prompt argument *and* piped stdin, codex appends
  stdin as a `<stdin>` block — the adapter redirects stdin from `/dev/null` so
  the message does not arrive twice.
- **Devin's `--resume` must come before `-p`.** `-p/--print` takes an optional
  inline prompt, so `devin -p "<text>" --resume <id>` is parsed wrong — and it
  does not error, it runs the wrong thing.
- **Devin cannot be given an id, and does not print the one it minted.** The
  only enumeration is `devin list --format json`, scoped to the current
  directory, so a new session's id must be *discovered* by diffing that list
  around the first invocation. Two new conversations starting in the same
  directory at the same instant would race, so `runtimes/devin.sh` serialises
  minting behind its own lock. This is the argument for Claude's caller-chosen
  uuid in one sentence: **an id you pick cannot be raced for, cannot be
  mis-attributed, and exists before the agent does.**

### `--bare` — measured, and **not** enabled by default

A handler pays cold start on every message, so `--bare` ("skip hooks, LSP,
plugin sync, attribution, auto-memory, background prefetches, keychain reads,
and CLAUDE.md auto-discovery") looks like an obvious win. Measured cold start
without it, on this machine:

| invocation | wall clock |
|---|---|
| `claude -p --session-id <uuid> "Reply with exactly one word: OK"` | **6s** |
| `claude -p --resume <uuid> "Reply with exactly one word: OK"` | **4s** |
| `claude --bare -p --session-id <uuid> …` | **0s — refused** |

```
$ claude --bare -p --session-id <uuid> --permission-mode bypassPermissions "Reply with exactly one word: OK"
Not logged in · Please run /login
```

Its own help says why: *"Anthropic auth is strictly `ANTHROPIC_API_KEY` or
`apiKeyHelper` via `--settings` (OAuth and keychain are never read)"*. On a
subscription-authenticated machine `--bare` cannot run at all, so **its latency
benefit is unmeasured here** — we will not quote a number we did not observe.

It is therefore **opt-in**: `SESSION_ROUTER_CLAUDE_BARE=1`. Turn it on only
where an API key is in the environment, and only if the handler does not depend
on `CLAUDE.md` / auto-memory for its answers — a support agent whose instructions
live in `CLAUDE.md` loses them under `--bare` and will still cheerfully reply.

---

## 9. Captured evidence

Run on **2026-09-11**, macOS 25.4, from a clean clone:

```bash
./tests/session-router.test.sh                      # offline, 46 assertions
SESSION_ROUTER_E2E=1 ./tests/session-router.test.sh # + live Claude, 54 assertions
```

The offline tests are not a weaker version of the live ones. They drive a **stub
runtime adapter** that is a genuinely stateful multi-turn agent — it just has a
lookup table where a model would be. Session memory, isolation, ordering and
locking are properties of the *router*, so the stub proves them exactly as well
as a model does, for nothing, on every CI run. The live tests exist to prove the
one thing a stub cannot: that the real CLIs behave as documented.

Verbatim output of the full run (ANSI stripped):

```
── A. the registry — shape, atomicity, corruption, eviction
  ok   first message opens a session
  ok   registry record has exactly the documented fields
  ok   field types are as documented
  ok   turns started at 1 after one message
  ok   top level is a flat conversationId map (no envelope)
  ok   --print-id returns the id without invoking anything
  ok   --print-id is stable when asked twice
  ok   a conversation literally named "version" is just a key
  ok   a corrupt registry does not take the router down
  ok   corruption is reported loudly on stderr
  ok   the corrupt file is preserved, not deleted
  ok   no .tmp file is left behind (write is tmp+rename)
  ok   age-based eviction drops a stale conversation
  ok   eviction leaves live conversations alone

── B. multi-turn memory — three messages, the third recalls the first
  ok   turn 3 recalls what turn 1 established
  ok   turns counted across all three
  ok   the session id never changed

── C. multi-user isolation — the headline test
  ok   user 1 gets its own codeword
  ok   user 2 gets its own codeword
  ok   no leak into user 1
  ok   no leak into user 2
  ok   two conversations hold two DIFFERENT session ids

── D. concurrency — parallel across conversations, serial within one
  ok   concurrent: user 1 still correct
  ok   concurrent: user 2 still correct
  ok   turn count incremented by BOTH messages (neither was dropped)
  ok   both messages landed in ONE session, in order (no fork, no interleave)
  ok   the second message WAITED for the first (4s)
  ok   --concurrency 1 forced two different conversations to queue
  ok   a busy conversation exits 66, not 1
  ok   and says why
  ok   a stale lock is reclaimed silently

── E. persistence across a restart
  ok   after a restart the same session is resumed
  ok   the session id survived
  ok   turns kept counting across the restart

── F. failure modes — loud, never silent
  ok   an unresumable session id exits 69
  ok   with a machine-readable code
  ok   the runtime's own reason is passed through, not swallowed
  ok   and nothing was printed to stdout as if it had worked
  ok   an unknown runtime exits 65
  ok   and names the adapter it looked for
  ok   an empty message exits 65
  ok   and says so
  ok   a missing --conversation exits 65
  ok   resuming under the wrong runtime exits 65
  ok   and explains the binding
  ok   --dry-run spawns no agent

── G. live agents (SESSION_ROUTER_E2E=1)
  [claude] turn 1 alice -> OK
  [claude] turn 1 bob   -> OK
  [claude] turn 2 alice -> NOTED
  [claude] turn 3 alice -> ALPACA
  [claude] turn 3 bob   -> PLATYPUS
  ok   live: alice's 3rd turn recalls her 1st
  ok   live: bob's 3rd turn recalls his 1st
  ok   live: no leak into alice
  ok   live: no leak into bob
  ok   live: two concurrent messages to ONE conversation both counted
  [claude] fork check -> 7 GREEN
  ok   live: the lock kept BOTH turns in one branch (number survived)
  ok   live: the lock kept BOTH turns in one branch (colour survived)
  ok   live: alice's turn count
  session ids: alice=2cf2d7b8-3215-4412-b6bb-5729e10f7c8b bob=a616497f-755a-4b5b-8676-073affb2b596

54 passed, 0 failed
```

Read section G as prose: **alice** and **bob** were given different codewords in
their first message. Alice's second message was unrelated. Their third messages
were fired *simultaneously*, in two separate processes, each asking "what was
the codeword I gave you?" — and each got its own back, with neither ever
mentioning the other's. Then two more messages were fired at alice's
conversation at the same instant; both survived, in one branch, and a sixth
message recalled both (`7 GREEN`). Every one of those agent invocations
was a process that started, answered once and exited — eight of them, no two
alive for the same conversation at the same time.

### The wire-up itself, end to end, offline

The `route-batch.sh` in §5 is not a sketch — it was extracted verbatim from this
document and run against the reference daemon and the in-repo memory adapter,
with the stub runtime standing in for a model (so this costs nothing and can be
repeated on any machine):

```
$ convo listen --adapter memory --adapter-opt store=/tmp/store.json --tag wire \
               --window 800 --on-batch './route-batch.sh'
{"started":true,"ready":true,"pid":62375,"tag":"wire",…}

# alice posts in #general and bob DMs, inside one coalescing window
[…] msg id=m-1 from=alice kind=channel in=#general n=1
[…] msg id=m-2 from=bob   kind=chat    in=dm:alice n=2
[…] handler out: {"ok":true,"id":"m-3","inReplyTo":"m-1","target":{"conversationId":"c-general","kind":"channel","threadId":"m-1","replyToId":"m-1"}}
[…] handler out: {"ok":true,"id":"m-4","inReplyTo":"m-2","target":{"conversationId":"c-dm-alice","kind":"chat"}}
[…] handler exit=0 n=2

# one batch, two conversations, TWO different sessions:
{ "c-general":  { "runtime":"stub", "sessionId":"1eaabb2a-…", "turns":1, … },
  "c-dm-alice": { "runtime":"stub", "sessionId":"633ef192-…", "turns":1, … } }

# alice posts again 13 seconds later — the SAME session is resumed
[…] msg id=m-5 from=alice kind=channel in=#general n=3
[…] handler exit=0 n=1
```

What the fake channel ended up holding:

```
m-1 alice | c-general   | remember the word PELICAN
m-2 bob   | c-dm-alice  | hello there
m-3 agent | c-general   | session 1eaabb2a-4a58-4b29-b9ed-3772d20732e6 has seen 1 turn(s)
m-4 agent | c-dm-alice  | session 633ef192-5c62-44ec-b101-8488db995fd5 has seen 1 turn(s)
m-5 alice | c-general   | what word did I give you?
m-6 agent | c-general   | session 1eaabb2a-4a58-4b29-b9ed-3772d20732e6 has seen 2 turn(s)
```

`m-3` was threaded under `m-1` in the channel and `m-4` went to the DM, because
`convo respond` routes from `source.kind` (`ARCHITECTURE.md` §6.3). Alice's second
message resumed `1eaabb2a…` and bob's session was never touched. No process was
alive between `m-4` and `m-5`.

### Codex, end to end through the router

```
$ printf 'Remember this codeword: OTTER. Reply with only the word OK.' \
    | session-router.sh --tag cx --runtime codex --conversation c-demo
OK
$ printf 'What codeword did I give you? Reply with only that word.' \
    | session-router.sh --tag cx --runtime codex --conversation c-demo
OTTER
$ printf 'What codeword did I give you? If none, reply NONE. One word only.' \
    | session-router.sh --tag cx --runtime codex --conversation c-other
NONE
```

```json
{
  "c-demo":  { "runtime": "codex", "sessionId": "01a08e8a-f540-7341-9054-929dd31d620b", "turns": 2, … },
  "c-other": { "runtime": "codex", "sessionId": "01a08e8b-7dbb-7911-8233-dac933eaa894", "turns": 1, … }
}
```

Memory and isolation, on a runtime that mints its own ids.

The failure path was also captured for real, before `-C` was removed from the
resume branch — this is what a broken adapter looks like from the outside, and
it is the behaviour §7 promises:

```
error: unexpected argument '-C' found
{"error": {"code": "runtimeFailed", "message": "runtime \"codex\" failed (exit 1) for conversation
 \"c-demo\" session \"01a08e8a-f540-7341-9054-929dd31d620b\" — see stderr above"}}
rc=69
```

The runtime's own message survived, the exit code was 69 and not 1, and nothing
was printed to stdout that a caller could mistake for an answer.

### Devin, end to end through the router

```
$ printf 'Remember this codeword: PLATYPUS. Reply with only the word OK.' \
    | session-router.sh --tag dv --runtime devin --conversation c-demo
OK
$ printf 'What codeword did I give you? Reply with only that word.' \
    | session-router.sh --tag dv --runtime devin --conversation c-demo
PLATYPUS
$ session-router.sh --tag dv --runtime devin --list
{
  "c-demo": { "runtime": "devin", "sessionId": "wary-cemetery", "turns": 2, … }
}
```

The id was discovered by diffing `devin list` around the first run, recorded,
and resumed — the session id did not change between turns.

### What did not work

- **`claude --bare`** cannot be used on a subscription-authenticated machine
  (§8). Not a bug in the router; a documented auth restriction. Opt-in only, and
  its latency benefit is unmeasured.
- **`codex exec resume` rejects `-C`.** Found by running it, not by reading
  help. The adapter `cd`s instead.
- **Devin's new-session id discovery is a diff, not a report.** It works, it is
  locked, and it is still the weakest link here: if Devin ever creates two
  sessions for one run, or the list is directory-scoped differently than
  expected, the wrong id gets recorded. Prefer a runtime with caller-chosen ids
  where you have the choice.
- Two bugs in the router itself were found *by* these tests and fixed: an EXIT
  trap ending on a failing `test` replaced every deliberate exit code with `1`
  under `set -e`, and `fail` called inside `$( )` exited only the subshell — both
  printed a perfect error message and then lied about the exit code. Which is
  the entire reason the suite asserts exit codes and not just messages.

---

## 10. This pattern vs the polling responder

| | **session routing** (this doc) | **polling responder** ([`POLLING.md`](POLLING.md)) |
|---|---|---|
| loop in the agent | **none, anywhere** | none in the *session* — the shell script blocks, hosted by a subagent |
| idle cost | **0 turns** | ~6–7 turns per idle hour |
| cost model | per message | per hour, plus per message |
| lifetime | nothing is alive between messages | the subagent is mortal; shifts must be bounded and supervised |
| conversational context | **via resume** — the runtime remembers | whatever the live subagent still has in context |
| latency | daemon poll + agent cold start (4–6s measured) | up to one backoff tier (minutes), plus the answer |
| can ask a follow-up question mid-turn | no — one invocation, one answer | yes, it is an attended session |
| many users | independent sessions, bounded pool | one consumer per tag, one context |
| survives the terminal closing | **yes** | no |
| what can silently break | a session id that no longer resumes | the responder dies while the daemon stays healthy |

**Use session routing when** nothing may loop, the listener must survive the
machine, traffic is bursty or idle most of the day, and different users must not
see each other's context. It is the floor of the design (W2) with memory added,
and its idle cost is genuinely zero.

**Use the polling responder when** you need an *attended* agent — one that can
ask the user a clarifying question inside the same turn, or draw on what the
human operator has been doing in that session all morning. You are paying ~6–7
turns an hour for interactivity; decide to pay it on purpose.

**They compose.** The read cursor and the single-consumer lock keep them from
fighting, and the journal means neither can lose a message. Running W2 session
routing for unattended coverage and a poll responder for an attended session is
a normal deployment — just do not point both at the same conversation's session
id, for the reason in §6.
