// wait.js — the consumer side: the blocking receive.
//
// This is the whole point of the architecture from the agent's seat. The agent
// never loops; it arms ONE tripwire — this process — and is woken when it
// exits. Rules it enforces, each of which exists because the absence of it was
// observed breaking a real system:
//
//   1. DRAIN THE BACKLOG FIRST. Messages that arrived while the agent was
//      thinking are already on disk. Block only after the journal is caught up,
//      or a busy channel produces an agent that is permanently one batch behind.
//
//   2. A TIMEOUT IS NOT AN ERROR. Aging out with nothing to say exits 64, which
//      no error path uses. If "nothing arrived" and "the transport is broken"
//      share an exit code, every agent that sees it either panics at silence or
//      — far worse — shrugs at a real failure.
//
//   3. REFUSE TO BLOCK AGAINST A CORPSE. If a daemon is expected but dead or
//      stale, exit 69 instead of waiting. A silent block against a dead listener
//      is indistinguishable from "nobody messaged me", and that is the defining
//      failure of this design (ARCHITECTURE §5.8).
//      The liveness check is also repeated WHILE blocked — a daemon can die
//      mid-wait, which is exactly when nobody is looking.
//
//   4. ONE CONSUMER PER TAG. Two waits on one read cursor race for messages and
//      tear down each other's state. The second exits 66 unless --takeover.
//
//   5. THE READ CURSOR ADVANCES ON DELIVERY, NOT ON ACK, and is persisted only
//      AFTER stdout has been written. See the header of journal.js.
import fs from 'node:fs';
import {
  paths, ensureHome, CliError, EXIT,
} from './config.js';
import * as journal from './journal.js';
import { listenerHealth, alive, readPid } from './daemon.js';
import { buildFilters, matches as filterMatches } from './filters.js';
import { renderMessage } from './format.js';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * Claim the one-consumer-per-tag lock. The returned release() only removes the
 * file if it is still OURS — otherwise a takeover victim's cleanup would delete
 * its successor's lock on the way out.
 */
async function claimConsumerLock(tag, { takeover = false } = {}) {
  ensureHome();
  const file = paths.consumerPid(tag);
  const incumbent = readPid(file);
  if (incumbent && alive(incumbent)) {
    if (!takeover) {
      throw new CliError(
        'consumerConflict',
        `another consumer already holds tag "${tag}" (pid ${incumbent}); two consumers on one read cursor race for messages — pass --takeover to replace it`,
        EXIT.CONSUMER_CONFLICT,
      );
    }
    try { process.kill(incumbent, 'SIGTERM'); } catch { /* already gone */ }
    for (let i = 0; i < 40 && alive(incumbent); i++) await sleep(50);
    if (alive(incumbent)) {
      try { process.kill(incumbent, 'SIGKILL'); } catch { /* ignore */ }
      for (let i = 0; i < 20 && alive(incumbent); i++) await sleep(50);
    }
  }
  fs.writeFileSync(file, `${process.pid}\n`);
  return () => { if (readPid(file) === process.pid) { try { fs.unlinkSync(file); } catch { /* ignore */ } } };
}

/**
 * @returns {Promise<number>} the process exit code (0 delivered, 64 timeout).
 */
export async function runWait({ tag, flags, out }) {
  ensureHome();
  const filters = buildFilters(flags);
  const format = flags.format || 'ndjson';
  const timeoutMs = Math.max(0, Number(flags.timeout ?? 900)) * 1000;
  const windowMs = Math.max(0, Number(flags.window ?? 0));
  // --count is the upper bound. With a coalescing --window and no explicit
  // --count, let the WINDOW decide when to stop collecting rather than
  // capping at the default of one.
  const want = flags.count != null
    ? Math.max(1, Number(flags.count))
    : (windowMs > 0 ? Infinity : 1);

  // ---- rule 3: never mistake a dead listener for silence ----
  const health = listenerHealth(tag);
  if (health.expected && !health.healthy) {
    throw new CliError('listenerDead',
      `refusing to wait: the daemon for tag "${tag}" is not delivering — ${health.reason}. Restart it with \`convo listen\`.`,
      EXIT.LISTENER_DEAD);
  }

  // ---- rule 4: one consumer ----
  const release = await claimConsumerLock(tag, { takeover: Boolean(flags.takeover) });

  let offset = journal.readReadCursor(tag).readOffset;
  const acked = new Set(journal.readAcked(tag));

  const collected = [];
  const seen = new Set();
  let firstArrivalAt = null;

  const push = (env) => {
    if (seen.has(env.id)) return;
    // An acked message is one the agent explicitly finished with; draining the
    // backlog must not hand it back.
    if (acked.has(env.id)) return;
    if (!filterMatches(env, filters)) return;
    seen.add(env.id);
    collected.push(env);
    if (firstArrivalAt === null) firstArrivalAt = Date.now();
  };

  // Scan every complete line from `offset`, advancing past each one we LOOK at
  // — delivered or filtered out — but stopping the moment `want` is satisfied,
  // so lines past that point stay unread and a later wait still sees them.
  const scanAvailable = () => {
    const r = journal.readLinesFrom(tag, offset);
    for (const { env, offset: lineEnd } of r.lines) {
      offset = lineEnd;
      push(env);
      if (collected.length >= want) break;
    }
  };

  // ---- rule 1: drain the backlog before blocking ----
  scanAvailable();

  const finish = (code) => {
    if (!collected.length) return code;
    const delivered = collected.slice(0, want === Infinity ? collected.length : want);
    out.write(delivered.map((m) => renderMessage(m, format)).join('\n') + '\n');
    // ---- rule 5: persist the read cursor only AFTER the handoff succeeded.
    // An interrupted wait leaves it untouched and redelivers next time:
    // at-least-once, which is the correct side to fail on.
    journal.writeReadCursor(tag, offset);
    if (flags.ack) journal.ack(tag, delivered.map((m) => m.id));
    return EXIT.OK;
  };

  if (collected.length >= want && windowMs === 0) { release(); return finish(EXIT.OK); }

  // Backlog was not enough and we are about to block: with no daemon at all,
  // nothing will ever append, so say so loudly instead of sleeping for 15
  // minutes and reporting "no messages".
  if (!health.expected) {
    release();
    if (collected.length) return finish(EXIT.OK);
    throw new CliError('noListener',
      `no daemon is running for tag "${tag}" — nothing will ever be journalled, so blocking would be a lie. Start one with \`convo listen\`.`,
      EXIT.LISTENER_DEAD);
  }

  // ---- block ----
  let watcher = null;
  let interrupted = false;
  const onSignal = () => { interrupted = true; };
  process.on('SIGINT', onSignal);
  process.on('SIGTERM', onSignal);

  let nudged = false;
  try {
    // fs.watch is a latency optimization only; the 250ms poll below is the
    // guarantee. Never depend on inotify semantics across platforms.
    watcher = fs.watch(paths.journalDir(), () => { nudged = true; });
  } catch { /* not available here — the poll fallback covers it */ }

  const cleanup = () => {
    if (watcher) { try { watcher.close(); } catch { /* ignore */ } }
    process.off('SIGINT', onSignal);
    process.off('SIGTERM', onSignal);
  };

  const hardDeadline = Date.now() + timeoutMs;
  let liveCheckAt = Date.now();
  try {
    while (Date.now() < hardDeadline && !interrupted) {
      scanAvailable();
      if (collected.length >= want) break;
      // Coalescing on the consumer side: once the first message has landed,
      // keep collecting until windowMs has elapsed since THAT arrival.
      if (windowMs > 0 && firstArrivalAt !== null && Date.now() - firstArrivalAt >= windowMs) break;

      // rule 3, continued: the daemon can die WHILE we block.
      if (Date.now() - liveCheckAt > 1000) {
        liveCheckAt = Date.now();
        const h = listenerHealth(tag);
        if (!h.healthy) {
          cleanup(); release();
          if (collected.length) return finish(EXIT.OK);
          throw new CliError('listenerDied',
            `the daemon for tag "${tag}" died while waiting — ${h.reason}`, EXIT.LISTENER_DEAD);
        }
      }

      const step = nudged ? 25 : 250;
      nudged = false;
      const windowDeadline = (windowMs > 0 && firstArrivalAt !== null) ? firstArrivalAt + windowMs : hardDeadline;
      const deadline = Math.min(hardDeadline, windowDeadline);
      await sleep(Math.min(step, Math.max(1, deadline - Date.now())));
    }
  } finally {
    cleanup();
    release();
  }

  // ---- rule 2: nothing arrived is exit 64, and 64 means only this ----
  return finish(EXIT.TIMEOUT);
}
