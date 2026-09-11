#!/usr/bin/env bash
# tests/session-router.test.sh — the captured proof for reference/skill/session-router.sh
#
#   ./tests/session-router.test.sh              offline only (no tokens, no network)
#   SESSION_ROUTER_E2E=1 ./tests/session-router.test.sh   also drives a REAL agent
#
# Everything runs in a throwaway $AGENT_CONVERSATIONS_HOME, exactly like
# examples/demo.js — no account, no npm install, nothing left behind.
#
# The offline tests are not a weaker version of the live ones. They use a STUB
# RUNTIME ADAPTER that is a real, stateful, multi-turn agent — it just has a
# lookup table where a model would be. Session memory, isolation, ordering and
# locking are properties of the ROUTER, so a stub proves them exactly as well as
# a model does, for nothing, every time CI runs. The live tests (§E) exist to
# prove the one thing a stub cannot: that the real CLIs behave as documented.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ROUTER="$ROOT/reference/skill/session-router.sh"

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); printf '  \033[32mok\033[0m   %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
head_() { printf '\n\033[1m── %s\033[0m\n' "$*"; }
is()   { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (want \"$3\", got \"$2\")"; fi; }
has()  { case "$2" in *"$3"*) ok "$1" ;; *) bad "$1 (\"$3\" not found in: $2)" ;; esac; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/session-router-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
export AGENT_CONVERSATIONS_HOME="$WORK/home"
mkdir -p "$AGENT_CONVERSATIONS_HOME"
REG="$AGENT_CONVERSATIONS_HOME/sessions.test.json"
R() { "$ROUTER" --tag test --registry "$REG" --runtimes-dir "$WORK/runtimes" "$@"; }

printf 'state dir: %s\n' "$AGENT_CONVERSATIONS_HOME"

# ============================================================ the stub runtime
# A real multi-turn agent with a lookup table instead of a model. It keeps a
# transcript per SESSION ID — so if the router ever handed two conversations the
# same id, or lost one, these tests would see it immediately.
mkdir -p "$WORK/runtimes" "$WORK/stub-state"
cat > "$WORK/runtimes/stub.sh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[ "${1:-}" = "--mint-id" ] && { python3 -c 'import uuid;print(uuid.uuid4())'; exit 0; }
STATE="${STUB_STATE:?}"; mkdir -p "$STATE"
TEXT="$(cat)"
SID="${SR_SESSION_ID:-}"
[ -n "$SID" ] || { echo "stub: no session id" >&2; exit 2; }
T="$STATE/$SID.transcript"
# An unknown session id must be a LOUD failure, never an empty new session —
# that is what a real runtime does with a purged/invalid id, and silently
# starting over is the failure mode this whole design exists to prevent.
if [ "${SR_NEW:-0}" != "1" ] && [ ! -f "$T" ]; then
  echo "stub: no such session \"$SID\" — cannot resume" >&2; exit 7
fi
[ -n "${STUB_SLEEP:-}" ] && sleep "$STUB_SLEEP"
printf '%s\n' "$TEXT" >> "$T"
printf '%s' "$SID" > "${SR_SESSION_ID_OUT:-/dev/null}"
case "$TEXT" in
  REMEMBER\ *) printf '%s\n' "${TEXT#REMEMBER }" > "$STATE/$SID.codeword"; echo "ok" ;;
  RECALL)      cat "$STATE/$SID.codeword" 2>/dev/null || echo "NONE" ;;
  TURNS)       wc -l < "$T" | tr -d ' ' ;;
  TRANSCRIPT)  tr '\n' '|' < "$T" ;;
  *)           echo "ack" ;;
esac
STUB
chmod +x "$WORK/runtimes/stub.sh"
export STUB_STATE="$WORK/stub-state"
S() { R --runtime stub "$@"; }

# ============================================================================
head_ "A. the registry — shape, atomicity, corruption, eviction"

out="$(printf 'REMEMBER ALPACA' | S --conversation 'c-alice' --cwd "$WORK" 2>/dev/null)"
is "first message opens a session" "$out" "ok"

python3 - "$REG" <<'PY' > "$WORK/shape"
import json, sys
d = json.load(open(sys.argv[1]))
r = d["c-alice"]
print(",".join(sorted(r)))
print(all(isinstance(r[k], str) for k in ("runtime","sessionId","createdAt","lastUsedAt","cwd")))
print(isinstance(r["turns"], int), r["turns"], r["runtime"])
PY
is "registry record has exactly the documented fields" \
   "$(sed -n 1p "$WORK/shape")" "createdAt,cwd,lastUsedAt,runtime,sessionId,turns"
is "field types are as documented" "$(sed -n 2p "$WORK/shape")" "True"
is "turns started at 1 after one message" "$(sed -n 3p "$WORK/shape")" "True 1 stub"

is "top level is a flat conversationId map (no envelope)" \
   "$(python3 -c 'import json,sys;print(list(json.load(open(sys.argv[1])))[0])' "$REG")" "c-alice"

SID_ALICE="$(R --conversation 'c-alice' --runtime stub --print-id)"
is "--print-id returns the id without invoking anything" \
   "$(wc -l < "$STUB_STATE/$SID_ALICE.transcript" | tr -d ' ')" "1"
is "--print-id is stable when asked twice" \
   "$(R --conversation 'c-alice' --runtime stub --print-id)" "$SID_ALICE"

# a conversation id that would collide with any envelope field name
printf 'hello' | S --conversation 'version' --cwd "$WORK" >/dev/null 2>&1
is "a conversation literally named \"version\" is just a key" \
   "$(python3 -c 'import json,sys;print("version" in json.load(open(sys.argv[1])))' "$REG")" "True"

# --- corrupt registry -------------------------------------------------------
cp "$REG" "$WORK/good.json"
printf '{"c-alice": {"runtime": "stu' > "$REG"          # truncated mid-write
out="$(printf 'hello' | S --conversation 'c-carol' --cwd "$WORK" 2>"$WORK/err")"
is "a corrupt registry does not take the router down" "$out" "ack"
has "corruption is reported loudly on stderr" "$(cat "$WORK/err")" "registryCorrupt"
is "the corrupt file is preserved, not deleted" \
   "$(ls "$AGENT_CONVERSATIONS_HOME" | grep -c 'sessions.test.json.corrupt.')" "1"
cp "$WORK/good.json" "$REG"

# --- atomic write -----------------------------------------------------------
is "no .tmp file is left behind (write is tmp+rename)" \
   "$(ls "$AGENT_CONVERSATIONS_HOME" | grep -c '\.tmp\.' || true)" "0"

# --- eviction ---------------------------------------------------------------
python3 - "$REG" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
d["c-ancient"] = {"runtime":"stub","sessionId":"zzz","createdAt":"2020-01-01T00:00:00Z",
                  "lastUsedAt":"2020-01-01T00:00:00Z","turns":3,"cwd":"/tmp"}
json.dump(d, open(sys.argv[1],"w"))
PY
R --evict --max-age-days 14 2>/dev/null
is "age-based eviction drops a stale conversation" \
   "$(python3 -c 'import json,sys;print("c-ancient" in json.load(open(sys.argv[1])))' "$REG")" "False"
is "eviction leaves live conversations alone" \
   "$(python3 -c 'import json,sys;print("c-alice" in json.load(open(sys.argv[1])))' "$REG")" "True"

# ============================================================================
head_ "B. multi-turn memory — three messages, the third recalls the first"

printf 'REMEMBER PELICAN' | S --conversation 'c-bob' --cwd "$WORK" >/dev/null 2>&1
printf 'what is the weather' | S --conversation 'c-bob' --cwd "$WORK" >/dev/null 2>&1
out="$(printf 'RECALL' | S --conversation 'c-bob' --cwd "$WORK" 2>/dev/null)"
is "turn 3 recalls what turn 1 established" "$out" "PELICAN"
is "turns counted across all three" \
   "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["c-bob"]["turns"])' "$REG")" "3"
is "the session id never changed" \
   "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["c-bob"]["sessionId"])' "$REG")" \
   "$(R --conversation c-bob --runtime stub --print-id)"

# ============================================================================
head_ "C. multi-user isolation — the headline test"

printf 'REMEMBER FALCON'  | S --conversation 'c-u1' --cwd "$WORK" >/dev/null 2>&1
printf 'REMEMBER MARMOT'  | S --conversation 'c-u2' --cwd "$WORK" >/dev/null 2>&1
a="$(printf 'RECALL' | S --conversation 'c-u1' --cwd "$WORK" 2>/dev/null)"
b="$(printf 'RECALL' | S --conversation 'c-u2' --cwd "$WORK" 2>/dev/null)"
is "user 1 gets its own codeword" "$a" "FALCON"
is "user 2 gets its own codeword" "$b" "MARMOT"
case "$a" in *MARMOT*) bad "user 1 leaked user 2's context" ;; *) ok "no leak into user 1" ;; esac
case "$b" in *FALCON*) bad "user 2 leaked user 1's context" ;; *) ok "no leak into user 2" ;; esac
is "two conversations hold two DIFFERENT session ids" \
   "$([ "$(R --conversation c-u1 --runtime stub --print-id)" != \
        "$(R --conversation c-u2 --runtime stub --print-id)" ] && echo different)" "different"

# ============================================================================
head_ "D. concurrency — parallel across conversations, serial within one"

# D1: no cross-talk when both are messaged at the SAME moment
printf 'RECALL' | S --conversation 'c-u1' --cwd "$WORK" > "$WORK/p1" 2>/dev/null &
printf 'RECALL' | S --conversation 'c-u2' --cwd "$WORK" > "$WORK/p2" 2>/dev/null &
wait
is "concurrent: user 1 still correct" "$(cat "$WORK/p1")" "FALCON"
is "concurrent: user 2 still correct" "$(cat "$WORK/p2")" "MARMOT"

# D2: two rapid messages to the SAME conversation are ordered, never interleaved.
# This is the test that fails if the per-conversation lock is removed: without
# it both processes resume one session id at once, and a runtime that FORKS a
# running session (Claude does — see SESSIONS.md §6) silently splits the
# conversation into two branches while still looking successful.
STUB_SLEEP=3 sh -c 'printf "FIRST" | "$0" --tag test --registry "$1" --runtimes-dir "$2" --runtime stub --conversation c-serial --cwd "$3" >/dev/null 2>&1' \
  "$ROUTER" "$REG" "$WORK/runtimes" "$WORK" &
sleep 1
t0=$(date +%s)
printf 'SECOND' | S --conversation 'c-serial' --cwd "$WORK" >/dev/null 2>&1
t1=$(date +%s)
wait
is "turn count incremented by BOTH messages (neither was dropped)" \
   "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["c-serial"]["turns"])' "$REG")" "2"
is "both messages landed in ONE session, in order (no fork, no interleave)" \
   "$(printf 'TRANSCRIPT' | S --conversation 'c-serial' --cwd "$WORK" 2>/dev/null)" "FIRST|SECOND|TRANSCRIPT|"
if [ $(( t1 - t0 )) -ge 1 ]; then ok "the second message WAITED for the first ($(( t1 - t0 ))s)"
else bad "the second message did not wait — the lock is not serialising"; fi

# D3: the concurrency bound is real
printf 'REMEMBER X' | S --conversation 'c-cap1' --cwd "$WORK" >/dev/null 2>&1
printf 'REMEMBER Y' | S --conversation 'c-cap2' --cwd "$WORK" >/dev/null 2>&1
t0=$(date +%s)
for c in c-cap1 c-cap2; do
  STUB_SLEEP=2 sh -c 'printf "ping" | "$0" --tag test --registry "$1" --runtimes-dir "$2" --runtime stub --concurrency 1 --conversation "$3" --cwd "$4" >/dev/null 2>&1' \
    "$ROUTER" "$REG" "$WORK/runtimes" "$c" "$WORK" &
done
wait
t1=$(date +%s)
if [ $(( t1 - t0 )) -ge 4 ]; then ok "--concurrency 1 forced two different conversations to queue"
else bad "--concurrency 1 did not bound the pool (took $((t1-t0))s)"; fi

# D4: a conversation whose lock is already held by a LIVE process is refused
LOCKDIR="$AGENT_CONVERSATIONS_HOME/locks/conv.test.$(printf '%s' 'c-busy' | shasum | cut -c1-16).lock"
mkdir -p "$LOCKDIR"; sleep 30 & HOLDER=$!; printf '%s\n' "$HOLDER" > "$LOCKDIR/pid"
printf 'hello' | S --conversation 'c-busy' --lock-timeout 1 --cwd "$WORK" >/dev/null 2>"$WORK/busy"
is "a busy conversation exits 66, not 1" "$?" "66"
has "and says why" "$(cat "$WORK/busy")" "conversationBusy"
kill "$HOLDER" 2>/dev/null; rm -rf "$LOCKDIR"

# D5: a lock held by a DEAD process is reclaimed, not a permanent wedge
mkdir -p "$LOCKDIR"; printf '999999\n' > "$LOCKDIR/pid"
out="$(printf 'hello' | S --conversation 'c-busy' --lock-timeout 2 --cwd "$WORK" 2>/dev/null)"
is "a stale lock is reclaimed silently" "$out" "ack"

# ============================================================================
head_ "E. persistence across a restart"

SID_BOB="$(R --conversation c-bob --runtime stub --print-id)"
cp "$REG" "$WORK/before-restart.json"
# A "restart" of this system is exactly: every process is gone and only the
# files remain. There is nothing in memory to lose, which is the point.
unset STUB_STATE; export STUB_STATE="$WORK/stub-state"
out="$(printf 'RECALL' | S --conversation 'c-bob' --cwd "$WORK" 2>/dev/null)"
is "after a restart the same session is resumed" "$out" "PELICAN"
is "the session id survived" "$(R --conversation c-bob --runtime stub --print-id)" "$SID_BOB"
is "turns kept counting across the restart" \
   "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["c-bob"]["turns"])' "$REG")" "4"

# ============================================================================
head_ "F. failure modes — loud, never silent"

# F1: an invalid / purged session id
python3 - "$REG" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
d["c-ghost"] = {"runtime":"stub","sessionId":"00000000-dead-dead-dead-000000000000",
                "createdAt":"2026-09-11T00:00:00Z","lastUsedAt":"2026-09-11T00:00:00Z",
                "turns":9,"cwd":"/tmp"}
json.dump(d, open(sys.argv[1],"w"))
PY
printf 'RECALL' | S --conversation 'c-ghost' --cwd "$WORK" >"$WORK/ghost.out" 2>"$WORK/ghost.err"
is "an unresumable session id exits 69" "$?" "69"
has "with a machine-readable code" "$(cat "$WORK/ghost.err")" "runtimeFailed"
has "the runtime's own reason is passed through, not swallowed" \
   "$(cat "$WORK/ghost.err")" "no such session"
is "and nothing was printed to stdout as if it had worked" "$(cat "$WORK/ghost.out")" ""

# F2: an unknown runtime
printf 'hi' | R --runtime nosuch --conversation 'c-x' >/dev/null 2>"$WORK/e2"
is "an unknown runtime exits 65" "$?" "65"
has "and names the adapter it looked for" "$(cat "$WORK/e2")" "unknownRuntime"

# F3: no message
: | S --conversation 'c-alice' >/dev/null 2>"$WORK/e3"
is "an empty message exits 65" "$?" "65"
has "and says so" "$(cat "$WORK/e3")" "emptyMessage"

# F4: missing --conversation
printf 'hi' | S >/dev/null 2>"$WORK/e4"
is "a missing --conversation exits 65" "$?" "65"

# F5: rebinding a conversation to a different runtime is refused
cp "$WORK/runtimes/stub.sh" "$WORK/runtimes/stub2.sh"
printf 'hi' | R --runtime stub2 --conversation 'c-alice' >/dev/null 2>"$WORK/e5"
is "resuming under the wrong runtime exits 65" "$?" "65"
has "and explains the binding" "$(cat "$WORK/e5")" "runtimeMismatch"

# F6: --dry-run invokes nothing
before="$(wc -l < "$STUB_STATE/$SID_ALICE.transcript" | tr -d ' ')"
printf 'hi' | S --conversation 'c-alice' --dry-run >/dev/null 2>&1
is "--dry-run spawns no agent" \
   "$(wc -l < "$STUB_STATE/$SID_ALICE.transcript" | tr -d ' ')" "$before"

# ============================================================================
head_ "G. live agents (SESSION_ROUTER_E2E=1)"

if [ "${SESSION_ROUTER_E2E:-0}" != "1" ]; then
  printf '  skipped — set SESSION_ROUTER_E2E=1 to drive a real agent (costs tokens)\n'
else
  RT="${SESSION_ROUTER_E2E_RUNTIME:-claude}"
  E2E_CWD="$WORK/e2e"; mkdir -p "$E2E_CWD"
  L() { "$ROUTER" --tag test --registry "$REG" --runtime "$RT" --timeout 180 --cwd "$E2E_CWD" "$@"; }
  ca="e2e-alice-$$"; cb="e2e-bob-$$"

  printf 'Remember this codeword: ALPACA. Reply with only the word OK.' \
    | L --conversation "$ca" > "$WORK/l1" 2>"$WORK/l1e"
  printf '  [%s] turn 1 alice -> %s\n' "$RT" "$(cat "$WORK/l1")"
  printf 'Remember this codeword: PLATYPUS. Reply with only the word OK.' \
    | L --conversation "$cb" > "$WORK/l2" 2>"$WORK/l2e"
  printf '  [%s] turn 1 bob   -> %s\n' "$RT" "$(cat "$WORK/l2")"

  printf 'Say nothing but the word NOTED.' | L --conversation "$ca" > "$WORK/l3" 2>/dev/null
  printf '  [%s] turn 2 alice -> %s\n' "$RT" "$(cat "$WORK/l3")"

  # concurrent recall — the headline test, under load
  printf 'What was the codeword I gave you? Reply with only that word.' \
    | L --conversation "$ca" > "$WORK/l4" 2>/dev/null &
  printf 'What was the codeword I gave you? Reply with only that word.' \
    | L --conversation "$cb" > "$WORK/l5" 2>/dev/null &
  wait
  a="$(cat "$WORK/l4")"; b="$(cat "$WORK/l5")"
  printf '  [%s] turn 3 alice -> %s\n  [%s] turn 3 bob   -> %s\n' "$RT" "$a" "$RT" "$b"
  has "live: alice's 3rd turn recalls her 1st" "$a" "ALPACA"
  has "live: bob's 3rd turn recalls his 1st"   "$b" "PLATYPUS"
  case "$a" in *PLATYPUS*) bad "live: alice leaked bob's codeword" ;; *) ok "live: no leak into alice" ;; esac
  case "$b" in *ALPACA*)   bad "live: bob leaked alice's codeword" ;; *) ok "live: no leak into bob" ;; esac
  # The fork test, against a REAL runtime. Two messages to ONE conversation,
  # fired at the same instant. Without the per-conversation lock Claude accepts
  # both, answers both, and silently BRANCHES the transcript — one turn stops
  # existing with no error anywhere (SESSIONS.md §6). With the lock they queue,
  # so the count must be exactly +2 and turn 5 must recall what turn 4 set.
  before="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))[sys.argv[2]]["turns"])' "$REG" "$ca")"
  printf 'Also remember the number 7. Reply with only OK.'  | L --conversation "$ca" > "$WORK/f1" 2>/dev/null &
  printf 'Also remember the colour GREEN. Reply with only OK.' | L --conversation "$ca" > "$WORK/f2" 2>/dev/null &
  wait
  after="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))[sys.argv[2]]["turns"])' "$REG" "$ca")"
  is "live: two concurrent messages to ONE conversation both counted" "$(( after - before ))" "2"
  both="$(printf 'What number and what colour did I ask you to remember? Reply as "<number> <colour>" and nothing else.' \
          | L --conversation "$ca" 2>/dev/null)"
  printf '  [%s] fork check -> %s\n' "$RT" "$both"
  has "live: the lock kept BOTH turns in one branch (number survived)" "$both" "7"
  has "live: the lock kept BOTH turns in one branch (colour survived)" "$(printf '%s' "$both" | tr '[:lower:]' '[:upper:]')" "GREEN"

  is "live: alice's turn count" \
     "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))[sys.argv[2]]["turns"])' "$REG" "$ca")" "6"
  printf '  session ids: alice=%s bob=%s\n' \
    "$(L --conversation "$ca" --print-id)" "$(L --conversation "$cb" --print-id)"
fi

# ============================================================================
printf '\n\033[1m%s passed, %s failed\033[0m\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
