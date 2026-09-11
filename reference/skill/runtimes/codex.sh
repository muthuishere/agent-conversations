#!/usr/bin/env bash
# runtimes/codex.sh — session adapter for the Codex CLI.
#
# Same contract as every other adapter (SESSIONS.md §3).
#
# MEASURED on this machine (codex-cli 0.153.4): `codex exec` is NOT single-shot
# any more. It has a `resume` subcommand, and it carries context:
#
#   $ codex exec --json --skip-git-repo-check -o last.txt "Reply with exactly one word: ZEBRA"
#   {"type":"thread.started","thread_id":"01a08e82-d07a-7dd3-a8a3-84d3fafcc077"}
#   {"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"ZEBRA"}}
#
#   $ codex exec resume 01a08e82-… --json -o last2.txt "What one word did you just say?"
#   {"type":"thread.started","thread_id":"01a08e82-d07a-7dd3-a8a3-84d3fafcc077"}
#   {"type":"item.completed","item":{"item":…,"text":"ZEBRA"}}   # same id, remembered
#
# Codex MINTS its own id, so --mint-id exits 3 — but unlike Devin it announces
# the id on the FIRST event of the first run, so there is no discovery race at
# all: we read `thread.started` out of the stream we are already consuming.
#
# Two flags do real work here:
#   --json  is what makes the id observable. Without it the id is not printed.
#   -o FILE is what makes the REPLY extractable without parsing the event
#           stream for the last agent_message. Use the tool's own machine-
#           readable mode instead of ad-hoc parsing (ARCHITECTURE §9).
set -euo pipefail

if [ "${1:-}" = "--mint-id" ]; then
  # Codex chooses the thread id itself; it is knowable only after the first run.
  exit 3
fi

CODEX_BIN="${SESSION_ROUTER_CODEX:-codex}"
command -v "$CODEX_BIN" >/dev/null 2>&1 || {
  printf 'codex.sh: %s is not on PATH\n' "$CODEX_BIN" >&2; exit 127; }

TEXT="$(cat)"
[ -n "$TEXT" ] || { printf 'codex.sh: empty message on stdin\n' >&2; exit 2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/codex-adapter.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

# MEASURED: `codex exec` accepts -C/--cd but `codex exec resume` REJECTS it
# ("unexpected argument '-C' found"), so the two branches cannot share a flag
# set. cd instead — it is the one thing both subcommands agree on.
cd "${SR_CWD:-$PWD}"

# stdin MUST be /dev/null: with a prompt argument AND piped stdin, codex appends
# stdin as a <stdin> block — the message would arrive twice.
if [ "${SR_NEW:-0}" = "1" ] || [ -z "${SR_SESSION_ID:-}" ]; then
  "$CODEX_BIN" exec --json --skip-git-repo-check \
      -o "$WORK/reply" "$TEXT" < /dev/null > "$WORK/events" 2> "$WORK/err" || {
        cat "$WORK/err" >&2; exit 1; }
else
  "$CODEX_BIN" exec resume "$SR_SESSION_ID" --json --skip-git-repo-check \
      -o "$WORK/reply" "$TEXT" < /dev/null \
      > "$WORK/events" 2> "$WORK/err" || { cat "$WORK/err" >&2; exit 1; }
fi

THREAD_ID="$(python3 - "$WORK/events" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    line = line.strip()
    if not line.startswith("{"):
        continue
    try:
        ev = json.loads(line)
    except ValueError:
        continue
    if ev.get("type") == "thread.started" and ev.get("thread_id"):
        print(ev["thread_id"]); break
PY
)"

if [ -z "$THREAD_ID" ]; then
  cat "$WORK/err" >&2 || true
  printf 'codex.sh: no thread.started event — cannot record a resumable session id\n' >&2
  exit 1
fi

[ -n "${SR_SESSION_ID_OUT:-}" ] && printf '%s' "$THREAD_ID" > "$SR_SESSION_ID_OUT"
cat "$WORK/reply"
exit 0
