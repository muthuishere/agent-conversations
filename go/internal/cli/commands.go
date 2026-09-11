package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
	"github.com/muthuishere/agent-conversations/go/internal/host/herdr"
)

// selfReport is what `convo self` answers.
type selfReport struct {
	InHost      bool   `json:"in_host"`
	Host        string `json:"host"`
	Session     string `json:"session,omitempty"`
	SocketPath  string `json:"socket_path,omitempty"`
	PaneID      string `json:"pane_id,omitempty"`
	TabID       string `json:"tab_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	BinPath     string `json:"bin_path,omitempty"`
	Agent       string `json:"agent,omitempty"`
	AgentState  string `json:"agent_state,omitempty"`
	Note        string `json:"note,omitempty"`
}

// cmdSelf answers the question every guest must ask before it does anything:
// am I inside an agent host, and if so WHICH AGENT AM I?
//
// The second half is not curiosity. Knowing our own pane is what makes the
// self-delivery guard possible — an agent that does not know its own address
// cannot refuse to prompt itself, and prompting itself is an instant loop.
//
// Outside a host this is a clean, distinct answer (exit 65), never a crash:
// "I am not in a pane" is a legitimate state a caller must be able to branch on.
func (a *App) cmdSelf(ctx context.Context, o options) error {
	env := herdr.Detect(a.Getenv)
	rep := selfReport{
		InHost:      env.InHost,
		Host:        "herdr",
		Session:     env.Session,
		SocketPath:  env.SocketPath,
		PaneID:      env.PaneID,
		TabID:       env.TabID,
		WorkspaceID: env.WorkspaceID,
		BinPath:     env.BinPath,
	}
	if !env.InHost {
		rep.Note = "not inside an agent host pane; host commands will have no targets"
		if o.asJSON {
			_ = a.print(o, "", rep)
		} else {
			fmt.Fprintln(a.Stdout, "in-host:  no")
			fmt.Fprintln(a.Stdout, "note:     "+rep.Note)
		}
		return convo.Wrap(convo.ErrNotInHost,
			"not inside an agent host pane (no HERDR_ENV / socket + pane id)")
	}

	// Which agent are we? Match our pane id against discovery. A failure to
	// reach the host here is reported, not fatal: the environment facts above
	// are still true and useful.
	h, _, err := a.host(options{hostKind: "herdr"})
	if err == nil {
		if hh, ok := h.(*herdr.Host); ok {
			if agent, found, serr := hh.Self(ctx); serr != nil {
				rep.Note = "host unreachable: " + errMessage(serr)
			} else if found {
				rep.Agent = agent.Name
				rep.AgentState = string(agent.State)
			} else {
				rep.Note = "in a pane the host does not list as an agent"
			}
		}
	}
	if o.asJSON {
		return a.print(o, "", rep)
	}
	fmt.Fprintln(a.Stdout, "in-host:  yes")
	fmt.Fprintln(a.Stdout, "session:  "+env.Session)
	fmt.Fprintln(a.Stdout, "socket:   "+env.SocketPath)
	fmt.Fprintln(a.Stdout, "pane:     "+env.PaneID)
	if rep.Agent != "" {
		fmt.Fprintf(a.Stdout, "agent:    %s (%s)\n", rep.Agent, rep.AgentState)
	}
	if rep.Note != "" {
		fmt.Fprintln(a.Stdout, "note:     "+rep.Note)
	}
	return nil
}

func (a *App) cmdHost(ctx context.Context, o options, args []string) error {
	if len(args) == 0 {
		return convo.Wrap(convo.ErrNotConfigured, "host needs a sub-command: list | state | deliver")
	}
	h, env, err := a.host(o)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		agents, err := h.Agents(ctx)
		if err != nil {
			return err
		}
		if o.asJSON {
			return a.print(o, "", map[string]any{"agents": agents})
		}
		if len(agents) == 0 {
			// An empty list is a valid answer, not an error — say so plainly
			// rather than letting silence be mistaken for a broken host.
			fmt.Fprintln(a.Stdout, "(no agents)")
			return nil
		}
		for _, ag := range agents {
			self := ""
			if ag.PaneID != "" && ag.PaneID == env.PaneID {
				self = "  <- this pane (delivery refused)"
			}
			fmt.Fprintf(a.Stdout, "%-24s %-8s %s%s\n", ag.Name, ag.State, ag.PaneID, self)
		}
		return nil

	case "state":
		if len(args) < 2 {
			return convo.Wrap(convo.ErrNotConfigured, "state needs an agent name")
		}
		ag, err := h.Get(ctx, args[1])
		if err != nil {
			return err
		}
		return a.print(o, string(ag.State), ag)

	case "deliver":
		if len(args) < 3 || strings.TrimSpace(args[2]) == "" {
			return convo.Wrap(convo.ErrNotConfigured, "deliver needs an agent name and text")
		}
		opts := convo.DeliverOpts{Wait: o.wait, Timeout: o.timeout}
		if opts.Wait {
			opts.Until = []convo.State{convo.StateIdle}
		}
		if opts.Timeout == 0 {
			opts.Timeout = 60 * time.Second
		}
		d, err := h.Deliver(ctx, args[1], args[2], opts)
		if err != nil {
			return err
		}
		human := fmt.Sprintf("delivered to %s (%s)", d.Agent, d.FinalState)
		if d.Output != "" {
			human += "\n" + d.Output
		}
		return a.print(o, human, d)

	default:
		return convo.Wrap(convo.ErrNotConfigured, "unknown host sub-command %q", args[0])
	}
}

// cmdJournal looks at the log. READING IS NOT CONSUMING: this never advances
// the read cursor, so an audit can never steal a message from a consumer.
func (a *App) cmdJournal(ctx context.Context, o options) error {
	st, err := a.store(o)
	if err != nil {
		return err
	}
	var msgs []convo.Message
	if o.newOnly {
		// --new is the DRAIN: journalled but never acked. A message a handler
		// declined is never offered again by the wake path, so without this it
		// simply leaks into the journal (CROSS-SESSION.md §11).
		msgs, err = st.Unacked()
	} else {
		msgs, err = st.Journal()
	}
	if err != nil {
		return err
	}
	return a.emitMessages(o, msgs)
}

// cmdNext hands outstanding messages to this consumer and advances the read
// cursor past exactly those. This is the ONLY command that advances it.
func (a *App) cmdNext(ctx context.Context, o options) error {
	st, err := a.store(o)
	if err != nil {
		return err
	}
	msgs, err := st.Next(o.count)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		// Aging out with nothing to deliver is exit 64 and is NOT an error.
		// Collapsing it into a generic failure is how an agent learns to treat
		// a quiet channel as a fault, or a dead one as quiet.
		return convo.Wrap(convo.ErrTimeout, "nothing outstanding")
	}
	if o.ack {
		ids := make([]string, 0, len(msgs))
		for _, m := range msgs {
			ids = append(ids, m.ID)
		}
		if err := st.Ack(ids); err != nil {
			return err
		}
	}
	return a.emitMessages(o, msgs)
}

func (a *App) cmdAck(ctx context.Context, o options, args []string) error {
	st, err := a.store(o)
	if err != nil {
		return err
	}
	ids := args
	if o.all {
		msgs, err := st.Unacked()
		if err != nil {
			return err
		}
		ids = nil
		for _, m := range msgs {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return convo.Wrap(convo.ErrNotConfigured, "ack needs message ids or --all")
	}
	if err := st.Ack(ids); err != nil {
		return err
	}
	return a.print(o, fmt.Sprintf("acked %d", len(ids)), map[string]any{"acked": len(ids)})
}

// emitMessages prints in one of two modes, on purpose.
//
// --json is for programs. The default is the `compact` form of
// INTERFACES.md §3: tab-separated, one line per message, the MESSAGE ID NEVER
// TRUNCATED because it is the join key a reply is addressed with. Only the
// trailing text is capped. A model reads this with its eyes and never has to
// parse JSON, which is where hand-parsing bugs come from.
func (a *App) emitMessages(o options, msgs []convo.Message) error {
	if o.asJSON {
		return a.print(o, "", msgs)
	}
	for _, m := range msgs {
		where := "[dm]"
		if m.Source.Kind == "channel" {
			where = "[" + m.Source.Name + "]"
		}
		text := strings.ReplaceAll(m.Text, "\n", " ")
		if len(text) > 200 {
			text = text[:200]
		}
		fmt.Fprintf(a.Stdout, "%s\t%s\t%s\t%s\n", m.ID, m.From.Name, where, text)
	}
	return nil
}

// cmdRespond replies to a message BY ID, and lets the tool do the routing.
//
// This is ARCHITECTURE.md §6.3 as a command. The caller never chooses between
// "send to the conversation" and "reply in the thread" — it supplies an id and
// the target is derived from that message's own `source.kind`. An agent asked
// to make that choice will get it wrong, the platform will return success, and
// the reply will vanish.
//
// An unknown id must fail LOUDLY (exit 65). A reply addressed to a message
// nobody has heard of is a bug, and silently sending it somewhere plausible is
// worse than not sending it at all.
func (a *App) cmdRespond(ctx context.Context, o options, args []string) error {
	if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
		return convo.Wrap(convo.ErrNotConfigured, "respond needs a message id and text")
	}
	id, text := args[0], args[1]

	st, err := a.store(o)
	if err != nil {
		return err
	}
	all, err := st.Journal()
	if err != nil {
		return err
	}
	var found *convo.Message
	for i := range all {
		if all[i].ID == id {
			found = &all[i]
			break
		}
	}
	if found == nil {
		return convo.Wrap(convo.ErrNotConfigured,
			"unknown message id %q — refusing to guess where this reply belongs", id)
	}

	ch, err := a.channel(o)
	if err != nil {
		return err
	}
	target := convo.ReplyTarget(*found)
	sentID, err := ch.Send(ctx, target, text)
	if err != nil {
		return err
	}
	return a.print(o, "sent "+sentID, map[string]any{
		"id":             sentID,
		"inReplyTo":      id,
		"conversationId": target.ConversationID,
		"threadId":       target.ThreadID,
		"kind":           target.Kind,
	})
}
