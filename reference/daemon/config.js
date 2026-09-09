// config.js — runtime layout, intervals, heartbeat.
//
// EVERY path derives from one env var so a test (or the example in examples/)
// can run fully hermetically:
//
//   AGENT_CONVERSATIONS_HOME   default ~/.config/agent-conversations
//
// The file layout IS an interface — other languages read it. See INTERFACES.md §2.
// Nothing here knows what a "channel" is; that is the adapter's job alone.
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';

/** A CLI error that carries the process exit code it should produce. */
export class CliError extends Error {
  constructor(code, message, exit = 1) {
    super(message);
    this.code = code;   // stable machine-readable string, e.g. 'listenerDead'
    this.exit = exit;   // process exit code — see INTERFACES.md §1
  }
}

export const EXIT = {
  OK: 0,
  ERROR: 1,
  TIMEOUT: 64,        // `wait` aged out with nothing to deliver. NOT an error.
  NOT_CONFIGURED: 65, // bad args / no adapter / unknown message id
  CONSUMER_CONFLICT: 66, // another consumer already holds this tag
  LISTENER_DEAD: 69,  // daemon expected but dead/stale, or transport unreachable
};

export function home() {
  return process.env.AGENT_CONVERSATIONS_HOME
    || path.join(os.homedir(), '.config', 'agent-conversations');
}

export const paths = {
  home,
  journalDir: () => path.join(home(), 'journal'),
  cursorDir: () => path.join(home(), 'cursor'),

  // Append-only durable log. Daemon writes, everyone reads. See INTERFACES.md §2.1
  journal: (tag) => path.join(home(), 'journal', `${tag}.ndjson`),

  // THREE separate cursor files, on purpose (ARCHITECTURE §5.5).
  // Fusing them is the single most-copied bug in this class of system, and
  // keeping them in one file also means two processes clobbering each other.
  fetchCursor: (tag) => path.join(home(), 'cursor', `${tag}.fetch.json`), // daemon-owned
  readCursor: (tag) => path.join(home(), 'cursor', `${tag}.read.json`),   // consumer-owned
  ackFile: (tag) => path.join(home(), 'cursor', `${tag}.ack.json`),       // consumer-owned

  heartbeat: (tag) => path.join(home(), `heartbeat.${tag}.json`),
  listenerPid: (tag) => path.join(home(), `listener.${tag}.pid`),
  listenerLog: (tag) => path.join(home(), `listener.${tag}.log`),
  consumerPid: (tag) => path.join(home(), `consumer.${tag}.pid`),
};

export function ensureHome() {
  fs.mkdirSync(paths.journalDir(), { recursive: true });
  fs.mkdirSync(paths.cursorDir(), { recursive: true });
}

/** Atomic small-JSON write: tmp file namespaced by pid, then rename. */
export function writeJsonAtomic(file, value) {
  ensureHome();
  const tmp = `${file}.${process.pid}.tmp`;
  try {
    fs.writeFileSync(tmp, JSON.stringify(value));
    fs.renameSync(tmp, file);
  } catch { /* an unwritable state file must read back as absent, which is correct */ }
}

export function readJson(file, fallback = null) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); } catch { return fallback; }
}

// ---- adaptive backoff (ARCHITECTURE §5.6) -------------------------------
//
//   traffic on the last poll        -> activeMs
//   quiet longer than idle1         -> midMs
//   quiet longer than idle2         -> idleMs
//   any message anywhere            -> SNAP straight back to activeMs
//
// Known trade, stated out loud: at the idle tier you cannot notice traffic
// sooner than `idleMs`. The first message after a long silence pays full idle
// latency — exactly when a human opens a conversation. Lower `--poll-idle`
// while a consumer is attached if that matters.
export const INTERVAL_DEFAULTS = {
  activeSeconds: 15,
  midSeconds: 60,
  idleSeconds: 300,
  idle1Seconds: 120,  // quiet for this long -> mid tier
  idle2Seconds: 600,  // quiet for this long -> idle tier
};

function secs(v) {
  if (v == null || v === true || v === false || v === '') return null;
  const n = Number(v);
  return Number.isFinite(n) && n > 0 ? n : null;
}

export function resolveIntervals(flags = {}, defaults = {}) {
  const d = { ...INTERVAL_DEFAULTS, ...defaults };
  const activeMs = Math.max(50, (secs(flags['poll-active']) ?? d.activeSeconds) * 1000);
  const idleMs = Math.max(activeMs, (secs(flags['poll-idle']) ?? d.idleSeconds) * 1000);
  const midMs = Math.max(activeMs, Math.min((secs(flags['poll-mid']) ?? d.midSeconds) * 1000, idleMs));
  return {
    activeMs,
    midMs,
    idleMs,
    idle1Ms: (secs(flags['idle-1']) ?? d.idle1Seconds) * 1000,
    idle2Ms: (secs(flags['idle-2']) ?? d.idle2Seconds) * 1000,
  };
}

// ---- heartbeat (ARCHITECTURE §5.8) --------------------------------------
//
// The defining failure of this architecture is SILENT DEAFNESS: the listener
// dies, the agent believes it is live, and nobody finds out until someone asks
// why they were ignored. The heartbeat is the only cure. Never report
// "listening" from memory — read this file.
export function writeHeartbeat(tag, hb) {
  writeJsonAtomic(paths.heartbeat(tag), hb);
}

export function readHeartbeat(tag) {
  return readJson(paths.heartbeat(tag), null);
}

/** Stale once older than 3x the interval the daemon said it was polling at. */
export function heartbeatHealth(tag, { pidAlive = null } = {}) {
  const hb = readHeartbeat(tag);
  if (!hb || !hb.ts) {
    return { state: 'none', ageMs: null, maxAgeMs: null, intervalMs: null, pollCount: null, lastMessageAt: null, startedAt: null };
  }
  const intervalMs = Number(hb.intervalMs) || 0;
  const maxAgeMs = Math.max(5000, intervalMs * 3);
  const ageMs = Date.now() - Date.parse(hb.ts);
  let state = ageMs > maxAgeMs ? 'stale' : 'fresh';
  if (pidAlive === false) state = 'stale';
  return {
    state,
    ageMs,
    maxAgeMs,
    intervalMs,
    pollCount: hb.pollCount ?? null,
    lastMessageAt: hb.lastMessageAt ?? null,
    startedAt: hb.startedAt ?? null,
    conversations: hb.conversations ?? null,
    ts: hb.ts,
  };
}
