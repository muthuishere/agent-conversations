package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	memchan "github.com/muthuishere/agent-conversations/go/internal/channel/memory"
	"github.com/muthuishere/agent-conversations/go/internal/convo"
	filestore "github.com/muthuishere/agent-conversations/go/internal/store/file"
)

// primingChannel is the memory channel plus the one optional capability both
// real adapters have: position a fresh cursor at NOW. The memory channel does
// not carry it itself so the older ingest tests keep exercising the
// cannot-prime fallthrough.
//
// Its discovery is also DRIFTING, like WhatsApp's capped chat listing: a room
// is only listed once someone has posted in it, so a room can surface mid-run
// with a backlog behind it.
type primingChannel struct{ *memchan.Channel }

func (p primingChannel) PrimeCursor(ctx context.Context, convID string) (string, error) {
	_, next, err := p.Fetch(ctx, convID, "")
	return next, err
}

func (p primingChannel) Conversations(ctx context.Context) ([]convo.Conversation, error) {
	all, err := p.Channel.Conversations(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, m := range p.All() {
		seen[m.Source.ConversationID] = true
	}
	var out []convo.Conversation
	for _, cv := range all {
		if seen[cv.ID] {
			out = append(out, cv)
		}
	}
	return out, nil
}

// A conversation the listener has never seen starts AT NOW. Measured on
// WhatsApp: discovery is capped, the top-N drifts between polls, and a chat
// that surfaces later arrives cursorless — under the old contract each one
// replayed its whole history (447 months-old messages in one pass, every one a
// potential agent wake). The journal is what happens while listening.
func TestNewConversationStartsAtNowByDefault(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"AGENT_CONVERSATIONS_HOME": home, "CONVO_CHANNEL": "x"}
	ch := primingChannel{memchan.New()}
	ch.Post("c-general", "u-bob", "bob", "ancient history")

	// First attach: the room's past is not ingested.
	if code, out, e := runWith(t, ch, env, "fetch"); code != 0 || !strings.Contains(out, "ingested 0") {
		t.Fatalf("first attach replayed history: exit %d, out %q, err %s", code, out, e)
	}
	// Traffic from here on is.
	ch.Post("c-general", "u-bob", "bob", "new question")
	if code, out, _ := runWith(t, ch, env, "fetch"); code != 0 || !strings.Contains(out, "ingested 1") {
		t.Fatalf("new traffic after prime = %q", out)
	}

	// A conversation DISCOVERED MID-RUN, with a backlog: 0 on first sight.
	for i := 0; i < 5; i++ {
		ch.Post("c-dm-alice", "u-alice", "alice", fmt.Sprintf("old %d", i))
	}
	if code, out, _ := runWith(t, ch, env, "fetch"); code != 0 || !strings.Contains(out, "ingested 0") {
		t.Fatalf("late-discovered room replayed its backlog: %q", out)
	}
	ch.Post("c-dm-alice", "u-alice", "alice", "fresh")
	if code, out, _ := runWith(t, ch, env, "fetch"); code != 0 || !strings.Contains(out, "ingested 1") {
		t.Fatalf("late-discovered room is deaf after prime: %q", out)
	}

	lines := journalLines(t, home, "default")
	if len(lines) != 2 || !strings.Contains(lines[0], "new question") || !strings.Contains(lines[1], "fresh") {
		t.Fatalf("journal = %v, want exactly the two messages sent while listening", lines)
	}

	// --prime is accepted and changes nothing: it names the default.
	ch.Post("c-dm-carol", "u-carol", "carol", "older")
	if code, out, _ := runWith(t, ch, env, "fetch", "--prime"); code != 0 || !strings.Contains(out, "ingested 0") {
		t.Fatalf("--prime = %q", out)
	}
}

// --replay-history is the explicit opt-in to the archive behaviour: a
// cursorless conversation is read from the beginning.
func TestReplayHistoryRestoresTheArchiveBehaviour(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"AGENT_CONVERSATIONS_HOME": home, "CONVO_CHANNEL": "x"}
	ch := primingChannel{memchan.New()}
	ch.Post("c-general", "u-bob", "bob", "one")
	ch.Post("c-general", "u-bob", "bob", "two")
	ch.Post("c-dm-alice", "u-alice", "alice", "three")

	if code, out, e := runWith(t, ch, env, "fetch", "--replay-history"); code != 0 || !strings.Contains(out, "ingested 3") {
		t.Fatalf("exit %d, out %q, err %s", code, out, e)
	}
	if lines := journalLines(t, home, "default"); len(lines) != 3 {
		t.Fatalf("journal has %d lines, want 3", len(lines))
	}
}

// roomsChannel is a channel with several rooms whose behaviour is scripted per
// room: a delay, a permanent failure, or a throttle that clears after N asks.
type roomsChannel struct {
	rooms []convo.Conversation
	msgs  map[string][]convo.Message
	delay time.Duration
	fail  map[string]error // permanent
	// throttle[id] = how many times the room answers 429 before succeeding
	throttle map[string]int

	mu       sync.Mutex
	asks     map[string]int
	inflight atomic.Int32
	peak     atomic.Int32
}

type throttled struct{ after time.Duration }

func (e throttled) Error() string             { return "HTTP 429" }
func (e throttled) Retryable() bool           { return true }
func (e throttled) RetryAfter() time.Duration { return e.after }

func (c *roomsChannel) Conversations(context.Context) ([]convo.Conversation, error) {
	return c.rooms, nil
}
func (c *roomsChannel) Identity(context.Context) (convo.Identity, error) {
	return convo.Identity{ID: "u-agent", Name: "agent"}, nil
}
func (c *roomsChannel) Send(context.Context, convo.Target, string) (string, error) {
	return "m-sent", nil
}
func (c *roomsChannel) Fetch(ctx context.Context, id, cur string) ([]convo.Message, string, error) {
	n := c.inflight.Add(1)
	defer c.inflight.Add(-1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			break
		}
	}
	c.mu.Lock()
	c.asks[id]++
	ask := c.asks[id]
	c.mu.Unlock()

	if c.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, cur, ctx.Err()
		case <-time.After(c.delay):
		}
	}
	if err := c.fail[id]; err != nil {
		return nil, cur, err
	}
	if left := c.throttle[id]; ask <= left {
		return nil, cur, throttled{after: 50 * time.Millisecond}
	}
	if cur != "" {
		return nil, cur, nil
	}
	return c.msgs[id], "done", nil
}

func room(id, name string, n int) (convo.Conversation, []convo.Message) {
	cv := convo.Conversation{ID: id, Kind: "channel", Name: name}
	var ms []convo.Message
	for i := 0; i < n; i++ {
		ms = append(ms, convo.Message{
			ID:   fmt.Sprintf("%s-%02d", id, i),
			At:   fmt.Sprintf("2026-09-09T10:00:%02dZ", i),
			From: convo.Author{ID: "u-bob", Name: "bob"},
			Text: fmt.Sprintf("%s message %d", name, i),
			Source: convo.Source{Kind: "channel", ConversationID: id, Name: name,
				ThreadID: fmt.Sprintf("%s-%02d", id, i)},
		})
	}
	return cv, ms
}

// The worker pool changes WHERE the fetch runs and nothing about what it
// guarantees. Asserted in one pass over eight rooms: they really are fetched
// concurrently; one room that fails permanently is reported and does not blind
// the seven that work; one room that is throttled is backed off and re-asked
// rather than abandoned; and within every room the journal order is the
// adapter's order.
func TestParallelFetchIsolatesRoomsAndBacksOffThrottles(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"AGENT_CONVERSATIONS_HOME": home, "CONVO_CHANNEL": "x"}

	ch := &roomsChannel{
		msgs: map[string][]convo.Message{}, asks: map[string]int{},
		delay:    30 * time.Millisecond,
		fail:     map[string]error{"c-3": convo.Wrap(convo.ErrHostUnavailable, "room 3 is gone")},
		throttle: map[string]int{"c-5": 2},
	}
	for i := 0; i < 8; i++ {
		cv, ms := room(fmt.Sprintf("c-%d", i), fmt.Sprintf("#room%d", i), 4)
		ch.rooms = append(ch.rooms, cv)
		ch.msgs[cv.ID] = ms
	}

	// No real sleeping in the throttle backoff, but count the waits.
	waits := atomic.Int32{}
	old := throttleSleep
	throttleSleep = func(d time.Duration) <-chan time.Time {
		waits.Add(1)
		if d != 50*time.Millisecond {
			t.Errorf("backoff waited %s, want the server's Retry-After of 50ms", d)
		}
		c := make(chan time.Time, 1)
		c <- time.Now()
		return c
	}
	defer func() { throttleSleep = old }()

	started := time.Now()
	code, _, errOut := runWith(t, ch, env, "fetch", "--replay-history", "--fetch-parallel", "4")
	took := time.Since(started)

	// A partial failure is reported as the failure it is (the room's own exit
	// code, the room named) — and the journal below proves the other seven
	// landed regardless.
	if code != convo.ExitUnavailable {
		t.Fatalf("exit = %d, want %d (the failing room's own code); stderr = %s", code, convo.ExitUnavailable, errOut)
	}
	if !strings.Contains(errOut, "1 of 8 conversations failed (#room3)") {
		t.Fatalf("stderr = %q — the failing room is not named", errOut)
	}
	if peak := ch.peak.Load(); peak < 2 || peak > 4 {
		t.Fatalf("peak concurrency %d — want >1 (it ran) and <=4 (it was bounded)", peak)
	}
	// 8 rooms x 30ms delay is 240ms sequentially, plus the throttled room's
	// two extra asks; four workers must land in well under that.
	if took > 200*time.Millisecond {
		t.Fatalf("pass took %s — the pool is not running rooms concurrently", took)
	}
	if ch.asks["c-5"] != 3 || waits.Load() != 2 {
		t.Fatalf("throttled room asked %d times with %d waits, want 3 and 2", ch.asks["c-5"], waits.Load())
	}

	// Per-room order is the adapter's; cross-room order may interleave.
	lines := journalLines(t, home, "default")
	last := map[string]int{}
	for _, l := range lines {
		var m convo.Message
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		var idx int
		fmt.Sscanf(m.ID[len(m.Source.ConversationID)+1:], "%02d", &idx)
		if prev, ok := last[m.Source.ConversationID]; ok && idx != prev+1 {
			t.Fatalf("room %s journalled %d after %d — in-room order was lost", m.Source.ConversationID, idx, prev)
		}
		last[m.Source.ConversationID] = idx
	}
	if len(lines) != 28 || len(last) != 7 {
		t.Fatalf("journal: %d lines from %d rooms, want 28 from 7", len(lines), len(last))
	}

	// Cursors: seven advanced, the failing one did not, so the next pass
	// re-asks only that room.
	fc := loadFetchCursor(fetchCursorPath(mustStore(t, home)))
	if fc.Tokens["c-3"] != "" || fc.Tokens["c-5"] != "done" || len(fc.Tokens) != 7 {
		t.Fatalf("cursors = %v", fc.Tokens)
	}
}

// --fetch-parallel 1 is the sequential path, still through the pool, and a
// room that stays throttled past the retry budget is a reported failure, not
// a hang.
func TestSequentialFetchAndExhaustedThrottle(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"AGENT_CONVERSATIONS_HOME": home, "CONVO_CHANNEL": "x"}
	ch := &roomsChannel{msgs: map[string][]convo.Message{}, asks: map[string]int{},
		throttle: map[string]int{"c-1": 100}}
	for i := 0; i < 3; i++ {
		cv, ms := room(fmt.Sprintf("c-%d", i), fmt.Sprintf("#room%d", i), 2)
		ch.rooms = append(ch.rooms, cv)
		ch.msgs[cv.ID] = ms
	}
	old := throttleSleep
	throttleSleep = func(time.Duration) <-chan time.Time {
		c := make(chan time.Time, 1)
		c <- time.Now()
		return c
	}
	defer func() { throttleSleep = old }()

	code, out, errOut := runWith(t, ch, env, "fetch", "--replay-history", "--fetch-parallel", "1")
	if code == 0 || !strings.Contains(errOut, "#room1") || !strings.Contains(errOut, "429") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errOut)
	}
	if ch.peak.Load() != 1 {
		t.Fatalf("peak concurrency %d with --fetch-parallel 1", ch.peak.Load())
	}
	if ch.asks["c-1"] != throttleRetries+1 {
		t.Fatalf("throttled room asked %d times, want %d", ch.asks["c-1"], throttleRetries+1)
	}
	if len(journalLines(t, home, "default")) != 4 {
		t.Fatalf("the two healthy rooms did not land")
	}
	if !errors.Is(convo.Wrap(convo.ErrHostUnavailable, "x"), convo.ErrHostUnavailable) {
		t.Fatal("sanity")
	}
}

// --json on journal and next is LINE JSON: one object per line, nothing else.
// The proof is parsing every line, which is what a script does.
func TestJournalAndNextJSONAreOneObjectPerLine(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"AGENT_CONVERSATIONS_HOME": home, "CONVO_CHANNEL": "memory"}
	ch := memchan.New()
	ch.Post("c-general", "u-bob", "bob", "line one\nwith a newline inside")
	ch.Post("c-general", "u-bob", "bob", `quotes "and" braces {}`)
	ch.Post("c-dm-alice", "u-alice", "alice", "three")
	if code, _, e := runWith(t, ch, env, "fetch"); code != 0 {
		t.Fatalf("fetch: %d %s", code, e)
	}

	parseLines := func(cmd string, out string, want int) {
		t.Helper()
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) != want {
			t.Fatalf("%s --json: %d lines, want %d:\n%s", cmd, len(lines), want, out)
		}
		for i, l := range lines {
			var m convo.Message
			if err := json.Unmarshal([]byte(l), &m); err != nil || m.ID == "" {
				t.Fatalf("%s --json line %d is not one message object: %v\n%q", cmd, i, err, l)
			}
		}
	}
	code, out, _ := runWith(t, ch, env, "journal", "--json")
	if code != 0 {
		t.Fatal(code)
	}
	parseLines("journal", out, 3)

	code, out, e := runWith(t, ch, env, "next", "--json", "--count", "2")
	if code != 0 {
		t.Fatalf("next: %d %s", code, e)
	}
	parseLines("next", out, 2)

	// An empty result is an empty stdout, not `[]` or `null`.
	code, out, _ = runWith(t, ch, env, "journal", "--json", "--from", "nobody")
	if code != 0 || out != "" {
		t.Fatalf("empty journal --json = %q, want nothing on stdout", out)
	}
}

func mustStore(t *testing.T, home string) *filestore.Store {
	t.Helper()
	st, err := filestore.New(home, "default")
	if err != nil {
		t.Fatal(err)
	}
	return st
}
