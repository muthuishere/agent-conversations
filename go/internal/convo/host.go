package convo

import (
	"context"
	"time"
)

// Agent is one addressable agent on a host.
//
// There is deliberately NO Id field. Herdr does not give agents one, and every
// verb takes the name directly — so resolving to an id is not merely
// unnecessary, it manufactures the stale-id hazard where a cached handle later
// resolves to a DIFFERENT pane and your message reaches a stranger
// (CROSS-SESSION.md §3, §9, §10). Address by Name; PaneID is for the
// self-delivery check and for logging, never for caching.
type Agent struct {
	Name   string `json:"name"`
	PaneID string `json:"pane_id,omitempty"`
	State  State  `json:"state"`
}

// DeliverOpts controls one hand-off.
type DeliverOpts struct {
	// Wait blocks until the agent reaches one of Until after submission.
	Wait bool
	// Until are the acceptable settled states. Empty means the host default.
	Until []State
	// Timeout bounds the wait. ALWAYS set one: an unbounded wait against a
	// session whose human went to lunch is an indefinite stall, and falling
	// back is the normal path, not the error path.
	Timeout time.Duration
}

// Delivery is the receipt for one hand-off.
type Delivery struct {
	Agent      string `json:"agent"`
	PaneID     string `json:"pane_id,omitempty"`
	Delivered  bool   `json:"delivered"`
	FinalState State  `json:"final_state"`
	// Output is the agent's reply when the host can capture one (a spawning
	// host can; a host that injects into someone's live pane cannot, and
	// leaves this empty — see the reply-path rule in CROSS-SESSION.md §5).
	Output string `json:"output,omitempty"`
}

// Host is the SEAM FOR AGENTS: where agents live and how a message is handed
// to one.
//
// Two very different things implement it, on purpose:
//
//   - a workspace manager that owns already-running agents (host/herdr) — you
//     are a GUEST there: the lifecycle is not yours, the target may be busy
//     with someone else's work, and you must respect backpressure;
//   - a plain spawner that starts a fresh agent per message (host/exechost) —
//     you are the OWNER: no backpressure exists because you started it, but
//     you pay a cold start and get no accumulated context.
//
// If both of those fit behind these four methods, so will tmux, an SSH
// runner, a container, or an HTTP agent service. That is the point of the
// interface; it is not decoration.
//
// A Host implementation MUST refuse to deliver into its own pane if it can
// detect that condition (see host/herdr). An agent prompting itself is an
// instant self-loop, and no caller should have to remember to check.
type Host interface {
	// Agents discovers targets. Discovery is cheap and must be re-run before
	// every delivery — a target can vanish between listing and sending, and
	// that is normal traffic, not an exception.
	Agents(ctx context.Context) ([]Agent, error)

	// Get resolves one agent BY NAME and returns its current state.
	// A missing target must return an error matching ErrTargetAbsent, so the
	// caller can fall back rather than guess.
	Get(ctx context.Context, name string) (Agent, error)

	// Deliver hands text to an agent. It must return ErrAgentBlocked rather
	// than sending into a session parked behind a dialog, and ErrSelfDelivery
	// rather than prompting our own pane.
	Deliver(ctx context.Context, name string, text string, opts DeliverOpts) (Delivery, error)

	// Wait blocks until the agent reaches one of `until`, or the timeout
	// expires. Timing out returns ErrTimeout — aging out is not a failure.
	Wait(ctx context.Context, name string, until []State, timeout time.Duration) (State, error)
}
