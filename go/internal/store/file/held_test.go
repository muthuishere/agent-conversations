package file

import (
	"testing"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// from builds a message with an explicit author, which is what a delivery
// filter discriminates on.
func from(id, who string) convo.Message {
	m := msg(id, id)
	m.From = convo.Author{ID: "u-" + who, Name: who}
	return m
}

func ids(msgs []convo.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.ID)
	}
	return out
}

func eq(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// The cursor rule, at the level that owns it. A rejected message is held, not
// skipped: the read cursor may move past it only because the hold list carries
// it forward, and an unfiltered call gets it back.
func TestNextMatchingHoldsRatherThanSkips(t *testing.T) {
	s, err := New(t.TempDir(), "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append([]convo.Message{from("m-1", "alice"), from("m-2", "bob"), from("m-3", "alice")}); err != nil {
		t.Fatal(err)
	}
	onlyBob := func(m convo.Message) bool { return m.From.Name == "bob" }

	got, err := s.NextMatching(0, onlyBob)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ids(got), "m-2")

	held, _ := s.Held()
	eq(t, held, "m-1", "m-3")

	// The cursor DID advance — otherwise m-2 would be handed over forever.
	off, _ := s.ReadOffset()
	if off == 0 {
		t.Fatal("the read cursor did not move; every later call would redeliver m-2")
	}
	// ... and the same filter does not re-offer what it already rejected.
	if got, _ = s.NextMatching(0, onlyBob); len(got) != 0 {
		t.Fatalf("held messages were re-offered through the same filter: %v", ids(got))
	}

	// Drop the filter: the held messages come back, oldest first, and the
	// already-delivered one does not.
	got, err = s.NextMatching(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ids(got), "m-1", "m-3")
	if held, _ = s.Held(); len(held) != 0 {
		t.Fatalf("hold list not cleared: %v", held)
	}
}

// Held messages are delivered AHEAD of newer traffic, and a --count limit
// leaves the remainder held rather than dropping it.
func TestHeldMessagesComeFirstAndRespectALimit(t *testing.T) {
	s, _ := New(t.TempDir(), "t")
	_ = s.Append([]convo.Message{from("m-1", "alice"), from("m-2", "alice")})
	nobody := func(convo.Message) bool { return false }
	if got, _ := s.NextMatching(0, nobody); len(got) != 0 {
		t.Fatal("a reject-everything filter delivered something")
	}
	_ = s.Append([]convo.Message{from("m-3", "bob")})

	got, err := s.NextMatching(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ids(got), "m-1")
	held, _ := s.Held()
	eq(t, held, "m-2")

	got, _ = s.NextMatching(0, nil)
	eq(t, ids(got), "m-2", "m-3")
}

// The journal is never touched by any of this.
func TestFilteringNeverTouchesTheJournal(t *testing.T) {
	s, _ := New(t.TempDir(), "t")
	_ = s.Append([]convo.Message{from("m-1", "alice"), from("m-2", "bob")})
	_, _ = s.NextMatching(0, func(m convo.Message) bool { return m.ID == "m-2" })
	all, err := s.Journal()
	if err != nil {
		t.Fatal(err)
	}
	eq(t, ids(all), "m-1", "m-2")
}
