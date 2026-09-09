#!/usr/bin/env node
// cli.js — `convo`. The command surface an agent skill (or any other program)
// drives. Zero npm dependencies; Node >= 20 builtins only.
//
// The full contract — commands, exit codes, on-disk files, output shapes and
// the handler protocol — is INTERFACES.md. This file is the implementation of
// that document, not a second source of truth.
import process from 'node:process';
import {
  paths, ensureHome, CliError, EXIT, readHeartbeat,
} from './config.js';
import * as journal from './journal.js';
import {
  runDaemon, startDetached, stopListener, listenerHealth,
} from './daemon.js';
import { runWait } from './wait.js';
import { loadAdapter, parseAdapterOptions } from './loader.js';
import { renderMessage } from './format.js';
import { buildFilters, matches as filterMatches } from './filters.js';

const VALUE_FLAGS = new Set([
  'tag', 'adapter', 'adapter-opt', 'on-batch', 'window', 'timeout', 'count',
  'format', 'from', 'exclude-from', 'in', 'match', 'kind', 'limit', 'to',
  'poll-active', 'poll-mid', 'poll-idle', 'idle-1', 'idle-2',
]);
// Repeats OR together (see filters.js).
const MULTI_FLAGS = new Set(['from', 'exclude-from', 'in', 'adapter-opt']);

const USAGE = `convo — conversations for agent sessions, without a polling loop

  receive
    convo listen  --adapter memory [--adapter-opt k=v] [--on-batch 'cmd'] [--window 400]
                  [--tag t] [--foreground] [--stop] [--status]
    convo wait    [--tag t] [--timeout 900] [--count 1] [--window ms] [--ack]
                  [--takeover] [--format ndjson|compact|pretty] [filters]
    convo journal [--tag t] [--all|--new] [--limit N] [--format ...] [filters]
    convo ack     <id...> | --all
    convo health  [--tag t]

  send
    convo respond <messageId> "text"      # routes itself from the message's source.kind
    convo send    --to <conversationId> "text"

  inspect
    convo identity | convo conversations

  filters (composable; repeats of one flag OR, different flags AND)
    --from <who>  --exclude-from <who>  --in <conversation>  --match <regex>
    --kind chat|channel  --mentions-me
    Filters apply at DELIVERY only. The journal always keeps everything.

  exit codes
    0  ok / messages delivered
    1  unexpected error
    64 wait timed out with nothing to deliver  (NOT an error)
    65 not configured, bad arguments, unknown message id
    66 another consumer already holds this tag
    69 daemon expected but dead/stale, or the transport is unreachable

  env
    AGENT_CONVERSATIONS_HOME   state dir (default ~/.config/agent-conversations)
    CONVO_TAG / CONVO_ADAPTER / CONVO_ADAPTER_OPT   defaults for the flags above`;

function parseArgs(argv) {
  const flags = {};
  const args = [];
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === '--') { args.push(...argv.slice(i + 1)); break; }
    if (a === '-h') { flags.help = true; continue; }
    if (!a.startsWith('--')) { args.push(a); continue; }
    const body = a.slice(2);
    const eq = body.indexOf('=');
    const put = (k, v) => {
      if (!MULTI_FLAGS.has(k)) { flags[k] = v; return; }
      if (flags[k] === undefined) flags[k] = v;
      else flags[k] = (Array.isArray(flags[k]) ? flags[k] : [flags[k]]).concat(v);
    };
    if (eq !== -1) { put(body.slice(0, eq), body.slice(eq + 1)); continue; }
    if (VALUE_FLAGS.has(body)) { put(body, argv[++i]); continue; }
    flags[body] = true;
  }
  return { flags, args };
}

const jsonOut = (v) => process.stdout.write(JSON.stringify(v) + '\n');

function tagOf(flags) {
  return String(flags.tag || process.env.CONVO_TAG || 'default');
}

async function adapterOf(flags) {
  const spec = flags.adapter || process.env.CONVO_ADAPTER || null;
  const raw = flags['adapter-opt']
    ?? (process.env.CONVO_ADAPTER_OPT ? process.env.CONVO_ADAPTER_OPT.split('\n').filter(Boolean) : null);
  return loadAdapter(spec, parseAdapterOptions(raw));
}

async function main() {
  const { flags, args } = parseArgs(process.argv.slice(2));
  const cmd = args[0];
  if (!cmd || flags.help) { process.stdout.write(USAGE + '\n'); return EXIT.OK; }
  const tag = tagOf(flags);
  ensureHome();

  switch (cmd) {
    // ---------------------------------------------------------------- listen
    case 'listen': {
      if (flags.stop) { jsonOut(await stopListener(tag)); return EXIT.OK; }
      if (flags.status) {
        const h = listenerHealth(tag);
        jsonOut({
          tag, running: h.running, pid: h.pid, healthy: h.healthy, problem: h.reason,
          heartbeat: h.heartbeat, journal: paths.journal(tag), journalBytes: journal.size(tag),
          log: paths.listenerLog(tag),
        });
        return h.healthy && h.running ? EXIT.OK : EXIT.LISTENER_DEAD;
      }
      const adapter = await adapterOf(flags);
      const self = await adapter.identity();
      if (!flags.foreground) {
        // The detached child re-runs this same CLI with --foreground, so every
        // flag (adapter, filters, window, intervals) carries over untouched.
        jsonOut(await startDetached(tag, process.argv.slice(2)));
        return EXIT.OK;
      }
      await runDaemon({ tag, adapter, self, flags });
      return EXIT.OK;
    }

    // ------------------------------------------------------------------ wait
    case 'wait':
      return runWait({ tag, flags, out: process.stdout });

    // --------------------------------------------------------------- journal
    // A LOOK, never a consume: this must not advance the read cursor.
    case 'journal': {
      const all = journal.readAll(tag);
      const acked = new Set(journal.readAcked(tag));
      const filters = buildFilters(flags);
      let list = all.filter((m) => filterMatches(m, filters));
      if (flags.new) list = list.filter((m) => !acked.has(m.id));
      const limit = flags.limit != null ? Math.max(1, Number(flags.limit)) : null;
      if (limit) list = list.slice(-limit);
      const format = flags.format || 'ndjson';
      if (list.length) process.stdout.write(list.map((m) => renderMessage(m, format)).join('\n') + '\n');
      return EXIT.OK;
    }

    // ------------------------------------------------------------------- ack
    case 'ack': {
      const ids = flags.all ? journal.readAll(tag).map((m) => m.id) : args.slice(1);
      if (!ids.length) throw new CliError('badArgs', 'ack needs message ids, or --all', EXIT.NOT_CONFIGURED);
      jsonOut({ acked: ids.length, total: journal.ack(tag, ids) });
      return EXIT.OK;
    }

    // ---------------------------------------------------------------- health
    // Never report "listening" from memory — this reads the heartbeat file.
    case 'health': {
      const h = listenerHealth(tag);
      jsonOut({
        tag,
        running: h.running,
        pid: h.pid,
        healthy: h.healthy,
        problem: h.reason,
        heartbeat: h.heartbeat,
        raw: readHeartbeat(tag),
        journalBytes: journal.size(tag),
        readOffset: journal.readReadCursor(tag).readOffset,
        ackedCount: journal.readAcked(tag).length,
      });
      return h.healthy && h.running ? EXIT.OK : EXIT.LISTENER_DEAD;
    }

    // --------------------------------------------------------------- respond
    // ARCHITECTURE §6.3: the AGENT supplies a message id and nothing else. The
    // tool routes from source.kind. Never make the agent choose between "send
    // to chat" and "reply in thread" — it will choose wrong, the platform will
    // return 200, and the reply will vanish. An unknown id fails LOUDLY.
    case 'respond': {
      const [, id, text] = args;
      if (!id || text == null) throw new CliError('badArgs', 'usage: convo respond <messageId> "text"', EXIT.NOT_CONFIGURED);
      const env = journal.findById(tag, id);
      if (!env) {
        throw new CliError('unknownMessage',
          `no message "${id}" in the journal for tag "${tag}" — refusing to guess a target`,
          EXIT.NOT_CONFIGURED);
      }
      const adapter = await adapterOf(flags);
      const target = env.source.kind === 'chat'
        ? { conversationId: env.source.conversationId, kind: 'chat' }
        : {
          conversationId: env.source.conversationId,
          kind: env.source.kind,
          threadId: env.source.threadId || env.id,
          replyToId: env.id,
        };
      const res = await adapter.send(target, text, {});
      await adapter.close?.();
      jsonOut({ ok: true, id: res?.id ?? null, inReplyTo: env.id, target });
      return EXIT.OK;
    }

    // ------------------------------------------------------------------ send
    case 'send': {
      const to = flags.to;
      const text = args[1];
      if (!to || text == null) throw new CliError('badArgs', 'usage: convo send --to <conversationId> "text"', EXIT.NOT_CONFIGURED);
      const adapter = await adapterOf(flags);
      const convs = await adapter.listConversations();
      const conv = convs.find((c) => c.id === to || c.name === to);
      const res = await adapter.send({ conversationId: conv?.id ?? to, kind: conv?.kind ?? 'chat' }, text, {});
      await adapter.close?.();
      jsonOut({ ok: true, id: res?.id ?? null, conversationId: conv?.id ?? to });
      return EXIT.OK;
    }

    // --------------------------------------------------------------- inspect
    case 'identity': {
      const adapter = await adapterOf(flags);
      jsonOut(await adapter.identity());
      await adapter.close?.();
      return EXIT.OK;
    }
    case 'conversations': {
      const adapter = await adapterOf(flags);
      const list = await adapter.listConversations();
      process.stdout.write(list.map((c) => JSON.stringify(c)).join('\n') + '\n');
      await adapter.close?.();
      return EXIT.OK;
    }

    default:
      throw new CliError('badArgs', `unknown command "${cmd}"\n\n${USAGE}`, EXIT.NOT_CONFIGURED);
  }
}

main()
  .then((code) => { process.exitCode = code ?? EXIT.OK; })
  .catch((e) => {
    // Errors are structured on STDERR so stdout stays a clean message stream.
    const code = e instanceof CliError ? e.code : 'error';
    process.stderr.write(JSON.stringify({ error: { code, message: e.message } }) + '\n');
    process.exitCode = e instanceof CliError ? e.exit : EXIT.ERROR;
  });
