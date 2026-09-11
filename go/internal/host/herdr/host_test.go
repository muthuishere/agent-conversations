package herdr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// recorder is a Runner that answers from a fixture table and remembers the
// argv it was given. Injecting the runner is what lets the entire guest path be
// tested with no server, no socket and no network.
type recorder struct {
	responses map[string]string // first three argv words -> JSON
	calls     [][]string
}

func (r *recorder) run(ctx context.Context, args []string) ([]byte, int, error) {
	r.calls = append(r.calls, args)
	key := strings.Join(args[:min(3, len(args))], " ")
	if out, ok := r.responses[key]; ok {
		return []byte(out), 0, nil
	}
	return []byte(missingErrJSON), 1, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func newHost(env Env, responses map[string]string) (*Host, *recorder) {
	r := &recorder{responses: responses}
	h := New(env)
	h.Run = r.run
	return h, r
}

var paneEnv = Env{
	InHost: true, Session: "demo", SocketPath: "/tmp/demo/herdr.sock",
	PaneID: "w1:p1", TabID: "w1:t1", WorkspaceID: "w1",
}

// THE HEADLINE GUARD. We are running in pane w1:p1; `responder-a` IS pane
// w1:p1. Delivering would make the agent prompt itself, which wakes it, which
// makes it answer, which wakes it again. There is no safe version of that, so
// it is refused by construction — and the refusal has its own exit code so a
// caller can tell it apart from "the target is busy".
func TestDeliverRefusesSelf(t *testing.T) {
	h, rec := newHost(paneEnv, map[string]string{
		"agent get responder-a": getJSON, // pane_id w1:p1 == our pane
	})
	_, err := h.Deliver(context.Background(), "responder-a", "hello", convo.DeliverOpts{})
	if !errors.Is(err, convo.ErrSelfDelivery) {
		t.Fatalf("want ErrSelfDelivery, got %v", err)
	}
	if convo.ExitCode(err) != convo.ExitSelfDelivery {
		t.Fatalf("want exit %d, got %d", convo.ExitSelfDelivery, convo.ExitCode(err))
	}
	// And nothing was sent: only the resolve call happened.
	for _, c := range rec.calls {
		if len(c) > 1 && c[1] == "prompt" {
			t.Fatal("a prompt was sent to our own pane")
		}
	}
}

// The guard is by PANE, not by name. The same name in a different pane is a
// legitimate target; a different name in OUR pane is not.
func TestSelfGuardComparesPanesNotNames(t *testing.T) {
	otherPane := Env{InHost: true, PaneID: "w9:p9"}
	h, _ := newHost(otherPane, map[string]string{
		"agent get responder-a":    getJSON,
		"agent prompt responder-a": promptJSON,
	})
	if _, err := h.Deliver(context.Background(), "responder-a", "hi", convo.DeliverOpts{}); err != nil {
		t.Fatalf("delivery to a different pane must succeed, got %v", err)
	}

	// Outside a pane entirely there is no self to guard against, and the guard
	// must not fire on two empty strings.
	h2, _ := newHost(Env{}, map[string]string{
		"agent get responder-a":    getJSON,
		"agent prompt responder-a": promptJSON,
	})
	if _, err := h2.Deliver(context.Background(), "responder-a", "hi", convo.DeliverOpts{}); err != nil {
		t.Fatalf("empty pane ids must not trigger the guard, got %v", err)
	}
}

func TestDeliverBackpressure(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		responses map[string]string
		wantErr   error
		wantExit  int
		wantSent  bool
	}{
		{
			name: "idle delivers",
			responses: map[string]string{
				"agent get responder-b":    `{"result":{"agent":{"name":"responder-b","agent_status":"idle","pane_id":"w1:p2"}}}`,
				"agent prompt responder-b": promptJSON,
			},
			wantSent: true,
		},
		{
			name: "blocked refuses and sends nothing",
			responses: map[string]string{
				"agent get responder-c": getBlockedJSON,
			},
			wantErr: convo.ErrAgentBlocked, wantExit: convo.ExitBlocked,
		},
		{
			name: "done is gone — fall back, do not resurrect",
			responses: map[string]string{
				"agent get responder-d": `{"result":{"agent":{"name":"responder-d","agent_status":"done","pane_id":"w3:p1"}}}`,
			},
			wantErr: convo.ErrTargetAbsent, wantExit: convo.ExitUnavailable,
		},
		{
			name:      "absent target is absent",
			responses: map[string]string{},
			wantErr:   convo.ErrTargetAbsent, wantExit: convo.ExitUnavailable,
		},
		{
			name: "working waits, then delivers",
			responses: map[string]string{
				"agent get responder-b":    `{"result":{"agent":{"name":"responder-b","agent_status":"working","pane_id":"w1:p2"}}}`,
				"agent wait responder-b":   waitJSON,
				"agent prompt responder-b": promptJSON,
			},
			// Note: the re-resolve after the wait still reports `working` from
			// the same fixture, so this exercises the re-resolve path too.
			wantSent: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := "responder-b"
			for k := range c.responses {
				if strings.HasPrefix(k, "agent get ") {
					name = strings.TrimPrefix(k, "agent get ")
				}
			}
			h, rec := newHost(paneEnv, c.responses)
			_, err := h.Deliver(ctx, name, "text", convo.DeliverOpts{Timeout: time.Second})
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("got %v, want %v", err, c.wantErr)
				}
				if convo.ExitCode(err) != c.wantExit {
					t.Fatalf("exit %d, want %d", convo.ExitCode(err), c.wantExit)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			sent := false
			for _, call := range rec.calls {
				if len(call) > 1 && call[1] == "prompt" {
					sent = true
				}
			}
			if sent != c.wantSent {
				t.Fatalf("prompt sent = %v, want %v", sent, c.wantSent)
			}
		})
	}
}

// Addressing is by NAME, always. If a cached id ever crept in, the argv would
// stop carrying the name, and the message could reach a stranger.
func TestAddressesByName(t *testing.T) {
	h, rec := newHost(paneEnv, map[string]string{
		"agent get responder-b":    `{"result":{"agent":{"name":"responder-b","agent_status":"idle","pane_id":"w1:p2"}}}`,
		"agent prompt responder-b": promptJSON,
	})
	if _, err := h.Deliver(context.Background(), "responder-b", "hello", convo.DeliverOpts{}); err != nil {
		t.Fatal(err)
	}
	for _, call := range rec.calls {
		if len(call) < 3 || call[2] != "responder-b" {
			t.Fatalf("call %v does not address the agent by name", call)
		}
	}
}

// --until is a REPEATED flag on this host, not a comma-joined list. Getting
// that wrong produces a wait that silently never matches.
func TestWaitRepeatsUntilFlag(t *testing.T) {
	h, rec := newHost(paneEnv, map[string]string{"agent wait responder-b": waitJSON})
	if _, err := h.Wait(context.Background(), "responder-b",
		[]convo.State{convo.StateIdle, convo.StateBlocked}, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(rec.calls[0], " ")
	if !strings.Contains(got, "--until idle --until blocked") {
		t.Fatalf("argv = %q, want repeated --until flags", got)
	}
	if !strings.Contains(got, "--timeout 2000") {
		t.Fatalf("argv = %q, want a bounded timeout in MILLISECONDS", got)
	}
}

// A wait with no timeout must still be bounded. An unbounded wait against a
// session whose human went to lunch is an indefinite stall.
func TestWaitIsAlwaysBounded(t *testing.T) {
	h, rec := newHost(paneEnv, map[string]string{"agent wait responder-b": waitJSON})
	if _, err := h.Wait(context.Background(), "responder-b", nil, 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(rec.calls[0], " "), "--timeout") {
		t.Fatal("a wait with no timeout must still pass one")
	}
}

func TestSelfResolvesOurAgent(t *testing.T) {
	h, _ := newHost(paneEnv, map[string]string{"agent list": listJSON})
	agent, found, err := h.Self(context.Background())
	if err != nil || !found {
		t.Fatalf("self: %v found=%v", err, found)
	}
	if agent.Name != "responder-a" {
		t.Fatalf("we are %q, want responder-a (the agent in pane w1:p1)", agent.Name)
	}

	// Outside a pane there is no self, and that is not an error.
	h2, _ := newHost(Env{}, map[string]string{"agent list": listJSON})
	if _, found, err := h2.Self(context.Background()); err != nil || found {
		t.Fatalf("outside a pane there is no self: found=%v err=%v", found, err)
	}
}

// ---------------------------------------------------------------------------
// The same guest path, driven through a FAKE `herdr` BINARY on PATH.
//
// This is the point of the Host interface stated as a test: the production code
// path — argv construction, process spawn, exit codes, stdout parsing — runs
// end to end with no server, no socket and no network, against a shell script
// in a temp dir. Everything above stubs the Runner; this stubs the world.
// ---------------------------------------------------------------------------

func fakeHerdr(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
# A fake herdr. Mirrors the measured 0.8.2 contract:
#  · list/get emit JSON unconditionally and take no options
#  · errors are JSON on STDOUT; get/prompt exit 1 on a missing target
#  · list exits 0 with an empty list
case "$1 $2" in
  "agent list")
    echo '{"id":"cli:agent:list","result":{"agents":[{"name":"responder-a","agent_status":"idle","pane_id":"w1:p1"},{"name":"responder-b","agent_status":"idle","pane_id":"w1:p2"},{"name":"responder-c","agent_status":"blocked","pane_id":"w2:p1"}]}}'
    exit 0 ;;
  "agent get")
    case "$3" in
      responder-a) echo '{"id":"cli:agent:get","result":{"agent":{"name":"responder-a","agent_status":"idle","pane_id":"w1:p1"}}}'; exit 0 ;;
      responder-b) echo '{"id":"cli:agent:get","result":{"agent":{"name":"responder-b","agent_status":"idle","pane_id":"w1:p2"}}}'; exit 0 ;;
      responder-c) echo '{"id":"cli:agent:get","result":{"agent":{"name":"responder-c","agent_status":"blocked","pane_id":"w2:p1"}}}'; exit 0 ;;
      *) echo '{"id":"cli:agent:get","error":{"code":"agent_not_found","message":"no such agent"}}'; exit 1 ;;
    esac ;;
  "agent prompt")
    if [ "$3" = "responder-c" ]; then
      echo '{"id":"cli:agent:prompt","error":{"code":"agent_blocked","message":"agent is blocked"}}'; exit 1
    fi
    echo "$3 $4" >> "$FAKE_HERDR_SENT"
    echo '{"id":"cli:agent:prompt","result":{"agent_status":"idle"}}'; exit 0 ;;
esac
echo '{"error":{"code":"unsupported","message":"fake herdr"}}'
exit 1
`
	path := filepath.Join(dir, "herdr")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAgainstFakeBinary(t *testing.T) {
	bin := fakeHerdr(t)
	sent := filepath.Join(t.TempDir(), "sent.log")
	t.Setenv("FAKE_HERDR_SENT", sent)

	// We are pane w1:p1 — i.e. we are responder-a.
	env := Env{InHost: true, PaneID: "w1:p1", SocketPath: "/tmp/demo/herdr.sock", BinPath: bin}
	h := New(env)
	ctx := context.Background()

	agents, err := h.Agents(ctx)
	if err != nil || len(agents) != 3 {
		t.Fatalf("agents: %v (%d)", err, len(agents))
	}

	self, found, err := h.Self(ctx)
	if err != nil || !found || self.Name != "responder-a" {
		t.Fatalf("self: %+v found=%v err=%v", self, found, err)
	}

	// 1. our own pane -> refused, exit 70, nothing sent
	if _, err := h.Deliver(ctx, "responder-a", "loop?", convo.DeliverOpts{}); !errors.Is(err, convo.ErrSelfDelivery) {
		t.Fatalf("self delivery not refused: %v", err)
	}
	// 2. a blocked sibling -> refused, exit 75, nothing sent
	if _, err := h.Deliver(ctx, "responder-c", "hi", convo.DeliverOpts{}); !errors.Is(err, convo.ErrAgentBlocked) {
		t.Fatalf("blocked not refused: %v", err)
	}
	// 3. an absent target -> exit 69 so the caller falls back
	if _, err := h.Deliver(ctx, "nobody", "hi", convo.DeliverOpts{}); !errors.Is(err, convo.ErrTargetAbsent) {
		t.Fatalf("absent target: %v", err)
	}
	// 4. an idle sibling -> delivered
	d, err := h.Deliver(ctx, "responder-b", "[message m-1 from alice in #general] status?", convo.DeliverOpts{})
	if err != nil || !d.Delivered {
		t.Fatalf("delivery to an idle sibling failed: %v %+v", err, d)
	}

	// Exactly one prompt reached the fake host, and it was the idle sibling.
	log, err := os.ReadFile(sent)
	if err != nil {
		t.Fatalf("nothing was ever sent: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "responder-b ") {
		t.Fatalf("sent log = %q, want exactly one prompt to responder-b", log)
	}
}

// A host binary that is not installed must be a clean "host unavailable",
// not a panic and not a mystery.
func TestMissingBinaryIsHostUnavailable(t *testing.T) {
	h := New(Env{InHost: true, PaneID: "w1:p1", BinPath: filepath.Join(t.TempDir(), "no-such-herdr")})
	_, err := h.Agents(context.Background())
	if !errors.Is(err, convo.ErrHostUnavailable) {
		t.Fatalf("want ErrHostUnavailable, got %v", err)
	}
	if convo.ExitCode(err) != convo.ExitUnavailable {
		t.Fatalf("want exit 69, got %d", convo.ExitCode(err))
	}
}

// A host that fails before it can serve the request answers on STDERR, with
// the same envelope. Measured: `agent list` with no server running exits
// non-zero, writes nothing to stdout, and puts
// {"error":{"code":"server_not_running",…}} on stderr. That must surface as a
// matchable code, not as an English blob nested inside another message.
func TestErrorEnvelopeOnStderrIsParsed(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
echo '{"id":"cli:agent:list","error":{"code":"server_not_running","message":"no herdr server is running"}}' >&2
exit 1
`
	p := filepath.Join(dir, "herdr")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h := New(Env{InHost: true, PaneID: "w1:p1", BinPath: p})
	_, err := h.Agents(context.Background())
	if !errors.Is(err, convo.ErrHostUnavailable) {
		t.Fatalf("want ErrHostUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "server_not_running") {
		t.Fatalf("the host's own code must survive: %v", err)
	}
	if convo.ExitCode(err) != convo.ExitUnavailable {
		t.Fatalf("want exit 69, got %d", convo.ExitCode(err))
	}
}

// Non-JSON on stderr (a banner, a panic) still produces a clean, one-line
// host-unavailable error rather than a wall of text in an agent's context.
func TestNonJSONStderrIsOneLine(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
printf 'error: nested herdr is disabled by default.\nsee configuration if you want to enable it.\n' >&2
exit 1
`
	p := filepath.Join(dir, "herdr")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	h := New(Env{InHost: true, PaneID: "w1:p1", BinPath: p})
	_, err := h.Agents(context.Background())
	if !errors.Is(err, convo.ErrHostUnavailable) {
		t.Fatalf("want ErrHostUnavailable, got %v", err)
	}
	if strings.Count(err.Error(), "\n") != 0 {
		t.Fatalf("diagnostic must be one line: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "nested herdr is disabled") {
		t.Fatalf("the reason must survive: %v", err)
	}
}
