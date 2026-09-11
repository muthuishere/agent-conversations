package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// A live test against a running Graph-shaped server, SKIPPED BY DEFAULT.
//
//	CONVO_LIVE_TEAMS=1 go test ./internal/channel/teams/ -run Live -v
//
// Gated on an env var rather than on "is something listening on :4000",
// because a test that talks to whatever happens to be running is a test that
// posts into somebody's channel. Unlike the host live test this one WRITES —
// it has to, since the thing being proved is that a reply reaches the thread —
// so point it only at a simulator you own:
//
//	CONVO_LIVE_TEAMS_URL=http://127.0.0.1:4000/v1.0   (default)
//
// It verifies the ARTIFACT, not the status code: after Send returns, the reply
// is read back out of the thread. Replies are threaded, and reading the room at
// top level shows nothing — which is how a successful run looks like a total
// failure.
func TestLiveTeamsChannel(t *testing.T) {
	if os.Getenv("CONVO_LIVE_TEAMS") != "1" {
		t.Skip("set CONVO_LIVE_TEAMS=1 to run against a running Teams simulator")
	}
	base := os.Getenv("CONVO_LIVE_TEAMS_URL")
	if base == "" {
		base = "http://127.0.0.1:4000/v1.0"
	}
	base = strings.TrimRight(base, "/")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lc := &liveClient{t: t, base: base}
	if !lc.reachable(ctx) {
		t.Fatalf("no Teams simulator at %s — start one, or unset CONVO_LIVE_TEAMS", base)
	}

	const agentName = "convo-agent"
	const personName = "Bob Martin"

	ch, err := New(Config{BaseURL: base, UserName: agentName})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	self, err := ch.Identity(ctx)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	t.Logf("identity: %s (%s)", self.Name, self.ID)

	// Setup: make sure the agent is in a team, the way a bot is added to one.
	team := lc.firstTeamID(ctx)
	lc.addTeamMember(ctx, team, self.ID)

	convs, err := ch.Conversations(ctx)
	if err != nil {
		t.Fatalf("Conversations: %v", err)
	}
	if len(convs) == 0 {
		t.Fatal("no conversations discovered")
	}
	var target convo.Conversation
	for _, cv := range convs {
		t.Logf("conversation: %-12s %-14s %s", cv.Kind, cv.Name, cv.ID)
		if cv.Kind == kindChannel && target.ID == "" {
			target = cv
		}
	}
	if target.ID == "" {
		t.Fatal("no channel conversation discovered")
	}

	// Start from NOW, so the assertion is about this run's traffic and not the
	// simulator's backlog.
	cursor, err := ch.PrimeCursor(ctx, target.ID)
	if err != nil {
		t.Fatalf("PrimeCursor: %v", err)
	}

	// A person posts.
	question := fmt.Sprintf("when does the deploy land? (live test %d)", time.Now().UnixNano())
	ref, _ := parseConvID(target.ID)
	posted := lc.postAs(ctx, personName,
		fmt.Sprintf("%s/teams/%s/channels/%s/messages", base, escSeg(ref.teamID), escSeg(ref.channelID)),
		question)
	t.Logf("%s posted %s in %s", personName, posted, target.Name)

	// We fetch it through the Channel.
	msgs, cursor, err := ch.Fetch(ctx, target.ID, cursor)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	var got *convo.Message
	for i := range msgs {
		t.Logf("fetched: id=%s from=%q kind=%s thread=%s text=%q",
			msgs[i].ID, msgs[i].From.Name, msgs[i].Source.Kind, msgs[i].Source.ThreadID, msgs[i].Text)
		if msgs[i].ID == posted {
			got = &msgs[i]
		}
	}
	if got == nil {
		t.Fatalf("the posted message never came back through Fetch (%d messages)", len(msgs))
	}
	if got.From.Name != personName || got.Text != question {
		t.Fatalf("envelope does not match what was posted: %+v", *got)
	}

	// We answer it, routed from the message itself — never by choosing.
	answer := fmt.Sprintf("friday, after the freeze (live test %d)", time.Now().UnixNano())
	replyID, err := ch.Send(ctx, convo.ReplyTarget(*got), answer)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	t.Logf("replied %s into thread %s", replyID, got.Source.ThreadID)

	// VERIFY THE ARTIFACT. Read the thread back out of the server.
	replies := lc.replies(ctx,
		fmt.Sprintf("%s/teams/%s/channels/%s/messages/%s/replies",
			base, escSeg(ref.teamID), escSeg(ref.channelID), escSeg(got.Source.ThreadID)))
	found := false
	for _, r := range replies {
		t.Logf("thread %s contains: id=%s from=%q text=%q",
			got.Source.ThreadID, r.ID, name(r), r.Body.Content)
		if r.ID == replyID && r.Body.Content == answer {
			found = true
		}
	}
	if !found {
		t.Fatalf("reply %s is NOT in thread %s — the send reported success and the "+
			"message is not there", replyID, got.Source.ThreadID)
	}

	// And the next poll must be quiet: our own reply is our echo, and a channel
	// that hands it back wakes the agent to answer itself.
	quiet, _, err := ch.Fetch(ctx, target.ID, cursor)
	if err != nil {
		t.Fatalf("third Fetch: %v", err)
	}
	for _, m := range quiet {
		if m.From.ID == self.ID {
			t.Fatalf("self-echo came back through Fetch: %+v", m)
		}
	}
	t.Logf("next poll returned %d messages, none of them ours", len(quiet))
}

func name(m graphMessage) string {
	if m.From != nil && m.From.User != nil {
		return m.From.User.DisplayName
	}
	return "(none)"
}

// liveClient is the test's own crude client. It is deliberately NOT the adapter
// — a test that verifies an adapter with the adapter proves nothing.
type liveClient struct {
	t    *testing.T
	base string
}

func (l *liveClient) do(ctx context.Context, method, url, asUser string, body any) []byte {
	l.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		l.t.Fatalf("%s %s: %v", method, url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if asUser != "" {
		req.Header.Set("x-user-name", asUser)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		l.t.Fatalf("%s %s: HTTP %d: %s", method, url, resp.StatusCode, out)
	}
	return out
}

func (l *liveClient) reachable(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.base+"/teams", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 400
}

func (l *liveClient) firstTeamID(ctx context.Context) string {
	var out collection[graphTeam]
	if err := json.Unmarshal(l.do(ctx, http.MethodGet, l.base+"/teams", "", nil), &out); err != nil {
		l.t.Fatalf("decoding teams: %v", err)
	}
	if len(out.Value) == 0 {
		l.t.Fatal("the simulator has no teams")
	}
	return out.Value[0].ID
}

func (l *liveClient) addTeamMember(ctx context.Context, teamID, userID string) {
	l.do(ctx, http.MethodPost, fmt.Sprintf("%s/teams/%s/members", l.base, escSeg(teamID)),
		"", map[string]string{"userId": userID})
}

func (l *liveClient) postAs(ctx context.Context, user, url, text string) string {
	var m graphMessage
	body := map[string]any{"body": map[string]string{"contentType": "text", "content": text}}
	if err := json.Unmarshal(l.do(ctx, http.MethodPost, url, user, body), &m); err != nil {
		l.t.Fatalf("decoding posted message: %v", err)
	}
	if m.ID == "" {
		l.t.Fatal("the simulator accepted a post and returned no id")
	}
	return m.ID
}

func (l *liveClient) replies(ctx context.Context, url string) []graphMessage {
	var out collection[graphMessage]
	if err := json.Unmarshal(l.do(ctx, http.MethodGet, url, "", nil), &out); err != nil {
		l.t.Fatalf("decoding replies: %v", err)
	}
	return out.Value
}
