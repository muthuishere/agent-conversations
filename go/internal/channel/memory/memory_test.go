package memory

import (
	"context"
	"testing"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
	filestore "github.com/muthuishere/agent-conversations/go/internal/store/file"
)

// The full offline round trip: a person posts, the daemon-shaped loop fetches
// with an opaque cursor, journals, hands over, and replies — with no network,
// no account and no server anywhere.
func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	ch := New()
	store, err := filestore.New(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}

	ch.Post("c-general", "u-alice", "alice", "deploy looks red")
	ch.Post("c-dm-alice", "u-alice", "alice", "status?")

	cursors := map[string]string{}
	convs, err := ch.Conversations(ctx)
	if err != nil || len(convs) != 2 {
		t.Fatalf("conversations: %v (%d)", err, len(convs))
	}
	for _, c := range convs {
		msgs, next, err := ch.Fetch(ctx, c.ID, cursors[c.ID])
		if err != nil {
			t.Fatal(err)
		}
		cursors[c.ID] = next
		if err := store.Append(msgs); err != nil {
			t.Fatal(err)
		}
	}

	batch, err := store.Next(0)
	if err != nil || len(batch) != 2 {
		t.Fatalf("batch = %d, want 2 (%v)", len(batch), err)
	}

	// Reply to each, routed from the message itself — the caller supplies an
	// id and never chooses between "send to chat" and "reply in thread".
	for _, m := range batch {
		if _, err := ch.Send(ctx, convo.ReplyTarget(m), "ack: "+m.Text); err != nil {
			t.Fatal(err)
		}
	}

	// A second fetch must return NOTHING. Our own replies are suppressed by
	// identity before anything sees them; without that the agent's reply wakes
	// it to reply again, forever, on a real channel, for real money.
	for _, c := range convs {
		msgs, next, err := ch.Fetch(ctx, c.ID, cursors[c.ID])
		if err != nil {
			t.Fatal(err)
		}
		cursors[c.ID] = next
		if len(msgs) != 0 {
			t.Fatalf("self-echo leaked back in: %+v", msgs)
		}
	}

	// The replies really were posted — verify the artifact, not the status.
	// Note WHERE they are: the channel reply is threaded under m-1, so reading
	// the room at top level and seeing nothing new would be the wrong check.
	var threaded, dm int
	for _, m := range ch.All() {
		if m.From.ID != "u-agent" {
			continue
		}
		if m.Source.Kind == "channel" && m.Source.ThreadID == "m-1" {
			threaded++
		}
		if m.Source.Kind == "chat" {
			dm++
		}
	}
	if threaded != 1 || dm != 1 {
		t.Fatalf("replies landed wrong: threaded=%d dm=%d", threaded, dm)
	}
}

// An unknown conversation must fail LOUDLY. A platform that returns success for
// a message nobody will ever see is how replies vanish silently.
func TestSendToUnknownConversationFailsLoudly(t *testing.T) {
	ch := New()
	_, err := ch.Send(context.Background(), convo.Target{ConversationID: "c-nope"}, "hi")
	if convo.ExitCode(err) != convo.ExitNotConfigured {
		t.Fatalf("want exit 65, got %v (%d)", err, convo.ExitCode(err))
	}
}

// The cursor is OPAQUE to everything above the Channel. This test only asserts
// that feeding back what Fetch returned resumes correctly — never what it looks
// like inside.
func TestCursorIsOpaqueButResumes(t *testing.T) {
	ctx := context.Background()
	ch := New()
	ch.Post("c-general", "u-alice", "alice", "one")
	_, next, err := ch.Fetch(ctx, "c-general", "")
	if err != nil {
		t.Fatal(err)
	}
	ch.Post("c-general", "u-bob", "bob", "two")
	msgs, _, err := ch.Fetch(ctx, "c-general", next)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "two" {
		t.Fatalf("resume from cursor = %v (%v)", msgs, err)
	}
}
