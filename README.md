# agent-conversations

A reference architecture for letting an AI agent hold conversations on a messaging channel
(Teams, Slack, WhatsApp, Telegram, email) **without a polling loop, without losing messages,
and without paying while nothing happens.**

Product-agnostic, runtime-agnostic, channel-agnostic. Docs, contracts, and a runnable offline
example — not a framework you install.

## The core principle

> **Separate receiving from waking from answering.** They have different lifetimes, different
> costs, and different failure modes. Fusing any two of them produces a system that either
> loses messages or burns money.

| Concern | Owner | Lifetime | Cost |
|---|---|---|---|
| **Receiving** — get messages out of the channel, durably | daemon | forever | ~zero, no model |
| **Waking** — tell an agent something arrived | tripwire | per message | zero while idle |
| **Answering** — decide and reply | agent | one turn per batch | the only real cost |

An agent session cannot be pushed to. Something must invoke it — and that something is never
the agent itself. A `while true: check; sleep 5` loop costs a turn per tick, still misses
anything arriving between ticks, and dies with the session.

## The three wake mechanisms

| # | Mechanism | Who invokes | Idle cost | Latency | Survives session death | Portability |
|---|---|---|---|---|---|---|
| **W1** | Blocked process exits → runtime re-invokes the session | the agent runtime | zero | instant | no | runtime-specific |
| **W2** | Daemon spawns a fresh headless agent per batch | the daemon | zero | spawn time | yes | **universal** |
| **W3** | Keystroke injection into a live interactive session | external driver | zero | ~1s | no | terminal-specific |

**W2 is the floor — build it first.** W1 and W3 are latency optimizations over it. Whether a
given runtime re-invokes a session when a background task exits (W1) must be **verified
empirically**; runtime documentation on this contradicts itself.

## Repo layout

| Path | What lives there |
|---|---|
| `ARCHITECTURE.md` | The full reference architecture — the problem, the split, the wake mechanisms, failure modes, build order |
| `INTERFACES.md` | The contracts: adapter interface, canonical envelope, daemon/consumer protocol |
| `OPTIONS-TEMPLATE.md` | How to write up a decision with real options — the method behind the choices in `ARCHITECTURE.md` |
| `reference/daemon/` | Daemon design notes — journal, cursor vs ack, coalescing, backoff, heartbeat, single-consumer lock |
| `reference/adapters/` | Per-channel adapter notes (Teams, Slack, Telegram, WhatsApp, IMAP) |
| `reference/skill/` | The agent-side operating discipline and policy: filters, reply routing, rules-first answering, re-arming |
| `examples/` | Runnable, dependency-free illustrations of the whole loop |
| `docs/index.html` | The published site |

## Run the offline example

No `npm install`, no network, no channel credentials. A mock adapter feeds synthetic messages
through the real daemon logic — journal, suppression, coalescing, wake, reply.

```sh
git clone https://github.com/muthuishere/agent-conversations
cd agent-conversations
node examples/demo.js
```

It starts a real daemon against an in-memory adapter in a throwaway temp directory and walks
the whole architecture in order: the heartbeat proves liveness; three near-simultaneous
messages from two senders coalesce into **one** batch; a self-echo never reaches the journal;
the agent answers by message id and the tool routes dm vs channel; nothing is redelivered on
the next wait; killing the daemon makes `wait` fail fast instead of blocking on a corpse; and
a second consumer on the same tag is refused.

## Adapt it to your channel

Only the adapter changes:

```
listConversations()                    -> [{id, kind, name}]
fetchSince(conversationId, cursor)     -> {messages[], nextCursor}
send(target, text, opts)               -> {id}
identity()                             -> {id, name}
```

Normalize into the canonical envelope, and everything above the adapter is untouched. If the
envelope and the daemon contract hold, **the agent skill needs no changes at all** — that is
the test of whether you implemented this correctly.

Build the poll path first. Push (websocket, Socket Mode, webhooks) is a latency bonus that not
every tenant or plan grants you; a poll-based daemon works everywhere and is the one you can
actually test.

## A word on security

Inbound messages are **untrusted input**. A message can request; it can never authorize.
Sender identity is not authorization. Give the handler a capability boundary — the channel and
its own product surface, not credentials and not an unrestricted shell. See section 8 of
`ARCHITECTURE.md`.

## More

- Site: https://muthuishere.github.io/agent-conversations/
- Architecture: [`ARCHITECTURE.md`](ARCHITECTURE.md)
- Contracts: [`INTERFACES.md`](INTERFACES.md)
- Writing a decision doc: [`OPTIONS-TEMPLATE.md`](OPTIONS-TEMPLATE.md)
- Agent-side discipline: [`reference/skill/`](reference/skill/)

## License

MIT © 2026 Muthukumaran Navaneethakrishnan. See [`LICENSE`](LICENSE).
