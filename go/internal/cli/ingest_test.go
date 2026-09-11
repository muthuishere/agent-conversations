package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	memchan "github.com/muthuishere/agent-conversations/go/internal/channel/memory"
	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// runWith is runCLI bound to one channel instance, so several invocations see
// the same room — which is the whole point of an ingest test.
func runWith(t *testing.T, ch convo.Channel, env map[string]string, argv ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	app := &App{
		Stdout: &out, Stderr: &errBuf,
		Getenv:     func(k string) string { return env[k] },
		NewChannel: func(string) (convo.Channel, error) { return ch, nil },
	}
	return app.Run(context.Background(), argv), out.String(), errBuf.String()
}

// The ingest pass is the half that was missing: before it existed nothing ever
// called Store.Append, so the journal was always empty and every consumer verb
// had nothing to read. This is that gap, closed, and asserted end to end.
//
// It also asserts IDEMPOTENCE, which is not a nicety here: `listen` calls this
// on a loop and a cron entry may call it twice, so a second pass over the same
// traffic has to be a no-op rather than a duplicate.
func TestFetchIngestsIntoTheJournalAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"AGENT_CONVERSATIONS_HOME": home, "CONVO_CHANNEL": "memory"}
	ch := memchan.New()
	ch.Post("c-general", "u-bob", "bob", "is the deploy red?")
	ch.Post("c-dm-alice", "u-alice", "alice", "ping")

	code, out, errOut := runWith(t, ch, env, "fetch")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "ingested 2") {
		t.Fatalf("fetch said %q", out)
	}

	// It is in the durable log, and reading it does not consume it.
	code, out, _ = runWith(t, ch, env, "journal")
	if code != 0 || !strings.Contains(out, "is the deploy red?") || !strings.Contains(out, "ping") {
		t.Fatalf("journal = %q", out)
	}

	// A second pass over the same traffic ingests nothing. The fetch cursor
	// moved; the id dedupe covers whatever the cursor hands back anyway.
	code, out, _ = runWith(t, ch, env, "fetch")
	if code != 0 || !strings.Contains(out, "ingested 0") {
		t.Fatalf("second fetch = %q (exit %d)", out, code)
	}
	lines := journalLines(t, home, "default")
	if len(lines) != 2 {
		t.Fatalf("journal has %d lines, want 2 — the pass is not idempotent", len(lines))
	}

	// New traffic after the cursor is still picked up: idempotent is not the
	// same as deaf.
	ch.Post("c-general", "u-bob", "bob", "still red")
	code, out, _ = runWith(t, ch, env, "fetch")
	if code != 0 || !strings.Contains(out, "ingested 1") {
		t.Fatalf("third fetch = %q", out)
	}
}

// echoChannel is a channel that does NOT suppress its own echo — a plausible
// bug in any new adapter, and the reason ingest suppresses by identity too.
type echoChannel struct {
	self convo.Identity
	msgs []convo.Message
}

func (c *echoChannel) Conversations(context.Context) ([]convo.Conversation, error) {
	return []convo.Conversation{{ID: "c-general", Kind: "channel", Name: "#general"}}, nil
}
func (c *echoChannel) Identity(context.Context) (convo.Identity, error) { return c.self, nil }
func (c *echoChannel) Fetch(_ context.Context, _ string, cur string) ([]convo.Message, string, error) {
	if cur != "" {
		return nil, cur, nil
	}
	return c.msgs, "done", nil
}
func (c *echoChannel) Send(context.Context, convo.Target, string) (string, error) {
	return "m-sent", nil
}

// Our own reply must never reach the journal. If it does, it is delivered to
// the agent, the agent answers it, the answer is fetched back — an unbounded
// self-reply loop running against a real channel and a real billing account.
// Suppression is BY IDENTITY, never by matching the text.
func TestFetchSuppressesOurOwnEchoByIdentity(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"AGENT_CONVERSATIONS_HOME": home, "CONVO_CHANNEL": "x"}
	ch := &echoChannel{
		self: convo.Identity{ID: "u-agent", Name: "agent"},
		msgs: []convo.Message{
			{ID: "m-1", At: "2026-09-09T10:00:00Z", From: convo.Author{ID: "u-bob", Name: "bob"},
				Text: "question", Source: convo.Source{Kind: "channel", ConversationID: "c-general", Name: "#general", ThreadID: "m-1"}},
			{ID: "m-2", At: "2026-09-09T10:00:01Z", From: convo.Author{ID: "u-agent", Name: "agent"},
				Text: "answer", Source: convo.Source{Kind: "channel", ConversationID: "c-general", Name: "#general", ThreadID: "m-1"}},
		},
	}
	if code, out, errOut := runWith(t, ch, env, "fetch"); code != 0 || !strings.Contains(out, "ingested 1") {
		t.Fatalf("exit %d, out %q, err %s", code, out, errOut)
	}
	lines := journalLines(t, home, "default")
	if len(lines) != 1 || !strings.Contains(lines[0], `"m-1"`) {
		t.Fatalf("journal = %v — our own message was journalled", lines)
	}
}

// The chain the product actually is: a person posts, ingest journals it, the
// consumer is handed it, and the reply is routed from the message's own source
// back into the same thread. Every step through the real CLI.
func TestFetchNextRespondRoundTrip(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"AGENT_CONVERSATIONS_HOME": home, "CONVO_CHANNEL": "memory"}
	ch := memchan.New()
	posted := ch.Post("c-general", "u-bob", "bob", "who owns the deploy?")

	if code, _, e := runWith(t, ch, env, "fetch"); code != 0 {
		t.Fatalf("fetch exit %d: %s", code, e)
	}
	code, out, _ := runWith(t, ch, env, "next", "--count", "1")
	if code != 0 || !strings.HasPrefix(out, posted.ID+"\tbob\t[#general]\t") {
		t.Fatalf("next = %q", out)
	}
	if code, _, e := runWith(t, ch, env, "respond", posted.ID, "I do"); code != 0 {
		t.Fatalf("respond exit %d: %s", code, e)
	}

	// Verify the ARTIFACT, not the exit code: the reply is on the channel, in
	// the thread of the message it answers.
	msgs, _, err := ch.Fetch(context.Background(), "c-general", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Text == "I do" {
			t.Fatalf("our reply came back out of Fetch — self-echo is not suppressed")
		}
	}
	if code, out, _ := runWith(t, ch, env, "fetch"); code != 0 || !strings.Contains(out, "ingested 0") {
		t.Fatalf("our own reply was re-ingested: %q", out)
	}
}

// --once is one pass, and it must leave the two files a consumer inspects: a
// heartbeat that proves the listener ran, and no stale pid lock behind it.
func TestListenOnceWritesHeartbeatAndReleasesTheLock(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"AGENT_CONVERSATIONS_HOME": home, "CONVO_CHANNEL": "memory"}
	ch := memchan.New()
	ch.Post("c-general", "u-bob", "bob", "hello")

	if code, out, e := runWith(t, ch, env, "listen", "--once"); code != 0 || !strings.Contains(out, "ingested 1") {
		t.Fatalf("exit %d, out %q, err %s", code, out, e)
	}

	var hb heartbeat
	b, err := os.ReadFile(filepath.Join(home, "heartbeat.default.json"))
	if err != nil {
		t.Fatalf("no heartbeat — a listener that cannot prove it ran is indistinguishable from a dead one: %v", err)
	}
	if err := json.Unmarshal(b, &hb); err != nil {
		t.Fatal(err)
	}
	if hb.PollCount != 1 || hb.PID != os.Getpid() || hb.Identity.ID != "u-agent" {
		t.Fatalf("heartbeat = %+v", hb)
	}
	if _, err := os.Stat(filepath.Join(home, "listener.default.pid")); !os.IsNotExist(err) {
		t.Fatalf("the pid lock outlived the pass — the next run would refuse to start")
	}
}

// Two listeners on one tag both write the fetch cursor, so each advances past
// messages the other never journalled and the overlap is lost for good. The
// second must refuse with its own exit code, not fight for it.
func TestSecondListenerIsRefused(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"AGENT_CONVERSATIONS_HOME": home, "CONVO_CHANNEL": "memory"}
	if err := os.WriteFile(filepath.Join(home, "listener.default.pid"),
		[]byte(strconv.Itoa(os.Getppid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runWith(t, memchan.New(), env, "listen", "--once")
	if code != convo.ExitConflict {
		t.Fatalf("exit = %d, want %d (conflict); stderr = %s", code, convo.ExitConflict, errOut)
	}
	if !strings.Contains(errOut, "conflict") {
		t.Fatalf("stderr = %q", errOut)
	}
}

// A pid file left behind by a crashed listener must not need a human to clear
// before the system can run again.
func TestStaleListenerLockIsClaimed(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"AGENT_CONVERSATIONS_HOME": home, "CONVO_CHANNEL": "memory"}
	// pid 0 never names a live process, and neither does a pid we invent.
	if err := os.WriteFile(filepath.Join(home, "listener.default.pid"), []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runWith(t, memchan.New(), env, "listen", "--once"); code != 0 {
		t.Fatalf("a stale lock blocked a fresh listener: exit %d, %s", code, e)
	}
}

// The backoff tiers of ARCHITECTURE.md §5.6, including the property that makes
// them safe: ANY traffic snaps the interval straight back to active, with no
// separate wake-up path to forget to call.
func TestAdaptiveBackoffTiers(t *testing.T) {
	tr := resolveTiers(options{})
	start := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)

	if got := tr.pick(start, nil, start.Add(30*time.Second)); got != defaultActive {
		t.Errorf("fresh listener = %s, want %s", got, defaultActive)
	}
	if got := tr.pick(start, nil, start.Add(3*time.Minute)); got != defaultMid {
		t.Errorf("quiet past idle-1 = %s, want %s", got, defaultMid)
	}
	if got := tr.pick(start, nil, start.Add(20*time.Minute)); got != defaultIdle {
		t.Errorf("quiet past idle-2 = %s, want %s", got, defaultIdle)
	}

	// Snap back: one message at minute 20 makes the very next interval active.
	traffic := start.Add(20 * time.Minute)
	if got := tr.pick(start, &traffic, traffic.Add(time.Second)); got != defaultActive {
		t.Errorf("after traffic = %s, want %s — the tier did not snap back", got, defaultActive)
	}
}

// Tiers are clamped rather than trusted: mid can never exceed idle and idle can
// never undercut active, whatever someone types on the command line. An
// inverted ladder polls a busy channel slower than a dead one.
func TestTiersAreClamped(t *testing.T) {
	tr := resolveTiers(options{
		pollActive: 30 * time.Second,
		pollMid:    5 * time.Second,
		pollIdle:   10 * time.Second,
	})
	if tr.idle < tr.active || tr.mid < tr.active || tr.mid > tr.idle {
		t.Fatalf("ladder is inverted: %+v", tr)
	}
}

func journalLines(t *testing.T, home, tag string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "journal", tag+".ndjson"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func itoa(n int) string {
	return strings.TrimSpace(strings.Trim(strings.Fields(strings.TrimSpace(sprintInt(n)))[0], " "))
}

func sprintInt(n int) string { b, _ := json.Marshal(n); return string(b) }
