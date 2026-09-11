#!/usr/bin/env bash
# session-router.sh — ONE PERSISTENT AGENT SESSION PER CONVERSATION, resumed on
# every inbound message, with NO LOOP ANYWHERE.
#
#   session-router.sh --conversation <id> [--runtime claude|devin|codex]
#                     [--cwd DIR] [--new] [--print-id] [--registry PATH]
#                     [--timeout N] [--dry-run]
#                     [--tag NAME] [--concurrency N] [--lock-timeout N]
#                     [--max-age-days N] [--runtimes-dir DIR] [--list] [--evict]
#
# The message text arrives on STDIN. The agent's reply leaves on STDOUT.
#
# Why this exists (see SESSIONS.md):
#   POLLING.md's responder gives you an attended agent by having a SUBAGENT host
#   a blocking poll. That costs ~6-7 turns an idle hour and the host is mortal.
#   This file is the other trade: the agent is never resident at all. The daemon
#   coalesces a batch (INTERFACES.md §4), groups it by conversation, and invokes
#   THIS script once per conversation. The script looks the conversation up in a
#   registry, resumes that conversation's agent session, gets one answer, and
#   exits. Nothing loops, nothing sleeps, nothing is alive between messages —
#   and the conversation is still multi-turn, because the RUNTIME remembers.
#
# The one idea:
#   conversationId -> agent sessionId, persisted. That map is the whole product.
#   Multi-user isolation is not a feature we implement; it is what having a
#   SEPARATE session id per conversation gives us for free. Two users can never
#   see each other's context because they were never in the same session.
#
# Exit codes — the daemon's own (INTERFACES.md §1), nothing invented:
#   0   the agent answered; the reply is on stdout
#   1   unexpected error
#   65  bad arguments / unknown runtime / empty message / no session id yet
#   66  another message for THIS conversation is still being processed, or the
#       concurrency pool stayed full past --lock-timeout
#   69  the runtime adapter failed — a dead/invalid session id lands here, LOUDLY
#
# Dependencies: bash, coreutils, python3 (JSON only — same as poll-responder.sh).
set -euo pipefail

EXIT_OK=0; EXIT_BADARGS=65; EXIT_CONFLICT=66; EXIT_RUNTIME=69

# ------------------------------------------------------------------ defaults
CONVERSATION=""
RUNTIME="${SESSION_ROUTER_RUNTIME:-claude}"
CWD="$PWD"
TAG="${CONVO_TAG:-default}"
REGISTRY=""
RUNTIMES_DIR=""
TIMEOUT="${SESSION_ROUTER_TIMEOUT:-300}"
CONCURRENCY="${SESSION_ROUTER_CONCURRENCY:-4}"
LOCK_TIMEOUT="${SESSION_ROUTER_LOCK_TIMEOUT:-120}"
MAX_AGE_DAYS="${SESSION_ROUTER_MAX_AGE_DAYS:-14}"
FORCE_NEW=0
PRINT_ID=0
DRY_RUN=0
DO_LIST=0
DO_EVICT=0

HOME_DIR="${AGENT_CONVERSATIONS_HOME:-$HOME/.config/agent-conversations}"

# stderr is ALWAYS one JSON object with a stable `code` (INTERFACES.md §1). A
# router that fails in prose forces the caller to parse English to decide what
# to do, and it will get it wrong.
fail() {
  local code="$1" exit_code="$2"; shift 2
  python3 - "$code" "$*" <<'PY' >&2
import json, sys
print(json.dumps({"error": {"code": sys.argv[1], "message": sys.argv[2]}}))
PY
  exit "$exit_code"
}
die() { fail badArguments "$EXIT_BADARGS" "session-router: $*"; }

# --------------------------------------------------------------- arg parsing
while [ $# -gt 0 ]; do
  case "$1" in
    --conversation) CONVERSATION="${2:?--conversation needs a value}"; shift 2 ;;
    --runtime)      RUNTIME="${2:?--runtime needs a value}"; shift 2 ;;
    --cwd)          CWD="${2:?--cwd needs a value}"; shift 2 ;;
    --tag)          TAG="${2:?--tag needs a value}"; shift 2 ;;
    --registry)     REGISTRY="${2:?--registry needs a value}"; shift 2 ;;
    --runtimes-dir) RUNTIMES_DIR="${2:?--runtimes-dir needs a value}"; shift 2 ;;
    --timeout)      TIMEOUT="${2:?--timeout needs a value}"; shift 2 ;;
    --concurrency)  CONCURRENCY="${2:?--concurrency needs a value}"; shift 2 ;;
    --lock-timeout) LOCK_TIMEOUT="${2:?--lock-timeout needs a value}"; shift 2 ;;
    --max-age-days) MAX_AGE_DAYS="${2:?--max-age-days needs a value}"; shift 2 ;;
    --new)          FORCE_NEW=1; shift ;;
    --print-id)     PRINT_ID=1; shift ;;
    --dry-run)      DRY_RUN=1; shift ;;
    --list)         DO_LIST=1; shift ;;
    --evict)        DO_EVICT=1; shift ;;
    -h|--help)      sed -n '2,32p' "$0"; exit "$EXIT_OK" ;;
    *)              die "unknown argument \"$1\"" ;;
  esac
done

[ -n "$REGISTRY" ] || REGISTRY="${SESSION_ROUTER_REGISTRY:-$HOME_DIR/sessions.$TAG.json}"
if [ -z "$RUNTIMES_DIR" ]; then
  RUNTIMES_DIR="${SESSION_ROUTER_RUNTIMES:-$(cd "$(dirname "$0")" && pwd)/runtimes}"
fi
LOCK_ROOT="$HOME_DIR/locks"
mkdir -p "$(dirname "$REGISTRY")" "$LOCK_ROOT"

# ============================================================== the registry
#
# ONE flat JSON object: every top-level key IS a conversation id, mapping to one
# record. There is deliberately NO envelope ({"version":…,"conversations":{…}}):
# an envelope introduces reserved names, and a conversation id is an OPAQUE
# string chosen by a channel we do not control. A channel that names a room
# "version" must not corrupt the file.
#
#   { "<conversationId>": { "runtime": "claude|devin|codex",
#                           "sessionId": "…", "createdAt": "…",
#                           "lastUsedAt": "…", "turns": 12, "cwd": "…" } }
#
# Written atomically (tmp in the same directory + rename(2)); a reader therefore
# sees the old file or the new one, never a half-written one. Read-modify-write
# is serialised by a lock, because two conversations updating their own records
# concurrently are still two writers of ONE file.
#
# CORRUPT FILE POLICY: a truncated or unparseable registry is MOVED ASIDE to
# <registry>.corrupt.<epoch>, a warning goes to stderr, and we carry on with an
# empty map. Refusing to start would make one bad byte a total outage for every
# user; silently deleting it would destroy the evidence. Moving it aside costs
# every conversation its memory ONCE — the next message opens a fresh session —
# which is the same graceful degradation as eviction, and it is recoverable by
# hand from the file we kept.
#
# EVICTION: age-based on lastUsedAt, default 14 days, applied lazily on every
# write (no cron, nothing extra to keep alive). Age, not an LRU count cap,
# because the cost being bounded is the RUNTIME's transcript storage, which is
# itself age-shaped, and because a count cap evicts a quiet-but-alive
# conversation the moment a crowd arrives — exactly the user who will notice.
# Evicting only drops the MAPPING; the runtime's own transcript is untouched.

REG_PY='
import json, os, sys, time, datetime

def load(path):
    if not os.path.exists(path):
        return {}, None
    try:
        with open(path) as f:
            data = json.load(f)
        if not isinstance(data, dict):
            raise ValueError("registry root is not an object")
        return data, None
    except Exception as e:
        moved = "%s.corrupt.%d" % (path, int(time.time()))
        try:
            os.rename(path, moved)
        except OSError:
            moved = None
        return {}, "%s (moved aside to %s)" % (e, moved)

def save(path, data, max_age_days):
    # lazy age-based eviction, on every write
    if max_age_days > 0:
        cutoff = datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(days=max_age_days)
        for cid in list(data):
            used = (data[cid] or {}).get("lastUsedAt")
            try:
                ts = datetime.datetime.strptime(used, "%Y-%m-%dT%H:%M:%SZ").replace(
                    tzinfo=datetime.timezone.utc)
            except Exception:
                continue
            if ts < cutoff:
                del data[cid]
                sys.stderr.write(json.dumps({"evicted": {"conversationId": cid,
                                                         "lastUsedAt": used}}) + "\n")
    tmp = "%s.tmp.%d" % (path, os.getpid())
    with open(tmp, "w") as f:
        json.dump(data, f, indent=2, sort_keys=True)
        f.write("\n")
        f.flush()
        os.fsync(f.fileno())
    os.rename(tmp, path)   # atomic within one filesystem
'

reg_get() {   # reg_get <registry> <conversationId> -> "runtime\tsessionId\tturns" or ""
  python3 - "$REGISTRY" "$1" <<PY
$REG_PY
data, warn = load(sys.argv[1])
if warn:
    sys.stderr.write(json.dumps({"warning": {"code": "registryCorrupt", "message": warn}}) + "\n")
r = data.get(sys.argv[2])
if r:
    print("\t".join([str(r.get("runtime", "")), str(r.get("sessionId") or ""), str(r.get("turns", 0))]))
PY
}

reg_put() {   # reg_put <cid> <runtime> <sessionId> <cwd> <bumpTurn 0|1>
  python3 - "$REGISTRY" "$1" "$2" "$3" "$4" "$5" "$MAX_AGE_DAYS" <<PY
$REG_PY
path, cid, runtime, sid, cwd, bump, max_age = sys.argv[1:8]
data, warn = load(path)
if warn:
    sys.stderr.write(json.dumps({"warning": {"code": "registryCorrupt", "message": warn}}) + "\n")
now = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
rec = data.get(cid) or {"createdAt": now, "turns": 0}
rec["runtime"] = runtime
rec["sessionId"] = sid or rec.get("sessionId")
rec["cwd"] = cwd
rec["lastUsedAt"] = now
rec.setdefault("createdAt", now)
if bump == "1":
    rec["turns"] = int(rec.get("turns", 0)) + 1
data[cid] = rec
save(path, data, int(max_age))
PY
}

reg_list() {
  python3 - "$REGISTRY" <<PY
$REG_PY
data, warn = load(sys.argv[1])
if warn:
    sys.stderr.write(json.dumps({"warning": {"code": "registryCorrupt", "message": warn}}) + "\n")
print(json.dumps(data, indent=2, sort_keys=True))
PY
}

# ================================================================== locking
#
# mkdir(2) is the portable atomic test-and-set: it either creates the directory
# or fails, with no window in between. flock(1) is not on macOS by default and
# `set -o noclobber` loses to a shell that re-execs; this works everywhere.
#
# A lock holding a pid that is no longer alive is STALE and is reclaimed
# silently — the alternative is one crashed router wedging a user forever.

lock_acquire() {  # lock_acquire <dir> <timeout-seconds> ; 0 = got it, 1 = timed out
  local dir="$1" budget="$2" waited=0
  while :; do
    if mkdir "$dir" 2>/dev/null; then
      printf '%s\n' "$$" > "$dir/pid"
      return 0
    fi
    local holder
    holder="$(cat "$dir/pid" 2>/dev/null || true)"
    if [ -n "$holder" ] && ! kill -0 "$holder" 2>/dev/null; then
      rm -rf "$dir"; continue                      # stale: the holder is dead
    fi
    [ "$waited" -ge "$budget" ] && return 1
    sleep 1; waited=$(( waited + 1 ))
  done
}

LOCKS_HELD=()
# NOTE: an EXIT trap must never end on a failing command. Under `set -e` that
# status REPLACES the real exit code, so every `fail` would report 1 and the
# whole point of having distinct exit codes would be lost — silently.
release_all() {
  local d
  for d in "${LOCKS_HELD[@]:-}"; do
    if [ -n "$d" ]; then rm -rf "$d"; fi
  done
  return 0
}
trap release_all EXIT INT TERM

# A conversation id is an opaque channel string, so it is not a safe filename.
# Hash it: collision-free enough for a lock name, and the registry still holds
# the real id.
conv_key() { printf '%s' "$1" | shasum | cut -c1-16; }

# ------------------------------------------------------- bounded concurrency
# N slot directories. 20 users messaging at once must not fork 20 agents: that
# is a fork bomb made of LLMs, and the box, not the queue, is what fails.
# Different conversations run in PARALLEL up to N; the (N+1)th waits its turn.
slot_acquire() {
  local budget="$LOCK_TIMEOUT" waited=0 i
  mkdir -p "$LOCK_ROOT/slots.$TAG"
  while :; do
    i=1
    while [ "$i" -le "$CONCURRENCY" ]; do
      if lock_acquire "$LOCK_ROOT/slots.$TAG/$i" 0; then
        LOCKS_HELD+=("$LOCK_ROOT/slots.$TAG/$i"); SLOT="$i"; return 0
      fi
      i=$(( i + 1 ))
    done
    [ "$waited" -ge "$budget" ] && return 1
    sleep 1; waited=$(( waited + 1 ))
  done
}

# =================================================== runtime adapter contract
#
# runtimes/<name>.sh is the seam. Adding a runtime must not touch this file.
#
#   invocation   runtimes/<name>.sh          — message text on STDIN
#                runtimes/<name>.sh --mint-id — print a caller-chosen session id,
#                                               or exit 3 if this runtime only
#                                               reveals an id after the first run
#   env in       SR_SESSION_ID   the id to resume, or "" for a new session
#                SR_NEW          "1" when a brand-new session is required
#                SR_CWD          working directory for the agent
#                SR_TIMEOUT      seconds
#                SR_SESSION_ID_OUT  path the adapter MUST write the authoritative
#                                   session id to (this is how Devin and Codex,
#                                   which MINT their own ids, report them back)
#   stdout       the reply, verbatim. stderr: diagnostics. exit 0 ok, non-zero fail.

# Sets $ADAPTER. Deliberately NOT a command substitution: `fail` calls `exit`,
# and inside $( ) that exits the subshell only — the error message would print
# and the real exit code would be lost.
ADAPTER=""
resolve_adapter() {
  ADAPTER="$RUNTIMES_DIR/$1.sh"
  [ -f "$ADAPTER" ] || fail unknownRuntime "$EXIT_BADARGS" \
    "no adapter for runtime \"$1\" — expected an executable at $ADAPTER"
  [ -x "$ADAPTER" ] || fail unknownRuntime "$EXIT_BADARGS" "adapter $ADAPTER is not executable"
}

# ====================================================================== main

if [ "$DO_LIST" = 1 ]; then reg_list; exit "$EXIT_OK"; fi
if [ "$DO_EVICT" = 1 ]; then
  # a write with no other change: save() runs the age sweep
  python3 - "$REGISTRY" "$MAX_AGE_DAYS" <<PY
$REG_PY
data, warn = load(sys.argv[1])
save(sys.argv[1], data, int(sys.argv[2]))
PY
  exit "$EXIT_OK"
fi

[ -n "$CONVERSATION" ] || die "--conversation is required"
resolve_adapter "$RUNTIME"

CONV_LOCK="$LOCK_ROOT/conv.$TAG.$(conv_key "$CONVERSATION").lock"

# ------------------------------------------------- per-conversation serialisation
# Two messages from the SAME person must never run concurrently: they would be
# two processes resuming one session id, and the runtime's transcript is a
# single file. Interleaving them corrupts the conversation — which is exactly
# the same reasoning as the daemon's single-consumer lock (ARCHITECTURE §5.9),
# one level up. Different conversations are never blocked by this.
if ! lock_acquire "$CONV_LOCK" "$LOCK_TIMEOUT"; then
  fail conversationBusy "$EXIT_CONFLICT" \
    "another message for conversation \"$CONVERSATION\" is still being processed (waited ${LOCK_TIMEOUT}s); messages to one conversation are serialised on purpose"
fi
LOCKS_HELD+=("$CONV_LOCK")

# ------------------------------------------------------------- resolve session
EXISTING="$(reg_get "$CONVERSATION" || true)"
SESSION_ID=""
if [ -n "$EXISTING" ]; then
  SESSION_ID="$(printf '%s' "$EXISTING" | cut -f2)"
  EXISTING_RUNTIME="$(printf '%s' "$EXISTING" | cut -f1)"
  # The registry is the authority on which runtime owns this conversation. A
  # caller passing a different --runtime would otherwise hand a Claude session
  # id to Devin, which fails in a way nobody can read.
  if [ -n "$EXISTING_RUNTIME" ] && [ "$EXISTING_RUNTIME" != "$RUNTIME" ] && [ "$FORCE_NEW" = 0 ]; then
    fail runtimeMismatch "$EXIT_BADARGS" \
      "conversation \"$CONVERSATION\" is bound to runtime \"$EXISTING_RUNTIME\"; refusing to resume it as \"$RUNTIME\" — pass --new to rebind it"
  fi
fi
[ "$FORCE_NEW" = 1 ] && SESSION_ID=""

IS_NEW=0
if [ -z "$SESSION_ID" ]; then
  IS_NEW=1
  # Ask the adapter whether this runtime lets the CALLER choose the id.
  # Claude does (a UUID we pick); Devin and Codex mint their own and tell us
  # afterwards, via SR_SESSION_ID_OUT.
  set +e
  SESSION_ID="$("$ADAPTER" --mint-id 2>/dev/null)"
  set -e
fi

# ------------------------------------------------------------------ --print-id
if [ "$PRINT_ID" = 1 ]; then
  if [ -z "$SESSION_ID" ]; then
    fail noSessionYet "$EXIT_BADARGS" \
      "conversation \"$CONVERSATION\" has no session yet and runtime \"$RUNTIME\" mints its id on first use — send it a message first"
  fi
  # A minted id is registered here so --print-id is STABLE: asking twice must
  # not hand out two different ids. No agent is invoked either way.
  [ "$IS_NEW" = 1 ] && reg_put "$CONVERSATION" "$RUNTIME" "$SESSION_ID" "$CWD" 0
  printf '%s\n' "$SESSION_ID"
  exit "$EXIT_OK"
fi

# --------------------------------------------------------------- the message
WORK="$(mktemp -d "${TMPDIR:-/tmp}/session-router.XXXXXX")"
cleanup() { rm -rf "$WORK"; release_all; return 0; }
trap cleanup EXIT INT TERM

cat > "$WORK/message"
[ -s "$WORK/message" ] || fail emptyMessage "$EXIT_BADARGS" \
  "no message on stdin — the router answers a message, it does not invent one"

if [ "$DRY_RUN" = 1 ]; then
  python3 - "$CONVERSATION" "$RUNTIME" "$SESSION_ID" "$IS_NEW" "$ADAPTER" "$CWD" "$TIMEOUT" "$REGISTRY" <<'PY'
import json, sys
c, r, s, new, a, cwd, t, reg = sys.argv[1:9]
print(json.dumps({"dryRun": True, "conversationId": c, "runtime": r,
                  "sessionId": s or None, "newSession": new == "1",
                  "adapter": a, "cwd": cwd, "timeoutSec": int(t),
                  "registry": reg}, indent=2))
PY
  exit "$EXIT_OK"
fi

if ! slot_acquire; then
  fail concurrencyFull "$EXIT_CONFLICT" \
    "all $CONCURRENCY router slots were busy for ${LOCK_TIMEOUT}s; raise --concurrency or let the queue drain"
fi

# ------------------------------------------------------------- run the agent
printf '%s' "$SESSION_ID" > "$WORK/sid"

# Called as `run_adapter || RC=$?`, which suspends `set -e` for the whole body.
# Do NOT re-enable it before `return`: a non-zero return under `set -e` aborts
# the script at the return statement, so the caller's error path never runs.
run_adapter() {
  local rc=0
  SR_SESSION_ID="$SESSION_ID" SR_NEW="$IS_NEW" SR_CWD="$CWD" \
  SR_TIMEOUT="$TIMEOUT" SR_SESSION_ID_OUT="$WORK/sid" \
  SR_CONVERSATION="$CONVERSATION" \
    "$ADAPTER" < "$WORK/message" > "$WORK/out" 2> "$WORK/err" &
  local pid=$! waited=0
  while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt "$TIMEOUT" ]; do
    sleep 1; waited=$(( waited + 1 ))
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null; sleep 1; kill -KILL "$pid" 2>/dev/null
    printf 'session-router: runtime "%s" exceeded %ss and was killed\n' "$RUNTIME" "$TIMEOUT" >&2
    rc=124
  else
    wait "$pid"; rc=$?
  fi
  return $rc
}

RC=0
run_adapter || RC=$?

if [ "$RC" -ne 0 ]; then
  # LOUDLY. An invalid or expired session id lands here, and the adapter's own
  # stderr is the most useful thing we have — pass it through verbatim rather
  # than flattening it into "runtime failed".
  cat "$WORK/err" >&2 || true
  fail runtimeFailed "$EXIT_RUNTIME" \
    "runtime \"$RUNTIME\" failed (exit $RC) for conversation \"$CONVERSATION\" session \"${SESSION_ID:-<new>}\" — see stderr above"
fi

# The adapter is the authority on the id after the fact: Devin and Codex mint
# one on the first run, and that is the ONLY moment we can learn it.
RESOLVED="$(cat "$WORK/sid" 2>/dev/null || true)"
[ -n "$RESOLVED" ] || RESOLVED="$SESSION_ID"
[ -n "$RESOLVED" ] || fail noSessionId "$EXIT_RUNTIME" \
  "runtime \"$RUNTIME\" answered but reported no session id; the next message could not resume it, so this turn is being treated as a failure"

reg_put "$CONVERSATION" "$RUNTIME" "$RESOLVED" "$CWD" 1
cat "$WORK/out"
exit "$EXIT_OK"
