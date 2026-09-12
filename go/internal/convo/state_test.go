package convo

import "testing"

// The five states are Herdr 0.8.2's exactly. Anything else — including an empty
// string or a typo — must land on `unknown`, never on `idle`: an unparseable
// state that read as deliverable would inject into a session nobody checked.
func TestParseState(t *testing.T) {
	cases := []struct {
		in   string
		want State
	}{
		{"idle", StateIdle},
		{"working", StateWorking},
		{"blocked", StateBlocked},
		{"done", StateDone},
		{"unknown", StateUnknown},
		{"IDLE", StateIdle},
		{"  working ", StateWorking},
		{"", StateUnknown},
		{"busy", StateUnknown},
		{"idle ", StateIdle},
	}
	for _, c := range cases {
		if got := ParseState(c.in); got != c.want {
			t.Errorf("ParseState(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The backpressure table of CROSS-SESSION.md §4, asserted as a table.
func TestPolicy(t *testing.T) {
	cases := []struct {
		state State
		want  Backpressure
	}{
		{StateIdle, DeliverNow},
		{StateWorking, WaitThenDeliver},
		{StateBlocked, RefuseBlocked},
		{StateDone, DeliverNow},
		{StateUnknown, FallBack},
	}
	for _, c := range cases {
		if got := Policy(c.state); got != c.want {
			t.Errorf("Policy(%q) = %v, want %v", c.state, got, c.want)
		}
	}
	// idle and done are deliverable; nothing else is. This is the assertion
	// that stops a future "working is probably fine" from shipping. `done`
	// belongs on the allowed side: it means the agent finished its turn and is
	// waiting at a prompt, measured against Herdr 0.8.2 in a live run.
	for _, s := range []State{StateWorking, StateBlocked, StateUnknown} {
		if s.Deliverable() {
			t.Errorf("%q must not be deliverable", s)
		}
	}
	if !StateDone.Deliverable() {
		t.Error("done must be deliverable — it is where an answering pane rests")
	}
	if !StateIdle.Deliverable() {
		t.Error("idle must be deliverable")
	}
}

// Exit codes are an interface. If one of these changes, every shell that
// branches on them breaks silently, so they are pinned here.
func TestExitCodes(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{ErrTimeout, 64},
		{ErrNotConfigured, 65},
		{ErrNotInHost, 65},
		{ErrConflict, 66},
		{ErrTargetAbsent, 69},
		{ErrHostUnavailable, 69},
		{ErrSelfDelivery, 70},
		{ErrAgentBlocked, 75},
		{Wrap(ErrSelfDelivery, "extra detail"), 70},
		{Wrap(ErrAgentBlocked, "extra detail"), 75},
	}
	for _, c := range cases {
		if got := ExitCode(c.err); got != c.want {
			t.Errorf("ExitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
	// A timeout must never share a code with a failure: an agent that cannot
	// tell "nobody messaged me" from "the listener is dead" either restarts a
	// healthy system or reports silence while deaf.
	if ExitCode(ErrTimeout) == ExitCode(ErrHostUnavailable) {
		t.Fatal("timeout and unavailable must not share an exit code")
	}
}

func TestReplyTargetRoutes(t *testing.T) {
	dm := Message{ID: "m-1", Source: Source{Kind: "chat", ConversationID: "c-dm-alice"}}
	if got := ReplyTarget(dm); got.ThreadID != "" || got.ConversationID != "c-dm-alice" {
		t.Errorf("dm reply should go to the conversation, got %+v", got)
	}
	ch := Message{ID: "m-2", Source: Source{Kind: "channel", ConversationID: "c-general"}}
	// A channel reply with no thread starts one on the message itself —
	// posting it to the room instead is how a reply vanishes from the thread.
	if got := ReplyTarget(ch); got.ThreadID != "m-2" {
		t.Errorf("channel reply should thread on the message, got %+v", got)
	}
}
