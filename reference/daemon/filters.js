// filters.js — delivery-time filters (ARCHITECTURE §6.2).
//
// Every filter here is applied AFTER a message is journalled, never before.
// The journal records what ARRIVED; changing who the agent answers must never
// change what was recorded. `journal --all` therefore always shows everything,
// filtered-out messages included, and stays auditable.
//
// Self-echo suppression is deliberately NOT in this file. It happens upstream,
// in the daemon, before the journal — it is a loop-safety invariant, not a
// preference (ARCHITECTURE §5.3).
//
// Composition:
//   repeats of the SAME flag OR together   (--from alice --from bob)
//   DIFFERENT flags AND together           (--from alice --match deploy)
import { CliError, EXIT } from './config.js';

export const KINDS = ['all', 'chat', 'channel'];

export function asList(v) {
  if (v == null || v === true || v === false) return [];
  return (Array.isArray(v) ? v : [v]).map(String).filter((s) => s.length > 0);
}

export function parseKind(flags = {}) {
  const raw = flags.kind;
  if (raw == null || raw === true) return 'all';
  const k = String(raw).toLowerCase();
  if (!KINDS.includes(k)) throw new CliError('badArgs', `--kind must be one of ${KINDS.join('|')}, got "${raw}"`, EXIT.NOT_CONFIGURED);
  return k;
}

// "is this the thing the operator named": exact id, exact name, or a
// case-insensitive substring of the name (so `--from alice` finds "Alice Ng").
function hits(candidates, want) {
  const w = String(want).toLowerCase();
  for (const c of candidates) {
    if (!c) continue;
    const v = String(c);
    if (v === String(want)) return true;
    const lv = v.toLowerCase();
    if (lv === w || lv.includes(w)) return true;
  }
  return false;
}

export function buildFilters(flags = {}) {
  const match = typeof flags.match === 'string' ? flags.match : null;
  let re = null;
  if (match) {
    try { re = new RegExp(match, 'i'); } catch (e) {
      throw new CliError('badArgs', `--match is not a valid regex: ${e.message}`, EXIT.NOT_CONFIGURED);
    }
  }
  return {
    kind: parseKind(flags),
    from: asList(flags.from),
    excludeFrom: asList(flags['exclude-from']),
    in: asList(flags.in),
    match,
    re,
    mentionsMe: Boolean(flags['mentions-me']),
  };
}

export function matches(env, f) {
  if (!f) return true;
  const src = env.source || {};
  if (f.kind !== 'all' && src.kind !== f.kind) return false;

  const who = [env.from?.name, env.from?.id];
  if (f.from.length && !f.from.some((n) => hits(who, n))) return false;
  if (f.excludeFrom.length && f.excludeFrom.some((n) => hits(who, n))) return false;

  if (f.in.length) {
    const where = [src.name, src.conversationId];
    if (!f.in.some((n) => hits(where, n))) return false;
  }

  if (f.re && !f.re.test(env.text || '')) return false;
  if (f.mentionsMe && !env.mentionsMe) return false;
  return true;
}
