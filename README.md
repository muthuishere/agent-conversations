# agent-conversations

A reference architecture for letting an AI agent hold conversations on a messaging channel
(Teams, Slack, WhatsApp, Telegram, email) **without a polling loop, without losing messages,
and without paying while nothing happens.**

Product-agnostic and channel-agnostic, with **one fixed dependency: the agent host.** Docs,
contracts, a Go CLI, and a runnable offline example — not a framework you install.

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

## The agent host is a prerequisite

**Waking is not a design choice here. It is a dependency — Herdr, the agent host.**

Four wake mechanisms were built and measured — a managed host, a blocked process the runtime
re-invokes, spawning a fresh headless agent per batch, and keystroke injection into a live
session. The host won, and `ARCHITECTURE.md` §4 keeps the whole comparison, because the
measurements are why the decision is what it is. The other three remain as **recorded
findings**, not as alternative paths to pick from.

What the host gives you, always, instead of sometimes:

| | |
|---|---|
| **typed agent states** | `idle` · `working` · `blocked` · `done` · `unknown` — a real backpressure policy instead of a guess |
| **stable addressing** | by name, re-resolved on every delivery; never a cached id that resolves to a stranger |
| **pane self-detection** | an agent that knows its own address can refuse to prompt itself |
| **supervision** | panes are supervised by something that is not us |

`blocked` is the one hand-rolled injection never has, and it is the one that matters: it
separates *finished* from *parked on a permission prompt nobody is watching*.

**The trade, stated once:** a mandatory host is a hard third-party dependency and a single
point of failure for delivery. It was accepted in exchange for the four guarantees above.

## The channel is the plugin axis

The host is fixed; **channels are what you add.** Teams today, WhatsApp next, then whatever
else. A new channel is one package implementing four methods, and nothing above it changes:

- the guide — [`go/internal/channel/README.md`](go/internal/channel/README.md)
- a worked implementation — [`go/internal/channel/teams`](go/internal/channel/teams) (Microsoft
  Graph: paging, delta cursors, threaded replies, throttling)
- the minimal shape — [`go/internal/channel/memory`](go/internal/channel/memory)

## Repo layout

| Path | What lives there |
|---|---|
| `ARCHITECTURE.md` | The full reference architecture — the problem, the split, the wake mechanisms, failure modes, build order |
| `INTERFACES.md` | The contracts: adapter interface, canonical envelope, daemon/consumer protocol |
| `CROSS-SESSION.md` | Delivering into an agent session you don't own — consent, backpressure, context contamination |
| `OPTIONS-TEMPLATE.md` | How to write up a decision with real options — the method behind the choices in `ARCHITECTURE.md` |
| `reference/daemon/` | Daemon design notes — journal, cursor vs ack, coalescing, backoff, heartbeat, single-consumer lock |
| `reference/adapters/` | Per-channel adapter notes (Teams, Slack, Telegram, WhatsApp, IMAP) |
| `reference/skill/` | The agent-side operating discipline and policy: filters, reply routing, rules-first answering, re-arming. `HERDR.md` is the delivery path |
| `go/` | The Go CLI: the three seams as interfaces, the Herdr host, the file store, and the channels |
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

## Add a channel

This is the extension path. Only the adapter changes:

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

The full method — per-method guarantees, opaque and composite cursors, self-echo suppression,
reply-target routing, rate limits and a checklist — is in
[`go/internal/channel/README.md`](go/internal/channel/README.md), with a second real
implementation next to it in [`go/internal/channel/teams`](go/internal/channel/teams).

## A word on security

Inbound messages are **untrusted input**. A message can request; it can never authorize.
Sender identity is not authorization. Give the handler a capability boundary — the channel and
its own product surface, not credentials and not an unrestricted shell. See section 8 of
`ARCHITECTURE.md`.

## More

- Site: https://muthuishere.github.io/agent-conversations/
- Architecture: [`ARCHITECTURE.md`](ARCHITECTURE.md)
- Contracts: [`INTERFACES.md`](INTERFACES.md)
- Borrowing someone else's session: [`CROSS-SESSION.md`](CROSS-SESSION.md)
- Writing a decision doc: [`OPTIONS-TEMPLATE.md`](OPTIONS-TEMPLATE.md)
- Agent-side discipline: [`reference/skill/`](reference/skill/)
- Delivering into an agent (the host path): [`reference/skill/HERDR.md`](reference/skill/HERDR.md)
- Writing a channel: [`go/internal/channel/README.md`](go/internal/channel/README.md)

## License

MIT © 2026 Muthukumaran Navaneethakrishnan. See [`LICENSE`](LICENSE).
