package exechost

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// The second implementation of convo.Host, and the reason the interface is an
// interface: this one owns the lifecycle, has no backpressure, and CAN capture
// the reply — the opposite of the guest path in every respect that matters.
func TestSpawnDelivers(t *testing.T) {
	h := New("spawn", `read -r line; echo "handled: $line"`)
	d, err := h.Deliver(context.Background(), "spawn", "status?", convo.DeliverOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Delivered || !strings.Contains(d.Output, "handled: status?") {
		t.Fatalf("got %+v", d)
	}
}

func TestSpawnDiscoveryAndAbsentTarget(t *testing.T) {
	h := New("spawn", "cat")
	agents, err := h.Agents(context.Background())
	if err != nil || len(agents) != 1 || agents[0].State != convo.StateIdle {
		t.Fatalf("agents = %+v (%v)", agents, err)
	}
	// Any other name is absent, so a caller falls back exactly as it would
	// against a vanished pane — the two hosts fail the same way on purpose.
	if _, err := h.Get(context.Background(), "responder-a"); !errors.Is(err, convo.ErrTargetAbsent) {
		t.Fatalf("want ErrTargetAbsent, got %v", err)
	}
}

func TestSpawnWithoutCommandIsNotConfigured(t *testing.T) {
	h := New("spawn", "")
	if _, err := h.Agents(context.Background()); !errors.Is(err, convo.ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}

// Bound everything. An agent that never returns is indistinguishable from one
// that is thinking, and a timeout is exit 64, not a failure.
func TestSpawnIsBounded(t *testing.T) {
	h := New("spawn", "sleep 5")
	h.Timeout = 100 * time.Millisecond
	_, err := h.Deliver(context.Background(), "spawn", "hi", convo.DeliverOpts{})
	if !errors.Is(err, convo.ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if convo.ExitCode(err) != convo.ExitTimeout {
		t.Fatalf("want exit 64, got %d", convo.ExitCode(err))
	}
}

func TestSpawnFailureIsHostUnavailable(t *testing.T) {
	h := New("spawn", "exit 3")
	if _, err := h.Deliver(context.Background(), "spawn", "hi", convo.DeliverOpts{}); !errors.Is(err, convo.ErrHostUnavailable) {
		t.Fatalf("want ErrHostUnavailable, got %v", err)
	}
}
