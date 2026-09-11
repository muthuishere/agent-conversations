package herdr

import (
	"errors"
	"testing"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// The fixtures below are the REAL response shapes measured on Herdr 0.8.2
// (CROSS-SESSION.md §10), not shapes inferred from a --help. Four of the
// obvious guesses are wrong, and each wrong guess has a case here:
//
//	· the payload is enveloped under "result", not a bare array
//	· the state field is "agent_status", not "status"
//	· an agent has NO id — only name and pane_id
//	· errors arrive as JSON on STDOUT, alongside a non-zero exit
const (
	listJSON = `{"id":"cli:agent:list","result":{"agents":[` +
		`{"name":"responder-a","agent_status":"idle","pane_id":"w1:p1"},` +
		`{"name":"responder-b","agent_status":"working","pane_id":"w1:p2"},` +
		`{"name":"responder-c","agent_status":"blocked","pane_id":"w2:p1"}]}}`
	listEmptyJSON  = `{"id":"cli:agent:list","result":{"agents":[]}}`
	getJSON        = `{"id":"cli:agent:get","result":{"agent":{"name":"responder-a","agent_status":"idle","pane_id":"w1:p1"}}}`
	getBlockedJSON = `{"id":"cli:agent:get","result":{"agent":{"name":"responder-c","agent_status":"blocked","pane_id":"w2:p1"}}}`
	blockedErrJSON = `{"id":"cli:agent:prompt","error":{"code":"agent_blocked","message":"agent is blocked"}}`
	missingErrJSON = `{"id":"cli:agent:get","error":{"code":"agent_not_found","message":"no such agent"}}`
	timeoutErrJSON = `{"id":"cli:agent:wait","error":{"code":"timeout","message":"deadline exceeded"}}`
	waitJSON       = `{"id":"cli:agent:wait","result":{"agent_status":"idle"}}`
	promptJSON     = `{"id":"cli:agent:prompt","result":{"agent_status":"idle"}}`
)

func TestParseList(t *testing.T) {
	agents, err := parseList([]byte(listJSON))
	if err != nil {
		t.Fatalf("parseList: %v", err)
	}
	if len(agents) != 3 {
		t.Fatalf("got %d agents, want 3", len(agents))
	}
	want := []convo.Agent{
		{Name: "responder-a", PaneID: "w1:p1", State: convo.StateIdle},
		{Name: "responder-b", PaneID: "w1:p2", State: convo.StateWorking},
		{Name: "responder-c", PaneID: "w2:p1", State: convo.StateBlocked},
	}
	for i, w := range want {
		if agents[i] != w {
			t.Errorf("agent %d = %+v, want %+v", i, agents[i], w)
		}
	}
}

// An empty list exits 0 and is a valid answer. Turning it into an error would
// make "nobody is listening" indistinguishable from "the host is broken".
func TestParseListEmptyIsNotAnError(t *testing.T) {
	agents, err := parseList([]byte(listEmptyJSON))
	if err != nil {
		t.Fatalf("empty list must not error: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("want 0 agents, got %d", len(agents))
	}
}

func TestParseGetAndErrors(t *testing.T) {
	ag, err := parseGet([]byte(getJSON), "responder-a")
	if err != nil {
		t.Fatalf("parseGet: %v", err)
	}
	if ag.State != convo.StateIdle || ag.PaneID != "w1:p1" {
		t.Fatalf("got %+v", ag)
	}

	cases := []struct {
		name string
		json string
		want error
	}{
		{"blocked maps to a typed error", blockedErrJSON, convo.ErrAgentBlocked},
		{"missing target is absent, not a crash", missingErrJSON, convo.ErrTargetAbsent},
		{"timeout is a timeout, never a failure", timeoutErrJSON, convo.ErrTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseGet([]byte(c.json), "responder-x")
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

// `herdr agent read` returns RAW TERMINAL TEXT. Anything non-JSON reaching the
// parser is a host problem, and it must say so rather than silently decoding to
// a zero value that reads as a perfectly healthy absent agent.
func TestParseRejectsRawTerminalText(t *testing.T) {
	_, err := parseList([]byte("$ ls\nREADME.md\n"))
	if !errors.Is(err, convo.ErrHostUnavailable) {
		t.Fatalf("raw terminal text must be a host error, got %v", err)
	}
}

func TestParseStateFromWaitAndPrompt(t *testing.T) {
	for _, j := range []string{waitJSON, promptJSON} {
		st, err := parseState([]byte(j), "responder-a")
		if err != nil {
			t.Fatalf("parseState: %v", err)
		}
		if st != convo.StateIdle {
			t.Fatalf("got %q, want idle", st)
		}
	}
	// A response with no state at all is `unknown`, which the policy treats as
	// gone — the safe direction.
	st, err := parseState([]byte(`{"id":"x","result":{}}`), "a")
	if err != nil || st != convo.StateUnknown {
		t.Fatalf("got %q, %v", st, err)
	}
}

func TestDetectEnv(t *testing.T) {
	cases := []struct {
		name   string
		env    map[string]string
		inHost bool
		pane   string
		bin    string
	}{
		{
			name: "a real pane",
			env: map[string]string{
				"HERDR_ENV": "1", "HERDR_SESSION": "demo",
				"HERDR_SOCKET_PATH": "/tmp/s/demo/herdr.sock",
				"HERDR_PANE_ID":     "w1:p1", "HERDR_TAB_ID": "w1:t1",
				"HERDR_WORKSPACE_ID": "w1", "HERDR_BIN_PATH": "/opt/bin/herdr",
			},
			inHost: true, pane: "w1:p1", bin: "/opt/bin/herdr",
		},
		{
			name:   "socket plus pane, marker missing",
			env:    map[string]string{"HERDR_SOCKET_PATH": "/tmp/x.sock", "HERDR_PANE_ID": "w1:p3"},
			inHost: true, pane: "w1:p3", bin: "herdr",
		},
		{
			name:   "nothing at all",
			env:    map[string]string{},
			inHost: false, bin: "herdr",
		},
		{
			name:   "a stale marker with no socket still reports in-host, but with no pane to speak of",
			env:    map[string]string{"HERDR_ENV": "1"},
			inHost: true, bin: "herdr",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := Detect(func(k string) string { return c.env[k] })
			if e.InHost != c.inHost {
				t.Errorf("InHost = %v, want %v", e.InHost, c.inHost)
			}
			if e.PaneID != c.pane {
				t.Errorf("PaneID = %q, want %q", e.PaneID, c.pane)
			}
			if e.Bin() != c.bin {
				t.Errorf("Bin() = %q, want %q", e.Bin(), c.bin)
			}
		})
	}
}
