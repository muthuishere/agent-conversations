package herdr

import (
	"context"
	"os"
	"testing"
)

// A live test against a REAL host, skipped by default.
//
// It is gated on an env var rather than on "is a socket present", because a
// test that silently starts talking to whatever workspace the developer happens
// to have open is a test that injects text into someone's work. Opt in
// deliberately:
//
//	CONVO_LIVE_HERDR=1 go test ./internal/host/herdr/ -run Live -v
//
// It is READ-ONLY on purpose. Discovery and self-identification prove the
// envelope parsing against the real binary; delivering into a live pane belongs
// in a session you started yourself, never in `go test`.
func TestLiveHostReadOnly(t *testing.T) {
	if os.Getenv("CONVO_LIVE_HERDR") != "1" {
		t.Skip("set CONVO_LIVE_HERDR=1 to run against a real host")
	}
	env := Detect(os.Getenv)
	if !env.InHost {
		t.Skip("not inside a host pane")
	}
	h := New(env)
	agents, err := h.Agents(context.Background())
	if err != nil {
		t.Fatalf("live list failed: %v", err)
	}
	t.Logf("live host reports %d agents", len(agents))
	self, found, err := h.Self(context.Background())
	if err != nil {
		t.Fatalf("live self failed: %v", err)
	}
	if !found {
		t.Logf("this pane is not listed as an agent (pane %s)", env.PaneID)
		return
	}
	if self.PaneID != env.PaneID {
		t.Fatalf("self resolved to pane %s but we are in %s", self.PaneID, env.PaneID)
	}
	t.Logf("we are %q (%s) in pane %s", self.Name, self.State, self.PaneID)
}
