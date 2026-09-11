package file

import (
	"testing"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

func msg(id, text string) convo.Message {
	return convo.Message{
		ID: id, At: "2026-09-09T10:00:00Z",
		From: convo.Author{ID: "u-alice", Name: "alice"},
		Text: text,
		Source: convo.Source{
			Kind: "channel", ConversationID: "c-general", Name: "#general", ThreadID: id,
		},
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// THE MOST-COPIED BUG IN THIS CLASS OF SYSTEM, asserted three ways:
//
//	· reading the journal must not consume;
//	· the read cursor advances on DELIVERY, with or without an ack;
//	· ack is separate, and acking does NOT move the cursor.
//
// Fuse any two of these and you get either infinite redelivery or silent loss.
func TestCursorVersusAck(t *testing.T) {
	s := newStore(t)
	if err := s.Append([]convo.Message{msg("m-1", "deploy looks red"), msg("m-2", "same here")}); err != nil {
		t.Fatal(err)
	}

	// Reading is not consuming.
	before, _ := s.ReadOffset()
	all, err := s.Journal()
	if err != nil || len(all) != 2 {
		t.Fatalf("journal: %v (%d)", err, len(all))
	}
	after, _ := s.ReadOffset()
	if before != after || after != 0 {
		t.Fatalf("Journal() moved the read cursor: %d -> %d", before, after)
	}

	// Delivery advances the cursor — and only past what was handed over.
	got, err := s.Next(1)
	if err != nil || len(got) != 1 || got[0].ID != "m-1" {
		t.Fatalf("Next(1) = %v, %v", got, err)
	}
	mid, _ := s.ReadOffset()
	if mid == 0 {
		t.Fatal("the read cursor did not advance on delivery")
	}

	// No ack was given, yet m-1 must not be redelivered: the CURSOR stops
	// redelivery, not the ack.
	got, err = s.Next(0)
	if err != nil || len(got) != 1 || got[0].ID != "m-2" {
		t.Fatalf("second Next = %v, %v — m-1 must not be redelivered", got, err)
	}

	// Both are delivered; neither is acked. `--new` must still show them,
	// because delivered-but-unacked is exactly what a failed answer leaves
	// behind, and it is the only way a declined message is ever seen again.
	un, err := s.Unacked()
	if err != nil || len(un) != 2 {
		t.Fatalf("unacked = %d, want 2", len(un))
	}

	// Ack is explicit and does not touch the cursor.
	curBefore, _ := s.ReadOffset()
	if err := s.Ack([]string{"m-1"}); err != nil {
		t.Fatal(err)
	}
	curAfter, _ := s.ReadOffset()
	if curBefore != curAfter {
		t.Fatalf("Ack moved the read cursor: %d -> %d", curBefore, curAfter)
	}
	un, _ = s.Unacked()
	if len(un) != 1 || un[0].ID != "m-2" {
		t.Fatalf("after acking m-1, unacked = %v", un)
	}
}

// Nothing outstanding is nothing outstanding — not an error, and not a
// redelivery of the whole log.
func TestNextOnEmptyAndDrained(t *testing.T) {
	s := newStore(t)
	got, err := s.Next(0)
	if err != nil || got != nil {
		t.Fatalf("empty store: %v %v", got, err)
	}
	_ = s.Append([]convo.Message{msg("m-1", "hi")})
	if got, _ = s.Next(0); len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if got, _ = s.Next(0); len(got) != 0 {
		t.Fatalf("drained store must return nothing, got %d", len(got))
	}
}

// A partial final line is a write caught mid-flight. It must be skipped and the
// cursor must NOT advance past it — the writer will finish it, and the next
// call picks it up. Parsing it would lose a message that was never lost.
func TestPartialFinalLineIsNotConsumed(t *testing.T) {
	s := newStore(t)
	_ = s.Append([]convo.Message{msg("m-1", "complete")})
	f, err := openAppend(s.JournalPath())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"id":"m-2","text":"half writ`)
	_ = f.Close()

	got, err := s.Next(0)
	if err != nil || len(got) != 1 || got[0].ID != "m-1" {
		t.Fatalf("got %v, %v — only the complete line may be delivered", got, err)
	}
	off, _ := s.ReadOffset()
	all, _ := s.Journal()
	if len(all) != 1 {
		t.Fatalf("journal read %d complete messages, want 1", len(all))
	}
	// The cursor stopped at the end of the complete line, not the file end.
	if off == 0 {
		t.Fatal("cursor should have advanced past the complete line")
	}
}

// Ack is idempotent: at-least-once delivery means a handler WILL see an id
// twice, and "I have already seen this" must never be an error.
func TestAckIsIdempotent(t *testing.T) {
	s := newStore(t)
	_ = s.Append([]convo.Message{msg("m-1", "hi")})
	for i := 0; i < 3; i++ {
		if err := s.Ack([]string{"m-1"}); err != nil {
			t.Fatal(err)
		}
	}
	acked, _ := s.Acked()
	if len(acked) != 1 {
		t.Fatalf("acked = %v, want one entry", acked)
	}
}

// The journal survives a round trip with every envelope field intact. `raw` in
// particular must never be dropped — you will need a field you did not
// anticipate.
func TestEnvelopeRoundTrip(t *testing.T) {
	s := newStore(t)
	html := "<p>hi</p>"
	reply := "m-0"
	in := convo.Message{
		ID: "m-9", At: "2026-09-09T10:00:00Z",
		From: convo.Author{ID: "u-bob", Name: "bob"},
		Text: "hi", HTML: &html,
		Source:     convo.Source{Kind: "chat", ConversationID: "c-dm-bob", Name: "dm:bob"},
		ReplyToID:  &reply,
		MentionsMe: true,
		Raw:        []byte(`{"unanticipated":"field"}`),
	}
	if err := s.Append([]convo.Message{in}); err != nil {
		t.Fatal(err)
	}
	out, _ := s.Journal()
	if len(out) != 1 {
		t.Fatal("round trip lost the message")
	}
	got := out[0]
	if got.ID != in.ID || got.Text != in.Text || !got.MentionsMe ||
		got.HTML == nil || *got.HTML != html ||
		got.ReplyToID == nil || *got.ReplyToID != reply ||
		string(got.Raw) != `{"unanticipated":"field"}` {
		t.Fatalf("round trip lost a field: %+v", got)
	}
}
