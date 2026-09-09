// memory-adapter.js — a fake channel with no network at all.
//
// This is what makes the repo immediately useful: the entire daemon, journal,
// coalescing, backoff, heartbeat, lock and consumer path can be exercised
// end-to-end offline, with no account, no token and no npm install.
//
// It is also the reference for what a MINIMAL correct adapter looks like:
// five methods, an opaque cursor, and an envelope.
//
// Two storage modes:
//   · in-process (default) — one module-level store; fine when the daemon and
//     the injector are the same process.
//   · file-backed (`--adapter-opt store=/path/store.json`) — several processes
//     share one fake channel, which is what the example does (a detached daemon
//     plus a separate injector).
//
// The file store is deliberately dumb: read-modify-write JSON with an exclusive
// create lock. It is a TEST DOUBLE, not a datastore — do not copy this bit into
// anything real.
import fs from 'node:fs';
import path from 'node:path';
import { Adapter, envelope } from './adapter.js';

const DEFAULT_CONVERSATIONS = [
  { id: 'c-general', kind: 'channel', name: '#general' },
  { id: 'c-dm-alice', kind: 'chat', name: 'dm:alice' },
];

function emptyStore(conversations) {
  return { seq: 0, conversations: conversations.map((c) => ({ ...c })), messages: [] };
}

// ---- in-process store ------------------------------------------------------
let inProcess = null;

// ---- file store ------------------------------------------------------------
function withLock(file, fn) {
  const lock = `${file}.lock`;
  fs.mkdirSync(path.dirname(file), { recursive: true });
  for (let i = 0; i < 200; i++) {
    let fd;
    try {
      fd = fs.openSync(lock, 'wx');       // exclusive create == our mutex
    } catch {
      // busy — spin briefly. Synchronous on purpose: callers are sync.
      const until = Date.now() + 5;
      while (Date.now() < until) { /* spin */ }
      continue;
    }
    try { return fn(); } finally {
      fs.closeSync(fd);
      try { fs.unlinkSync(lock); } catch { /* ignore */ }
    }
  }
  throw new Error(`memory-adapter: could not lock ${lock}`);
}

function makeStore({ file, conversations }) {
  if (!file) {
    if (!inProcess) inProcess = emptyStore(conversations);
    return {
      read: () => inProcess,
      mutate: (fn) => fn(inProcess),
    };
  }
  const read = () => {
    try { return JSON.parse(fs.readFileSync(file, 'utf8')); } catch { return emptyStore(conversations); }
  };
  return {
    read,
    mutate: (fn) => withLock(file, () => {
      const s = read();
      const out = fn(s);
      fs.writeFileSync(file, JSON.stringify(s));
      return out;
    }),
  };
}

/**
 * Post a message into the fake channel, as anyone.
 * Exported so tests and the example can inject traffic without going near the
 * adapter instance the daemon is holding.
 */
export function post(store, { conversationId, from, text, at, html = null, mentionsMe = false }) {
  return store.mutate((s) => {
    s.seq += 1;
    const msg = {
      id: `m-${s.seq}`,
      seq: s.seq,
      conversationId,
      at: at || new Date().toISOString(),
      from: { id: from?.id || `u-${(from?.name || 'unknown').toLowerCase()}`, name: from?.name || 'unknown' },
      text,
      html,
      mentionsMe,
    };
    s.messages.push(msg);
    return msg;
  });
}

class MemoryAdapter extends Adapter {
  constructor(options = {}) {
    super(options);
    this.self = {
      id: options.selfId || 'u-agent',
      name: options.selfName || 'agent',
    };
    this.store = makeStore({
      file: options.store || null,
      conversations: options.conversations || DEFAULT_CONVERSATIONS,
    });
    // A fake channel has no rate limit, so poll fast — it keeps the example
    // snappy without pretending a real API would tolerate this.
    this.defaults = { activeSeconds: 0.2, midSeconds: 1, idleSeconds: 2, idle1Seconds: 5, idle2Seconds: 20 };
  }

  async identity() { return { ...this.self }; }

  async listConversations() {
    return this.store.read().conversations.map((c) => ({ id: c.id, kind: c.kind, name: c.name }));
  }

  /**
   * The cursor is an opaque string as far as the daemon is concerned; here it
   * happens to be a monotonic sequence watermark. Note that a nextCursor is
   * returned even when nothing matched — a fetch that only advances its cursor
   * when it finds something replays the same page forever.
   */
  async fetchSince(conversationId, cursor) {
    const s = this.store.read();
    const since = cursor == null ? 0 : Number(cursor) || 0;
    let high = since;
    const messages = [];
    for (const m of s.messages) {
      if (m.conversationId !== conversationId) continue;
      if (m.seq <= since) continue;
      messages.push(m);
      if (m.seq > high) high = m.seq;
    }
    // Even with no matches, hand back the store's high-water mark so a brand
    // new conversation does not re-scan the whole log on every poll.
    return { messages, nextCursor: String(Math.max(high, since)) };
  }

  async send(target, text) {
    const msg = post(this.store, {
      conversationId: target.conversationId,
      from: this.self,
      text,
    });
    return { id: msg.id };
  }

  normalize(raw, conversation) {
    return envelope({
      id: raw.id,
      at: raw.at,
      from: raw.from,
      text: raw.text,
      html: raw.html,
      source: {
        kind: conversation.kind,
        conversationId: conversation.id,
        name: conversation.name,
        threadId: raw.threadId ?? raw.id,
      },
      replyToId: raw.replyToId ?? null,
      mentionsMe: Boolean(raw.mentionsMe),
      raw,
    });
  }
}

export default function createAdapter(options = {}) {
  return new MemoryAdapter(options);
}

/** Convenience for scripts: build a bare store handle without an adapter. */
export function openStore(file, conversations = DEFAULT_CONVERSATIONS) {
  return makeStore({ file, conversations });
}
