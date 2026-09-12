package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// fakeHerdrOnPath writes a fake `herdr` into a temp dir and returns its path.
// Same contract as the host package's fake: enveloped JSON, errors on stdout,
// `responder-a` occupying pane w1:p1.
func fakeHerdrOnPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1 $2" in
  "agent list")
    echo '{"id":"cli:agent:list","result":{"agents":[{"name":"responder-a","agent_status":"idle","pane_id":"w1:p1"},{"name":"responder-b","agent_status":"idle","pane_id":"w1:p2"}]}}'
    exit 0 ;;
  "agent get")
    case "$3" in
      responder-a) echo '{"result":{"agent":{"name":"responder-a","agent_status":"idle","pane_id":"w1:p1"}}}'; exit 0 ;;
      responder-b) echo '{"result":{"agent":{"name":"responder-b","agent_status":"idle","pane_id":"w1:p2"}}}'; exit 0 ;;
      *) echo '{"error":{"code":"agent_not_found","message":"no such agent"}}'; exit 1 ;;
    esac ;;
  "agent prompt")
    echo '{"result":{"agent_status":"idle"}}'; exit 0 ;;
esac
echo '{"error":{"code":"unsupported","message":"fake"}}'; exit 1
`
	p := filepath.Join(dir, "herdr")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func runCLI(t *testing.T, env map[string]string, argv ...string) (int, string, string) {
	t.Helper()
	env = withDefaultHerdr(t, env)
	var out, errBuf bytes.Buffer
	app := &App{
		Stdout: &out, Stderr: &errBuf,
		Getenv: func(k string) string { return env[k] },
	}
	code := app.Run(context.Background(), argv)
	return code, out.String(), errBuf.String()
}

// withDefaultHerdr makes sure every test in this package sees a working herdr
// on $HERDR_BIN_PATH unless it deliberately supplied its own (paneEnv does, to
// test specific agent states). `host list|state|deliver` and `next` now run a
// shared herdr preflight (ADR-001), so a test that was never about herdr's
// reachability — most of the `next`/filter/ingest suite — needs a herdr that
// always answers, not a real absent one.
func withDefaultHerdr(t *testing.T, env map[string]string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	if out["HERDR_BIN_PATH"] == "" {
		out["HERDR_BIN_PATH"] = fakeHerdrOnPath(t)
	}
	return out
}

func paneEnv(t *testing.T) map[string]string {
	return map[string]string{
		"HERDR_ENV":          "1",
		"HERDR_SESSION":      "demo",
		"HERDR_SOCKET_PATH":  "/tmp/demo/herdr.sock",
		"HERDR_PANE_ID":      "w1:p1",
		"HERDR_TAB_ID":       "w1:t1",
		"HERDR_WORKSPACE_ID": "w1",
		"HERDR_BIN_PATH":     fakeHerdrOnPath(t),
	}
}

// Outside a pane, `convo self` must answer CLEANLY and distinctly — not crash,
// and not pretend. "I am not inside a host" is a legitimate state a caller
// branches on, and it gets exit 65 (not configured), never 1 (something broke).
func TestSelfOutsideHost(t *testing.T) {
	code, out, errOut := runCLI(t, map[string]string{}, "self")
	if code != convo.ExitNotConfigured {
		t.Fatalf("exit = %d, want %d", code, convo.ExitNotConfigured)
	}
	if !strings.Contains(out, "in-host:  no") {
		t.Fatalf("stdout = %q", out)
	}
	var e struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal([]byte(errOut), &e); err != nil {
		t.Fatalf("stderr must be one JSON object: %q", errOut)
	}
	if e.Error.Code != "notInHost" {
		t.Fatalf("code = %q, want notInHost", e.Error.Code)
	}
}

// Inside a pane it reports the context AND which agent it is — the pane id is
// matched against discovery, because an agent has no id and its name was chosen
// by whoever started it.
func TestSelfInsideHostIdentifiesUs(t *testing.T) {
	code, out, errOut := runCLI(t, paneEnv(t), "self", "--json")
	if code != convo.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut)
	}
	var rep selfReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("bad JSON: %q", out)
	}
	if !rep.InHost || rep.Session != "demo" || rep.PaneID != "w1:p1" ||
		rep.SocketPath != "/tmp/demo/herdr.sock" || rep.Agent != "responder-a" ||
		rep.AgentState != "idle" {
		t.Fatalf("self report = %+v", rep)
	}
}

// The self-delivery refusal, end to end through the CLI: exit 70, its own code,
// distinguishable from busy (66), blocked (75) and absent (69).
func TestHostDeliverRefusesSelf(t *testing.T) {
	code, _, errOut := runCLI(t, paneEnv(t), "host", "deliver", "responder-a", "hello")
	if code != convo.ExitSelfDelivery {
		t.Fatalf("exit = %d, want %d; stderr = %s", code, convo.ExitSelfDelivery, errOut)
	}
	if !strings.Contains(errOut, "selfDelivery") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestHostListAndDeliverToSibling(t *testing.T) {
	env := paneEnv(t)
	code, out, errOut := runCLI(t, env, "host", "list")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	// Our own pane is marked, so an agent reading this list can see at a glance
	// which row it must never address.
	if !strings.Contains(out, "responder-a") || !strings.Contains(out, "this pane") {
		t.Fatalf("host list = %q", out)
	}
	code, out, errOut = runCLI(t, env, "host", "deliver", "responder-b", "status?")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "delivered to responder-b") {
		t.Fatalf("deliver said %q", out)
	}
}

// A message's text is arbitrary. Text that begins with a dash is ordinary
// traffic on a real channel, and a CLI that parses it as a flag drops messages.
func TestDeliverTextIsNotParsedAsFlags(t *testing.T) {
	code, out, errOut := runCLI(t, paneEnv(t), "host", "deliver", "responder-b", "--wait is a weird thing to say")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "delivered") {
		t.Fatalf("out = %q", out)
	}
}

func TestUnknownTargetExitsDistinctly(t *testing.T) {
	code, _, errOut := runCLI(t, paneEnv(t), "host", "state", "nobody")
	if code != convo.ExitUnavailable {
		t.Fatalf("exit = %d, want 69; stderr = %s", code, errOut)
	}
}

// journal / next / ack through the CLI, asserting the same cursor-vs-ack split
// the store enforces: `journal` never consumes, `next` does, `ack` is separate.
func TestJournalNextAck(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"AGENT_CONVERSATIONS_HOME": home, "CONVO_TAG": "t1"}

	// Seed the journal the way a daemon would: append-only NDJSON.
	if err := os.MkdirAll(filepath.Join(home, "journal"), 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"id":"m-1","at":"2026-09-09T10:00:00Z","from":{"id":"u-alice","name":"alice"},` +
		`"text":"deploy looks red","html":null,"source":{"kind":"channel","conversationId":"c-general",` +
		`"name":"#general","threadId":"m-1"},"replyToId":null,"mentionsMe":false}` + "\n"
	if err := os.WriteFile(filepath.Join(home, "journal", "t1.ndjson"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	// compact output: the id is first and is NEVER truncated — it is the join
	// key a reply is addressed with.
	code, out, _ := runCLI(t, env, "journal")
	if code != 0 || !strings.HasPrefix(out, "m-1\talice\t[#general]\t") {
		t.Fatalf("exit %d, journal = %q", code, out)
	}

	// Reading did not consume: `next` still has it.
	code, out, _ = runCLI(t, env, "next")
	if code != 0 || !strings.Contains(out, "m-1") {
		t.Fatalf("exit %d, next = %q", code, out)
	}

	// Delivered. A second `next` has nothing — which is exit 64, NOT an error.
	code, _, errOut := runCLI(t, env, "next")
	if code != convo.ExitTimeout {
		t.Fatalf("exit = %d, want 64; stderr = %s", code, errOut)
	}

	// Delivered but unacked is still outstanding — the drain.
	code, out, _ = runCLI(t, env, "journal", "--new")
	if code != 0 || !strings.Contains(out, "m-1") {
		t.Fatalf("--new = %q", out)
	}
	if code, _, _ = runCLI(t, env, "ack", "m-1"); code != 0 {
		t.Fatalf("ack exit %d", code)
	}
	code, out, _ = runCLI(t, env, "journal", "--new")
	if code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("after ack, --new = %q", out)
	}
}

// The exec host reachable from the same CLI, proving the host flag is a real
// seam and not a special case for one product.
func TestExecHostThroughCLI(t *testing.T) {
	env := map[string]string{"CONVO_HOST": "exec", "CONVO_EXEC_CMD": `read -r l; echo "echo: $l"`}
	code, out, errOut := runCLI(t, env, "host", "deliver", "spawn", "ping")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "echo: ping") {
		t.Fatalf("out = %q", out)
	}
}

func TestBadArgs(t *testing.T) {
	cases := [][]string{
		{},
		{"nonsense"},
		{"host"},
		{"host", "deliver", "responder-b"},
		{"ack"},
	}
	for _, argv := range cases {
		if code, _, _ := runCLI(t, paneEnv(t), argv...); code != convo.ExitNotConfigured {
			t.Errorf("%v: exit = %d, want 65", argv, code)
		}
	}
}

// seedJournal writes one channel message and one DM the way a daemon would.
func seedJournal(t *testing.T, home string) map[string]string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "journal"), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := `{"id":"m-1","at":"2026-09-09T10:00:00Z","from":{"id":"u-alice","name":"alice"},"text":"deploy looks red","html":null,"source":{"kind":"channel","conversationId":"c-general","name":"#general","threadId":"m-1"},"replyToId":null,"mentionsMe":false}
{"id":"m-2","at":"2026-09-09T10:00:01Z","from":{"id":"u-bob","name":"bob"},"text":"status?","html":null,"source":{"kind":"chat","conversationId":"c-dm-alice","name":"dm:alice"},"replyToId":null,"mentionsMe":false}
`
	if err := os.WriteFile(filepath.Join(home, "journal", "t1.ndjson"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	return map[string]string{"AGENT_CONVERSATIONS_HOME": home, "CONVO_TAG": "t1", "CONVO_CHANNEL": "memory"}
}

// A reply is addressed by MESSAGE ID and the tool routes it: the channel post
// is threaded, the DM goes to the conversation. This is the decision that, made
// by the agent, silently loses replies.
func TestRespondRoutesFromTheMessage(t *testing.T) {
	env := seedJournal(t, t.TempDir())

	code, out, errOut := runCLI(t, env, "respond", "m-1", "on it")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	_ = out
	code, out, errOut = runCLI(t, env, "respond", "--json", "m-1", "on it")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var r struct {
		ConversationID string `json:"conversationId"`
		ThreadID       string `json:"threadId"`
		Kind           string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad JSON: %q", out)
	}
	if r.Kind != "channel" || r.ConversationID != "c-general" || r.ThreadID != "m-1" {
		t.Fatalf("channel reply routed wrong: %+v", r)
	}

	code, out, errOut = runCLI(t, env, "respond", "--json", "m-2", "all good")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("bad JSON: %q", out)
	}
	if r.Kind != "chat" || r.ConversationID != "c-dm-alice" || r.ThreadID != "" {
		t.Fatalf("dm reply routed wrong: %+v", r)
	}
}

// An unknown id must fail LOUDLY. Guessing a plausible destination is worse
// than not sending at all.
func TestRespondUnknownIdFailsLoudly(t *testing.T) {
	env := seedJournal(t, t.TempDir())
	code, _, errOut := runCLI(t, env, "respond", "m-999", "hello")
	if code != convo.ExitNotConfigured {
		t.Fatalf("exit = %d, want 65", code)
	}
	if !strings.Contains(errOut, "unknown message id") {
		t.Fatalf("stderr = %q", errOut)
	}
}

// With no channel configured, respond must refuse rather than report success
// for a reply that went nowhere.
func TestRespondWithoutChannelRefuses(t *testing.T) {
	env := seedJournal(t, t.TempDir())
	delete(env, "CONVO_CHANNEL")
	code, _, errOut := runCLI(t, env, "respond", "m-1", "hi")
	if code != convo.ExitNotConfigured {
		t.Fatalf("exit = %d, want 65", code)
	}
	if !strings.Contains(errOut, "no channel configured") {
		t.Fatalf("stderr = %q", errOut)
	}
}

// Reply text is arbitrary and must not be parsed as flags.
func TestRespondTextIsVerbatim(t *testing.T) {
	env := seedJournal(t, t.TempDir())
	code, _, errOut := runCLI(t, env, "respond", "m-1", "--json is a flag, not my answer")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}
