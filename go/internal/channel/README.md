# Writing a `Channel`

**The agent host is fixed. The channel is the axis this product grows along.**

Delivery into an agent always goes through Herdr (`internal/host/herdr`), which is a required
dependency — see [`../README.md`](../README.md) and [`../../ARCHITECTURE.md`](../../ARCHITECTURE.md)
§4. Nothing about that changes when you add Teams, WhatsApp, Slack or IMAP. What changes is
one package under `internal/channel/`, and this file is how to write it.

Two implementations exist to copy from, and they are deliberately unalike:

| package | what it is | read it for |
|---|---|---|
| [`memory`](memory/) | in-process fake, no network | the minimal shape of a correct `Channel` |
| [`teams`](teams/) | Microsoft Graph over HTTP | paging, delta cursors, threading, throttling, encoding traps |
| [`whatsapp`](whatsapp/) | two subprocesses over a local store | a transport that is **not** an HTTP API, a cursor invented where the platform has none, and an identity broker instead of a credential |

---

## 1. The four methods

```go
type Channel interface {
    Conversations(ctx) ([]Conversation, error)
    Fetch(ctx, convID, cursor string) (msgs []Message, next string, err error)
    Send(ctx, target Target, text string) (id string, err error)
    Identity(ctx) (Identity, error)
}
```

The full contract with its reasoning is in [`../convo/channel.go`](../convo/channel.go). What
each method must **guarantee**:

### `Conversations` — discovery, re-run forever

Return every room, DM and mailbox worth polling. Follow the API's paging until it stops
offering a next link; nothing above this seam should ever see a continuation token.

Discovery is re-run periodically, not once at boot. Rooms appear, DMs are opened, a bot is
removed from a team. **A listener that discovers once goes quietly deaf to everything created
afterwards, and looks perfectly healthy doing it.**

`Conversation.ID` is one string and it must be enough to reach that conversation from a cold
start, days later, out of a journalled message. If your platform needs two identifiers, make a
composite and put the **kind in the id itself** — `teams` uses `channel:<teamId>/<channelId>`
and `chat:<chatId>` — so `Send` can *check* the routing instead of inferring it.

### `Fetch` — everything new since an opaque cursor

- The cursor's format is entirely yours. Nothing above interprets it, so it can be a delta
  token, a timestamp, a UID watermark, or a base64 blob of all three.
- An **empty** cursor means "from wherever you consider the beginning".
- An **unreadable** cursor is a loud error, never a silent restart. Quietly treating it as
  empty replays the whole backlog into the journal and looks like a flood of new traffic.
- Return messages **oldest first**, whatever order the API used. Graph returns channel
  listings newest-first; a journal appended in that order replays conversations backwards and
  the agent answers the wrong question.
- Drop what is not conversation: deleted messages, system/join/leave events, messages with no
  human author. Each one you pass through costs an agent turn.

### `Send` — post, and return an id you can point at

Route from `Target.Kind`. Never make the agent choose between "post in the room" and "reply
in the thread": it will choose wrong, the platform will return **200**, and the reply will be
invisible to the person who asked.

A 2xx with no message id is not a success. Fail rather than report a message as sent when you
cannot name the artifact.

### `Identity` — who *we* are

Self-echo suppression compares against this and nothing else. Cache it: identity does not
change mid-process, and re-asking on every poll doubles your request count.

---

## 2. Cursors and delta handling

Poll first. Push (websocket, SSE, webhook) is a latency optimisation that not every tenant or
plan grants you; a poll-based fetch works everywhere and is the one you can actually test.

Three rules that survive every platform:

1. **One cursor per conversation.** Even when the platform's token is global — the Teams
   simulator's delta token is a global write counter — keeping one per conversation is what
   stops you skipping messages.
2. **The cursor is opaque, so make it composite when you must.** Teams needs *two* positions
   per channel: a `$deltatoken` for top-level messages and a per-thread watermark for replies,
   because `…/messages/delta` does not carry threaded replies and `…/replies/delta` does not
   exist (measured: 404). Both go into one base64 JSON string, and the interface did not have
   to change. Version it, and refuse a cursor whose version you do not recognise.
   A Teams *chat* has no delta at all — measured on a live tenant, HTTP 400 "Change tracking
   is not supported against 'microsoft.graph.chatMessage'", while the simulator happily served
   the endpoint — so a chat's position is a `createdDateTime` watermark plus the ids sharing
   that timestamp, and the adapter reads the plain newest-first listing back only until it
   passes the watermark: one request per quiet chat per poll. It keys on `createdDateTime`,
   not `lastModifiedDateTime` (which a reaction also bumps), so an edited message is not
   redelivered. **Test the simulator's endpoints against the real API's documentation** — a
   mock that is more generous than the platform hides exactly this class of bug.
3. **Bound anything that grows.** A per-thread watermark map must be pruned to the threads you
   still scan, or a long-lived listener's cursor grows forever.

**First attach replays history.** A delta endpoint called with no token returns the backlog.
That is faithful to the platform and a foot-gun in production: it wakes an agent for every
message anyone ever sent. Offer a way to start from *now* — `teams` exposes `PrimeCursor`,
which runs the fetch and returns only the cursor. This is a package method, **not** part of
`convo.Channel`; the interface has no vocabulary for "position me at the present", and that is
a real gap, recorded here rather than papered over by widening the interface. The ingest loop
uses it **by default**: a conversation it has no cursor for is primed at now and its past is not
ingested (`--replay-history` opts back into the backlog). Priming is one-shot per conversation —
it happens exactly once, when the cursor is first created — and it has to be the default rather
than a flag because discovery drifts: a capped, moving chat listing keeps surfacing "new"
conversations on later polls, and each one arrives cursorless.

---

## 3. The canonical envelope

Flatten every payload into `convo.Message` ([`../convo/message.go`](../convo/message.go)).
That flattening is the only reason everything above the seam is portable.

| envelope field | what it must hold |
|---|---|
| `id` | the platform's own message id. It is the join key `respond` routes with. |
| `at` | RFC3339, so string comparison sorts correctly |
| `from` | `{id, name}` — the **id** is what suppression and filters compare; the name is display text anyone may be able to choose |
| `text` | always a string, never null. Strip markup, unescape entities |
| `html` | the original rich body, or nil. Stripping loses links, mentions and code blocks |
| `source.kind` | `chat` \| `channel` \| `email` — decides reply routing |
| `source.conversationId` | your composite id, good enough to route from days later |
| `source.threadId` | **a root's thread is itself; a reply's thread is its root.** Carry it now; at reply time the information is gone |
| `replyToId` | the parent message, when the platform has one |
| `mentionsMe` | compare mention ids against `Identity().ID`, never the display name |
| `raw` | the untouched payload. Never discard it — you will need a field you did not anticipate |

---

## 4. Self-echo suppression — the money fire

Drop any message authored by our own identity **inside `Fetch`, before it is returned**, so it
can never reach a journal.

Without it: the agent replies → the next poll fetches that reply → it wakes the agent → it
replies again. On a real tenant, in a real room, until someone notices the bill.

Suppress by **identity id**. Never by content matching, never by remembering the ids you sent
— a restart loses that memory and the loop starts. If the platform lets you post under a
distinct bot identity, do that and keep only human identities inbound.

`teams`' offline test asserts this against a fixture containing one message from a person and
one from us, and expects exactly one message back.

---

## 5. Reply-target routing

`convo.ReplyTarget(msg)` builds the `Target` from the message's own source. Your `Send` then
maps it to an endpoint. For Teams:

```
chat                       -> POST /chats/{id}/messages
channel, with a thread id  -> POST /teams/{t}/channels/{c}/messages/{threadId}/replies
channel, no thread id      -> POST /teams/{t}/channels/{c}/messages          (starts a thread)
```

Refuse a target whose `Kind` disagrees with its conversation id. That disagreement is a
routing bug upstream, and sending anyway is how a reply lands in a stranger's DM.

**Verify the artifact, never the status code.** Replies are threaded: reading the room at top
level shows nothing, which is how a successful run looks like a total failure. The live test
in `teams` reads the reply back out of the thread.

---

## 6. Rate limits and backoff

Two different backoffs, and they belong in different places:

| | what it is | who owns it |
|---|---|---|
| **poll interval** | how often you ask when nothing is happening | the daemon (ARCHITECTURE.md §5.6) |
| **retry backoff** | what you do when the API says *stop* | **the channel** |

Inside the channel: retry `429` and `5xx`, honour `Retry-After` verbatim when the server sends
one, exponential with a ceiling when it does not, and a bounded number of attempts. Ignoring a
throttle does not just fail one request — it gets the whole app's quota cut. `teams` does this
in `graph.go` and asserts it with a fake that answers 429 once.

Do **not** put the idle/active poll tiering in the channel. That is policy about how often to
look, and it belongs above the seam.

---

## 7. What is NOT yours

The daemon and the store already do all of this. A channel that also does it is a channel that
does it *differently*, and now there are two answers:

- journalling, the read cursor, acks and the drain
- coalescing a burst into one agent invocation
- filters — who gets answered
- the poll interval and its adaptive tiering
- heartbeat and liveness
- waking an agent, backpressure, self-delivery refusal (that is the **host**, and it is Herdr)
- any decision that needs a model

Your channel fetches, normalises, suppresses its own echo, and sends. That is the whole job.

---

## 8. Checklist for a new adapter

```
[ ] Config struct: base URL, credentials FROM THE ENVIRONMENT, injectable HTTP client, clock
[ ] New() does no I/O — a constructor that reaches the network cannot be tested
[ ] Conversations: paging followed, kind-prefixed composite ids, re-runnable
[ ] Identity: cached, and a hard error if the platform will not tell you who you are
[ ] Fetch: opaque cursor, versioned; empty = beginning; unreadable = loud failure
[ ] Fetch: oldest-first, self-echo dropped, non-conversation payloads skipped, raw kept
[ ] Threading: threadId set on every message, root = itself
[ ] Send: routes on Kind, refuses a kind/id mismatch, refuses an id-less 2xx
[ ] Retry: 429 + 5xx, Retry-After honoured, attempts bounded
[ ] Path encoding: every id percent-encoded per segment (see the trap below)
[ ] Offline tests: table-driven against recorded fixtures, no server, no network
[ ] Live test: gated behind an env var, skipped by default, verifies the ARTIFACT
[ ] var _ convo.Channel = (*Channel)(nil)
```

**The encoding trap, because it costs everyone a day:** a Teams channel id is
`19:abc@thread.tacv2`. Both `:` and `@` are legal in a URL path segment, so `url.PathEscape`
leaves them alone and every request 404s — a silently deaf listener that looks perfectly
healthy. `teams` has `escSeg` for this, and the offline test asserts the encoding on the wire.
It caught the bug before the live server did.

---

## 9. The next three channels

Fetch/push/threading shapes, so the next implementer sees the outline before starting.
**Verified** means measured in this repo. Everything else is from platform documentation and
must be re-checked against the live API before you rely on it.

| channel | fetch | push | threading | reply target | notes |
|---|---|---|---|---|---|
| **Teams / Graph** ✅ *verified* | `…/messages/delta` + `$deltatoken`, one per conversation | change notifications (needs a public HTTPS endpoint) — *unverified* | channels are 2-level (root + replies); chats are flat | thread replies vs chat messages, different endpoints | delta does **not** carry replies, and `…/replies/delta` is 404. Ids need `%3A`/`%40`. Throttles hard |
| **WhatsApp (BSP)** — *unverified* | typically **no** poll API; the webhook is the only inbound path | webhook, usually mandatory | flat, with an optional quoted-message reference | one conversation per phone number | you will probably need a public callback and a store-and-forward front end, which changes the daemon's shape more than the other two |
| **WhatsApp via `wacli`** ✅ *verified* | `wacli messages list --chat <jid> --after <ts> --asc` against a **local** store | none — the store is filled by a separate `wacli sync` | flat: a message's thread is itself; quoting is `--reply-to <msgId>` | the JID; `@g.us` = group, `@s.whatsapp.net` = 1:1 | no cursor at all, so the adapter invents a timestamp watermark + boundary ids and re-asks one second early (timestamps are second-granular). `--message` is a **flag**. Reads are `wacli --account <label>`, sends go through `apl with whatsapp:<label> --`, so no credential ever reaches the process. **A read is not authoritative**: sync can miss the newest messages |
| **Slack** — *unverified* | `conversations.history` + `cursor`; threads via `conversations.replies` | Events API / Socket Mode — Socket Mode is a genuine push with no public endpoint | 2-level like Teams: `thread_ts` marks a threaded reply | `chat.postMessage` with or without `thread_ts` | `ts` is both the id and the ordering key; bot vs user identity matters for suppression |

The shape to notice: **Teams and Slack are both two-level**, so the composite-cursor pattern in
`teams` ports almost directly. WhatsApp is the one that breaks the poll-first assumption — the
row above is how it was resolved in practice: not by talking to a BSP webhook, but by polling a
local store somebody else syncs, and being honest in the package comment that a quiet fetch
therefore means "nothing new **locally**" rather than "nobody wrote".

The other thing `whatsapp` demonstrates: **the two optional affordances did not become interface
methods.** `PrimeCursor` is a package method on both real adapters, and identity brokering is a
config field, not a fifth method. An interface that grows a method every time one platform needs
something is an interface that stops being portable.
