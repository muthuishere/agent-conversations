#!/usr/bin/env node
// poll-demo.js — the PORTABLE wake path, end to end, offline.
//
//     node examples/poll-demo.js
//
// No network, no npm install, no account: the in-repo memory adapter is the
// "channel" and a throwaway temp dir is the state, exactly like demo.js.
//
// demo.js shows the two zero-cost wake paths (a blocked `wait` that the runtime
// re-invokes on exit, and a handler the daemon spawns). Both are great and
// neither is portable: one needs a runtime that re-invokes a session when a
// background task exits, the other needs no live session at all.
//
// This demo shows the third option — reference/skill/poll-responder.sh — where
// a SUBAGENT hosts a blocking poll with an escalating backoff. The main session
// stays free, the sleeps happen inside the shell (so they cost zero tokens),
// and a turn is spent only when the call returns.
//
// What it proves, in order:
//   1. a message already waiting returns IMMEDIATELY — it never sleeps first
//   2. two empty runs ESCALATE the tier 2s -> 4s -> 6s, exit 64 each time
//   3. the tier lives in a STATE FILE, because no single call can span the
//      whole schedule (a foreground tool call is capped at ~600s)
//   4. a message arriving SNAPS the tier back to the first one, exit 0
//   5. --max-cycles / --max-runtime end the shift with exit 75 = "respawn me":
//      the subagent hosting this is replaceable, never immortal
//   6. a dead daemon is exit 69 IMMEDIATELY — it never sleeps against a corpse
import { spawn, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { openStore, post } from '../reference/adapters/memory-adapter.js';

const CLI = fileURLToPath(new URL('../reference/daemon/cli.js', import.meta.url));
const POLLER = fileURLToPath(new URL('../reference/skill/poll-responder.sh', import.meta.url));

const HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'convo-poll-demo-'));
const STORE = path.join(HOME, 'channel.json');
const TAG = 'polldemo';
const ENV = {
  ...process.env,
  AGENT_CONVERSATIONS_HOME: HOME,
  CONVO_TAG: TAG,
  // The poller finds the CLI itself, but being explicit is what a real skill does.
  CONVO: `${process.execPath} ${CLI}`,
};
const ADAPTER = ['--adapter', 'memory', '--adapter-opt', `store=${STORE}`];
const TIERS = ['--tiers', '2,4,6'];   // tiny, so the demo finishes in seconds

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const step = (n, s) => console.log(`\n\x1b[1m── ${n}. ${s}\x1b[0m`);

function convo(args, { quiet = false } = {}) {
  const r = spawnSync(process.execPath, [CLI, ...args], { env: ENV, encoding: 'utf8' });
  if (!quiet && r.stdout?.trim()) console.log(r.stdout.trimEnd());
  return { code: r.status, out: r.stdout || '' };
}

/** Run the poller and report how long it blocked — the number that matters. */
function poll(args, { quiet = false } = {}) {
  const t0 = Date.now();
  const r = spawnSync('bash', [POLLER, '--tag', TAG, ...args], { env: ENV, encoding: 'utf8' });
  const ms = Date.now() - t0;
  if (!quiet) {
    if (r.stdout?.trim()) console.log(r.stdout.trimEnd());
    if (r.stderr?.trim()) console.log(`\x1b[2m${r.stderr.trimEnd()}\x1b[0m`);
  }
  return { code: r.status, out: r.stdout || '', ms };
}

/**
 * The same, but ASYNC — spawnSync would block this process's event loop, so a
 * message "arriving mid-block" could never actually be posted. (That is not a
 * demo artefact: it is the same reason a main session must not host the loop.)
 */
function pollAsync(args) {
  const t0 = Date.now();
  return new Promise((resolve) => {
    const c = spawn('bash', [POLLER, '--tag', TAG, ...args], { env: ENV, encoding: 'utf8' });
    let out = ''; let err = '';
    c.stdout.on('data', (d) => { out += d; });
    c.stderr.on('data', (d) => { err += d; });
    c.on('close', (code) => {
      if (out.trim()) console.log(out.trimEnd());
      if (err.trim()) console.log(`\x1b[2m${err.trimEnd()}\x1b[0m`);
      resolve({ code, out, ms: Date.now() - t0 });
    });
  });
}

const state = () => JSON.parse(fs.readFileSync(path.join(HOME, `poll-responder.${TAG}.json`), 'utf8'));
const tierOf = (s) => s.tiers[s.tier_index];

const store = openStore(STORE);
const say = (text, from = 'alice') =>
  post(store, { conversationId: 'c-general', from: { name: from }, text });

try {
  console.log(`state dir: ${HOME}`);

  // -----------------------------------------------------------------------
  step(1, 'start the daemon, and have one message already waiting');
  convo(['listen', ...ADAPTER, '--poll-active', '1', '--window', '200'], { quiet: true });
  await sleep(1200);
  say('deploy looks red');
  await sleep(1200);
  const h = JSON.parse(convo(['health'], { quiet: true }).out);
  console.log(`daemon healthy=${h.healthy} heartbeat=${h.heartbeat.state} — checked, never assumed`);

  // -----------------------------------------------------------------------
  step(2, 'the poller CHECKS FIRST and returns at once — it never sleeps before looking');
  const first = poll([...TIERS, '--max-block', '10']);
  console.log(`exit=${first.code} after ${first.ms}ms   (0 = messages delivered; the tiers were never used)`);

  // -----------------------------------------------------------------------
  step(3, 'nothing waiting: two empty checkpoint runs ESCALATE the backoff tier');
  console.log(`tier before: ${tierOf(state())}s`);
  const e1 = poll([...TIERS, '--once']);
  console.log(`  run 1 -> exit=${e1.code}  tier now ${tierOf(state())}s`);
  const e2 = poll([...TIERS, '--once']);
  console.log(`  run 2 -> exit=${e2.code}  tier now ${tierOf(state())}s`);
  const e3 = poll([...TIERS, '--once']);
  console.log(`  run 3 -> exit=${e3.code}  tier now ${tierOf(state())}s (clamped at the last tier)`);
  console.log('64 = "aged out, nothing arrived". It is NOT an error, and nothing else uses 64.');

  // -----------------------------------------------------------------------
  step(4, 'WHY the tier is on disk and not in a variable');
  const s4 = state();
  console.log(`  ${path.join(HOME, `poll-responder.${TAG}.json`)}`);
  console.log(`  ${JSON.stringify({
    tier_index: s4.tier_index, consecutive_empty: s4.consecutive_empty, cycles: s4.cycles,
  })}`);
  console.log('  A foreground tool call is capped (~600s in some runtimes), so ONE call cannot');
  console.log('  span a 2+4+6 schedule. The tier is therefore carried ACROSS invocations.');

  // -----------------------------------------------------------------------
  step(5, 'it blocks on the current tier — and a message mid-block returns immediately');
  const blocking = pollAsync([...TIERS, '--max-block', '20']);
  await sleep(2500);
  say('are we up?', 'bob');
  console.log('  (bob posted 2.5s into a block that was sleeping on the 6s tier)');
  const got = await blocking;
  console.log(`exit=${got.code} after ${got.ms}ms   (blocked in the SHELL — zero tokens while asleep)`);
  console.log(`tier after a message: ${tierOf(state())}s — SNAPPED back to the first tier`);

  // -----------------------------------------------------------------------
  step(6, 'the shift ends deliberately: --max-cycles exits 75 = "respawn me"');
  console.log('(a subagent hosting this is NOT immortal — every returned poll grows its context.');
  console.log(' So it is made REPLACEABLE instead: journal + read cursor + ack file + this state');
  console.log(' file hold everything, so a fresh subagent resumes with no handoff at all.)');
  // The cycle counter is cumulative across invocations — it belongs to the
  // SHIFT, not to one call — so budget one more check than we have already run.
  const budget = String(state().cycles + 1);
  const shift = poll([...TIERS, '--max-block', '6', '--max-cycles', budget]);
  console.log(`exit=${shift.code}   (75 = shift over. Distinct from 64 "quiet" and 69 "dead".)`);
  console.log(`shift counters reset for the replacement: ${JSON.stringify(
    { cycles: state().cycles, tier_index: state().tier_index })}`);
  console.log('The tier is left exactly as it was: the backoff belongs to the conversation, not');
  console.log('to whoever happens to be holding the pager.');

  // -----------------------------------------------------------------------
  step(7, 'kill the daemon — the poller must NOT sleep through a dead listener');
  const pid = Number(fs.readFileSync(path.join(HOME, `listener.${TAG}.pid`), 'utf8').trim());
  process.kill(pid, 'SIGKILL');
  await sleep(200);
  const dead = poll([...TIERS, '--max-block', '30'], { quiet: true });
  console.log(`exit=${dead.code} after ${dead.ms}ms   (69 = listener dead; it did NOT sleep 30s)`);
  console.log('Silence against a corpse is deafness, not quiet — the one failure this design');
  console.log('exists to prevent (ARCHITECTURE §5.8).');

  console.log('\n\x1b[1mdone.\x1b[0m the loop is portable, the sleeps are free, and the poller is replaceable.');
} finally {
  convo(['listen', '--stop'], { quiet: true });
  fs.rmSync(HOME, { recursive: true, force: true });
}
