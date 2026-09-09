# `convo` — the reference daemon

A working, dependency-free implementation of the daemon described in
[`../../ARCHITECTURE.md`](../../ARCHITECTURE.md) §5. Node ≥ 20, ESM, **zero npm
dependencies**, one channel-specific seam.

This is reference code meant to be *read*. It is small on purpose: every rule in
the architecture is a few lines here with a comment saying which incident it
came from. It is not a framework, and there is nothing to configure but an
adapter.

The full contract — commands, exit codes, on-disk files, output shapes, handler
protocol — is [`../../INTERFACES.md`](../../INTERFACES.md).

---

## Run the demo (offline, 15 seconds)

```bash
node examples/demo.js
```

No network, no npm install, no account: it uses the in-repo memory adapter as a
fake channel and a throwaway temp directory as state.

---

## Files

| file | what it is |
|---|---|
| `cli.js` | the `convo` command surface — the only entry point |
| `daemon.js` | discovery, fetch, normalize, self-echo suppression, coalescing, dispatch, heartbeat |
| `journal.js` | the append-only NDJSON log **and the three separate cursors** |
| `wait.js` | the consumer side: drain-then-block, liveness refusal, single-consumer lock |
| `filters.js` | delivery-time filters (never applied to the journal) |
| `format.js` | NDJSON for programs, compact one-line-per-message for agents |
| `loader.js` | resolves `--adapter <name\|path>` |
| `config.js` | paths, exit codes, adaptive-backoff tiers, heartbeat health |
| `../adapters/adapter.js` | **the transport interface** — the only channel-specific seam |
| `../adapters/memory-adapter.js` | in-process fake channel, so everything is testable offline |
| `../adapters/http-adapter.js` | generic REST/poll adapter with `TODO(channel)` markers |

Read them in that order; `journal.js`'s header comment is the single most
important thing in the directory.

---

## Quick start with your own channel

1. Copy `../adapters/http-adapter.js`, fill in the four `TODO(channel)` markers.
2. `node reference/daemon/cli.js listen --adapter ./my-adapter.js --adapter-opt baseUrl=… --on-batch './handler.sh'`
3. Nothing else changes. If porting to a new channel forces an edit outside your
   adapter file, the seam is in the wrong place.

---

## The parts that are easy to get wrong

Each of these is a real failure that was observed, not a theoretical one.

- **Three cursors, not one.** *Fetch* position (daemon ↔ channel), *read*
  position (delivery to a consumer), and *ack* (processed) are three different
  things in three different files. Fusing read and ack gives you either infinite
  redelivery or silent loss, with nothing in between. See `journal.js`.
- **The read cursor advances on delivery, not on ack**, and is persisted *after*
  stdout is written — so an interrupted `wait` redelivers rather than drops.
- **Self-echo suppression is by identity, before the journal.** Content matching
  is not a substitute. Without it the agent's own reply wakes the agent.
- **A handler must be pinned to the daemon's identity.** The daemon exports
  `CONVO_SELF_ID` / `CONVO_SELF_NAME` / `CONVO_ADAPTER` for exactly this. A
  handler replying as somebody else is a self-reply loop that suppression
  cannot see.
- **A timeout is not an error.** `wait` exits **64** when nothing arrived, and no
  error path uses 64.
- **`wait` refuses to block against a stale heartbeat** (exit 69), and re-checks
  liveness *while* blocked. Silence must never be mistaken for "no messages".
- **Reading is not consuming.** `convo journal` never advances anything.
- **A handler can never kill the daemon.** Spawn failures, crashes and non-zero
  exits are logged and swallowed.

---

## Expected output of `node examples/demo.js`

```
state dir: /tmp/convo-demo-XXXXXX

── 1. start the daemon — and prove it is alive from the heartbeat file
{"started":true,"ready":true,"pid":84598,"tag":"demo","log":"…/listener.demo.log"}
running=true pid=84598 healthy=true heartbeat=fresh age=50ms polls=4

── 2. three messages land at once from two senders — plus one self-echo
posted: alice+bob in #general, alice in dm, and one message as ourselves

── 3. the agent blocks ONCE and is handed the whole room as one batch
m-1	alice	[#general]	deploy looks red
m-2	bob	[#general]	same here, ping?
m-3	alice	[dm]	status?
exit=0   (0 = messages delivered)

── 4. answer by message id — the tool routes dm vs channel itself
{"ok":true,"id":"m-8","inReplyTo":"m-1","target":{"conversationId":"c-general","kind":"channel","threadId":"m-1","replyToId":"m-1"}}
{"ok":true,"id":"m-9","inReplyTo":"m-2","target":{"conversationId":"c-general","kind":"channel","threadId":"m-2","replyToId":"m-2"}}
{"ok":true,"id":"m-10","inReplyTo":"m-3","target":{"conversationId":"c-dm-alice","kind":"chat"}}

── 5. the same wait again — nothing is redelivered, and our own replies never appear
exit=64   (64 = timed out with nothing new. NOT an error.)

── 6. meanwhile the daemon ALSO spawned a fresh handler for the same batch (wake path W2)
(both wake paths are armed here on purpose: the blocking `wait` above is W1,
 the spawned handler is W2 — the one that survives the session dying.)
  handler out: handler: batch of 3 (CONVO_BATCH_COUNT=3)
  handler out: handler: m-1 <- "noted, alice — a human will pick this up" exit=0
  handler out: handler: m-2 <- "noted, bob — a human will pick this up" exit=0
  handler out: handler: m-3 <- "all green" exit=0
  handler exit=0 n=3

── 7. the journal — durable, append-only, complete, and reading it consumed nothing
m-1	alice	[#general]	deploy looks red
m-2	bob	[#general]	same here, ping?
m-3	alice	[dm]	status?
journalBytes=1275 readOffset=1275 (the read cursor advanced on DELIVERY, not on ack; ack count=0)
note: "MY OWN ECHO" is absent — suppressed by identity before the journal

── 8. a second consumer on the same tag is refused
{"error":{"code":"consumerConflict","message":"another consumer already holds tag \"demo\" (pid 87402); two consumers on one read cursor race for messages — pass --takeover to replace it"}}
exit=66   (66 = another consumer holds this tag)

── 9. kill the daemon — wait must fail FAST, not block on a corpse
{"error":{"code":"listenerDead","message":"refusing to wait: the daemon for tag \"demo\" is not delivering — daemon pid 84598 is no longer alive. Restart it with `convo listen`."}}
exit=69 after 33ms   (69 = listener dead; it did NOT wait 30s)

done. silence is never mistaken for "no messages".
```

Message ids and pids vary per run; everything else is stable.

---

## What is deliberately **not** here

- No retention or compaction of the journal. It is the source of truth and the
  audit record; rotating it is a separate, explicit concern.
- No push transport. Poll works everywhere and is the one you can actually test
  (ARCHITECTURE §5.1). Push is a latency optimization to add per-adapter.
- No answering policy. Who to answer and what to say live in the agent skill,
  not here (ARCHITECTURE §7). The `examples/handler.js` rule table is an
  illustration, not part of the daemon.
- No authorization model. Inbound messages are **untrusted input**: a message can
  request, it can never authorize (ARCHITECTURE §8). Give your handler a narrow
  toolset.
