// Package herdr implements convo.Host against Herdr, a terminal workspace
// manager that exposes running agents as addressable targets.
//
// You are a GUEST here (CROSS-SESSION.md §2): the agents were started by
// someone else, they may be mid-task on something unrelated, and injecting
// into one permanently contaminates its context. Everything in this package is
// shaped by that — discover then deliver, never cache an address, refuse to
// deliver into `blocked`, and refuse absolutely to deliver into our own pane.
//
// Verified against Herdr 0.8.2. The interface facts that matter, because four
// of them contradict what a `--help` suggests (CROSS-SESSION.md §10):
//
//   - `agent list` and `agent get` take NO options and emit JSON unconditionally.
//   - the response is enveloped: {"id":"cli:agent:list","result":{"agents":[…]}}
//   - an agent has NO id field; the address is `name` (preferred) or `pane_id`.
//   - the state field is `agent_status`, at .result.agent.agent_status.
//   - errors are JSON on STDOUT, and get/wait/prompt exit 1 on a missing target
//     while list exits 0 with an empty list — so checking only $? misses the reason.
//   - `agent read` returns RAW TERMINAL TEXT, not JSON. It is not used here.
package herdr

import "os"

// Env is the Herdr context exported into every pane. Detecting it is how a
// process learns it is itself running inside an agent pane — and therefore
// which agent it is, which is what makes the self-delivery guard possible.
type Env struct {
	InHost      bool   `json:"in_host"`
	Session     string `json:"session,omitempty"`
	SocketPath  string `json:"socket_path,omitempty"`
	PaneID      string `json:"pane_id,omitempty"`
	TabID       string `json:"tab_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	BinPath     string `json:"bin_path,omitempty"`
}

// Detect reads the pane environment. getenv is injected so this is testable
// without touching the real process environment.
//
// HERDR_SOCKET_PATH is the field that matters most operationally: `herdr status`
// reports only the DEFAULT socket and will say "not running" while several
// named-session servers are live. A guest that probes with `status` concludes
// no host exists when one does, so always prefer the exported socket path
// (CROSS-SESSION.md §11).
func Detect(getenv func(string) string) Env {
	if getenv == nil {
		getenv = os.Getenv
	}
	e := Env{
		Session:     getenv("HERDR_SESSION"),
		SocketPath:  getenv("HERDR_SOCKET_PATH"),
		PaneID:      getenv("HERDR_PANE_ID"),
		TabID:       getenv("HERDR_TAB_ID"),
		WorkspaceID: getenv("HERDR_WORKSPACE_ID"),
		BinPath:     getenv("HERDR_BIN_PATH"),
	}
	// HERDR_ENV=1 is the marker, but a pane with a socket and a pane id is
	// unambiguously inside one too. Requiring only the marker would misreport a
	// real pane; requiring only the socket would be fooled by a stale export.
	e.InHost = getenv("HERDR_ENV") == "1" || (e.SocketPath != "" && e.PaneID != "")
	return e
}

// Bin is the herdr executable to invoke: the exported absolute path when the
// host gave us one, otherwise whatever is on PATH. A guest is a different
// process with a different PATH, so preferring the exported path is not a
// nicety (CROSS-SESSION.md §10).
func (e Env) Bin() string {
	if e.BinPath != "" {
		return e.BinPath
	}
	return "herdr"
}
