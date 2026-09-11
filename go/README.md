# `convo` — a generic Go CLI for agent conversations

A single static binary, Go 1.26, **standard library only, zero third-party Go
modules**.

**Prerequisite: the agent host.** `convo` delivers into an agent through Herdr,
and that is not one option among several — it is a required dependency. Install
it before anything here does anything useful; `convo self` tells you whether you
are inside a pane. The reasoning and the measurements behind fixing the host are
in [`../ARCHITECTURE.md`](../ARCHITECTURE.md) §4.

**The channel is the plugin axis.** Because the host is fixed, the growth is
sideways: Teams today, WhatsApp next, whatever after that. Adding one is a new
package under `internal/channel/` implementing four methods, and nothing above it
changes — [`internal/channel/README.md`](internal/channel/README.md) is the
guide.

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
| **`Host`** | [`internal/convo/host.go`](internal/convo/host.go) | where do agents live, and how is a message handed to one? (**answer: Herdr**) |
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
| [`internal/host/herdr`](internal/host/herdr) | `Host` | **the delivery path.** Shells out to the agent host, respects backpressure, refuses to prompt its own pane |
| [`internal/host/exechost`](internal/host/exechost) | `Host` | **a test double and a reference for the interface** — spawns a fresh agent per message. Not the recommended path; see below |
| [`internal/channel/teams`](internal/channel/teams) | `Channel` | a real second channel: Microsoft Graph over HTTP — paging, delta cursors, threaded replies, throttling |
| [`internal/channel/memory`](internal/channel/memory) | `Channel` | an in-process fake so the whole system is testable offline |
| [`internal/store/file`](internal/store/file) | `Store` | the NDJSON journal + read cursor + acks of `INTERFACES.md` §2 |

### Why `Host` and `exechost` still exist

Herdr is mandatory, so it is fair to ask why there is an interface in front of it
at all. Two reasons, and neither is decoration:

1. **`Host` is what makes the delivery path testable with no host running.** The
   self-delivery guard is asserted end to end against a **fake host binary** — a
   shell script in `t.TempDir()` — so real argv construction, process spawn, exit
   codes and stdout parsing all execute offline. Delete the interface and those
   tests go with it. It is not dead abstraction; do not remove it.
2. **A single implementation behind an interface is indirection, not an
   interface.** `exechost` is as different from `herdr` as two things can be and
   still be "hand a message to an agent", which is the evidence the seam is in
   the right place.

|  | `herdr` (the path) | `exechost` (the double) |
|---|---|---|
| lifecycle | someone else's | ours |
| backpressure | real — `idle`/`working`/`blocked` | none; we start it |
| context | accumulated, and contaminable | clean, and empty |
| the reply | the agent posts it itself | captured from stdout |
| cold start | none | one per message |

`exechost` was once described here as "the portable floor". It is not that any
more — the floor is Herdr. Keep it for the two reasons above, and for the honest
record in `ARCHITECTURE.md` §4 of what was measured.

---

## Commands

```
convo self                          am I inside an agent host, and which agent am I?
convo host list                     discover addressable agents
convo host state <name>             one agent's state
convo host deliver <name> <text>    hand a message over, respecting backpressure
convo fetch                         one ingest pass: channel -> journal
convo listen [--once]               fetch on a loop, with adaptive backoff
convo journal [--new]               look at the durable log (NEVER consumes)
convo next [--count n] [--ack]      hand outstanding messages to this consumer
convo ack <id...> | --all           mark messages processed
convo respond <messageId> <text>    reply, routed from the message's own source
convo version
```

Global flags: `--host herdr|exec` (default `herdr`; `exec` is the spawning test
double, not a supported deployment) · `--exec-cmd '<cmd>'` · `--channel <name>` ·
`--tag <t>` · `--home <dir>` · `--json` · `--wait` · `--timeout <dur>`.

Ingest flags (`fetch`, `listen`): `--in <needle>` · `--prime` · `--once` ·
`--poll-active/-mid/-idle` · `--idle-1` · `--idle-2`.

Teams flags: `--teams-base-url <url>` · `--teams-token-env <VAR>` ·
`--teams-user <name>` · `--teams-scan-depth <n>`.

Environment: `CONVO_HOST`, `CONVO_EXEC_CMD`, `CONVO_CHANNEL`, `CONVO_TAG`,
`AGENT_CONVERSATIONS_HOME`, `CONVO_TEAMS_BASE_URL`, `CONVO_TEAMS_TOKEN_ENV`,
`CONVO_TEAMS_USER`, `CONVO_TEAMS_SCAN_DEPTH`.

`--channel` has **no default**. Two are built in: `memory` (in-process; a test
double, useless across processes) and `teams`, the real adapter that
[`internal/channel/README.md`](internal/channel/README.md) documents. `teams`
needs a base URL and, against a real tenant, a credential — and the credential
is passed **by the NAME of an environment variable**, never by value:

```
convo listen --channel teams \
  --teams-base-url https://graph.microsoft.com/v1.0 \
  --teams-token-env TEAMS_BEARER
```

`--teams-token-env TEAMS_BEARER` makes the CLI read `$TEAMS_BEARER` itself, so
the secret never reaches the shell history, `ps` output, or a log line that
echoes the command. `--teams-user` is a Graph-shaped simulator affordance and a
real tenant ignores it.

An unconfigured `respond` exits **65** rather than reporting a reply as sent
when it had nowhere to go.

### `convo self` — the one that matters

Run inside a host pane it reports the pane context **and which agent it is**,
found by matching the host's pane id (`HERDR_PANE_ID`) against the discovery
list. Because the host is a hard prerequisite, that id is always there when it
matters — which is what makes the self-delivery guard possible at all:

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
  thread is how a successful run looks like a total failure;
- **the Teams channel, against recorded JSON fixtures** captured from a real
  Graph-shaped server: discovery, self-echo suppression, threaded replies picked
  up through a composite cursor, a quiet second poll, the send-routing table
  (dm / thread / new thread / kind mismatch / unknown id / empty text), a 429
  retried with `Retry-After`, and the assertion that a channel id reaches the
  wire percent-encoded — `19:…@thread.tacv2` unencoded is a 404 on every request
  and a silently deaf listener. That last one caught a real bug before any live
  server did.

Two live tests exist and both are **skipped by default**:

```bash
CONVO_LIVE_HERDR=1 go test ./internal/host/herdr/  -run Live -v   # read-only
CONVO_LIVE_TEAMS=1 go test ./internal/channel/teams/ -run Live -v  # writes
```

They are gated on an env var rather than on "is something listening", because a
test that talks to whatever happens to be running is a test that injects text
into somebody's work, or posts into somebody's channel.

The host one is read-only on purpose. The Teams one **writes**, because the thing
being proved cannot be proved any other way: a person posts, the message is
fetched through the `Channel`, a reply is sent, and then the reply is **read back
out of the thread**. Point it only at a simulator you own
(`CONVO_LIVE_TEAMS_URL`, default `http://127.0.0.1:4000/v1.0`). Verify the
artifact, never the exit code.

---

## Using it from inside an agent pane

See [`../reference/skill/HERDR.md`](../reference/skill/HERDR.md) — the
agent-facing doc for **the** delivery path, including the reply-path rule, the
consent-gating boundary, and the honest warnings about context contamination.

## Adding a channel

[`internal/channel/README.md`](internal/channel/README.md) — the four methods and
what each must guarantee, opaque and composite cursors, the envelope mapping,
self-echo suppression, reply-target routing, rate limits and backoff, what
belongs in the channel versus what the daemon already does for you, a checklist,
and the shape of WhatsApp and Slack next to the verified Teams one.
