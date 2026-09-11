// Package exechost implements convo.Host by spawning a FRESH agent process per
// message. It is the portable floor (ARCHITECTURE.md §4, wake mechanism W2):
// it needs no workspace manager, no live session and no terminal, it survives
// the terminal closing, and it exists everywhere.
//
// It is in this repo for a second reason. A single interface with a single
// implementation is not an interface, it is indirection. This host and
// host/herdr are about as different as two things can be and still be "hand a
// message to an agent":
//
//	                 herdr                        exechost
//	lifecycle        someone else's               ours
//	backpressure     real — idle/working/blocked  none; we start it
//	context          accumulated, contaminable    clean, and empty
//	reply            the agent posts it itself    captured from stdout
//	cold start       none                         one per message
//
// If convo.Host fits both, it will fit tmux, SSH, a container or an HTTP agent
// service. That is the proof the seam is in the right place.
package exechost

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// Host spawns Command (via `sh -c`) with the message on stdin and takes the
// reply from stdout — the same shape as the handler contract in
// INTERFACES.md §4, so a handler script written for the daemon works unchanged.
type Host struct {
	// Name is the single synthetic target this host offers.
	Name string
	// Command is run through `sh -c`. The message arrives on stdin.
	Command string
	// Timeout bounds one spawn. Bound everything: an agent that never returns
	// is indistinguishable from one that is thinking.
	Timeout time.Duration
	// Env is added to the child's environment.
	Env []string
}

// New builds a spawning host.
func New(name, command string) *Host {
	if name == "" {
		name = "spawn"
	}
	return &Host{Name: name, Command: command, Timeout: 120 * time.Second}
}

// Agents offers exactly one target, always idle. A spawner has nothing to
// discover: a fresh process is available whenever you ask for one, which is
// precisely why this host has no backpressure to respect.
func (h *Host) Agents(ctx context.Context) ([]convo.Agent, error) {
	if h.Command == "" {
		return nil, convo.Wrap(convo.ErrNotConfigured,
			"exec host has no command configured (set --exec-cmd or $CONVO_EXEC_CMD)")
	}
	return []convo.Agent{{Name: h.Name, State: convo.StateIdle}}, nil
}

// Get resolves the one name this host answers to. Any other name is absent, so
// a caller falls back exactly as it would against a vanished pane.
func (h *Host) Get(ctx context.Context, name string) (convo.Agent, error) {
	agents, err := h.Agents(ctx)
	if err != nil {
		return convo.Agent{}, err
	}
	if name != h.Name {
		return convo.Agent{}, convo.Wrap(convo.ErrTargetAbsent,
			"exec host only serves %q, not %q", h.Name, name)
	}
	return agents[0], nil
}

// Wait returns immediately: a process we are about to start is already idle.
// The method exists so the interface holds, not because there is anything to
// wait for — and returning idle unconditionally is honest here, unlike a host
// that guesses.
func (h *Host) Wait(ctx context.Context, name string, until []convo.State, timeout time.Duration) (convo.State, error) {
	if _, err := h.Get(ctx, name); err != nil {
		return convo.StateUnknown, err
	}
	for _, s := range until {
		if s == convo.StateIdle {
			return convo.StateIdle, nil
		}
	}
	if len(until) == 0 {
		return convo.StateIdle, nil
	}
	// Asked to wait for something a fresh spawn will never be.
	return convo.StateIdle, convo.Wrap(convo.ErrTimeout,
		"a spawning host is only ever idle; cannot wait for %v", until)
}

// Deliver spawns one agent, feeds it the message, and returns its stdout as the
// reply. Unlike the guest path there is no self-delivery hazard — we are not
// prompting a pane, we are starting a process — but the spawn is still bounded.
func (h *Host) Deliver(ctx context.Context, name, text string, opts convo.DeliverOpts) (convo.Delivery, error) {
	if _, err := h.Get(ctx, name); err != nil {
		return convo.Delivery{}, err
	}
	if text == "" {
		return convo.Delivery{}, convo.Wrap(convo.ErrNotConfigured, "refusing to deliver empty text")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = h.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", h.Command)
	cmd.Env = append(os.Environ(), h.Env...)
	cmd.Stdin = strings.NewReader(text)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return convo.Delivery{Agent: h.Name, FinalState: convo.StateUnknown},
				convo.Wrap(convo.ErrTimeout, "agent spawn exceeded %s", timeout)
		}
		return convo.Delivery{Agent: h.Name, FinalState: convo.StateUnknown},
			convo.Wrap(convo.ErrHostUnavailable, "agent spawn failed: %v: %s",
				err, strings.TrimSpace(errBuf.String()))
	}
	return convo.Delivery{
		Agent:      h.Name,
		Delivered:  true,
		FinalState: convo.StateDone,
		Output:     strings.TrimRight(out.String(), "\n"),
	}, nil
}

var _ convo.Host = (*Host)(nil)
