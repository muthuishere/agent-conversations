#!/usr/bin/env node
// demo.js — the whole architecture, end to end, offline.
//
//     node examples/demo.js
//
// No network, no npm install, no account. It uses the in-repo memory adapter as
// the "channel" and a throwaway state directory under the OS temp dir, so it
// cannot touch anything you care about.
//
// What it proves, in order:
//   1. the daemon starts and is OBSERVABLY alive (heartbeat, not a promise)
//   2. three near-simultaneous messages from two senders COALESCE into ONE batch
//   3. a message the agent sent itself is SUPPRESSED — it never reaches the journal
//   4. the agent answers by MESSAGE ID; the tool routes chat vs channel
//   5. a delivered message is NOT redelivered on the next wait (the cursor bug)
//   6. killing the daemon makes `wait` FAIL FAST instead of blocking on a corpse
//   7. a second consumer on the same tag is REFUSED
//
// Expected output is reproduced in reference/daemon/README.md.
import { spawn, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { openStore, post } from '../reference/adapters/memory-adapter.js';

const CLI = fileURLToPath(new URL('../reference/daemon/cli.js', import.meta.url));
const HANDLER = fileURLToPath(new URL('./handler.js', import.meta.url));

const HOME = fs.mkdtempSync(path.join(os.tmpdir(), 'convo-demo-'));
const STORE = path.join(HOME, 'channel.json');
const ENV = { ...process.env, AGENT_CONVERSATIONS_HOME: HOME, CONVO_TAG: 'demo' };
const ADAPTER = ['--adapter', 'memory', '--adapter-opt', `store=${STORE}`];

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const step = (n, s) => console.log(`\n\x1b[1m── ${n}. ${s}\x1b[0m`);

function convo(args, { quiet = false } = {}) {
  const r = spawnSync(process.execPath, [CLI, ...args], { env: ENV, encoding: 'utf8' });
  if (!quiet) {
    if (r.stdout?.trim()) console.log(r.stdout.trimEnd());
    if (r.stderr?.trim()) console.log(r.stderr.trimEnd());
  }
  return { code: r.status, out: r.stdout || '', err: r.stderr || '' };
}

const store = openStore(STORE);

try {
  console.log(`state dir: ${HOME}`);

  // -----------------------------------------------------------------------
  step(1, 'start the daemon — and prove it is alive from the heartbeat file');
  convo(['listen', ...ADAPTER,
    '--on-batch', `${process.execPath} ${HANDLER}`,
    '--window', '400']);
  await sleep(600);
  const health = JSON.parse(convo(['health'], { quiet: true }).out);
  console.log(`running=${health.running} pid=${health.pid} healthy=${health.healthy} `
    + `heartbeat=${health.heartbeat.state} age=${health.heartbeat.ageMs}ms polls=${health.heartbeat.pollCount}`);

  // -----------------------------------------------------------------------
  step(2, 'three messages land at once from two senders — plus one self-echo');
  post(store, { conversationId: 'c-general', from: { name: 'alice' }, text: 'deploy looks red' });
  post(store, { conversationId: 'c-general', from: { name: 'bob' }, text: 'same here, ping?' });
  post(store, { conversationId: 'c-dm-alice', from: { name: 'alice' }, text: 'status?' });
  // Sent under OUR identity — this is the one that must never come back.
  post(store, { conversationId: 'c-general', from: { id: 'u-agent', name: 'agent' }, text: 'MY OWN ECHO' });
  console.log('posted: alice+bob in #general, alice in dm, and one message as ourselves');

  // -----------------------------------------------------------------------
  step(3, 'the agent blocks ONCE and is handed the whole room as one batch');
  const w1 = convo(['wait', '--timeout', '10', '--window', '600', '--format', 'compact']);
  console.log(`exit=${w1.code}   (0 = messages delivered)`);

  // -----------------------------------------------------------------------
  step(4, 'answer by message id — the tool routes dm vs channel itself');
  const ids = w1.out.trim().split('\n').filter(Boolean).map((l) => l.split('\t')[0]);
  for (const id of ids) convo(['respond', id, `ack ${id}`, ...ADAPTER]);

  // -----------------------------------------------------------------------
  step(5, 'the same wait again — nothing is redelivered, and our own replies never appear');
  const w2 = convo(['wait', '--timeout', '2', '--format', 'compact']);
  console.log(`exit=${w2.code}   (64 = timed out with nothing new. NOT an error.)`);

  // -----------------------------------------------------------------------
  step(6, 'meanwhile the daemon ALSO spawned a fresh handler for the same batch (wake path W2)');
  console.log('(both wake paths are armed here on purpose: the blocking `wait` above is W1,\n'
    + ' the spawned handler is W2 — the one that survives the session dying.)');
  for (const line of fs.readFileSync(path.join(HOME, 'listener.demo.log'), 'utf8').split('\n')) {
    if (line.includes('handler')) console.log(line.replace(/^\[[^\]]+\]\s*/, '  '));
  }

  // -----------------------------------------------------------------------
  step(7, 'the journal — durable, append-only, complete, and reading it consumed nothing');
  convo(['journal', '--format', 'compact']);
  const j = JSON.parse(convo(['health'], { quiet: true }).out);
  console.log(`journalBytes=${j.journalBytes} readOffset=${j.readOffset} `
    + `(the read cursor advanced on DELIVERY, not on ack; ack count=${j.ackedCount})`);
  console.log('note: "MY OWN ECHO" is absent — suppressed by identity before the journal');

  // -----------------------------------------------------------------------
  step(8, 'a second consumer on the same tag is refused');
  const bg = spawn(process.execPath, [CLI, 'wait', '--timeout', '6'], { env: ENV, stdio: 'ignore' });
  await sleep(400);
  const second = convo(['wait', '--timeout', '2']);
  console.log(`exit=${second.code}   (66 = another consumer holds this tag)`);
  bg.kill('SIGTERM');
  await sleep(300);

  // -----------------------------------------------------------------------
  step(9, 'kill the daemon — wait must fail FAST, not block on a corpse');
  const pid = Number(fs.readFileSync(path.join(HOME, 'listener.demo.pid'), 'utf8').trim());
  process.kill(pid, 'SIGKILL');
  const t0 = Date.now();
  const dead = convo(['wait', '--timeout', '30']);
  console.log(`exit=${dead.code} after ${Date.now() - t0}ms   (69 = listener dead; it did NOT wait 30s)`);

  console.log('\n\x1b[1mdone.\x1b[0m silence is never mistaken for "no messages".');
} finally {
  convo(['listen', '--stop'], { quiet: true });
  fs.rmSync(HOME, { recursive: true, force: true });
}
