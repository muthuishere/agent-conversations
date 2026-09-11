# `convo` — a generic Go CLI for agent conversations

A single static binary, Go 1.26, **standard library only, zero third-party
dependencies**. It is mostly an *interface*: three seams defined cleanly so any
channel and any agent-host can be plugged in, with enough behind each seam to
prove the shape is right.

The prose in [`../ARCHITECTURE.md`](../ARCHITECTURE.md),
[`../INTERFACES.md`](../INTERFACES.md) and
[`../CROSS-SESSION.md`](../CROSS-SESSION.md) says *why*. This is one
implementation of the parts of it that are worth having in a compiled language:
the file interface, the envelope, and the guest path into an agent session you
do not own.

```
go build ./...
go test ./...          # passes offline: no network, no server, no host
go run ./cmd/convo self
```

---

## The three seams

Each interface lives in its own file with a doc comment explaining what it is
for and what it must *not* do.

| interface | file | question it answers |
|---|---|---|
| **`Channel`** | [`internal/convo/channel.go`](internal/convo/channel.go) | where do messages come from, and where do replies go? |
| **`Host`** | [`internal/convo/host.go`](internal/convo/host.go) | where do agents live, and how is a message handed to one? |
| **`Store`** | [`internal/convo/store.go`](internal/convo/store.go) | what survives a crash, and what counts as delivered? |

```go
type Channel interface {
    Conversations(ctx) ([]Conversation, error)
    Fetch(ctx, convID, cursor string) ([]Message, string, error)
    Send(ctx, target Target, text string) (string, error)
    Identity(ctx) (Identity, error)
}

type Host interface {
    Agents(ctx) ([]Agent, error)                 // discovery
    Get(ctx, name string) (Agent, error)         // state
    Deliver(ctx, name, text string, DeliverOpts) (Delivery, error)
    Wait(ctx, name string, until []State, timeout) (State, error)
}
```

Plus the canonical envelope of `ARCHITECTURE.md` §5.2 as a struct
([`message.go`](internal/convo/message.go)) and the five-value state enum
([`state.go`](internal/convo/state.go)).

### What is behind each

| package | implements | why it is here |
|---|---|---|
| [`internal/host/herdr`](internal/host/herdr) | `Host` | the **guest** path — shells out to a real workspace manager, respects backpressure, refuses to prompt its own pane |
| [`internal/host/exechost`](internal/host/exechost) | `Host` | the **owner** path — spawns a fresh agent per message; the portable floor (W2) |
| [`internal/channel/memory`](internal/channel/memory) | `Channel` | an in-process fake so the whole system is testable offline |
| [`internal/store/file`](internal/store/file) | `Store` | the NDJSON journal + read cursor + acks of `INTERFACES.md` §2 |

The two hosts are deliberately as different as two implementations can be:

|  | `herdr` | `exechost` |
|---|---|---|
| lifecycle | someone else's | ours |
| backpressure | real — `idle`/`working`/`blocked` | none; we start it |
| context | accumulated, and contaminable | clean, and empty |
| the reply | the agent posts it itself | captured from stdout |
| cold start | none | one per message |

A single implementation behind an interface is not an interface, it is
indirection. If `Host` fits both of those, it will fit tmux, SSH, a container,
or an HTTP agent service.

---

## Commands

```
convo self                          am I inside an agent host, and which agent am I?
convo host list                     discover addressable agents
convo host state <name>             one agent's state
convo host deliver <name> <text>    hand a message over, respecting backpressure
convo journal [--new]               look at the durable log (NEVER consumes)
convo next [--count n] [--ack]      hand outstanding messages to this consumer
convo ack <id...> | --all           mark messages processed
convo respond <messageId> <text>    reply, routed from the message's own source
convo version
```

Global flags: `--host herdr|exec` · `--exec-cmd '<cmd>'` · `--channel <name>` ·
`--tag <t>` · `--home <dir>` · `--json` · `--wait` · `--timeout <dur>`.

Environment: `CONVO_HOST`, `CONVO_EXEC_CMD`, `CONVO_CHANNEL`, `CONVO_TAG`,
`AGENT_CONVERSATIONS_HOME`.

`--channel` has **no default**, and the only built-in is `memory` (in-process;
a test double, useless across processes). A real deployment implements
`convo.Channel` and wires it in — that is the transport seam. An unconfigured
`respond` exits **65** rather than reporting a reply as sent when it had
nowhere to go.

### `convo self` — the one that matters

Run inside a host pane it reports the pane context **and which agent it is**,
found by matching `HERDR_PANE_ID` against the discovery list:

```
$ convo self
in-host:  yes
session:  demo
socket:   /…/sessions/demo/herdr.sock
pane:     w1:p2
agent:    responder-a (working)
```

The second half is not trivia. Knowing our own pane is what makes the
self-delivery guard possible — **an agent that does not know its own address
cannot refuse to prompt itself.**

Outside a pane it answers cleanly and exits **65**, never crashing and never
pretending:

```
$ convo self
in-host:  no
note:     not inside an agent host pane; host commands will have no targets
```

### `convo host deliver` — backpressure and two refusals

Per `CROSS-SESSION.md` §4, and in this order:

```
resolve by name (never a cached id)
├── OUR OWN PANE     → refuse, exit 70. Nothing is sent.
├── idle             → deliver
├── working          → bounded wait, re-resolve, deliver; still busy → exit 64
├── blocked          → refuse, exit 75. A human is needed; the message is held.
└── done / unknown / absent → exit 69, so the caller falls back
```

**The self-delivery refusal is mandatory and unconditional.** An agent
prompting its own pane wakes itself, answers itself, and wakes itself again.
The guard compares **pane ids, not names**: a name can be changed and reused,
while a pane id identifies the terminal the process is literally running in.

### `convo respond` — the tool routes, not the agent

```
convo respond m-1 "on it"     # channel message -> threaded reply
convo respond m-2 "all good"  # dm              -> that conversation
```

The caller supplies a **message id** and the target is derived from that
message's own `source.kind`. An agent asked to choose between "send to the
conversation" and "reply in the thread" will get it wrong, the platform will
return success, and the reply will vanish. An unknown id fails **loudly**
(exit 65) rather than being sent somewhere plausible.

---

## Exit codes

The first five are `INTERFACES.md` §1 verbatim — a Go CLI inventing its own
would break every shell that already branches on them. Two are new.

| code | meaning |
|---|---|
| **0** | success |
| **1** | unexpected error |
| **64** | nothing to deliver / a bounded wait aged out. **Not an error.** |
| **65** | bad arguments, unknown id, not configured, **not inside a host pane** |
| **66** | conflict — another consumer or another delivery holds this |
| **69** | the host or the target is dead, stale or absent → **fall back** |
| **70** | **self-delivery refused** — the target resolves to our own pane |
| **75** | **the target is blocked** — a human is needed; the message was NOT sent |

Why 70 and 75 exist rather than collapsing into `1`: they demand *different*
reactions. 69 means "try another target". 75 means "do not try again, tell a
person, keep the message". 70 means "you have a bug in your routing". An agent
that cannot tell them apart has to parse an error string to decide, and it will
get it wrong.

`stderr` is always exactly one JSON object, `{"error":{"code":…,"message":…}}`,
with a stable `code`. `stdout` carries data only.

```bash
convo host deliver responder-a "$TEXT"
case $? in
  0)  ;;                                        # handed over
  64) ;;                                        # busy past the wait — try later
  69) fall_back_to_our_own_session ;;           # gone
  70) echo "routing bug: that is us" >&2 ;;     # never retry this
  75) alert_a_human; leave_unacked ;;           # blocked
esac
```

---

## The cursor-vs-ack split

The most-copied bug in this class of system, enforced by `store/file` and
asserted three ways in its tests:

| | meaning | advances when | if you get it wrong |
|---|---|---|---|
| **read cursor** | delivery position (a byte offset) | **whenever a consumer is handed a message**, ack or not | never → infinite redelivery |
| **ack** | "I finished with this" | explicitly, by the consumer | fused with the cursor → forever, or silent loss |

`convo journal` reads and never advances anything. `convo next` is the only
command that advances the cursor. `convo journal --new` lists journalled but
unacked messages — which is the **drain** `CROSS-SESSION.md` §11 found missing:
a message a handler declined is never offered again by the wake path, so
without a drain, "queue in the journal" is "leak into the journal".

---

## Tests

`go test ./...` passes **offline**: no network, no host server, no credentials.

- envelope parsing, against the **real measured JSON shapes** as fixtures —
  including the four that contradict a naive reading of `--help` (enveloped
  `result`, `agent_status` not `status`, no `id` field, errors as JSON on
  stdout);
- the state machine and the backpressure table;
- **the self-delivery guard**, twice: with an injected runner, and end to end
  against a **fake `herdr` binary** — a shell script in `t.TempDir()` — so the
  real argv construction, process spawn, exit codes and stdout parsing all run
  with no server anywhere. That is the interface earning its keep;
- cursor-vs-ack, partial-line safety, ack idempotence, envelope round trip
  (including `raw`);
- the memory channel round trip, including self-echo suppression and the fact
  that a channel reply lands in a **thread** — verifying the room instead of the
  thread is how a successful run looks like a total failure.

A live test against a real host exists and is **skipped by default**; it is
read-only and opt-in:

```bash
CONVO_LIVE_HERDR=1 go test ./internal/host/herdr/ -run Live -v
```

It is gated on an env var rather than on "is a socket present", because a test
that talks to whatever workspace happens to be open is a test that injects text
into somebody's work.

---

## Using it from inside an agent pane

See [`../reference/skill/HERDR.md`](../reference/skill/HERDR.md) — the
agent-facing doc, including the reply-path rule, the consent-gating boundary,
and the honest warnings about context contamination.
