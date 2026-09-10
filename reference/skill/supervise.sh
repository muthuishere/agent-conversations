#!/usr/bin/env bash
# supervise.sh — keep a responder alive across shift changes and crashes.
#
# The supervisor is deliberately NOT an agent. An agent supervising an agent
# just moves the liveness problem up a level: now two things can die quietly.
# This is a shell loop blocked in wait(2) — it costs nothing and it cannot
# "forget" to check, because it has nothing to forget.
#
# It reads the responder's exit code and acts on it:
#
#   0   answered a batch          -> respawn immediately (more may be waiting)
#   64  nothing arrived           -> respawn immediately (the tier does the waiting)
#   75  shift ended normally      -> respawn immediately (fresh context)
#   69  the DAEMON is dead        -> do not spin; back off and alert
#   *   unexpected                -> exponential back-off and alert
#
# Everything the responder needs to resume lives on disk (journal, read cursor,
# ack marks, backoff tier), so a replacement picks up mid-stream with no handoff.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RESPONDER="${RESPONDER:-$HERE/poll-responder.sh}"
BACKOFF_MIN="${BACKOFF_MIN:-5}"
BACKOFF_MAX="${BACKOFF_MAX:-300}"
ALERT_CMD="${ALERT_CMD:-}"          # optional: notified on 69 / unexpected exits
LOG="${SUPERVISE_LOG:-/dev/stderr}"

log() { printf '%s supervise: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >>"$LOG"; }
alert() { [ -n "$ALERT_CMD" ] && printf '%s\n' "$*" | sh -c "$ALERT_CMD" >/dev/null 2>&1 || true; }

backoff=$BACKOFF_MIN
shifts=0
trap 'log "supervisor stopping (signal)"; exit 0' INT TERM

log "starting; responder=$RESPONDER"
while :; do
  "$RESPONDER" "$@"
  code=$?
  case $code in
    0|64|75)
      shifts=$((shifts+1))
      [ $code -eq 75 ] && log "shift $shifts ended cleanly; starting the next one"
      backoff=$BACKOFF_MIN          # healthy: reset the back-off
      ;;
    69)
      # The listener is dead. Respawning the responder cannot fix that, so do
      # not hot-loop against a corpse -- back off, and make it loud.
      log "daemon is not delivering (69); backing off ${backoff}s"
      alert "conversation daemon is down: responder exited 69"
      sleep "$backoff"
      backoff=$(( backoff*2 > BACKOFF_MAX ? BACKOFF_MAX : backoff*2 ))
      ;;
    *)
      log "responder exited $code (unexpected); backing off ${backoff}s"
      alert "conversation responder exited $code"
      sleep "$backoff"
      backoff=$(( backoff*2 > BACKOFF_MAX ? BACKOFF_MAX : backoff*2 ))
      ;;
  esac
done
