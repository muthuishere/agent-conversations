package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	memchan "github.com/muthuishere/agent-conversations/go/internal/channel/memory"
	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// scenario is a busy room: four senders, exactly one of whom addresses the
// agent. This is the group-usability case the filters exist for — point the
// tool at a channel like this with no filter and every message costs an agent
// turn, including the three that were never meant for it.
func scenario(t *testing.T) (home string, env map[string]string, ch *memchan.Channel) {
	t.Helper()
	home = t.TempDir()
	env = map[string]string{"AGENT_CONVERSATIONS_HOME": home, "CONVO_CHANNEL": "memory"}
	ch = memchan.New()
	ch.Post("c-general", "u-alice", "alice", "standup in five")
	ch.Post("c-general", "u-bob", "bob", "@u-agent is the deploy red?")
	ch.Post("c-general", "u-carol", "carol", "lunch?")
	ch.Post("c-dm-alice", "u-alice", "alice", "ping")

	code, _, errOut := runWith(t, ch, env, "fetch")
	if code != 0 {
		t.Fatalf("fetch exit %d: %s", code, errOut)
	}
	return home, env, ch
}

// The headline case, end to end: --mentions-me delivers ONE message, the other
// three are still in the journal, and they become deliverable the moment the
// filter is dropped. All three halves matter; the first alone is a filter that
// eats messages.
func TestMentionsMeDeliversOnlyTheMentionAndHoldsTheRest(t *testing.T) {
	home, env, ch := scenario(t)

	code, out, errOut := runWith(t, ch, env, "next", "--mentions-me")
	if code != 0 {
		t.Fatalf("next --mentions-me exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "is the deploy red?") {
		t.Fatalf("the mention was not delivered: %q", out)
	}
	for _, unwanted := range []string{"standup in five", "lunch?", "ping"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("a message that does not mention us was delivered: %q", out)
		}
	}

	// THE JOURNAL IS UNTOUCHED. A filter is a delivery decision; it may never
	// be a delete. --all says so explicitly by ignoring every filter.
	code, out, _ = runWith(t, ch, env, "journal", "--all", "--mentions-me")
	if code != 0 {
		t.Fatalf("journal --all exit %d", code)
	}
	for _, want := range []string{"standup in five", "is the deploy red?", "lunch?", "ping"} {
		if !strings.Contains(out, want) {
			t.Fatalf("journal --all lost %q: %s", want, out)
		}
	}
	if n := len(journalLines(t, home, "default")); n != 4 {
		t.Fatalf("the journal file holds %d messages, want 4 — the filter reached the log", n)
	}

	// AND THEY ARE STILL DELIVERABLE. Drop the filter and the three held
	// messages arrive, in order, ahead of anything newer. This is the whole
	// reason `next` may not simply advance the cursor past a rejected message.
	code, out, errOut = runWith(t, ch, env, "next")
	if code != 0 {
		t.Fatalf("unfiltered next exit %d: %s", code, errOut)
	}
	for _, want := range []string{"standup in five", "lunch?", "ping"} {
		if !strings.Contains(out, want) {
			t.Fatalf("a filtered-out message was destroyed, not held: %q", out)
		}
	}
	if strings.Contains(out, "is the deploy red?") {
		t.Fatal("the already-delivered mention was redelivered")
	}

	// Nothing left over, and a quiet queue is exit 64, not an error.
	if code, _, _ = runWith(t, ch, env, "next"); code != convo.ExitTimeout {
		t.Fatalf("a drained queue exited %d, want %d", code, convo.ExitTimeout)
	}
}

// The cursor rule, stated as an assertion: after a filtered `next`, a LATER
// filtered `next` still sees new traffic, and the held messages do not come
// back until the filter lets them. A filter must not wedge the cursor either.
func TestFilteredNextStillSeesNewTrafficWithoutRedeliveringHeldOnes(t *testing.T) {
	_, env, ch := scenario(t)

	if code, _, _ := runWith(t, ch, env, "next", "--mentions-me"); code != 0 {
		t.Fatal("first filtered next failed")
	}
	if code, _, _ := runWith(t, ch, env, "next", "--mentions-me"); code != convo.ExitTimeout {
		t.Fatal("a held message was redelivered through the same filter")
	}

	ch.Post("c-general", "u-dave", "dave", "@u-agent second question")
	if code, _, _ := runWith(t, ch, env, "fetch"); code != 0 {
		t.Fatal("fetch failed")
	}
	code, out, _ := runWith(t, ch, env, "next", "--mentions-me")
	if code != 0 || !strings.Contains(out, "second question") {
		t.Fatalf("new traffic did not get through a filter holding older messages: %q", out)
	}
	if strings.Contains(out, "standup in five") {
		t.Fatalf("a still-rejected message was delivered: %q", out)
	}
}

// --from ORs within the flag, --exclude-from wins over it, and different flags
// AND together. Those three sentences are the whole composition contract.
func TestFromExcludeFromAndComposition(t *testing.T) {
	_, env, ch := scenario(t)

	code, out, _ := runWith(t, ch, env, "next", "--from", "alice", "--from", "carol")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "standup in five") || !strings.Contains(out, "lunch?") || !strings.Contains(out, "ping") {
		t.Fatalf("--from did not OR its repeats: %q", out)
	}
	if strings.Contains(out, "is the deploy red?") {
		t.Fatalf("--from delivered a message from somebody else: %q", out)
	}

	// bob is all that is left, and excluding him leaves nothing.
	if code, _, _ = runWith(t, ch, env, "next", "--exclude-from", "bob"); code != convo.ExitTimeout {
		t.Fatal("--exclude-from did not reject the only remaining sender")
	}
	// ... and he is still there once the exclusion goes.
	code, out, _ = runWith(t, ch, env, "next", "--from", "bob")
	if code != 0 || !strings.Contains(out, "is the deploy red?") {
		t.Fatalf("the excluded message was not held: %q (exit %d)", out, code)
	}
}

func TestMatchInAndKindFilters(t *testing.T) {
	_, env, ch := scenario(t)

	code, out, _ := runWith(t, ch, env, "next", "--match", "(?i)DEPLOY")
	if code != 0 || !strings.Contains(out, "is the deploy red?") {
		t.Fatalf("--match regexp did not deliver: %q (exit %d)", out, code)
	}
	if strings.Contains(out, "lunch?") {
		t.Fatalf("--match delivered a non-matching message: %q", out)
	}

	code, out, _ = runWith(t, ch, env, "next", "--kind", "chat")
	if code != 0 || !strings.Contains(out, "ping") {
		t.Fatalf("--kind chat did not deliver the DM: %q (exit %d)", out, code)
	}
	if strings.Contains(out, "standup") {
		t.Fatalf("--kind chat delivered a channel message: %q", out)
	}

	code, out, _ = runWith(t, ch, env, "next", "--in", "#general")
	if code != 0 || !strings.Contains(out, "standup in five") || !strings.Contains(out, "lunch?") {
		t.Fatalf("--in did not select the room: %q (exit %d)", out, code)
	}
}

// journal --new is the drain, and a filter narrows what it shows without
// touching what it counts: the messages are still unacked and still there.
func TestJournalFiltersOutputOnlyNeverTheLog(t *testing.T) {
	home, env, ch := scenario(t)

	code, out, _ := runWith(t, ch, env, "journal", "--new", "--mentions-me")
	if code != 0 || !strings.Contains(out, "is the deploy red?") {
		t.Fatalf("journal --new --mentions-me = %q (exit %d)", out, code)
	}
	if strings.Contains(out, "lunch?") {
		t.Fatalf("journal ignored the filter: %q", out)
	}
	// Unfiltered, everything unacked is still listed.
	code, out, _ = runWith(t, ch, env, "journal", "--new")
	if code != 0 || !strings.Contains(out, "lunch?") {
		t.Fatalf("the drain lost a message to a filter used in an earlier call: %q", out)
	}
	if n := len(journalLines(t, home, "default")); n != 4 {
		t.Fatalf("journal file holds %d lines, want 4", n)
	}
}

// A bad filter fails LOUDLY at exit 65. Silently matching nothing is
// indistinguishable from nobody having written — the silent deafness this repo
// exists to prevent.
func TestBadFilterArgumentsFailLoudly(t *testing.T) {
	_, env, ch := scenario(t)

	if code, _, _ := runWith(t, ch, env, "next", "--match", "([unclosed"); code != convo.ExitNotConfigured {
		t.Fatal("an invalid regexp did not fail at exit 65")
	}
	if code, _, _ := runWith(t, ch, env, "next", "--kind", "chanel"); code != convo.ExitNotConfigured {
		t.Fatal("an unknown --kind did not fail at exit 65")
	}
}

// The hold list is durable state on disk beside the read cursor, because half
// a delivery position in memory is no delivery position at all.
func TestHeldMessagesSurviveOnDisk(t *testing.T) {
	home, env, ch := scenario(t)
	if code, _, _ := runWith(t, ch, env, "next", "--mentions-me"); code != 0 {
		t.Fatal("filtered next failed")
	}
	b, err := os.ReadFile(filepath.Join(home, "cursor", "default.held.json"))
	if err != nil {
		t.Fatalf("no hold list on disk: %v", err)
	}
	if !strings.Contains(string(b), "heldIds") {
		t.Fatalf("hold list = %s", b)
	}
}
