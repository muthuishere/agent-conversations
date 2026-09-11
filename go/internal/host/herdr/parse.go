package herdr

import (
	"encoding/json"
	"fmt"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// envelope is Herdr's uniform response shape. Both a result and an error come
// back on stdout; the exit code alone never tells you the reason.
type envelope struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *hostError      `json:"error"`
}

type hostError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// rawAgent is an agent as the host spells it. Note `agent_status`, not
// `status`, and note the absence of any id.
type rawAgent struct {
	Name        string `json:"name"`
	AgentStatus string `json:"agent_status"`
	PaneID      string `json:"pane_id"`
}

func (r rawAgent) toAgent() convo.Agent {
	return convo.Agent{Name: r.Name, PaneID: r.PaneID, State: convo.ParseState(r.AgentStatus)}
}

// decode splits a response into its result payload or a typed error.
// Anything that is not JSON at all is a host problem, not a target problem:
// `agent read` returns raw terminal text and must never be fed through here.
func decode(out []byte) (json.RawMessage, *hostError, error) {
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, nil, convo.Wrap(convo.ErrHostUnavailable,
			"unparseable response from the agent host (not JSON): %s", snippet(out))
	}
	if env.Error != nil {
		return nil, env.Error, nil
	}
	return env.Result, nil, nil
}

// parseList reads {"result":{"agents":[…]}}. An empty list is a valid answer,
// not an error — the host exits 0 for it.
func parseList(out []byte) ([]convo.Agent, error) {
	res, herr, err := decode(out)
	if err != nil {
		return nil, err
	}
	if herr != nil {
		return nil, mapError(herr, "")
	}
	var body struct {
		Agents []rawAgent `json:"agents"`
	}
	if err := json.Unmarshal(res, &body); err != nil {
		return nil, convo.Wrap(convo.ErrHostUnavailable, "malformed agent list: %v", err)
	}
	agents := make([]convo.Agent, 0, len(body.Agents))
	for _, a := range body.Agents {
		agents = append(agents, a.toAgent())
	}
	return agents, nil
}

// parseGet reads {"result":{"agent":{…}}}.
func parseGet(out []byte, name string) (convo.Agent, error) {
	res, herr, err := decode(out)
	if err != nil {
		return convo.Agent{}, err
	}
	if herr != nil {
		return convo.Agent{}, mapError(herr, name)
	}
	var body struct {
		Agent *rawAgent `json:"agent"`
	}
	if err := json.Unmarshal(res, &body); err != nil || body.Agent == nil {
		return convo.Agent{}, convo.Wrap(convo.ErrTargetAbsent,
			"no agent %q in the response", name)
	}
	return body.Agent.toAgent(), nil
}

// parseState pulls a settled state out of a wait/prompt response. The host
// spells it in a couple of places depending on the verb, so look in both rather
// than guessing; an absent state is `unknown`, which the policy treats as gone.
func parseState(out []byte, name string) (convo.State, error) {
	res, herr, err := decode(out)
	if err != nil {
		return convo.StateUnknown, err
	}
	if herr != nil {
		return convo.StateUnknown, mapError(herr, name)
	}
	var body struct {
		AgentStatus string    `json:"agent_status"`
		Status      string    `json:"status"`
		Agent       *rawAgent `json:"agent"`
	}
	_ = json.Unmarshal(res, &body)
	switch {
	case body.AgentStatus != "":
		return convo.ParseState(body.AgentStatus), nil
	case body.Agent != nil && body.Agent.AgentStatus != "":
		return convo.ParseState(body.Agent.AgentStatus), nil
	case body.Status != "":
		return convo.ParseState(body.Status), nil
	}
	return convo.StateUnknown, nil
}

// mapError turns the host's error code into one of our typed errors, so a
// caller branches on a sentinel instead of matching an English message.
//
// The codes that must not be collapsed into a generic failure:
//   - agent_blocked      — a human is needed; the message was NOT sent
//   - timeout / stalled  — aged out; not a failure, fall back
//
// Everything else against a named target means "that target is not usable",
// which for a guest is the same instruction: fall back, do not resurrect.
func mapError(h *hostError, name string) error {
	if h == nil {
		return nil
	}
	msg := h.Message
	if msg == "" {
		msg = h.Code
	}
	switch h.Code {
	case "agent_blocked":
		return convo.Wrap(convo.ErrAgentBlocked,
			"agent %s is blocked and needs a human; nothing was sent (%s)", display(name), msg)
	case "timeout", "agent_prompt_stalled":
		return convo.Wrap(convo.ErrTimeout, "agent %s: %s (%s)", display(name), msg, h.Code)
	case "":
		return convo.Wrap(convo.ErrHostUnavailable, "agent host error: %s", msg)
	default:
		if name == "" {
			return convo.Wrap(convo.ErrHostUnavailable, "agent host error %s: %s", h.Code, msg)
		}
		return convo.Wrap(convo.ErrTargetAbsent,
			"agent %q unusable (%s: %s) — fall back, do not resurrect", name, h.Code, msg)
	}
}

func display(name string) string {
	if name == "" {
		return "(unnamed)"
	}
	return fmt.Sprintf("%q", name)
}

func snippet(b []byte) string {
	const max = 120
	s := string(b)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
