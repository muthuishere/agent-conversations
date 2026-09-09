// journal.js — the durable append-only log, plus the THREE separate cursors.
//
// ===========================================================================
// READ THIS BEFORE TOUCHING ANYTHING IN HERE. (ARCHITECTURE §5.4 + §5.5)
//
// There are three distinct positions in this system and conflating any two of
// them is the most-copied bug in this class of software:
//
//   1. FETCH CURSOR   (cursor/<tag>.fetch.json — DAEMON writes, daemon reads)
//      "how far the daemon has pulled from the CHANNEL", one opaque token per
//      conversation, plus the ids it has already journalled (replay guard).
//      Nothing outside the daemon may write this file.
//
//   2. READ CURSOR    (cursor/<tag>.read.json — CONSUMER writes)
//      "how far a consumer has been HANDED messages", a byte offset into the
//      journal. It advances WHENEVER a consumer is handed a message — with or
//      without an ack. This is what gives exactly-once delivery per consumer.
//        · never advanced -> the same message is delivered forever;
//        · advanced before the handoff actually succeeded -> silent loss.
//      So: persist it immediately AFTER a successful write to stdout, never
//      before. A `wait` interrupted mid-flight has NOT advanced it and will
//      redeliver — at-least-once, which is the correct side to fail on.
//
//   3. ACK            (cursor/<tag>.ack.json — CONSUMER writes)
//      "the consumer says it has PROCESSED this". Explicit, and NOT what stops
//      redelivery — the read cursor does that unconditionally. Ack exists so a
//      non-blocking look-around (`journal --new`) can show what nobody has
//      finished with yet.
//
// They live in three separate FILES rather than three keys of one file for two
// reasons: it makes the distinction impossible to miss, and it means the daemon
// and the consumer never clobber each other's writes.
//
// And the rule that makes the whole architecture work:
//   READING IS NOT CONSUMING. `journal` (the look-around command) must never
//   advance anything. Only `wait` advances the read cursor. The log is
//   append-only and is never truncated or rewritten by a read — it is the audit
//   record of who asked for what (ARCHITECTURE §8.5).
// ===========================================================================
import fs from 'node:fs';
import { paths, ensureHome, readJson, writeJsonAtomic } from './config.js';

const SEEN_CAP = 5000;   // bounded replay guard; the journal itself is the truth
const ACK_CAP = 5000;

// ---- 1. fetch cursor (daemon-owned) --------------------------------------
const EMPTY_FETCH = { tokens: {}, seenIds: [], updatedAt: null };

export function readFetchCursor(tag) {
  return { ...structuredClone(EMPTY_FETCH), ...(readJson(paths.fetchCursor(tag), null) || {}) };
}

export function writeFetchCursor(tag, cur) {
  writeJsonAtomic(paths.fetchCursor(tag), { ...cur, updatedAt: new Date().toISOString() });
}

// ---- 2. read cursor (consumer-owned) -------------------------------------
export function readReadCursor(tag) {
  const c = readJson(paths.readCursor(tag), null) || {};
  return { readOffset: Number(c.readOffset) || 0, updatedAt: c.updatedAt || null };
}

export function writeReadCursor(tag, readOffset) {
  writeJsonAtomic(paths.readCursor(tag), { readOffset, updatedAt: new Date().toISOString() });
}

// ---- 3. ack (consumer-owned) ---------------------------------------------
export function readAcked(tag) {
  const a = readJson(paths.ackFile(tag), null) || {};
  return Array.isArray(a.ackedIds) ? a.ackedIds : [];
}

export function ack(tag, ids) {
  const acked = readAcked(tag);
  const set = new Set(acked);
  for (const id of ids) set.add(String(id));
  let out = [...set];
  if (out.length > ACK_CAP) out = out.slice(-ACK_CAP);
  writeJsonAtomic(paths.ackFile(tag), { ackedIds: out, updatedAt: new Date().toISOString() });
  return out.length;
}

// ---- the journal ----------------------------------------------------------

/**
 * Append one envelope, before any consumer can see it, so a crash between
 * "received" and "delivered" loses nothing.
 *
 * Returns true if written, false if it was a duplicate.
 *
 * Dedupe is by ID ONLY, never by a timestamp watermark: conversations are
 * polled concurrently, so a perfectly valid message from a quiet DM routinely
 * lands after a newer message from a busy channel. Ordering by clock across
 * conversations is a lie; ids are not.
 */
export function append(tag, env, state = null) {
  ensureHome();
  const cur = state || readFetchCursor(tag);
  if (cur.seenIds.includes(env.id)) return false;

  // O_APPEND + a single write of one line: concurrent appends interleave
  // between lines, never inside one, so a reader never sees a torn record.
  const fd = fs.openSync(paths.journal(tag), 'a');
  try { fs.writeSync(fd, JSON.stringify(env) + '\n'); } finally { fs.closeSync(fd); }

  cur.seenIds.push(env.id);
  if (cur.seenIds.length > SEEN_CAP) cur.seenIds = cur.seenIds.slice(-SEEN_CAP);
  if (!state) writeFetchCursor(tag, cur);
  return true;
}

/** Mark ids seen without delivering them — used to prime a newly-discovered conversation. */
export function markSeen(tag, ids) {
  if (!ids.length) return;
  const cur = readFetchCursor(tag);
  for (const id of ids) if (!cur.seenIds.includes(id)) cur.seenIds.push(id);
  if (cur.seenIds.length > SEEN_CAP) cur.seenIds = cur.seenIds.slice(-SEEN_CAP);
  writeFetchCursor(tag, cur);
}

export function size(tag) {
  try { return fs.statSync(paths.journal(tag)).size; } catch { return 0; }
}

/** Whole journal, oldest first. A look, never a consume. */
export function readAll(tag) {
  let txt = '';
  try { txt = fs.readFileSync(paths.journal(tag), 'utf8'); } catch { return []; }
  const out = [];
  for (const line of txt.split('\n')) {
    if (!line.trim()) continue;
    try { out.push(JSON.parse(line)); } catch { /* skip a torn tail line */ }
  }
  return out;
}

export function findById(tag, id) {
  const all = readAll(tag);
  for (let i = all.length - 1; i >= 0; i--) if (all[i].id === String(id)) return all[i];
  return null;
}

/**
 * Read complete lines from a byte offset, returning for each the exact offset
 * IMMEDIATELY AFTER it.
 *
 * Why byte offsets and not "message N": the consumer must be able to advance
 * its cursor to precisely the last message it actually looked at — never past
 * one it stopped short of because --count was already satisfied. Anything
 * coarser silently drops messages at a batch boundary.
 *
 * A partial final line (the daemon is mid-write) is left unread; it will be
 * complete on the next pass.
 */
export function readLinesFrom(tag, offset) {
  let stat;
  try { stat = fs.statSync(paths.journal(tag)); } catch { return { lines: [], offset: 0 }; }
  if (stat.size <= offset) return { lines: [], offset: Math.min(offset, stat.size) };

  const fd = fs.openSync(paths.journal(tag), 'r');
  try {
    const len = stat.size - offset;
    const buf = Buffer.alloc(len);
    fs.readSync(fd, buf, 0, len, offset);
    const txt = buf.toString('utf8');
    const lastNl = txt.lastIndexOf('\n');
    if (lastNl === -1) return { lines: [], offset };   // no complete line yet

    const lines = [];
    let pos = offset;
    for (const raw of txt.slice(0, lastNl + 1).split('\n')) {
      if (raw === '') continue;                        // artifact after the final \n
      pos += Buffer.byteLength(raw, 'utf8') + 1;       // +1 for the newline
      if (!raw.trim()) continue;
      let env = null;
      try { env = JSON.parse(raw); } catch { /* offset still advances past a bad line */ }
      if (env) lines.push({ env, offset: pos });
    }
    return { lines, offset: pos };
  } finally {
    fs.closeSync(fd);
  }
}
