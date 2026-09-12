# 005 — Policy lives in filters, never in plumbing

**Status:** accepted · **Decider:** project owner · **Reversibility:** two-way door

## Context
"Who does this agent answer?" changes constantly: one person today, a group tomorrow, only
messages that mention it the day after. The first implementation scoped *receiving* to one
channel — and missed a direct message entirely because the message arrived somewhere the
plumbing wasn't pointed. The lesson generalises: every time scope was encoded in the
transport, changing scope meant re-plumbing, and re-plumbing lost messages.

## Options

### A. Scope at ingest — only fetch what the agent should answer
Cheap on the wire. **Against:** anything outside scope is never journaled, so it cannot be
recovered when scope widens; the missed-DM bug is this option working as designed.

### B. Ingest everything; decide at delivery with filters
The journal is complete. Changing who the agent answers is changing a flag
(`--mentions-me`, `--from`, `--in`, `--kind`). **Against:** more traffic journaled than
answered; the read cursor must not skip past a message a filter rejected, or a filter becomes
a silent shredder — a real rule that must be tested, not assumed.

### C. Scope in the agent's prompt
No code. **Against:** the agent is handed everything and asked to ignore most of it — paying
tokens to not answer, and trusting judgement for what should be a rule.

## The axis that dominates
**Whether a message that wasn't answered can still be found.** Only B keeps it.

## Decision
**B.** The daemon delivers everything to the journal. Filters apply at `next`/`journal`, never
at ingest and never to the file. Self-echo suppression is the one exception — it runs before
the journal because it prevents a loop, not a policy.

**Accepting:** a complete journal on a busy group is large; retention is a separate concern.
**Not deciding:** what the *right* default filter is for a group — that is per deployment.

## What would change our mind
- Journal volume on a real group becomes an operational problem → add retention, not ingest
  scoping.
- A filter is found to have advanced the cursor past something → that is a bug in B's rule,
  not a reason to switch options.
