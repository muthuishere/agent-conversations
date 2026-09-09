// http-adapter.js — a generic REST/poll adapter.
//
// It talks to a plain JSON HTTP API with no SDK and no npm dependency (Node's
// global fetch only), and shows the ONE pattern that matters: the delta-token /
// `since` cursor. Every polled channel — Slack `conversations.history`,
// Microsoft Graph `messages/delta`, IMAP `UID SEARCH SINCE`, a Telegram
// `getUpdates` offset, your own inbox table — is this shape with different
// field names.
//
// It will not work against your channel unmodified. That is deliberate: the
// TODO markers are where a real channel's specifics go, and there are only
// four of them.
//
// Configure with `--adapter-opt`:
//   baseUrl=https://api.example.test    (required)
//   token=...        bearer token; PREFER tokenEnv= so a secret never lands in argv
//   tokenEnv=MY_API_TOKEN
//   selfId=... selfName=...             skips the identity round-trip
//   timeoutMs=10000
//   userAgent=agent-conversations/1
import { Adapter, envelope } from './adapter.js';

class HttpAdapter extends Adapter {
  constructor(options = {}) {
    super(options);
    if (!options.baseUrl) throw new Error('http-adapter: --adapter-opt baseUrl=<url> is required');
    this.baseUrl = String(options.baseUrl).replace(/\/+$/, '');
    // Read the secret from the environment by name. A token passed as
    // `--adapter-opt token=…` is visible in `ps` to every user on the box.
    this.token = options.tokenEnv ? process.env[options.tokenEnv] : (options.token || null);
    this.timeoutMs = Number(options.timeoutMs) || 10000;
    this.userAgent = options.userAgent || 'agent-conversations-reference/1';
    this._self = (options.selfId || options.selfName)
      ? { id: options.selfId || null, name: options.selfName || 'agent' }
      : null;
    // Real APIs throttle. Start conservative; the daemon's adaptive backoff
    // (ARCHITECTURE §5.6) climbs from here when the room is quiet.
    this.defaults = { activeSeconds: 15, midSeconds: 60, idleSeconds: 300 };
  }

  async request(pathOrUrl, { method = 'GET', body = null, query = null } = {}) {
    const url = new URL(pathOrUrl.startsWith('http') ? pathOrUrl : this.baseUrl + pathOrUrl);
    if (query) for (const [k, v] of Object.entries(query)) if (v != null) url.searchParams.set(k, String(v));

    const ac = new AbortController();
    const timer = setTimeout(() => ac.abort(), this.timeoutMs);
    try {
      const res = await fetch(url, {
        method,
        signal: ac.signal,
        headers: {
          accept: 'application/json',
          'user-agent': this.userAgent,
          ...(this.token ? { authorization: `Bearer ${this.token}` } : {}),
          ...(body ? { 'content-type': 'application/json' } : {}),
        },
        body: body ? JSON.stringify(body) : undefined,
      });
      // TODO(channel): honour the API's rate-limit signal. Almost every real one
      // answers 429 with Retry-After; throwing here lets the daemon log the
      // failure and retry on its own schedule, which is usually good enough,
      // but a busy tenant will want the header respected explicitly.
      if (res.status === 429) {
        throw new Error(`rate limited (retry-after: ${res.headers.get('retry-after') || '?'})`);
      }
      if (!res.ok) throw new Error(`HTTP ${res.status} ${method} ${url.pathname}`);
      if (res.status === 204) return null;
      return await res.json();
    } finally {
      clearTimeout(timer);
    }
  }

  async identity() {
    if (this._self) return this._self;
    // TODO(channel): the "who am I" endpoint. Graph: /me. Slack: auth.test.
    const me = await this.request('/me');
    this._self = { id: String(me.id), name: me.name || me.displayName || 'agent' };
    return this._self;
  }

  async listConversations() {
    // TODO(channel): the conversation-listing endpoint(s), plus pagination.
    // Many channels need TWO calls (channels and DMs) merged into one list.
    const data = await this.request('/conversations');
    const items = Array.isArray(data) ? data : (data.value || data.items || []);
    return items.map((c) => ({
      id: String(c.id),
      kind: c.kind || (c.isChannel ? 'channel' : 'chat'),
      name: c.name || c.displayName || String(c.id),
    }));
  }

  /**
   * THE PATTERN. `cursor` is opaque to the daemon: whatever the API gave us
   * last time, handed straight back. Three common flavours, all the same shape:
   *
   *   delta token   ?deltatoken=<opaque>      -> next token in the response body
   *   timestamp     ?since=<iso8601>          -> max(at) of what you received
   *   id watermark  ?after=<messageId>        -> id of the last message
   *
   * Two rules that are not optional:
   *  1. ALWAYS return a nextCursor, even when `messages` is empty. Advancing
   *     only on success replays history forever the moment a page comes back
   *     empty for another reason.
   *  2. Drain pagination HERE. The daemon calls fetchSince once per poll and
   *     assumes it got everything up to nextCursor. Half a page silently
   *     becomes lost messages.
   */
  async fetchSince(conversationId, cursor) {
    const messages = [];
    // TODO(channel): the delta/history endpoint + its cursor parameter name.
    let next = cursor ? { since: cursor } : {};
    let url = `/conversations/${encodeURIComponent(conversationId)}/messages`;
    let nextCursor = cursor ?? null;

    for (let page = 0; page < 20; page++) {           // bound it: never loop forever
      const data = await this.request(url, { query: next });
      const batch = Array.isArray(data) ? data : (data.value || data.messages || []);
      messages.push(...batch);
      // TODO(channel): the API's own names for these.
      const nextLink = data && (data.nextLink || data['@odata.nextLink']);
      const delta = data && (data.deltaToken || data['@odata.deltaLink'] || data.cursor);
      if (delta) nextCursor = String(delta);
      if (!nextLink) break;
      url = nextLink;
      next = null;
    }

    // Fallback when the API has no token of its own: use the newest timestamp
    // we actually saw as the watermark.
    if (nextCursor === (cursor ?? null) && messages.length) {
      const newest = messages.reduce((a, m) => {
        const t = m.createdAt || m.createdDateTime || m.ts;
        return (!a || String(t) > a) ? String(t) : a;
      }, null);
      if (newest) nextCursor = newest;
    }
    return { messages, nextCursor };
  }

  async send(target, text, opts = {}) {
    // Reply routing is decided by the DAEMON from source.kind and handed to us
    // in `target` — the agent never picks an endpoint (ARCHITECTURE §6.3).
    // TODO(channel): a threaded reply is often a different endpoint from a new
    // message. Get this wrong and the platform returns 200 while the reply
    // vanishes; that failure mode is why `target` carries kind + threadId.
    const body = { text };
    if (target.threadId) body.threadId = target.threadId;
    if (target.replyToId) body.replyToId = target.replyToId;
    if (opts.html) body.html = opts.html;
    const res = await this.request(
      `/conversations/${encodeURIComponent(target.conversationId)}/messages`,
      { method: 'POST', body },
    );
    return { id: String(res?.id ?? '') };
  }

  normalize(raw, conversation) {
    // TODO(channel): map the payload's real field names. Keep `raw` intact —
    // you WILL need a field you did not anticipate.
    const body = raw.body || {};
    const text = raw.text
      ?? body.content_plain
      ?? (typeof body.content === 'string' ? body.content.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim() : '');
    return envelope({
      id: raw.id,
      at: raw.createdAt || raw.createdDateTime || raw.ts,
      from: {
        id: raw.from?.id ?? raw.user?.id ?? raw.userId,
        name: raw.from?.name ?? raw.user?.name ?? raw.from?.displayName ?? 'unknown',
      },
      text,
      html: body.contentType === 'html' ? body.content : (raw.html ?? null),
      source: {
        kind: conversation.kind,
        conversationId: conversation.id,
        name: conversation.name,
        threadId: raw.threadId ?? raw.thread_ts ?? raw.id,
      },
      replyToId: raw.replyToId ?? raw.inReplyTo ?? null,
      mentionsMe: Boolean(raw.mentionsMe),
      raw,
    });
  }
}

export default function createAdapter(options = {}) {
  return new HttpAdapter(options);
}
