package herdr

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// Runner executes the host CLI. It is an injected function, not a hard-coded
// exec call, for one reason: the entire host can then be tested against
// recorded JSON with no server running — which is the whole point of putting an
// interface here.
type Runner func(ctx context.Context, args []string) (stdout []byte, exitCode int, err error)

// DefaultWait bounds a `working` target. Never wait unbounded: a session whose
// human went to lunch would stall the caller forever, and falling back is the
// normal path.
const DefaultWait = 60 * time.Second

// Host implements convo.Host against Herdr.
type Host struct {
	Env Env
	Run Runner
}

// New builds a Host from the pane environment.
func New(env Env) *Host {
	h := &Host{Env: env}
	h.Run = h.execRunner
	return h
}

// execRunner shells out to the herdr binary.
//
// HERDR_SOCKET_PATH is passed through explicitly. Herdr runs one server per
// named session, and `herdr status` only ever reports the default one — a guest
// that does not carry the socket path addresses the wrong server, or concludes
// there is no server at all.
func (h *Host) execRunner(ctx context.Context, args []string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, h.Env.Bin(), args...)
	cmd.Env = os.Environ()
	if h.Env.SocketPath != "" {
		cmd.Env = append(cmd.Env, "HERDR_SOCKET_PATH="+h.Env.SocketPath)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return out.Bytes(), 0, nil
	case errors.As(err, &exitErr):
		// Non-zero exit is normal here: the reason is JSON on stdout. But a
		// non-zero exit with EMPTY stdout means the host failed before it could
		// answer — no server, a nested-environment refusal, a bad argument —
		// and it explained itself on stderr. Surfacing that is the difference
		// between "unparseable response" and "no herdr server is running".
		if len(bytes.TrimSpace(out.Bytes())) == 0 {
			reason := bytes.TrimSpace(errBuf.Bytes())
			// Measured: a host that fails BEFORE it can serve the request
			// (no server on the socket, a nested-environment refusal) still
			// emits the same JSON envelope — just on stderr. Hand it to the
			// normal parser so `server_not_running` stays a matchable code
			// instead of becoming an English blob inside another message.
			if len(reason) > 0 && reason[0] == '{' {
				return reason, exitErr.ExitCode(), nil
			}
			if len(reason) == 0 {
				reason = []byte("no output")
			}
			return nil, exitErr.ExitCode(), convo.Wrap(convo.ErrHostUnavailable,
				"agent host is not reachable: %s", firstLine(string(reason)))
		}
		return out.Bytes(), exitErr.ExitCode(), nil
	default:
		return nil, -1, convo.Wrap(convo.ErrHostUnavailable,
			"cannot run %q: %v", h.Env.Bin(), err)
	}
}

func (h *Host) run(ctx context.Context, args ...string) ([]byte, error) {
	out, _, err := h.Run(ctx, args)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Agents lists every addressable agent. `agent list` takes no options and emits
// JSON unconditionally; an empty list exits 0 and is a valid answer.
func (h *Host) Agents(ctx context.Context) ([]convo.Agent, error) {
	out, err := h.run(ctx, "agent", "list")
	if err != nil {
		return nil, err
	}
	return parseList(out)
}

// Get resolves one agent BY NAME. Re-resolve on every delivery: a cached
// address that later resolves to a different pane sends your message to a
// stranger, which is the worst outcome available.
func (h *Host) Get(ctx context.Context, name string) (convo.Agent, error) {
	if name == "" {
		return convo.Agent{}, convo.Wrap(convo.ErrNotConfigured, "no agent name given")
	}
	out, err := h.run(ctx, "agent", "get", name)
	if err != nil {
		return convo.Agent{}, err
	}
	return parseGet(out, name)
}

// Self returns the agent occupying OUR pane, if we are inside one. This is the
// answer to "which agent am I", and it is found by matching HERDR_PANE_ID
// against the discovery list — the agent has no id, and its name was chosen by
// whoever started it, so the pane is the only reliable join key.
func (h *Host) Self(ctx context.Context) (convo.Agent, bool, error) {
	if !h.Env.InHost || h.Env.PaneID == "" {
		return convo.Agent{}, false, nil
	}
	agents, err := h.Agents(ctx)
	if err != nil {
		return convo.Agent{}, false, err
	}
	for _, a := range agents {
		if a.PaneID == h.Env.PaneID {
			return a, true, nil
		}
	}
	return convo.Agent{}, false, nil
}

// Wait blocks until the agent settles into one of `until`.
// Timing out returns convo.ErrTimeout — aging out is not a failure.
func (h *Host) Wait(ctx context.Context, name string, until []convo.State, timeout time.Duration) (convo.State, error) {
	args := []string{"agent", "wait", name}
	for _, s := range until {
		args = append(args, "--until", string(s)) // repeated flag, not comma-joined
	}
	if timeout <= 0 {
		timeout = DefaultWait // never unbounded
	}
	args = append(args, "--timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
	out, err := h.run(ctx, args...)
	if err != nil {
		return convo.StateUnknown, err
	}
	return parseState(out, name)
}

// Deliver hands text to an agent, applying the backpressure policy of
// CROSS-SESSION.md §4 and refusing two things outright.
//
// The order of the checks is the contract:
//
//  1. resolve by name — absent means fall back, not resurrect;
//  2. SELF-DELIVERY GUARD — if the resolved pane is our pane, refuse. An agent
//     prompting itself wakes itself, answers itself and wakes itself again;
//     there is no safe version of it, so it is refused by construction rather
//     than left to a caller to remember;
//  3. blocked — refuse, report, hold the message. The host rejects it too, so
//     our job is detect-and-report, but we must not present it as delivered;
//  4. done/unknown — treat as gone;
//  5. working — bounded wait, then re-check; a target that is still not idle
//     is a timeout, and the caller falls back.
func (h *Host) Deliver(ctx context.Context, name, text string, opts convo.DeliverOpts) (convo.Delivery, error) {
	if text == "" {
		return convo.Delivery{}, convo.Wrap(convo.ErrNotConfigured, "refusing to deliver empty text")
	}
	agent, err := h.Get(ctx, name)
	if err != nil {
		return convo.Delivery{}, err
	}

	if err := h.guardSelf(agent); err != nil {
		return convo.Delivery{}, err
	}

	switch convo.Policy(agent.State) {
	case convo.RefuseBlocked:
		return convo.Delivery{Agent: agent.Name, PaneID: agent.PaneID, FinalState: agent.State},
			convo.Wrap(convo.ErrAgentBlocked,
				"agent %q is blocked and needs a human; message held, nothing sent", name)
	case convo.FallBack:
		return convo.Delivery{Agent: agent.Name, PaneID: agent.PaneID, FinalState: agent.State},
			convo.Wrap(convo.ErrTargetAbsent,
				"agent %q is %s — fall back, do not resurrect", name, agent.State)
	case convo.WaitThenDeliver:
		wait := opts.Timeout
		if wait <= 0 {
			wait = DefaultWait
		}
		st, werr := h.Wait(ctx, name, []convo.State{convo.StateIdle}, wait)
		if werr != nil {
			return convo.Delivery{Agent: agent.Name, PaneID: agent.PaneID, FinalState: convo.StateWorking}, werr
		}
		if st != convo.StateIdle {
			return convo.Delivery{Agent: agent.Name, PaneID: agent.PaneID, FinalState: st},
				convo.Wrap(convo.ErrTimeout,
					"agent %q was still %s after the bounded wait — fall back", name, st)
		}
		// Re-resolve: the pane could have been replaced while we waited.
		agent, err = h.Get(ctx, name)
		if err != nil {
			return convo.Delivery{}, err
		}
		if err := h.guardSelf(agent); err != nil {
			return convo.Delivery{}, err
		}
	}

	args := []string{"agent", "prompt", name, text}
	if opts.Wait {
		args = append(args, "--wait")
		for _, s := range opts.Until {
			args = append(args, "--until", string(s))
		}
		t := opts.Timeout
		if t <= 0 {
			t = DefaultWait
		}
		args = append(args, "--timeout", strconv.FormatInt(t.Milliseconds(), 10))
	}
	out, err := h.run(ctx, args...)
	if err != nil {
		return convo.Delivery{}, err
	}
	final, err := parseState(out, name)
	if err != nil {
		// A prompt that comes back agent_blocked was rejected BEFORE any input
		// was sent, so Delivered stays false. Never report it as handled.
		return convo.Delivery{Agent: agent.Name, PaneID: agent.PaneID, FinalState: convo.StateUnknown}, err
	}
	return convo.Delivery{
		Agent:      agent.Name,
		PaneID:     agent.PaneID,
		Delivered:  true,
		FinalState: final,
	}, nil
}

// guardSelf is the self-delivery refusal. It compares pane ids, not names,
// because a name can be changed and reused while a pane id identifies the
// terminal we are literally running in.
func (h *Host) guardSelf(agent convo.Agent) error {
	if h.Env.PaneID == "" || agent.PaneID == "" {
		return nil
	}
	if agent.PaneID != h.Env.PaneID {
		return nil
	}
	return convo.Wrap(convo.ErrSelfDelivery,
		"agent %q is this pane (%s) — refusing to prompt ourselves; that is an instant self-loop",
		agent.Name, agent.PaneID)
}

var _ convo.Host = (*Host)(nil)

// firstLine keeps a host's diagnostic to one line: these messages are printed
// into an agent's context, and a wall of banner text is worse than no message.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 200
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
