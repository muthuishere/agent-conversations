// daemon.js — the receive engine.
//
// It is INFRASTRUCTURE: no model, no judgement, no product policy. If something
// in here would need an LLM to decide, it belongs in the agent instead
// (ARCHITECTURE §7). The daemon's whole job, in order:
//
//   discover -> fetch -> normalize -> suppress self -> APPEND TO JOURNAL
//            -> filter -> coalesce -> dispatch          + heartbeat every cycle
//
// The journal write comes before every consumer, every filter and every
// handler. That is what makes "a message that arrives while the agent is
// thinking" a non-event instead of a lost message.
import fs from 'node:fs';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import {
  paths, ensureHome, CliError, EXIT, resolveIntervals, writeHeartbeat, heartbeatHealth,
} from './config.js';
import * as journal from './journal.js';
import { buildFilters, matches as filterMatches } from './filters.js';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const FETCH_CONCURRENCY = 8;

// A conversation seen for the first time must not dump its entire history into
// the journal — but it must also not swallow the message that made us notice it
// (someone opens a brand new DM and says "hi"). So on first sight we deliver
// only what was created after this daemon started, minus a small grace for
// clock skew between us and the channel.
const FIRST_SIGHT_GRACE_MS = 1000;

// ---- process liveness -----------------------------------------------------
export function readPid(file) {
  try {
    const raw = fs.readFileSync(file, 'utf8').trim();
    const n = Number(raw);
    return Number.isFinite(n) && n > 0 ? n : null;
  } catch { return null; }
}

export function alive(pid) {
  if (!pid) return false;
  // signal 0 == "does this pid exist"; EPERM means it exists but isn't ours.
  try { process.kill(pid, 0); return true; } catch (e) { return e.code === 'EPERM'; }
}

/**
 * The health answer every consumer must consult before it blocks.
 *
 * "expected" = a pid file exists, i.e. someone started a daemon for this tag.
 * If a daemon is expected but dead — or alive but not heartbeating — silence is
 * NOT "no messages", it is deafness, and a consumer must say so rather than
 * wait forever on a corpse (ARCHITECTURE §5.8, §9).
 */
export function listenerHealth(tag) {
  const pidFile = paths.listenerPid(tag);
  const expected = fs.existsSync(pidFile);
  const pid = readPid(pidFile);
  const running = Boolean(pid && alive(pid));
  const heartbeat = heartbeatHealth(tag, { pidAlive: expected ? running : null });

  let reason = null;
  if (expected && !running) reason = `daemon pid ${pid || '?'} is no longer alive`;
  else if (expected && heartbeat.state === 'none') reason = 'daemon is running but has never written a heartbeat';
  else if (expected && heartbeat.state === 'stale') {
    reason = `heartbeat is ${heartbeat.ageMs}ms old, past the ${heartbeat.maxAgeMs}ms limit (3x its poll interval)`;
  }
  return { tag, expected, running, pid: running ? pid : null, heartbeat, healthy: expected ? !reason : true, reason };
}

// ---- bounded concurrency --------------------------------------------------
async function mapLimit(items, limit, fn) {
  const out = new Array(items.length);
  let next = 0;
  const n = Math.max(1, Math.min(limit, items.length));
  await Promise.all(Array.from({ length: n }, async () => {
    for (;;) {
      const i = next++;
      if (i >= items.length) return;
      out[i] = await fn(items[i]);
    }
  }));
  return out;
}

/**
 * makeSource — discovery + polling + normalization + self-echo suppression.
 *
 * Efficiency rules it enforces:
 *   · conversations are polled concurrently, bounded to FETCH_CONCURRENCY;
 *   · each conversation carries its own `nextDueAt`, so a quiet one is not
 *     re-fetched sooner than the current adaptive interval;
 *   · a conversation that DID return something drops back to the active
 *     interval, and traffic ANYWHERE snaps the whole schedule to active.
 */
export function makeSource(adapter, self, {
  tag = 'default', intervals, onMessage, onLog = () => {}, onPoll = () => {},
} = {}) {
  const iv = intervals;
  const startedAt = Date.now();
  const horizonIso = new Date(startedAt - FIRST_SIGHT_GRACE_MS).toISOString();

  let stopped = false;
  let wake = null;
  const targets = new Map();                       // conversationId -> {conv, nextDueAt}
  const fetchState = journal.readFetchCursor(tag); // {tokens, seenIds}
  let lastMessageAt = null;
  let pollCount = 0;
  let lastDiscoverAt = 0;

  function currentIntervalMs() {
    const quiet = Date.now() - (lastMessageAt || startedAt);
    if (quiet >= iv.idle2Ms) return iv.idleMs;
    if (quiet >= iv.idle1Ms) return iv.midMs;
    return iv.activeMs;
  }

  // Discovery must keep up with conversations that appear at runtime without
  // becoming the thing that hammers the API. Track the poll cadence, cap at 60s.
  const discoverEveryMs = () => Math.max(1000, Math.min(60000, currentIntervalMs() * 2));

  async function refreshTargets() {
    lastDiscoverAt = Date.now();
    let found;
    try { found = await adapter.listConversations(); } catch (e) { onLog(`discover failed: ${e.message}`); return; }
    const seen = new Set();
    for (const conv of found || []) {
      if (!conv || !conv.id) continue;
      seen.add(conv.id);
      const existing = targets.get(conv.id);
      if (existing) { existing.conv = conv; continue; }
      onLog(`discovered ${conv.kind} ${conv.name || conv.id}`);
      targets.set(conv.id, { conv, nextDueAt: 0 });
    }
    for (const id of [...targets.keys()]) if (!seen.has(id)) targets.delete(id);
  }

  async function fetchTarget(t) {
    const known = Object.prototype.hasOwnProperty.call(fetchState.tokens, t.conv.id);
    let r;
    try {
      r = await adapter.fetchSince(t.conv.id, known ? fetchState.tokens[t.conv.id] : undefined);
    } catch (e) {
      // A transient channel error must never kill the daemon. Log, keep the
      // old cursor, try again next tick.
      onLog(`fetch ${t.conv.name || t.conv.id}: ${e.message}`);
      return { t, envelopes: [] };
    }
    fetchState.tokens[t.conv.id] = r?.nextCursor ?? fetchState.tokens[t.conv.id] ?? null;

    const envelopes = [];
    const primed = [];
    for (const raw of r?.messages || []) {
      let env;
      try { env = adapter.normalize(raw, t.conv); } catch (e) { onLog(`normalize: ${e.message}`); continue; }
      if (!known && !(env.at > horizonIso)) { primed.push(env.id); continue; } // first-sight backfill
      envelopes.push(env);
    }
    if (primed.length) journal.markSeen(tag, primed);
    return { t, envelopes };
  }

  async function pollDue() {
    const now = Date.now();
    const due = [...targets.values()].filter((t) => (t.nextDueAt || 0) <= now);
    if (!due.length) return 0;

    const interval = currentIntervalMs();
    const results = await mapLimit(due, FETCH_CONCURRENCY, fetchTarget);

    const batch = [];
    for (const r of results) {
      if (!r) continue;
      r.t.nextDueAt = Date.now() + (r.envelopes.length ? iv.activeMs : interval);
      batch.push(...r.envelopes);
    }
    pollCount++;

    let emitted = 0;
    if (batch.length) {
      batch.sort((a, b) => String(a.at).localeCompare(String(b.at)));
      lastMessageAt = Date.now();
      // SNAP BACK: any traffic anywhere pulls the whole schedule to active.
      const snapTo = Date.now() + iv.activeMs;
      for (const t of targets.values()) t.nextDueAt = Math.min(t.nextDueAt || snapTo, snapTo);

      for (const env of batch) {
        // ---- SELF-ECHO SUPPRESSION (ARCHITECTURE §5.3) ------------------
        // Upstream of the journal, upstream of every filter, by IDENTITY —
        // never by content matching. Without this, the agent's own reply is
        // fetched back, wakes the agent, which replies again: an infinite loop
        // that costs real money and spams a real channel.
        //
        // The subtle trap: if a spawned handler is configured with a different
        // identity than this daemon, it replies as someone else, this check
        // does not recognise it, and the loop reappears. The daemon therefore
        // exports its identity to every handler (see CONVO_SELF_* below) —
        // pin the handler to it.
        if (isSelf(env, self)) continue;
        emitted++;
        onMessage(env);
      }
    }
    journal.writeFetchCursor(tag, fetchState);
    return emitted;
  }

  // A nap any caller can cut short, so "sweep now" is possible without waiting
  // out a 5-minute idle interval.
  function napUntilNudged(ms) {
    return new Promise((resolve) => {
      const t = setTimeout(() => { wake = null; resolve(); }, ms);
      wake = () => { clearTimeout(t); wake = null; resolve(); };
    });
  }

  async function run() {
    await refreshTargets();
    while (!stopped) {
      try { await pollDue(); } catch (e) { onLog(`poll: ${e.message}`); }
      if (stopped) return;
      const intervalMs = currentIntervalMs();
      onPoll({
        pollCount,
        intervalMs,
        lastMessageAt: lastMessageAt ? new Date(lastMessageAt).toISOString() : null,
        conversations: targets.size,
      });
      if (Date.now() - lastDiscoverAt >= discoverEveryMs()) {
        try { await refreshTargets(); } catch (e) { onLog(`discover: ${e.message}`); }
      }
      if (stopped) return;
      // Sleep in slices of at most 2s even on a 300s idle interval, so shutdown
      // stays responsive AND the heartbeat keeps proving liveness. A daemon that
      // only heartbeats once per idle period looks dead for most of it.
      let nextDue = Infinity;
      for (const t of targets.values()) nextDue = Math.min(nextDue, t.nextDueAt || 0);
      const until = Number.isFinite(nextDue) ? nextDue - Date.now() : intervalMs;
      await napUntilNudged(Math.max(25, Math.min(2000, until)));
    }
  }

  return {
    stats: () => ({
      pollCount,
      intervalMs: currentIntervalMs(),
      lastMessageAt: lastMessageAt ? new Date(lastMessageAt).toISOString() : null,
      conversations: targets.size,
    }),
    start: () => run(),
    nudge() { for (const t of targets.values()) t.nextDueAt = 0; if (wake) wake(); },
    stop() { stopped = true; if (wake) wake(); },
  };
}

/** Is this message ours? Identity, not content. */
export function isSelf(env, self) {
  if (!self) return false;
  if (self.id && env.from?.id && String(env.from.id) === String(self.id)) return true;
  if (self.name && env.from?.name && String(env.from.name) === String(self.name)) return true;
  return false;
}

// ---- coalescing + dispatch (ARCHITECTURE §5.7) ----------------------------
//
// Deliver a BATCH, not a message. After the first message lands, keep
// collecting for `windowMs` and hand over everything that arrives.
//
// Five people typing at once become ONE handler invocation instead of five.
// This is the single biggest cost lever in the design, and it improves answers
// too: the agent sees the whole room at once instead of five disconnected
// fragments.
//
// Execution is SERIALIZED — one handler in flight at a time. A burst arriving
// mid-run is collected and flushed as the next batch. And a handler can never
// kill the daemon: spawn failures, crashes and non-zero exits are logged and
// swallowed. Losing the listener because someone's shell script had a typo is
// exactly the silent deafness this architecture exists to prevent.
export function makeBatchDispatcher(cmd, windowMs, log, env = {}) {
  let pending = [];
  let timer = null;
  let running = false;

  function scheduleFlush() {
    if (timer) return;
    timer = setTimeout(() => { flush().catch((e) => log(`dispatch: ${e.message}`)); }, windowMs);
  }

  function execBatch(batch) {
    return new Promise((resolve) => {
      let child;
      try {
        child = spawn('sh', ['-c', cmd], {
          stdio: ['pipe', 'pipe', 'pipe'],
          env: { ...process.env, ...env, CONVO_BATCH_COUNT: String(batch.length) },
        });
      } catch (e) { log(`handler spawn failed: ${e.message}`); return resolve(); }

      child.on('error', (e) => { log(`handler error: ${e.message}`); resolve(); });
      child.stdout.on('data', (d) => log(`handler out: ${String(d).trimEnd()}`));
      child.stderr.on('data', (d) => log(`handler err: ${String(d).trimEnd()}`));
      child.on('close', (code) => { log(`handler exit=${code} n=${batch.length}`); resolve(); });
      try {
        child.stdin.write(batch.map((e) => JSON.stringify(e)).join('\n') + '\n');
        child.stdin.end();
      } catch (e) { log(`handler stdin: ${e.message}`); }
    });
  }

  async function flush() {
    timer = null;
    if (!pending.length || running) return;
    running = true;
    const batch = pending;
    pending = [];
    await execBatch(batch);
    running = false;
    if (pending.length) scheduleFlush();   // arrived while we were executing
  }

  return (envelope) => {
    const wasEmpty = pending.length === 0;
    pending.push(envelope);
    if (wasEmpty) scheduleFlush();
  };
}

// ---- the daemon itself ----------------------------------------------------
function writePidFile(file) { ensureHome(); fs.writeFileSync(file, `${process.pid}\n`); }
function clearFile(file) { try { fs.unlinkSync(file); } catch { /* ignore */ } }

export async function stopListener(tag) {
  const file = paths.listenerPid(tag);
  const pid = readPid(file);
  if (!pid || !alive(pid)) {
    clearFile(file); clearFile(paths.heartbeat(tag));
    return { stopped: false, reason: 'not running' };
  }
  try { process.kill(pid, 'SIGTERM'); } catch { /* ignore */ }
  for (let i = 0; i < 60 && alive(pid); i++) await sleep(50);
  clearFile(file); clearFile(paths.heartbeat(tag));
  return { stopped: true, pid };
}

/**
 * Start the daemon detached, and — importantly — do not return "started" until
 * it has claimed the pid file AND published a first heartbeat. "Started" must
 * mean "observably alive", or the liveness contract is a lie from second one.
 */
export async function startDetached(tag, argv) {
  const cli = path.resolve(fileURLToPath(new URL('./cli.js', import.meta.url)));
  ensureHome();
  clearFile(paths.heartbeat(tag));
  const logFd = fs.openSync(paths.listenerLog(tag), 'a');
  const child = spawn(process.execPath, [cli, ...argv.filter((a) => a !== '--detach'), '--foreground'], {
    detached: true,
    stdio: ['ignore', logFd, logFd],
    env: process.env,
  });
  child.unref();

  let ready = false;
  for (let i = 0; i < 200; i++) {
    if (readPid(paths.listenerPid(tag)) && heartbeatHealth(tag).state !== 'none') { ready = true; break; }
    if (!alive(child.pid)) break;
    await sleep(25);
  }
  return { started: ready, ready, pid: readPid(paths.listenerPid(tag)) || child.pid, tag, log: paths.listenerLog(tag) };
}

/** The foreground daemon body. Returns when it is shut down. */
export async function runDaemon({ tag, adapter, self, flags }) {
  ensureHome();
  const intervals = resolveIntervals(flags, adapter.defaults || {});

  const existing = readPid(paths.listenerPid(tag));
  if (existing && alive(existing)) {
    throw new CliError('alreadyRunning', `a daemon is already running for tag "${tag}" (pid ${existing})`, EXIT.ERROR);
  }
  if (existing) clearFile(paths.listenerPid(tag)); // stale pid file, take over

  const log = (m) => {
    try { fs.appendFileSync(paths.listenerLog(tag), `[${new Date().toISOString()}] ${m}\n`); } catch { /* ignore */ }
  };

  writePidFile(paths.listenerPid(tag));
  const startedAt = new Date().toISOString();
  writeHeartbeat(tag, {
    ts: startedAt, startedAt, pollCount: 0, lastMessageAt: null,
    intervalMs: intervals.activeMs, conversations: 0, identity: self, pid: process.pid,
  });
  log(`start pid=${process.pid} tag=${tag} identity=${self.name}(${self.id}) active=${intervals.activeMs}ms mid=${intervals.midMs}ms idle=${intervals.idleMs}ms`);

  // Filters apply to DISPATCH only — never to the journal (§6.2).
  const handlerFilters = buildFilters(flags);
  const dispatch = flags['on-batch']
    ? makeBatchDispatcher(
      flags['on-batch'],
      Math.max(0, Number(flags.window ?? 400)),
      log,
      {
        CONVO_TAG: tag,
        CONVO_HOME: paths.home(),
        CONVO_JOURNAL: paths.journal(tag),
        // Pin the handler to OUR identity so its replies are recognised as
        // ours and suppressed. See the trap noted in pollDue().
        CONVO_SELF_ID: String(self.id ?? ''),
        CONVO_SELF_NAME: String(self.name ?? ''),
        // ...and to our transport, so `convo respond` inside the handler talks
        // to the same channel without re-deriving any configuration.
        CONVO_ADAPTER: String(flags.adapter ?? ''),
        CONVO_ADAPTER_OPT: (Array.isArray(flags['adapter-opt']) ? flags['adapter-opt'] : [flags['adapter-opt']])
          .filter((x) => typeof x === 'string').join('\n'),
      },
    )
    : null;

  let count = 0;
  const source = makeSource(adapter, self, {
    tag,
    intervals,
    onLog: log,
    onPoll: (s) => writeHeartbeat(tag, {
      ts: new Date().toISOString(),
      startedAt,
      pollCount: s.pollCount,
      lastMessageAt: s.lastMessageAt,
      intervalMs: s.intervalMs,
      conversations: s.conversations,
      identity: self,
      pid: process.pid,
    }),
    onMessage: (env) => {
      // JOURNAL FIRST. Nothing — no filter, no handler, no consumer — sees a
      // message before it is durable on disk.
      if (!journal.append(tag, env)) return;   // duplicate
      count++;
      log(`msg id=${env.id} from=${env.from.name} kind=${env.source.kind} in=${env.source.name} n=${count}`);
      if (!filterMatches(env, handlerFilters)) return;
      if (dispatch) dispatch(env);
    },
  });

  let done;
  const finished = new Promise((r) => { done = r; });
  const shutdown = (sig) => {
    log(`shutdown on ${sig}`);
    source.stop();
    clearFile(paths.listenerPid(tag));
    clearFile(paths.heartbeat(tag));
    Promise.resolve(adapter.close?.()).catch(() => {});
    done();
    setTimeout(() => process.exit(0), 50).unref();
  };
  process.on('SIGTERM', () => shutdown('SIGTERM'));
  process.on('SIGINT', () => shutdown('SIGINT'));
  // SIGUSR1 = "sweep now" — lets a consumer force a poll instead of reporting a
  // journal that is up to one idle interval behind reality.
  process.on('SIGUSR1', () => { try { source.nudge(); } catch { /* ignore */ } });
  // A crash in one poll must not take the listener down silently.
  process.on('uncaughtException', (e) => log(`uncaught: ${e?.stack || e}`));
  process.on('unhandledRejection', (e) => log(`unhandled: ${e?.stack || e}`));

  source.start().catch((e) => log(`source died: ${e.message}`));
  await finished;
}
