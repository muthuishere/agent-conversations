#!/usr/bin/env node
// handler.js — what the daemon spawns for each coalesced batch (wake path W2).
//
// The contract (INTERFACES.md §4):
//   · stdin  = NDJSON, one canonical envelope per line, the whole batch
//   · env    = CONVO_BATCH_COUNT, CONVO_TAG, CONVO_HOME, CONVO_JOURNAL,
//              CONVO_SELF_ID, CONVO_SELF_NAME, CONVO_ADAPTER, CONVO_ADAPTER_OPT
//   · exit 0 = handled. Anything else is logged; the daemon keeps running either
//              way, and the batch is NOT retried by it.
//   · MUST BE IDEMPOTENT — delivery is at-least-once. Dedupe on message id.
//
// This one is the "rules first, model second" layer from ARCHITECTURE §6.4: a
// regex table answers ping/help/status instantly and for free, and only what it
// cannot match would be escalated to a model. Most channel traffic is
// acknowledgements and one-word questions; paying an LLM for those is how a
// cheap design turns expensive.
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const CLI = fileURLToPath(new URL('../reference/daemon/cli.js', import.meta.url));

const RULES = [
  [/^\s*ping\b/i, 'pong'],
  [/\bhelp\b/i, 'commands: ping · status · help'],
  [/\bstatus\b/i, 'all green'],
];

function ruleAnswer(text) {
  for (const [re, reply] of RULES) if (re.test(text || '')) return reply;
  return null;   // -> escalate to a model, with a NARROW toolset (§8.4)
}

const chunks = [];
for await (const c of process.stdin) chunks.push(c);
const batch = Buffer.concat(chunks).toString('utf8')
  .split('\n').filter((l) => l.trim())
  .map((l) => JSON.parse(l));

console.log(`handler: batch of ${batch.length} (CONVO_BATCH_COUNT=${process.env.CONVO_BATCH_COUNT})`);

for (const m of batch) {
  const answer = ruleAnswer(m.text) || `noted, ${m.from.name} — a human will pick this up`;
  // `respond` takes a MESSAGE ID and routes itself. The handler never picks an
  // endpoint, and it inherits the daemon's adapter + identity from the env, so
  // its replies are recognised as ours and suppressed (no self-reply loop).
  const r = spawnSync(process.execPath, [CLI, 'respond', m.id, answer], { encoding: 'utf8' });
  console.log(`handler: ${m.id} <- ${JSON.stringify(answer)} exit=${r.status}`);
}
