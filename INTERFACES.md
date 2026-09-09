# The daemon's interfaces

> *What does the daemon expose, and how does an agent skill, a subagent, or a
> program in another language read from it?*

This is the contract. [`ARCHITECTURE.md`](ARCHITECTURE.md) says *why*; this says
*exactly what*. The reference implementation in
[`reference/daemon/`](reference/daemon/) implements this document — where they
disagree, this document is the bug report.

There are **four** surfaces, and they are deliberately different sizes:

| # | surface | who consumes it | stability |
|---|---|---|---|
| 1 | [CLI](#1-cli) | an agent skill, a shell, CI | the main one |
| 2 | [files on disk](#2-files-on-disk) | a subagent, a sidecar, **any language** | the portable one |
| 3 | [stdout formats](#3-stdout-formats) | a program (NDJSON) or a model (compact) | two, on purpose |
| 4 | [handler contract](#4-handler-contract) | a process the daemon spawns per batch | the unattended one |

An agent uses #1 and #3. Anything that is not a shell uses #2. #4 is how the
system works with no agent session alive at all.

---

## 1. CLI

```
convo <command> [flags]
```

`convo` is `node reference/daemon/cli.js` in this repo. Every command takes
`--tag <t>` (default `default`, or `$CONVO_TAG`) — a tag partitions one
listener's journal, cursors, heartbeat and lock from another's, so unrelated
products can share a machine.

### Commands

| command | what it does |
|---|---|
| `convo listen --adapter <a> [--on-batch 'cmd'] [--window ms]` | start the daemon (detached by default) |
| `convo listen --status` / `--stop` | inspect / stop it |
| `convo wait [--timeout s] [--count n] [--window ms] [--ack]` | **block** until messages arrive; drains the backlog first |
| `convo journal [--all\|--new] [--limit n]` | non-blocking look at the log. **Never consumes.** |
| `convo ack <id…> \| --all` | mark messages processed |
| `convo health` | read the heartbeat and say whether the listener is really delivering |
| `convo respond <messageId> "text"` | reply, routed automatically from the message's `source.kind` |
| `convo send --to <conversationId> "text"` | send unprompted |
| `convo identity` / `convo conversations` | inspect the adapter |

### Flags that matter

| flag | applies to | meaning |
|---|---|---|
| `--adapter <name\|path>` | listen, send, respond, identity, conversations | the transport module. `memory`, `http`, or a path |
| `--adapter-opt k=v` | same | repeatable; dotted keys nest. **Pass secrets by env-var *name*, never by value** |
| `--on-batch '<cmd>'` | listen | spawn `<cmd>` per coalesced batch (see §4) |
| `--window <ms>` | listen, wait | coalescing window; default 400ms for `listen`, 0 for `wait` |
| `--timeout <s>` | wait | default 900. On expiry: exit 64 |
| `--count <n>` | wait | upper bound on messages returned. Default 1, or unbounded when `--window` is set |
| `--ack` | wait | also mark delivered messages processed |
| `--takeover` | wait | replace an existing consumer instead of failing |
| `--format ndjson\|compact\|pretty` | wait, journal | see §3 |
| `--foreground` | listen | run in this process instead of detaching |
| `--poll-active/-mid/-idle`, `--idle-1/-2` | listen | adaptive backoff tiers, in seconds |
| `--from`, `--exclude-from`, `--in`, `--match`, `--kind`, `--mentions-me` | wait, journal, listen | delivery filters |

Filters compose: **repeats of one flag OR together**, **different flags AND
together**. They apply at *delivery only* — a filtered-out message is still in
the journal and still auditable. Changing who the agent answers is a filter
change, never a transport change.

### stdin / stdout / stderr

- **stdout** carries data only: messages (§3) or a single JSON object for
  command results. Nothing else is ever written there.
- **stderr** carries errors, always as one JSON object:
  `{"error":{"code":"listenerDead","message":"…"}}`. The `code` is stable and
  machine-matchable; the `message` is for a human and may change.
- **stdin** is unused except by a spawned handler (§4).

### Exit codes

| code | meaning |
|---|---|
| **0** | success — for `wait`, at least one message was delivered on stdout |
| **1** | unexpected error |
| **64** | `wait` timed out with nothing to deliver. **This is not an error.** |
| **65** | not configured / bad arguments / unknown message id |
| **66** | another consumer already holds this tag |
| **69** | a daemon is expected but is dead or stale; or the transport is unreachable |

#### Why a timeout must not share an exit code with an error

This is the single most consequential line in the contract.

An agent that cannot tell *"nobody messaged me for 15 minutes"* from *"the
listener is dead"* will do one of two things, and both are bad:

- treat every quiet period as a fault — it restarts a healthy daemon, loses the
  in-flight batch, and pages a human at 2am because a channel was quiet; or,
  far more likely,
- treat every fault as a quiet period — it reports *"still listening, nothing
  came in"* while the transport has been down for an hour. That is **silent
  deafness**, the defining failure of this architecture (ARCHITECTURE §5.8):
  the failure looks exactly like success, so nobody finds out until a person
  asks why they were ignored.

So: **64 means "aged out, nothing arrived" and nothing else uses 64.** A skill
can branch on it without parsing a single byte of output:

```bash
convo wait --timeout 300 --window 800 --format compact
case $? in
  0)  ;;                                  # answer these
  64) ;;                                  # normal quiet — re-arm, say nothing
  66) echo "another consumer holds this tag" >&2; exit 1 ;;
  69) echo "LISTENER IS DEAD — restart it" >&2; exit 1 ;;
  *)  echo "unexpected failure" >&2; exit 1 ;;
esac
```

The same reasoning gives **66** and **69** their own codes: "someone else is
consuming" and "the listener is dead" need different reactions, and collapsing
either into a generic `1` forces the agent to parse an error string to decide.

---

## 2. Files on disk

**This is the interface for anything that is not a shell** — a subagent, a
sidecar process, a dashboard, a script in Python or Go. The CLI is a convenience
over these files; the files are the contract.

All paths are under `$AGENT_CONVERSATIONS_HOME` (default
`~/.config/agent-conversations`). `<tag>` is the listener tag.

```
$AGENT_CONVERSATIONS_HOME/
  journal/<tag>.ndjson          append-only log        daemon writes · anyone reads
  cursor/<tag>.fetch.json       channel position       DAEMON ONLY
  cursor/<tag>.read.json        delivery position      consumer writes
  cursor/<tag>.ack.json         processed marks        consumer writes
  heartbeat.<tag>.json          proof of life          daemon writes · anyone reads
  listener.<tag>.pid            daemon pid             daemon writes
  listener.<tag>.log            daemon log             daemon appends
  consumer.<tag>.pid            single-consumer lock   consumer writes
```

### 2.1 `journal/<tag>.ndjson` — the durable log

One canonical envelope per line, oldest first, **append-only**. Written *before*
any consumer, filter or handler sees the message, so a crash between "received"
and "delivered" loses nothing.

```json
{
  "id": "m-1",
  "at": "2026-09-09T10:00:00.000Z",
  "from": { "id": "u-alice", "name": "alice" },
  "text": "deploy looks red",
  "html": null,
  "source": {
    "kind": "channel",
    "conversationId": "c-general",
    "name": "#general",
    "threadId": "m-1"
  },
  "replyToId": null,
  "mentionsMe": false,
  "raw": { }
}
```

`text` is always a string (never null). `raw` is the untouched channel payload —
never discard it; you will need a field you did not anticipate.

**Writer:** the daemon, one `write()` of one line to a file opened `O_APPEND`, so
concurrent appends interleave between lines and never inside one.
**Readers:** anyone, concurrently, with no lock. A reader may see a partial final
line if it catches the daemon mid-write — read up to the last `\n` and wait for
the rest.
**Reading is not consuming.** Nothing about reading this file changes delivery
state. Only §2.3 does that.

### 2.2 `cursor/<tag>.fetch.json` — channel position (daemon-owned)

How far the daemon has pulled *from the channel*. **Do not write this file from
anywhere but the daemon.**

```json
{
  "tokens": { "c-general": "42", "c-dm-alice": "17" },
  "seenIds": ["m-1", "m-2", "m-3"],
  "updatedAt": "2026-09-09T10:00:01.000Z"
}
```

`tokens` is one **opaque** cursor per conversation — a delta token, a timestamp,
an id watermark, whatever the adapter returned. `seenIds` is a bounded replay
guard (dedupe is by id, never by clock: conversations are polled concurrently,
so a valid message from a quiet DM routinely arrives after a newer one from a
busy channel).

### 2.3 `cursor/<tag>.read.json` — delivery position (consumer-owned)

```json
{ "readOffset": 1275, "updatedAt": "2026-09-09T10:00:02.000Z" }
```

A **byte offset** into the journal: everything before it has been handed to a
consumer. Byte offsets rather than message counts because a consumer must be
able to stop mid-batch (`--count` satisfied) and resume at exactly the right
line; anything coarser silently drops messages at a batch boundary.

It advances **whenever a consumer is handed a message**, with or without an ack,
and is persisted **after** the handoff succeeds — so an interrupted `wait`
redelivers rather than drops. At-least-once is the correct side to fail on.

> **This is the most-copied bug in this class of system.** Never advance it on
> ack; never advance it on a read; never let two processes write it (that is
> what §2.7 is for).

### 2.4 `cursor/<tag>.ack.json` — processed marks (consumer-owned)

```json
{ "ackedIds": ["m-1", "m-2"], "updatedAt": "2026-09-09T10:00:03.000Z" }
```

**Ack is not what prevents redelivery** — the read cursor does that,
unconditionally. Ack is an explicit *"I finished with this"*, used by
`convo journal --new` to show what nobody has completed. An agent can consume a
message without acking it and still see it listed as outstanding, which is
exactly what you want when a batch was delivered but the answer failed.

### 2.5 `heartbeat.<tag>.json` — proof of life

Rewritten atomically (tmp + rename) every poll cycle, and every ≤2s even at a
5-minute idle interval.

```json
{
  "ts": "2026-09-09T10:00:04.539Z",
  "startedAt": "2026-09-09T10:00:00.531Z",
  "pollCount": 6,
  "lastMessageAt": null,
  "intervalMs": 200,
  "conversations": 2,
  "identity": { "id": "u-agent", "name": "agent" },
  "pid": 84598
}
```

**Stale = `now - ts > max(5s, intervalMs × 3)`.** A missing file means no daemon
has ever run for this tag.

Two rules, both non-negotiable:

- **Never report "listening" from memory. Read this file.**
- **A consumer must refuse to block against a stale heartbeat**, and must
  re-check *while* blocked — a daemon can die mid-wait, which is precisely when
  nobody is looking.

### 2.6 `listener.<tag>.pid` / `listener.<tag>.log`

The pid file is a bare pid and a newline, so `cat` output is directly usable. Its
*existence* means "a daemon is expected for this tag" — which is exactly the
question a consumer must answer before it blocks. The log is plain text, one
`[iso] message` per line.

### 2.7 `consumer.<tag>.pid` — the single-consumer lock

A bare pid. Two consumers sharing one read cursor race for messages and tear
down each other's state; the second exits **66** unless it passes `--takeover`,
which SIGTERMs (then SIGKILLs) the incumbent and takes the lock. A lock whose pid
is no longer alive is stale and is claimed silently.

### Concurrency summary

| file | concurrent readers | concurrent writers |
|---|---|---|
| journal ndjson | ✅ safe, lock-free | one (the daemon) |
| fetch cursor | ✅ | one (the daemon) |
| read cursor | ✅ | one (the lock in §2.7 enforces it) |
| ack | ✅ | one (same lock) |
| heartbeat | ✅ atomic rename, never torn | one (the daemon) |
| pid files | ✅ | the owning process |

---

## 3. stdout formats

`--format` selects one. There are two for a reason.

### `ndjson` (default) — for programs

One JSON envelope per line, exactly as journalled.

```
{"id":"m-1","at":"2026-09-09T10:00:00.000Z","from":{"id":"u-alice","name":"alice"},"text":"deploy looks red","html":null,"source":{"kind":"channel","conversationId":"c-general","name":"#general","threadId":"m-1"},"replyToId":null,"mentionsMe":false,"raw":{…}}
```

### `compact` — for an agent

Tab-separated, one line per message:

```
<messageId>	<from>	[dm]|[#conversation]	<text, one line, ≤200 chars>
```

```
m-1	alice	[#general]	deploy looks red
m-2	bob	[#general]	same here, ping?
m-3	alice	[dm]	status?
```

The **message id is never truncated** — it is the join key for `convo respond`,
so a long body must not push it off the line or make the split ambiguous. Only
the trailing text is capped.

### `pretty` — for a human

`10:00 alice › deploy looks red  [#general]`

### Why both exist

Because a model parsing JSON by hand is slow, expensive and wrong just often
enough to hurt. Given a wall of envelopes it will hallucinate a field name,
mis-split on a brace inside a string, or quietly ignore an error object it did
not expect — and the resulting bug looks like a daemon bug, so you go and debug
the wrong thing. Ad-hoc parsing of tool output is listed as its own failure mode
in ARCHITECTURE §9 for that reason.

The rule: **any tool an agent drives must offer a machine-readable *and* a
human-readable mode.** The agent reads `compact` with its eyes, extracts the id
from before the first tab, and calls `respond <id>`. No parsing step, so no
parsing bugs. Scripts and other languages take `ndjson` and parse it properly.

---

## 4. Handler contract

With `--on-batch '<cmd>'` the daemon spawns `sh -c '<cmd>'` for each coalesced
batch. This is wake mechanism **W2** (ARCHITECTURE §4) — the universal one: it
needs no live agent session, survives the terminal closing, and is the floor
below which you do not have a product.

### What the handler receives

**stdin:** NDJSON, one envelope per line, the whole batch, then EOF. Never
empty — a handler is only spawned when there is at least one message.

**environment:**

| variable | meaning |
|---|---|
| `CONVO_BATCH_COUNT` | number of lines on stdin |
| `CONVO_TAG` | the listener tag |
| `CONVO_HOME` | the state directory (§2) |
| `CONVO_JOURNAL` | full path to the journal file |
| `CONVO_SELF_ID` / `CONVO_SELF_NAME` | **the daemon's identity** |
| `CONVO_ADAPTER` / `CONVO_ADAPTER_OPT` | the daemon's transport, so `convo respond` inside the handler reaches the same channel |

`CONVO_SELF_*` is not informational. **Pin the handler to the daemon's
identity.** If the handler replies as anyone else, self-echo suppression does not
recognise the reply, the daemon journals it, wakes the handler, which replies
again — an infinite loop that spends real money and spams a real channel
(ARCHITECTURE §5.3, §9).

### What the handler's exit code means

| exit | meaning |
|---|---|
| 0 | handled |
| non-zero | logged as a failure with its stderr |

**The daemon keeps running either way, and does not retry the batch.** A handler
can never kill the listener: spawn failures, crashes, timeouts and non-zero
exits are all logged and swallowed. Losing the listener because someone's shell
script had a typo is exactly the silent deafness this design exists to prevent.
If you need retries, own them inside the handler — the batch is still in the
journal, so nothing was lost.

### Execution model

- **Serialized.** One handler in flight at a time. A burst arriving mid-run is
  collected and flushed as the next batch.
- **Fresh process per batch.** Nothing long-lived can go stale holding the
  listener, and a stateless handler with a small toolset is far easier to reason
  about than a long-lived agent accumulating capability (ARCHITECTURE §8).
- **Coalesced.** Five people typing at once become **one** invocation, not five.
  The single biggest cost lever in the design — and it improves answers, because
  the handler sees the whole room at once.

### Idempotence is required

Delivery is **at-least-once**. A handler can run twice for the same message: the
daemon crashes after spawning but before the child finishes, a batch is
redelivered after a restart, an operator replays a journal. So:

- **dedupe on `id`** before doing anything with a side effect;
- make replies safe to repeat, or record the ids you have answered;
- never treat "I have already seen this id" as an error.

A minimal, correct handler:

```bash
#!/usr/bin/env bash
set -euo pipefail
SEEN="$CONVO_HOME/handled.$CONVO_TAG"
touch "$SEEN"
while IFS= read -r line; do
  id=$(printf '%s' "$line" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
  grep -qxF "$id" "$SEEN" && continue          # idempotence
  convo respond "$id" "got it"
  echo "$id" >> "$SEEN"
done
```

A worked example with a rules-first answering table (ARCHITECTURE §6.4) is in
[`examples/handler.js`](examples/handler.js).

---

## Reading from another language

The file interface (§2) is the portable one: an append-only NDJSON log plus a
JSON heartbeat. Anything that can read a file can be a consumer — no CLI, no
Node, no HTTP.

### Python — tail the journal, refuse to trust silence

```python
import json, os, time
home = os.environ.get("AGENT_CONVERSATIONS_HOME", os.path.expanduser("~/.config/agent-conversations"))
tag  = os.environ.get("CONVO_TAG", "default")
beat = os.path.join(home, f"heartbeat.{tag}.json")

with open(os.path.join(home, "journal", f"{tag}.ndjson")) as f:
    f.seek(0, os.SEEK_END)                                  # tail, don't replay
    while True:
        line = f.readline()
        if not line.endswith("\n"):                         # partial or nothing yet
            hb = json.load(open(beat))                      # liveness BEFORE waiting
            if time.time() - time.mktime(time.strptime(hb["ts"][:19], "%Y-%m-%dT%H:%M:%S")) > 3 * hb["intervalMs"] / 1000:
                raise SystemExit("listener is stale — silence here is deafness, not quiet")
            time.sleep(0.25)
            continue
        m = json.loads(line)
        print(f'{m["from"]["name"]} in {m["source"]["name"]}: {m["text"]}')
```

Runnable version: [`examples/tail_journal.py`](examples/tail_journal.py).

Note the shape of it: check the heartbeat **before** deciding that no line means
no traffic. That check is the whole difference between a consumer and a corpse
watcher, and it is available to any language, because it is just a JSON file.

### Shell — no curl, no dependencies

```bash
# who said what, most recent 5, no JSON parser required
tail -5 "$AGENT_CONVERSATIONS_HOME/journal/default.ndjson" |
  sed 's/.*"name":"\([^"]*\)".*"text":"\([^"]*\)".*/\1: \2/'

# is the listener actually delivering? (exit 0 = yes)
convo health >/dev/null

# follow the log live
tail -f "$AGENT_CONVERSATIONS_HOME/journal/default.ndjson"
```

A subagent given nothing but `$AGENT_CONVERSATIONS_HOME` and this document can
read every message, check liveness, and see what has been acked — without the
ability to consume anything or to disturb the consumer that owns the cursor.
That separation is the point.
