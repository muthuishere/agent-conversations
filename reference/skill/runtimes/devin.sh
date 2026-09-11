#!/usr/bin/env bash
# runtimes/devin.sh — session adapter for the Devin CLI.
#
# Same contract as every other adapter (SESSIONS.md §3).
#
# MEASURED (devin 2026.8.18):
#   devin --resume <id> -p "<text>"     continues an existing session, full context
#
# TWO traps, both real:
#
#   1. `--resume <id>` MUST come BEFORE `-p`. `-p/--print` takes an OPTIONAL
#      inline prompt, so `devin -p "<text>" --resume <id>` lets clap bind the
#      prompt to -p and then re-parse; put --resume first and the prompt is
#      unambiguous. Getting this wrong does not error — it runs the wrong thing.
#
#   2. Devin gives the caller NO way to choose a session id, and does not print
#      the id it minted. The only enumeration is `devin list --format json`
#      (fields: id, working_directory, title), scoped to the CURRENT DIRECTORY.
#      So a new session's id has to be DISCOVERED by diffing that list around
#      the first invocation — which is a race if two new conversations start in
#      the same directory at the same time. We serialise minting with a lock and
#      say so out loud rather than pretending the race is not there.
#
#      This is the argument for Claude's caller-chosen uuid in one paragraph: an
#      id you pick cannot be raced for, cannot be mis-attributed, and exists
#      before the agent does.
set -euo pipefail

if [ "${1:-}" = "--mint-id" ]; then
  exit 3            # Devin mints its own; we learn it after the first run.
fi

DEVIN_BIN="${SESSION_ROUTER_DEVIN:-devin}"
command -v "$DEVIN_BIN" >/dev/null 2>&1 || {
  printf 'devin.sh: %s is not on PATH\n' "$DEVIN_BIN" >&2; exit 127; }

TEXT="$(cat)"
[ -n "$TEXT" ] || { printf 'devin.sh: empty message on stdin\n' >&2; exit 2; }

cd "${SR_CWD:-$PWD}"

list_ids() { "$DEVIN_BIN" list --format json 2>/dev/null | python3 -c '
import json, sys
try:
    rows = json.load(sys.stdin)
except Exception:
    rows = []
for r in rows if isinstance(rows, list) else []:
    if isinstance(r, dict) and r.get("id"):
        print(r["id"])
' | sort; }

if [ -n "${SR_SESSION_ID:-}" ] && [ "${SR_NEW:-0}" != "1" ]; then
  # --resume BEFORE -p. Not a style preference; see trap 1 above.
  "$DEVIN_BIN" --resume "$SR_SESSION_ID" -p "$TEXT" < /dev/null
  [ -n "${SR_SESSION_ID_OUT:-}" ] && printf '%s' "$SR_SESSION_ID" > "$SR_SESSION_ID_OUT"
  exit 0
fi

# ---- new session: mint under a lock, then discover the id by diffing the list
MINT_LOCK="${AGENT_CONVERSATIONS_HOME:-$HOME/.config/agent-conversations}/locks/devin-mint.lock"
mkdir -p "$(dirname "$MINT_LOCK")"
waited=0
while ! mkdir "$MINT_LOCK" 2>/dev/null; do
  holder="$(cat "$MINT_LOCK/pid" 2>/dev/null || true)"
  if [ -n "$holder" ] && ! kill -0 "$holder" 2>/dev/null; then rm -rf "$MINT_LOCK"; continue; fi
  waited=$(( waited + 1 ))
  if [ "$waited" -gt "${SR_TIMEOUT:-300}" ]; then
    printf 'devin.sh: timed out waiting to mint a session id\n' >&2; exit 1
  fi
  sleep 1
done
printf '%s\n' "$$" > "$MINT_LOCK/pid"
trap 'rm -rf "$MINT_LOCK"' EXIT

BEFORE="$(list_ids)"
"$DEVIN_BIN" -p "$TEXT" < /dev/null
AFTER="$(list_ids)"

NEW_ID="$(comm -13 <(printf '%s\n' "$BEFORE") <(printf '%s\n' "$AFTER") | head -1)"
if [ -z "$NEW_ID" ]; then
  printf 'devin.sh: the run finished but no new session appeared in `devin list` for %s — nothing resumable to record\n' "$PWD" >&2
  exit 1
fi
[ -n "${SR_SESSION_ID_OUT:-}" ] && printf '%s' "$NEW_ID" > "$SR_SESSION_ID_OUT"
exit 0
