// adapter.js — THE transport interface. This is the only channel-specific seam
// in the whole system (ARCHITECTURE §5.1). Everything else — journal, cursors,
// coalescing, backoff, heartbeat, dispatch, the agent skill — is portable and
// must never learn what channel it is talking to.
//
// The test of a correct implementation: porting to a new channel touches ONE
// file (an adapter) and nothing else. If a channel change forces a daemon
// change, the seam is in the wrong place.
//
// ---------------------------------------------------------------------------
// A module in this directory is loaded by the daemon and must default-export a
// factory:
//
//     export default function createAdapter(options) -> Adapter
//
// `options` comes from `--adapter-opt key=value` on the CLI (dotted keys nest:
// `--adapter-opt auth.token=…` becomes `{auth: {token: …}}`).
// ---------------------------------------------------------------------------
//
// THE FIVE METHODS
//
//   async listConversations()
//       -> [ { id, kind: 'chat'|'channel', name } ]
//     Every conversation this identity can currently see. Called at startup and
//     re-called periodically: new conversations appear at runtime (someone opens
//     a DM, a bot is added to a channel) and a daemon that only discovers once
//     is permanently deaf to them.
//     `kind` matters: it is what reply routing keys off (ARCHITECTURE §6.3).
//
//   async fetchSince(conversationId, cursor)
//       -> { messages: [raw], nextCursor }
//     `cursor` is OPAQUE to the daemon — a delta token, an ISO timestamp, a
//     message-id watermark, a page token, whatever the channel offers. The
//     daemon only persists it and hands it back. `undefined` means "first ever
//     fetch for this conversation".
//     ALWAYS return a `nextCursor`, even when `messages` is empty; a fetch that
//     forgets to advance its cursor replays history forever.
//     Build POLL first. Push (websocket/SSE/webhook) is a latency optimization
//     that not every tenant or plan grants you; poll works everywhere and is the
//     one you can actually test.
//
//   async send(target, text, opts)
//       -> { id }
//     `target` is `{ conversationId, kind, threadId?, replyToId? }`. The daemon
//     computes it from a message id (see `respond`) so the AGENT never has to
//     choose between "send to chat" and "reply in thread" — it will choose
//     wrong, the platform will return 200, and the message will vanish.
//
//   async identity()
//       -> { id, name }
//     Who WE are. Used for self-echo suppression, which is not optional:
//     without it the agent's own reply wakes the agent, which replies again.
//     An infinite loop that costs real money and spams a real channel.
//
//   normalize(raw, conversation)
//       -> canonical envelope (below)
//     Synchronous. Flattens one channel-native payload into the one shape the
//     rest of the system understands.
//
// Optional:
//   async close()          — release sockets/handles on shutdown.
//   defaults               — object; per-adapter interval defaults, e.g. a local
//                            fake polls fast, a throttling SaaS API does not.
//
// ---------------------------------------------------------------------------
// THE CANONICAL ENVELOPE (ARCHITECTURE §5.2)
//
//   {
//     "id":        "m-1",                       // stable, unique, the join key for respond
//     "at":        "2026-09-09T10:00:00.000Z",  // ISO 8601 UTC
//     "from":      { "id": "u-alice", "name": "alice" },
//     "text":      "plain text, ALWAYS present (never null)",
//     "html":      "<p>original rich body</p>" | null,
//     "source":    { "kind": "chat"|"channel"|"email",
//                    "conversationId": "c-general",
//                    "name": "#general" | "dm",
//                    "threadId": "m-1" | null },
//     "replyToId": null,
//     "mentionsMe": false,
//     "raw":       { ...the untouched original... }
//   }
//
// Two fields carry disproportionate weight:
//   · source.kind + source.conversationId — how a reply is routed. Wrong here
//     means replies go into the void, silently.
//   · raw — NEVER discard the original. You will need a field you didn't
//     anticipate, and by then the message is a week old.
// ---------------------------------------------------------------------------

/** The method names every adapter must provide. */
export const ADAPTER_METHODS = ['listConversations', 'fetchSince', 'send', 'identity', 'normalize'];

/**
 * Fail loudly at load time rather than at 3am on the first inbound message.
 * @param {object} adapter
 * @param {string} label  where it came from, for the error message
 */
export function assertAdapter(adapter, label = 'adapter') {
  if (!adapter || typeof adapter !== 'object') {
    throw new TypeError(`${label}: factory did not return an object`);
  }
  const missing = ADAPTER_METHODS.filter((m) => typeof adapter[m] !== 'function');
  if (missing.length) {
    throw new TypeError(`${label}: missing required method(s): ${missing.join(', ')}`);
  }
  return adapter;
}

/**
 * Envelope helper. Adapters are free to build the object by hand; using this
 * guarantees every field exists with the right default, which matters because
 * consumers in other languages index these keys blindly.
 */
export function envelope({
  id, at, from, text, html = null, source, replyToId = null, mentionsMe = false, raw = null,
}) {
  if (!id) throw new TypeError('envelope: id is required');
  if (!source || !source.conversationId) throw new TypeError('envelope: source.conversationId is required');
  return {
    id: String(id),
    at: at ? new Date(at).toISOString() : new Date().toISOString(),
    from: { id: from?.id != null ? String(from.id) : null, name: from?.name || 'unknown' },
    text: typeof text === 'string' ? text : (text == null ? '' : String(text)),
    html: html ?? null,
    source: {
      kind: source.kind || 'chat',
      conversationId: String(source.conversationId),
      name: source.name || String(source.conversationId),
      threadId: source.threadId ?? null,
    },
    replyToId: replyToId ?? null,
    mentionsMe: Boolean(mentionsMe),
    raw: raw ?? null,
  };
}

/**
 * Reference-only base class. Extending it is optional — a plain object with the
 * five methods is a perfectly good adapter — but it documents the shape and
 * gives you loud "not implemented" errors instead of `undefined is not a
 * function`.
 */
export class Adapter {
  constructor(options = {}) { this.options = options; }
  // eslint-disable-next-line class-methods-use-this
  async listConversations() { throw new Error('listConversations() not implemented'); }
  async fetchSince() { throw new Error('fetchSince() not implemented'); }
  async send() { throw new Error('send() not implemented'); }
  async identity() { throw new Error('identity() not implemented'); }
  normalize() { throw new Error('normalize() not implemented'); }
  async close() { /* optional */ }
}
