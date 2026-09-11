package convo

import "strings"

// State is an agent's lifecycle state as an agent host reports it.
//
// The five values match Herdr 0.8.2 exactly (`idle`, `working`, `blocked`,
// `done`, `unknown`), because a smaller enum would lose the one that matters.
// Blocked is the state hand-rolled injectors never have: it distinguishes
// "finished" from "parked on a permission prompt nobody is watching", and a
// guest that cannot tell those apart will report a message as handled while it
// sits behind a dialog (CROSS-SESSION.md §1, §4).
type State string

const (
	StateIdle    State = "idle"
	StateWorking State = "working"
	StateBlocked State = "blocked"
	StateDone    State = "done"
	StateUnknown State = "unknown"
)

// ParseState normalises a host's spelling. Anything unrecognised — including
// the empty string — becomes StateUnknown, which the backpressure policy treats
// as "gone", so an unparseable state can never be mistaken for idle.
func ParseState(s string) State {
	switch State(strings.ToLower(strings.TrimSpace(s))) {
	case StateIdle:
		return StateIdle
	case StateWorking:
		return StateWorking
	case StateBlocked:
		return StateBlocked
	case StateDone:
		return StateDone
	default:
		return StateUnknown
	}
}

// Deliverable reports whether a message may be handed over right now.
// Only idle qualifies. Everything else needs a decision, not a delivery.
func (s State) Deliverable() bool { return s == StateIdle }

// Gone reports whether the target should be treated as absent: a finished
// session and an unrecognised one are both "fall back, do not resurrect"
// (CROSS-SESSION.md §2.3, §6).
func (s State) Gone() bool { return s == StateDone || s == StateUnknown }

// Backpressure is the decision CROSS-SESSION.md §4 prescribes, as a value so it
// can be tested without a host.
type Backpressure int

const (
	// DeliverNow — the target is idle.
	DeliverNow Backpressure = iota
	// WaitThenDeliver — the target is working; wait, bounded, then retry.
	WaitThenDeliver
	// RefuseBlocked — the target needs a human. Never deliver into blocked.
	RefuseBlocked
	// FallBack — the target is done, unknown or absent.
	FallBack
)

// Policy maps a state to the action a guest is allowed to take.
func Policy(s State) Backpressure {
	switch s {
	case StateIdle:
		return DeliverNow
	case StateWorking:
		return WaitThenDeliver
	case StateBlocked:
		return RefuseBlocked
	default:
		return FallBack
	}
}
