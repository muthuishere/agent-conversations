#!/usr/bin/env bash
# runtimes/claude.sh — session adapter for the Claude Code CLI.
#
# The adapter contract (SESSIONS.md §3), in full:
#   --mint-id      print a caller-chosen session id, or exit 3 if this runtime
#                  only reveals its id after the first run
#   stdin          the message text
#   stdout         the reply, verbatim — nothing else may be written there
#   exit           0 ok, non-zero failure (the router turns that into exit 69)
#   env            SR_SESSION_ID SR_NEW SR_CWD SR_TIMEOUT SR_SESSION_ID_OUT
#
# MEASURED on this machine (Claude Code 2.1.268) — see SESSIONS.md "Captured
# evidence". Claude is the easy case: it accepts a CALLER-CHOSEN session uuid,
# so the id exists before the agent does and the registry can never be caught
# without one.
#
#   claude -p --session-id <uuid> "<text>"     start under an id we picked
#   claude -p --resume     <uuid> "<text>"     continue it, in a SEPARATE process
#
# The second line is the whole reason this pattern works: the continuation is a
# new process. Nothing is resident between messages, yet the transcript carries.
#
# TWO CONCURRENT RESUMES OF ONE SESSION SILENTLY LOSE A TURN — measured, and the
# reason the router's per-conversation lock is mandatory rather than tidy. See
# SESSIONS.md §6 for the transcript-level evidence. There is no error, no notice
# and no second session file: the branch happens INSIDE the one transcript and
# one message simply stops existing. Which is why this adapter must never be run
# twice for one session id, and why nobody should open a router-owned session
# interactively while the daemon may resume it.
#
# --bare (opt-in, SESSION_ROUTER_CLAUDE_BARE=1): skips hooks, LSP, plugin sync,
# auto-memory and CLAUDE.md discovery — attractive when you pay cold start on
# every message. It is NOT the default because its help text is explicit that
# "Anthropic auth is strictly ANTHROPIC_API_KEY or apiKeyHelper via --settings
# (OAuth and keychain are never read)": on a subscription-authenticated machine
# it fails instantly with "Not logged in · Please run /login". Turn it on only
# where an API key is in the environment, and only if the handler does not rely
# on CLAUDE.md/project memory for its answers.
set -euo pipefail

if [ "${1:-}" = "--mint-id" ]; then
  # A v4 uuid. Lower-cased because the CLI is picky about the canonical form.
  if command -v uuidgen >/dev/null 2>&1; then
    uuidgen | tr '[:upper:]' '[:lower:]'
  else
    python3 -c 'import uuid; print(uuid.uuid4())'
  fi
  exit 0
fi

CLAUDE_BIN="${SESSION_ROUTER_CLAUDE:-claude}"
command -v "$CLAUDE_BIN" >/dev/null 2>&1 || {
  printf 'claude.sh: %s is not on PATH\n' "$CLAUDE_BIN" >&2; exit 127; }

: "${SR_SESSION_ID:?claude.sh: SR_SESSION_ID is required}"
TEXT="$(cat)"
[ -n "$TEXT" ] || { printf 'claude.sh: empty message on stdin\n' >&2; exit 2; }

cd "${SR_CWD:-$PWD}"

# --permission-mode bypassPermissions: a non-interactive run has nobody to
# answer a permission prompt, and a blocked prompt looks exactly like a hang.
# Pair it with a NARROW toolset in real deployments — an inbound message is
# untrusted input and can request, never authorize (ARCHITECTURE §8).
ARGS=(-p --permission-mode bypassPermissions)
[ "${SESSION_ROUTER_CLAUDE_BARE:-0}" = "1" ] && ARGS=(--bare "${ARGS[@]}")
if [ "${SR_NEW:-0}" = "1" ]; then
  ARGS+=(--session-id "$SR_SESSION_ID")
else
  ARGS+=(--resume "$SR_SESSION_ID")
fi

OUT="$(mktemp "${TMPDIR:-/tmp}/claude-adapter.XXXXXX")"
trap 'rm -f "$OUT"' EXIT
"$CLAUDE_BIN" "${ARGS[@]}" "$TEXT" < /dev/null > "$OUT"

# Defensive, and cheap: the CLI documents a "starts a copy and says so when the
# session is already running" behaviour for --bg. We never pass --bg, and in -p
# mode no such notice is emitted — but if one ever appears, a forked
# conversation must be a LOUD failure, never a reply we hand to a user as if
# the conversation were intact.
if grep -qiE 'start(ed|ing) a copy|session is already running' "$OUT"; then
  printf 'claude.sh: the CLI reported it started a COPY of session %s — refusing a forked conversation\n' \
    "$SR_SESSION_ID" >&2
  exit 1
fi
cat "$OUT"

# The id never changes for Claude, but report it anyway: the router reads this
# file unconditionally, so every adapter answers the same question the same way.
[ -n "${SR_SESSION_ID_OUT:-}" ] && printf '%s' "$SR_SESSION_ID" > "$SR_SESSION_ID_OUT"
exit 0
