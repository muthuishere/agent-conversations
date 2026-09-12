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
//
// idle and done both qualify. `done` was modelled as "gone" until a live run
// proved otherwise: in Herdr, `done` means the agent FINISHED ITS LAST TURN and
// is sitting at a ready prompt — it is not a corpse, it is the state every
// answering pane settles into after it answers. Treating it as absent made the
// second message of every conversation undeliverable, which is to say it broke
// multi-turn entirely. Herdr agrees: `agent prompt --wait` lists idle, done and
// blocked as settled states, and prompting a done agent is accepted.
func (s State) Deliverable() bool { return s == StateIdle || s == StateDone }

// Gone reports whether the target should be treated as absent. Only an
// unrecognised state qualifies: there is no evidence anyone is there, so the
// rule is "fall back, do not resurrect" (CROSS-SESSION.md §2.3, §6).
func (s State) Gone() bool { return s == StateUnknown }

// Backpressure is the decision CROSS-SESSION.md §4 prescribes, as a value so it
// can be tested without a host.
type Backpressure int

const (
	// DeliverNow — the target is idle, or done with its last turn.
	DeliverNow Backpressure = iota
	// WaitThenDeliver — the target is working; wait, bounded, then retry.
	WaitThenDeliver
	// RefuseBlocked — the target needs a human. Never deliver into blocked.
	RefuseBlocked
	// FallBack — the target is unknown or absent.
	FallBack
)

// Policy maps a state to the action a guest is allowed to take.
func Policy(s State) Backpressure {
	switch s {
	case StateIdle, StateDone:
		return DeliverNow
	case StateWorking:
		return WaitThenDeliver
	case StateBlocked:
		return RefuseBlocked
	default:
		return FallBack
	}
}
