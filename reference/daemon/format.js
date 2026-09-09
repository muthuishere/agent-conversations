// format.js — two output modes, and the reason there are two.
//
// NDJSON (default) is for programs: one JSON object per line, no ambiguity.
//
// COMPACT is for an AGENT reading with its own eyes. A model asked to parse
// JSON by hand is slow, expensive and wrong just often enough to be dangerous —
// it will hallucinate a field, mis-split on a brace inside a string, or quietly
// swallow an error object it did not expect. One tab-separated line per message
// removes the parsing step entirely.
//
// The message id is NEVER truncated: it is the join key for `respond`, so a
// long body must not be able to push it off the line or make the split
// ambiguous. Only the trailing text is capped.
const COMPACT_TEXT_MAX = 200;

function hhmm(iso) {
  const d = iso ? new Date(iso) : new Date();
  if (Number.isNaN(d.getTime())) return '--:--';
  return `${String(d.getUTCHours()).padStart(2, '0')}:${String(d.getUTCMinutes()).padStart(2, '0')}`;
}

export function compactTarget(source) {
  if (!source) return '[?]';
  return source.kind === 'chat' ? '[dm]' : `[${source.name || source.conversationId}]`;
}

/** <id> TAB <from> TAB [dm]|[#channel] TAB <one-line text> */
export function compactMessage(m) {
  const text = (m.text || '').replace(/\s+/g, ' ').trim();
  const clipped = text.length > COMPACT_TEXT_MAX ? `${text.slice(0, COMPACT_TEXT_MAX - 1)}…` : text;
  return [m.id, m.from?.name || 'unknown', compactTarget(m.source), clipped].join('\t');
}

export function prettyMessage(m) {
  const text = (m.text || '').replace(/\s*\n\s*/g, ' ⏎ ');
  return `${hhmm(m.at)} ${m.from?.name || 'unknown'} › ${text}  ${compactTarget(m.source)}`;
}

export function renderMessage(m, format = 'ndjson') {
  if (format === 'compact') return compactMessage(m);
  if (format === 'pretty') return prettyMessage(m);
  return JSON.stringify(m);
}
