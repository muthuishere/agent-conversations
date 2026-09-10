#!/usr/bin/env bash
# poll-responder.sh — the PORTABLE wake mechanism: a blocking poll with an
# escalating backoff, hosted by a subagent so the main session stays free.
#
#   ./poll-responder.sh [--tag NAME] [--tiers 120,240,360] [--max-block 540]
#                       [--answer-cmd '<cmd>'] [--answer-timeout 120]
#                       [--filter-args '...']
#                       [--max-cycles N] [--max-runtime SECONDS]
#                       [--once] [--state DIR] [--dry-run]
#
# Why this exists (see POLLING.md):
#   The zero-cost wakes are runtime-specific — one runtime re-invokes a session
#   when a background task exits, another only offers a stop-hook long-poll.
#   Neither is portable, and neither is asynchronous from the main session.
#   A subagent hosting THIS script is portable: the sleeps happen inside the
#   shell, so they cost zero tokens; a turn is spent only when the call returns.
#
# The one constraint that shapes the whole design:
#   A FOREGROUND TOOL CALL IS CAPPED (~600s in some runtimes, less in others).
#   So a 2+4+6-minute schedule CANNOT live inside one call. The backoff tier is
#   therefore carried in a STATE FILE ACROSS INVOCATIONS, and --max-block keeps
#   any single call safely under the cap.
#
# Exit codes are the daemon's own (INTERFACES.md §1) — none are invented here:
#   0   messages delivered on stdout (or piped to --answer-cmd)
#   64  aged out with nothing to deliver. NOT an error.
#   65  bad arguments
#   66  another consumer already holds this tag
#   69  the daemon is dead/stale — we refuse to sleep against a corpse
#   75  SHIFT OVER: --max-cycles / --max-runtime reached. Not an error and not
#       silence — it means "respawn me". The host subagent is REPLACEABLE, not
#       immortal: every byte of state lives on disk (journal, read cursor, ack
#       file, and this script's tier state file), so a fresh subagent resumes
#       with no handoff. See POLLING.md, "Shift change".
#
# Dependencies: the `convo` CLI and coreutils. python3 is used for JSON only.
set -euo pipefail

# ---------------------------------------------------------------- exit codes
EXIT_OK=0; EXIT_TIMEOUT=64; EXIT_BADARGS=65; EXIT_CONFLICT=66; EXIT_DEAD=69
EXIT_SHIFT_OVER=75  # poller-level, deliberately distinct from 64 and 69

# ------------------------------------------------------------------ defaults
TAG="${CONVO_TAG:-default}"
TIERS_RAW="120,240,360"
MAX_BLOCK=540              # safely below a ~600s foreground tool-call cap
ANSWER_CMD=""
ANSWER_TIMEOUT="${POLL_ANSWER_TIMEOUT:-120}"
FILTER_RAW=""
ONCE=0
DRY_RUN=0
MAX_CYCLES=0               # 0 = unlimited. A hosted listener should set this.
MAX_RUNTIME=0              # seconds of wall clock per shift. 0 = unlimited.
STATE_DIR=""
BATCH_MAX=500              # upper bound on messages handed over in one call

die() { printf '%s\n' "$*" >&2; exit "$EXIT_BADARGS"; }

# --------------------------------------------------------------- arg parsing
while [ $# -gt 0 ]; do
  case "$1" in
    --tag)          TAG="${2:?--tag needs a value}"; shift 2 ;;
    --tiers)        TIERS_RAW="${2:?--tiers needs a value}"; shift 2 ;;
    --max-block)    MAX_BLOCK="${2:?--max-block needs a value}"; shift 2 ;;
    --answer-cmd)   ANSWER_CMD="${2:?--answer-cmd needs a value}"; shift 2 ;;
    --answer-timeout) ANSWER_TIMEOUT="${2:?--answer-timeout needs a value}"; shift 2 ;;
    --filter-args)  FILTER_RAW="${2:?--filter-args needs a value}"; shift 2 ;;
    --max-cycles)   MAX_CYCLES="${2:?--max-cycles needs a value}"; shift 2 ;;
    --max-runtime)  MAX_RUNTIME="${2:?--max-runtime needs a value}"; shift 2 ;;
    --state)        STATE_DIR="${2:?--state needs a value}"; shift 2 ;;
    --once)         ONCE=1; shift ;;
    --dry-run)      DRY_RUN=1; shift ;;
    -h|--help)      sed -n '2,36p' "$0"; exit "$EXIT_OK" ;;
    *)              die "poll-responder: unknown argument \"$1\"" ;;
  esac
done

# ------------------------------------------------------- resolve the convo CLI
# $CONVO wins; then a `convo` on PATH; then the reference daemon next door, so
# the script works straight out of a clone with nothing installed.
HERE="$(cd "$(dirname "$0")" && pwd)"
if [ -n "${CONVO:-}" ]; then
  # shellcheck disable=SC2206
  CONVO_CMD=($CONVO)
elif command -v convo >/dev/null 2>&1; then
  CONVO_CMD=(convo)
elif [ -f "$HERE/../daemon/cli.js" ]; then
  CONVO_CMD=("${NODE:-node}" "$HERE/../daemon/cli.js")
else
  printf '%s\n' 'poll-responder: no `convo` CLI found (set $CONVO)' >&2
  exit "$EXIT_BADARGS"
fi

# --------------------------------------------------------------- validate
case "$MAX_BLOCK" in (''|*[!0-9]*) die "--max-block must be a whole number of seconds" ;; esac
case "$MAX_CYCLES" in (''|*[!0-9]*) die "--max-cycles must be a whole number" ;; esac
case "$MAX_RUNTIME" in (''|*[!0-9]*) die "--max-runtime must be a whole number of seconds" ;; esac
[ "$MAX_BLOCK" -gt 0 ] || die "--max-block must be > 0"
[ "$MAX_BLOCK" -le 590 ] || printf 'poll-responder: warning: --max-block %ss is close to the ~600s tool-call cap\n' "$MAX_BLOCK" >&2

TIERS=()
IFS=',' read -r -a _tiers <<< "$TIERS_RAW"
for t in "${_tiers[@]}"; do
  t="${t// /}"
  case "$t" in (''|*[!0-9]*) die "--tiers must be a comma-separated list of seconds, got \"$TIERS_RAW\"" ;; esac
  [ "$t" -gt 0 ] || die "--tiers values must be > 0"
  TIERS+=("$t")
done
[ "${#TIERS[@]}" -gt 0 ] || die "--tiers must list at least one tier"
LAST_TIER_INDEX=$(( ${#TIERS[@]} - 1 ))

# Filters are passed STRAIGHT THROUGH to the daemon — this script never decides
# who gets answered (ARCHITECTURE §6.2). eval'd so quoted values survive.
FILTER_ARGS=()
if [ -n "$FILTER_RAW" ]; then
  eval "FILTER_ARGS=($FILTER_RAW)" || die "--filter-args is not a parsable argument list"
fi

# ------------------------------------------------------------------ state
# {tier_index, last_message_at, consecutive_empty} — the tier must survive
# across invocations because no single call can span the whole schedule.
# The same file also carries the SHIFT counters (cycles, shift_started_at), for
# the same reason: the subagent hosting this poller is replaceable, so nothing
# it would need to hand over may live in its head.
if [ -z "$STATE_DIR" ]; then
  STATE_DIR="${AGENT_CONVERSATIONS_HOME:-$HOME/.config/agent-conversations}"
fi
mkdir -p "$STATE_DIR"
STATE_FILE="$STATE_DIR/poll-responder.$TAG.json"

TIER_INDEX=0
CONSECUTIVE_EMPTY=0
LAST_MESSAGE_AT="null"
CYCLES=0
SHIFT_STARTED_AT=0
NOW_EPOCH="$(date -u +%s)"

read_state() {
  [ -f "$STATE_FILE" ] || return 0
  local parsed
  # A missing OR CORRUPT state file resets to tier 0. Never abort on it: a
  # truncated JSON file must not take the listener down.
  parsed="$(python3 - "$STATE_FILE" <<'PY' 2>/dev/null || true
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    if not isinstance(d, dict): raise ValueError
    ti = int(d.get("tier_index", 0) or 0)
    ce = int(d.get("consecutive_empty", 0) or 0)
    lm = d.get("last_message_at")
    cy = int(d.get("cycles", 0) or 0)
    ss = int(d.get("shift_started_at", 0) or 0)
    print(max(ti, 0)); print(max(ce, 0)); print(json.dumps(lm))
    print(max(cy, 0)); print(max(ss, 0))
except Exception:
    pass
PY
)"
  [ -n "$parsed" ] || { printf 'poll-responder: state file unreadable — resetting to tier 0\n' >&2; return 0; }
  TIER_INDEX="$(printf '%s\n' "$parsed" | sed -n 1p)"
  CONSECUTIVE_EMPTY="$(printf '%s\n' "$parsed" | sed -n 2p)"
  LAST_MESSAGE_AT="$(printf '%s\n' "$parsed" | sed -n 3p)"
  CYCLES="$(printf '%s\n' "$parsed" | sed -n 4p)"
  SHIFT_STARTED_AT="$(printf '%s\n' "$parsed" | sed -n 5p)"
  [ "$TIER_INDEX" -le "$LAST_TIER_INDEX" ] || TIER_INDEX="$LAST_TIER_INDEX"
}

write_state() {
  local tmp="$STATE_FILE.$$.tmp"
  printf '{"tier_index":%s,"last_message_at":%s,"consecutive_empty":%s,"cycles":%s,"shift_started_at":%s,"tiers":[%s],"updated_at":"%s"}\n' \
    "$TIER_INDEX" "$LAST_MESSAGE_AT" "$CONSECUTIVE_EMPTY" "$CYCLES" "$SHIFT_STARTED_AT" \
    "$(IFS=,; echo "${TIERS[*]}")" \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$tmp" 2>/dev/null || return 0
  mv -f "$tmp" "$STATE_FILE" 2>/dev/null || rm -f "$tmp"
}

read_state
CURRENT_TIER="${TIERS[$TIER_INDEX]}"
[ "$SHIFT_STARTED_AT" -gt 0 ] || SHIFT_STARTED_AT="$NOW_EPOCH"

# ----------------------------------------------------------------- signals
# A SIGINT/SIGTERM is "stop waiting", not a fault: exit 64, leave the state file
# exactly as it was so the next invocation resumes on the same tier.
on_signal() { trap - INT TERM; printf 'poll-responder: interrupted while waiting\n' >&2; exit "$EXIT_TIMEOUT"; }
trap on_signal INT TERM

WORK="$(mktemp -d "${TMPDIR:-/tmp}/poll-responder.XXXXXX")"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

# ------------------------------------------------------------------ dry run
if [ "$DRY_RUN" -eq 1 ]; then
  printf '{"tag":"%s","tiers":[%s],"tier_index":%s,"current_tier":%s,"max_block":%s,"once":%s,"state_file":"%s","convo":"%s","filters":"%s","answer_cmd":"%s","cycles":%s,"max_cycles":%s,"max_runtime":%s}\n' \
    "$TAG" "$(IFS=,; echo "${TIERS[*]}")" "$TIER_INDEX" "$CURRENT_TIER" "$MAX_BLOCK" \
    "$ONCE" "$STATE_FILE" "${CONVO_CMD[*]}" "${FILTER_ARGS[*]:-}" "$ANSWER_CMD" \
    "$CYCLES" "$MAX_CYCLES" "$MAX_RUNTIME"
  exit "$EXIT_OK"
fi

# --------------------------------------------------------------- shift budget
# THE SUBAGENT HOSTING THIS POLLER IS NOT IMMORTAL. Every poll that returns adds
# context to it, so it will eventually hit a limit and die — and a listener that
# dies silently is indistinguishable from a quiet channel (daemon healthy,
# journal filling, nobody answering). So end the shift DELIBERATELY, with its
# own exit code, and let a supervisor start a replacement. The handoff is free:
# journal + read cursor + ack file + this state file hold everything.
budget_spent() {
  [ "$MAX_CYCLES" -gt 0 ] && [ "$CYCLES" -ge "$MAX_CYCLES" ] && return 0
  if [ "$MAX_RUNTIME" -gt 0 ]; then
    [ $(( $(date -u +%s) - SHIFT_STARTED_AT )) -ge "$MAX_RUNTIME" ] && return 0
  fi
  return 1
}

end_shift() {
  local why="$1"
  # Reset the shift counters so the REPLACEMENT starts a clean shift. The
  # backoff tier is deliberately NOT reset — that belongs to the conversation,
  # not to whoever happens to be holding the pager.
  CYCLES=0
  SHIFT_STARTED_AT=0
  write_state
  printf 'poll-responder: shift over (%s) — exit %s means RESPAWN ME, not an error and not silence\n' \
    "$why" "$EXIT_SHIFT_OVER" >&2
  exit "$EXIT_SHIFT_OVER"
}

# ----------------------------------------------------------------- liveness
# INTERFACES.md §2.5: never report "listening" from memory, and never sleep
# against a stale heartbeat — silence there is deafness, not quiet. We reuse the
# daemon's own health command and its own exit code (69) rather than
# reimplementing the staleness rule with a second, subtly different threshold.
daemon_alive() {
  "${CONVO_CMD[@]}" health --tag "$TAG" >/dev/null 2>&1
}

# ------------------------------------------------------------------- check
# One non-blocking drain. `wait --timeout 0` gives exactly the semantics needed:
# it drains the backlog, advances the read cursor on delivery, exits 0 with
# messages / 64 with none / 69 against a dead listener / 66 on a second consumer.
CHECK_FORMAT="compact"
[ -n "$ANSWER_CMD" ] && CHECK_FORMAT="ndjson"

check_now() {
  local rc=0
  set +e
  "${CONVO_CMD[@]}" wait --tag "$TAG" --timeout 0 --count "$BATCH_MAX" \
    --format "$CHECK_FORMAT" "${FILTER_ARGS[@]+"${FILTER_ARGS[@]}"}" \
    > "$WORK/out" 2> "$WORK/err"
  rc=$?
  set -e
  CYCLES=$(( CYCLES + 1 ))
  return $rc
}

# Bound the answer command: a slow or hung answerer must not become the reason
# nobody is listening. A FAILING answerer must not kill the poller either — the
# batch is still in the journal, so nothing was lost (INTERFACES.md §4).
run_answer() {
  local rc=0
  set +e
  # The redirect MUST be on the backgrounded command itself: a shell gives an
  # async command /dev/null for stdin unless it is redirected explicitly, so an
  # inherited redirect silently feeds the answerer nothing and it "succeeds"
  # having answered no one. That failure is invisible — exactly the shape this
  # whole design exists to avoid.
  "$WORK/answer.sh" < "$WORK/out" &
  local pid=$!
  local waited=0
  while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt "$ANSWER_TIMEOUT" ]; do
    sleep 1; waited=$(( waited + 1 ))
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null; sleep 1; kill -KILL "$pid" 2>/dev/null
    printf 'poll-responder: answer-cmd exceeded %ss and was killed\n' "$ANSWER_TIMEOUT" >&2
    rc=1
  else
    wait "$pid"; rc=$?
  fi
  set -e
  return $rc
}

deliver() {
  local n
  n="$(wc -l < "$WORK/out" | tr -d ' ')"
  if [ -n "$ANSWER_CMD" ]; then
    printf '#!/bin/sh\n%s\n' "$ANSWER_CMD" > "$WORK/answer.sh"
    chmod +x "$WORK/answer.sh"
    # Same environment the daemon gives a spawned handler (INTERFACES.md §4),
    # so one answerer script works under both wake paths unchanged.
    if CONVO_TAG="$TAG" CONVO_BATCH_COUNT="$n" \
       CONVO_HOME="${AGENT_CONVERSATIONS_HOME:-$HOME/.config/agent-conversations}" \
       CONVO_JOURNAL="${AGENT_CONVERSATIONS_HOME:-$HOME/.config/agent-conversations}/journal/$TAG.ndjson" \
       run_answer; then
      printf 'poll-responder: answered %s message(s)\n' "$n" >&2
    else
      printf 'poll-responder: answer-cmd failed (exit non-zero) on %s message(s) — they are still journalled\n' "$n" >&2
    fi
  else
    cat "$WORK/out"
  fi
  # Snap back to the first tier: a conversation has started (ARCHITECTURE §5.6).
  TIER_INDEX=0
  CONSECUTIVE_EMPTY=0
  LAST_MESSAGE_AT="\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\""
  write_state
  exit "$EXIT_OK"
}

empty_out() {
  # Escalate one tier, clamped at the last, then report "nothing arrived" with
  # the daemon's own code. 64 is never an error.
  [ "$TIER_INDEX" -lt "$LAST_TIER_INDEX" ] && TIER_INDEX=$(( TIER_INDEX + 1 ))
  CONSECUTIVE_EMPTY=$(( CONSECUTIVE_EMPTY + 1 ))
  write_state
  printf 'poll-responder: nothing arrived (tier now %ss, consecutive empty %s)\n' \
    "${TIERS[$TIER_INDEX]}" "$CONSECUTIVE_EMPTY" >&2
  exit "$EXIT_TIMEOUT"
}

fail_through() {
  # Any other daemon exit code is passed through untouched — 66 and 69 mean
  # different things and must not collapse into a generic failure.
  local rc="$1"
  cat "$WORK/err" >&2 || true
  exit "$rc"
}

# ============================================================ the poll itself
# 0. If the shift budget is already spent, hand over BEFORE doing any work.
budget_spent && end_shift "budget already spent on entry"

# 1. CHECK IMMEDIATELY. Never sleep before the first check: the backlog that
#    arrived while the agent was thinking is already on disk.
rc=0; check_now || rc=$?
case "$rc" in
  "$EXIT_OK")      deliver ;;
  "$EXIT_TIMEOUT") ;;                    # fall through to the sleep loop
  *)               fail_through "$rc" ;;
esac

# --once is a checkpoint: one look, no sleeping.
[ "$ONCE" -eq 1 ] && empty_out

# Nothing waiting and the shift is up: end it cleanly rather than starting a
# sleep the replacement would have to inherit.
budget_spent && end_shift "budget spent after the immediate check"

# 2. Sleep the CURRENT tier and re-check, never exceeding --max-block.
elapsed=0
while [ "$elapsed" -lt "$MAX_BLOCK" ]; do
  remaining=$(( MAX_BLOCK - elapsed ))
  nap="$CURRENT_TIER"
  [ "$nap" -gt "$remaining" ] && nap="$remaining"

  # Liveness BEFORE the sleep. Sleeping through a dead listener turns a fault
  # into "nobody messaged me", which is the failure this whole design exists to
  # prevent (ARCHITECTURE §5.8).
  if ! daemon_alive; then
    printf 'poll-responder: the daemon for tag "%s" is dead or stale — refusing to sleep against a corpse\n' "$TAG" >&2
    exit "$EXIT_DEAD"
  fi

  sleep "$nap"
  elapsed=$(( elapsed + nap ))

  rc=0; check_now || rc=$?
  case "$rc" in
    "$EXIT_OK")      deliver ;;
    "$EXIT_TIMEOUT") ;;
    *)               fail_through "$rc" ;;
  esac

  budget_spent && end_shift "budget spent mid-block"
done

# 3. --max-block reached with nothing to show: escalate and hand the turn back.
empty_out
