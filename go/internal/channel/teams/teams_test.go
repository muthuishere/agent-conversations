package teams

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// The ids below are the ones in testdata/, recorded from a real Graph-shaped
// server. They are kept verbatim rather than prettified: a channel id really
// does contain `:` and `@`, and a test using a tidy fake id would not catch the
// URL-encoding bug that makes every request 404.
const (
	fxTeam    = "b1e19f99-58ec-4721-826b-9a216d2d1212"
	fxChannel = "19:b39cd6954d0f41989e186d42f9b3c174@thread.tacv2"
	fxChat    = "a0a6b1ec-cb4c-4a6f-80e1-e6edc07246e8"
	fxRoot    = "1789155367080001"
	fxReply   = "1789155367098003"
	fxSelfID  = "85d2185b-8f72-4d8f-8575-82ea4880a610"
	fxBobID   = "392b1a78-1b52-4fc0-9756-f8eafe592bce"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

// recordedRequest is what the fake server saw, so a test can assert on the
// REQUEST as well as the response — routing bugs live in the request.
type recordedRequest struct {
	Method string
	Path   string // escaped: the encoding is the thing under test
	Query  string
	Body   string
}

type fakeGraph struct {
	t    *testing.T
	mu   sync.Mutex
	seen []recordedRequest
	// routes maps "METHOD <escaped path>" to a handler.
	routes map[string]func(r *http.Request) (int, string)
	srv    *httptest.Server
}

func newFakeGraph(t *testing.T) *fakeGraph {
	t.Helper()
	f := &fakeGraph{t: t, routes: map[string]func(*http.Request) (int, string){}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGraph) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.seen = append(f.seen, recordedRequest{
		Method: r.Method, Path: r.URL.EscapedPath(),
		Query: r.URL.RawQuery, Body: string(body),
	})
	f.mu.Unlock()

	if h, ok := f.routes[r.Method+" "+r.URL.EscapedPath()]; ok {
		code, out := h(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, out)
		return
	}
	// An unrouted GET collection answers empty rather than 404: a test should
	// fail on a wrong assertion, not on the fake being incomplete.
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"value":[]}`)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"error":{"code":"itemNotFound","message":"no route in fake"}}`)
}

func (f *fakeGraph) on(method, path string, h func(*http.Request) (int, string)) {
	f.routes[method+" "+path] = h
}

func (f *fakeGraph) json(method, path string, body []byte) {
	f.on(method, path, func(*http.Request) (int, string) { return 200, string(body) })
}

func (f *fakeGraph) requests() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.seen))
	copy(out, f.seen)
	return out
}

// escaped path helpers — these are what the adapter must produce.
var (
	chESC      = "19%3Ab39cd6954d0f41989e186d42f9b3c174%40thread.tacv2"
	pChanBase  = "/v1.0/teams/" + fxTeam + "/channels/" + chESC
	pChanDelta = pChanBase + "/messages/delta"
	pChanMsgs  = pChanBase + "/messages"
	pChanReply = pChanBase + "/messages/" + fxRoot + "/replies"
	pChatDelta = "/v1.0/chats/" + fxChat + "/messages/delta"
	pChatMsgs  = "/v1.0/chats/" + fxChat + "/messages"
	emptyDelta = `{"value":[],"@odata.deltaLink":"http://x/v1.0/messages/delta?%24deltatoken=999"}`
)

// newTestChannel wires the adapter to the fake, with the standard fixtures for
// identity mounted, since every path needs an identity.
func newTestChannel(t *testing.T, f *fakeGraph) *Channel {
	t.Helper()
	f.json(http.MethodGet, "/v1.0/me", fixture(t, "me.json"))
	c, err := New(Config{
		BaseURL:        f.srv.URL + "/v1.0",
		UserName:       "convo-agent",
		HTTPClient:     f.srv.Client(),
		ReplyScanDepth: 10,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestIdentity(t *testing.T) {
	f := newFakeGraph(t)
	c := newTestChannel(t, f)

	id, err := c.Identity(context.Background())
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id.ID != fxSelfID || id.Name != "convo-agent" {
		t.Fatalf("identity = %+v, want id %s name convo-agent", id, fxSelfID)
	}
	// Identity is cached: it cannot change mid-process, and re-asking on every
	// fetch turns one poll into two requests.
	if _, err := c.Identity(context.Background()); err != nil {
		t.Fatalf("second Identity: %v", err)
	}
	n := 0
	for _, r := range f.requests() {
		if r.Path == "/v1.0/me" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("/me called %d times, want 1 (identity must be cached)", n)
	}
}

func TestConversations(t *testing.T) {
	f := newFakeGraph(t)
	c := newTestChannel(t, f)
	f.json(http.MethodGet, "/v1.0/me/joinedTeams", fixture(t, "joined-teams.json"))
	f.json(http.MethodGet, "/v1.0/teams/"+fxTeam+"/channels", fixture(t, "channels.json"))
	f.json(http.MethodGet, "/v1.0/me/chats", fixture(t, "chats.json"))

	got, err := c.Conversations(context.Background())
	if err != nil {
		t.Fatalf("Conversations: %v", err)
	}
	want := []convo.Conversation{
		{ID: "channel:" + fxTeam + "/" + fxChannel, Kind: "channel", Name: "#General"},
		{ID: "channel:" + fxTeam + "/19:c47d68b7bbcb43f79ba59fbd3216f28d@thread.tacv2", Kind: "channel", Name: "#Engineering"},
		{ID: "channel:" + fxTeam + "/19:00e2a7cf4cb6416fa5bb4f4ba8d9f2f6@thread.tacv2", Kind: "channel", Name: "#Random"},
		{ID: "chat:" + fxChat, Kind: "chat", Name: "dm:Bob Martin"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d conversations, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		// Only the General channel id is asserted verbatim; the other two come
		// from the same fixture and are compared on kind and name, which is
		// what the caller routes on.
		if got[i].Kind != want[i].Kind || got[i].Name != want[i].Name {
			t.Errorf("conversation %d = %+v, want kind %s name %s",
				i, got[i], want[i].Kind, want[i].Name)
		}
	}
	if got[0].ID != want[0].ID {
		t.Errorf("channel conversation id = %q, want %q", got[0].ID, want[0].ID)
	}
	if got[3].ID != want[3].ID {
		t.Errorf("chat conversation id = %q, want %q", got[3].ID, want[3].ID)
	}
}

// TestFetchChatSuppressesSelfEcho is the test that stops the money fire.
func TestFetchChatSuppressesSelfEcho(t *testing.T) {
	f := newFakeGraph(t)
	c := newTestChannel(t, f)
	f.json(http.MethodGet, pChatDelta, fixture(t, "chat-delta-initial.json"))

	convID := "chat:" + fxChat
	msgs, next, err := c.Fetch(context.Background(), convID, "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// The fixture holds TWO messages: one from a person, one from us.
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1 (our own echo must be dropped): %+v", len(msgs), msgs)
	}
	m := msgs[0]
	switch {
	case m.From.ID != fxBobID:
		t.Errorf("from.id = %q, want %q", m.From.ID, fxBobID)
	case m.From.Name != "Bob Martin":
		t.Errorf("from.name = %q", m.From.Name)
	case m.Text != "are you around?":
		t.Errorf("text = %q", m.Text)
	case m.Source.Kind != "chat":
		t.Errorf("source.kind = %q, want chat", m.Source.Kind)
	case m.Source.ConversationID != convID:
		t.Errorf("source.conversationId = %q, want %q", m.Source.ConversationID, convID)
	case m.Source.ThreadID != "":
		t.Errorf("a chat has no thread, got threadId %q", m.Source.ThreadID)
	case len(m.Raw) == 0:
		t.Error("raw payload was discarded")
	}
	if next == "" {
		t.Fatal("no next cursor")
	}
	cs, err := parseCursor(next)
	if err != nil {
		t.Fatalf("cursor did not round-trip: %v", err)
	}
	if cs.Delta == "" {
		t.Errorf("cursor carries no delta token: %+v", cs)
	}
}

// TestFetchChannelSeesThreadedReplies is the finding this adapter exists to
// prove: the delta stream carries the ROOT only, so an adapter that follows it
// alone is deaf to every reply — including replies to its own answers.
func TestFetchChannelSeesThreadedReplies(t *testing.T) {
	f := newFakeGraph(t)
	c := newTestChannel(t, f)
	f.on(http.MethodGet, pChanDelta, func(r *http.Request) (int, string) {
		if strings.Contains(r.URL.RawQuery, "deltatoken") {
			return 200, emptyDelta
		}
		return 200, string(fixture(t, "channel-delta-initial.json"))
	})
	f.json(http.MethodGet, pChanMsgs, fixture(t, "channel-messages-top.json"))
	f.json(http.MethodGet, pChanReply, fixture(t, "channel-replies.json"))

	convID := "channel:" + fxTeam + "/" + fxChannel
	msgs, next, err := c.Fetch(context.Background(), convID, "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (root + threaded reply): %+v", len(msgs), msgs)
	}
	// Oldest first. A journal appended newest-first replays a conversation
	// backwards, and the agent answers the wrong question.
	if msgs[0].ID != fxRoot || msgs[1].ID != fxReply {
		t.Fatalf("order = %s,%s want %s,%s (oldest first)",
			msgs[0].ID, msgs[1].ID, fxRoot, fxReply)
	}
	// A root's thread is itself; a reply's thread is its root. Both must reply
	// into the SAME thread or the conversation splits in two.
	if msgs[0].Source.ThreadID != fxRoot {
		t.Errorf("root threadId = %q, want its own id %q", msgs[0].Source.ThreadID, fxRoot)
	}
	if msgs[1].Source.ThreadID != fxRoot {
		t.Errorf("reply threadId = %q, want the root %q", msgs[1].Source.ThreadID, fxRoot)
	}
	if msgs[1].ReplyToID == nil || *msgs[1].ReplyToID != fxRoot {
		t.Errorf("reply replyToId = %v, want %q", msgs[1].ReplyToID, fxRoot)
	}

	// Second poll with the returned cursor: everything is already seen, so a
	// quiet channel must be silent. A watermark that does not hold redelivers
	// the same reply on every tick, forever.
	again, _, err := c.Fetch(context.Background(), convID, next)
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second poll returned %d messages, want 0: %+v", len(again), again)
	}

	// The channel id must reach the wire percent-encoded. Unencoded, every one
	// of these requests is a 404 and the listener is silently deaf.
	for _, r := range f.requests() {
		if strings.Contains(r.Path, "thread.tacv2") && !strings.Contains(r.Path, "%3A") {
			t.Errorf("channel id was not URL-encoded in path %q", r.Path)
		}
	}
}

func TestFetchRejectsBadInput(t *testing.T) {
	f := newFakeGraph(t)
	c := newTestChannel(t, f)

	cases := []struct {
		name   string
		convID string
		cursor string
	}{
		{"no kind prefix", "just-an-id", ""},
		{"channel without a team", "channel:onlyonepart", ""},
		{"empty chat id", "chat:", ""},
		{"unreadable cursor", "chat:" + fxChat, "not-base64-%%%"},
		{"cursor from another version", "chat:" + fxChat, mustEncode(t, cursor{V: 99})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := c.Fetch(context.Background(), tc.convID, tc.cursor)
			if !errors.Is(err, convo.ErrNotConfigured) {
				t.Fatalf("err = %v, want ErrNotConfigured — a cursor or id we cannot "+
					"read must fail loudly, never silently restart from the beginning", err)
			}
		})
	}
}

func mustEncode(t *testing.T, c cursor) string {
	t.Helper()
	s, err := c.encode()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSendRouting is the table that matters most: a reply routed to the wrong
// target returns 200 and vanishes.
func TestSendRouting(t *testing.T) {
	cases := []struct {
		name     string
		target   convo.Target
		text     string
		wantPath string
		wantErr  bool
	}{
		{
			name:     "dm goes to the conversation",
			target:   convo.Target{ConversationID: "chat:" + fxChat, Kind: "chat"},
			text:     "on it",
			wantPath: pChatMsgs,
		},
		{
			name: "channel message goes to the THREAD, not the room",
			target: convo.Target{
				ConversationID: "channel:" + fxTeam + "/" + fxChannel,
				Kind:           "channel", ThreadID: fxRoot,
			},
			text:     "friday",
			wantPath: pChanReply,
		},
		{
			name: "channel with no thread starts one",
			target: convo.Target{
				ConversationID: "channel:" + fxTeam + "/" + fxChannel,
				Kind:           "channel",
			},
			text:     "heads up",
			wantPath: pChanMsgs,
		},
		{
			name: "kind disagreeing with the id is refused",
			target: convo.Target{
				ConversationID: "chat:" + fxChat, Kind: "channel",
			},
			text:    "nope",
			wantErr: true,
		},
		{
			name:    "unknown conversation id is refused",
			target:  convo.Target{ConversationID: "carrier-pigeon:17", Kind: "chat"},
			text:    "nope",
			wantErr: true,
		},
		{
			name:    "empty text is refused",
			target:  convo.Target{ConversationID: "chat:" + fxChat, Kind: "chat"},
			text:    "   ",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGraph(t)
			c := newTestChannel(t, f)
			sent := `{"id":"m-sent-1"}`
			f.json(http.MethodPost, pChatMsgs, []byte(sent))
			f.json(http.MethodPost, pChanMsgs, []byte(sent))
			f.json(http.MethodPost, pChanReply, []byte(sent))

			id, err := c.Send(context.Background(), tc.target, tc.text)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got id %q — a reply with nowhere to go "+
						"must fail loudly", id)
				}
				if !errors.Is(err, convo.ErrNotConfigured) {
					t.Fatalf("err = %v, want ErrNotConfigured", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if id != "m-sent-1" {
				t.Errorf("id = %q, want the id the platform returned", id)
			}
			var posted *recordedRequest
			reqs := f.requests()
			for i := range reqs {
				if reqs[i].Method == http.MethodPost {
					posted = &reqs[i]
				}
			}
			if posted == nil {
				t.Fatal("nothing was POSTed")
			}
			if posted.Path != tc.wantPath {
				t.Errorf("posted to %q, want %q", posted.Path, tc.wantPath)
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(posted.Body), &body); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			b, _ := body["body"].(map[string]any)
			if b["content"] != tc.text {
				t.Errorf("content = %v, want %q", b["content"], tc.text)
			}
		})
	}
}

// TestSendRefusesAnIdlessSuccess: a 2xx with no id is the platform saying
// "fine" while having done nothing we can point at. Verify the artifact, never
// the status code.
func TestSendRefusesAnIdlessSuccess(t *testing.T) {
	f := newFakeGraph(t)
	c := newTestChannel(t, f)
	f.json(http.MethodPost, pChatMsgs, []byte(`{"ok":true}`))

	_, err := c.Send(context.Background(),
		convo.Target{ConversationID: "chat:" + fxChat, Kind: "chat"}, "hello")
	if err == nil {
		t.Fatal("a send with no returned id must not be reported as sent")
	}
}

func TestNormalisation(t *testing.T) {
	cases := []struct {
		name     string
		body     graphBody
		wantText string
		wantHTML bool
	}{
		{"plain text passes through", graphBody{ContentType: "text", Content: "ship it"}, "ship it", false},
		{
			name:     "html is flattened but never discarded",
			body:     graphBody{ContentType: "html", Content: `<p>hi <at id="0">agent</at> &amp; team</p>`},
			wantText: "hi agent & team",
			wantHTML: true,
		},
		{"an empty body is a string, never null", graphBody{ContentType: "text"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, htmlBody := bodyText(tc.body)
			if text != tc.wantText {
				t.Errorf("text = %q, want %q", text, tc.wantText)
			}
			if (htmlBody != nil) != tc.wantHTML {
				t.Errorf("html present = %v, want %v", htmlBody != nil, tc.wantHTML)
			}
		})
	}
}

func TestMentionsMe(t *testing.T) {
	m := graphMessage{Mentions: []graphMention{
		{Mentioned: &graphFrom{User: &graphUser{ID: fxSelfID}}},
	}}
	if !mentionsUser(m, fxSelfID) {
		t.Error("mention of us was not detected")
	}
	if mentionsUser(m, "somebody-else") {
		t.Error("someone else's mention was attributed to us")
	}
}

// TestThrottleIsRetried: Graph answers 429 with Retry-After, and a channel that
// ignores it gets the whole app's quota cut.
func TestThrottleIsRetried(t *testing.T) {
	f := newFakeGraph(t)
	c := newTestChannel(t, f)
	var calls int
	f.on(http.MethodGet, pChatDelta, func(*http.Request) (int, string) {
		calls++
		if calls == 1 {
			return http.StatusTooManyRequests, `{"error":{"code":"throttled","message":"slow down"}}`
		}
		return 200, emptyDelta
	})

	if _, _, err := c.Fetch(context.Background(), "chat:"+fxChat, ""); err != nil {
		t.Fatalf("Fetch after a 429: %v", err)
	}
	if calls != 2 {
		t.Fatalf("delta called %d times, want 2 (one throttled, one retried)", calls)
	}
}

func TestNotFoundIsTyped(t *testing.T) {
	f := newFakeGraph(t)
	c := newTestChannel(t, f)
	f.on(http.MethodGet, pChatDelta, func(*http.Request) (int, string) {
		return http.StatusNotFound, `{"error":{"code":"itemNotFound","message":"Chat not found"}}`
	})
	_, _, err := c.Fetch(context.Background(), "chat:"+fxChat, "")
	if !errors.Is(err, convo.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured for a 404", err)
	}
}

// TestPrimeCursorEmitsNothing: attaching a listener to a busy room must not
// replay its whole history into the journal and wake an agent for every
// message anyone ever sent.
func TestPrimeCursorEmitsNothing(t *testing.T) {
	f := newFakeGraph(t)
	c := newTestChannel(t, f)
	f.json(http.MethodGet, pChatDelta, fixture(t, "chat-delta-initial.json"))

	cur, err := c.PrimeCursor(context.Background(), "chat:"+fxChat)
	if err != nil {
		t.Fatalf("PrimeCursor: %v", err)
	}
	cs, err := parseCursor(cur)
	if err != nil || cs.Delta == "" {
		t.Fatalf("primed cursor is not usable: %+v %v", cs, err)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	in := newCursor()
	in.Delta = "102"
	in.Threads = map[string]string{"root-1": "2026-09-11T19:36:07.098Z"}
	enc := mustEncode(t, in)
	if strings.Contains(enc, "root-1") {
		t.Error("the cursor is readable — nothing above the seam should be tempted to parse it")
	}
	out, err := parseCursor(enc)
	if err != nil {
		t.Fatalf("parseCursor: %v", err)
	}
	if out.Delta != in.Delta || out.Threads["root-1"] != in.Threads["root-1"] {
		t.Fatalf("round trip lost data: %+v", out)
	}
	// Pruning keeps the cursor a constant size on a long-lived listener.
	out.prune([]string{"some-other-root"})
	if len(out.Threads) != 0 {
		t.Fatalf("prune kept %d threads outside the scan window", len(out.Threads))
	}
}
