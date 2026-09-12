package teams

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// A live, READ-ONLY test against a real Microsoft tenant through apl.
// SKIPPED BY DEFAULT:
//
//	CONVO_LIVE_APL_HANDLE=ms:<label> go test ./internal/channel/teams/ -run LiveAPL -v
//
// It is deliberately not the sibling of TestLiveTeamsChannel, which WRITES.
// Writing is how you prove a reply reaches a thread, and it is also how you post
// into a real colleague's channel by accident. Against a tenant that belongs to
// somebody, the only test worth running unattended is one that cannot say
// anything: this lists conversations, resolves identity, and primes a cursor.
// Nothing here calls Send, and nothing here should ever be made to.
//
// No handle, id, tenant or display name is hardcoded. The handle comes from the
// environment, and nothing read from the tenant is asserted against a literal —
// a test that pins a real person's name into a public repository has leaked it
// whether or not it passes.
func TestLiveAPLTeamsChannel(t *testing.T) {
	handle := os.Getenv("CONVO_LIVE_APL_HANDLE")
	if handle == "" {
		t.Skip("set CONVO_LIVE_APL_HANDLE=ms:<label> (see `apl accounts`) to run against a real tenant")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ch, err := NewViaAPL(Config{}, handle)
	if err != nil {
		t.Fatalf("NewViaAPL: %v", err)
	}

	self, err := ch.Identity(ctx)
	if err != nil {
		t.Fatalf("Identity through apl: %v", err)
	}
	if self.ID == "" {
		t.Fatal("/me returned no id — self-echo suppression would have nothing to compare against")
	}
	// Count only. The name is a real person's.
	t.Logf("identity resolved: id=%d chars, name=%d chars", len(self.ID), len(self.Name))

	convs, err := ch.Conversations(ctx)
	if err != nil {
		t.Fatalf("Conversations through apl: %v", err)
	}
	if len(convs) == 0 {
		t.Fatal("no conversations on a real tenant — the handle is probably not a member of anything")
	}
	var channels, chats int
	for _, c := range convs {
		switch c.Kind {
		case kindChannel:
			channels++
		case kindChat:
			chats++
		default:
			t.Fatalf("conversation %q has an unknown kind %q", c.ID, c.Kind)
		}
		if _, err := parseConvID(c.ID); err != nil {
			t.Fatalf("a discovered conversation id does not round-trip: %v", err)
		}
	}
	t.Logf("discovered %d conversations (%d channels, %d chats)", len(convs), channels, chats)

	// Prime, then fetch. Priming first is the point: an empty cursor against a
	// real tenant replays the entire history of a room into the journal, which
	// looks exactly like a flood of new traffic and wakes an agent for every
	// message anyone ever sent.
	var target string
	for _, c := range convs {
		if c.Kind == kindChannel {
			target = c.ID
			break
		}
	}
	if target == "" {
		t.Skip("handle is in no team channels; nothing to prime")
	}
	cur, err := ch.PrimeCursor(ctx, target)
	if err != nil {
		t.Fatalf("PrimeCursor through apl: %v", err)
	}
	if cur == "" {
		t.Fatal("PrimeCursor returned an empty cursor — the next fetch would replay everything")
	}
	msgs, next, err := ch.Fetch(ctx, target, cur)
	if err != nil {
		t.Fatalf("Fetch through apl: %v", err)
	}
	if next == "" {
		t.Fatal("Fetch returned an empty cursor — position was lost")
	}
	// A primed cursor should see nothing new, but a message can genuinely land
	// mid-test, so this is a shape check, not a count assertion.
	for _, m := range msgs {
		if m.From.ID == self.ID {
			t.Fatal("our own message survived self-echo suppression")
		}
		if m.Source.ConversationID != target {
			t.Fatalf("message routed to the wrong conversation: %q", m.Source.ConversationID)
		}
		if m.Source.Kind == kindChannel && m.Source.ThreadID == "" {
			t.Fatal("a channel message arrived with no thread id — a reply to it would land in the room")
		}
		if !strings.HasPrefix(m.At, "20") {
			t.Fatalf("timestamp is not RFC3339-ish: %q", m.At)
		}
	}
	t.Logf("fetched %d new message(s) after priming", len(msgs))
}
