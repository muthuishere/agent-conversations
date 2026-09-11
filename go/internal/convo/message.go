// Package convo holds the portable core: the canonical message envelope, the
// agent state enum, the typed errors, and the three interfaces that are the
// whole point of this module — Channel, Host and Store.
//
// Nothing in this package knows what a Teams payload looks like, how Herdr
// spells its JSON, or where a journal lives on disk. That is deliberate: the
// seams are here, the implementations are in sibling packages, and a new
// channel or a new agent host is a new file that implements one interface.
package convo

import "encoding/json"

// Identity is who *we* are on a channel. Self-echo suppression is by identity,
// never by content matching (ARCHITECTURE.md §5.3), so every Channel must be
// able to answer this question.
type Identity struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Conversation is a room, a DM, or a mailbox — whatever unit the channel
// partitions messages by, and the unit a fetch cursor is kept per.
type Conversation struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // chat | channel | email
	Name string `json:"name"`
}

// Author is the sender of a message. Note that a name is display text a channel
// may let anyone choose: identity is not authorization (ARCHITECTURE.md §8).
type Author struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Source is how a reply is routed back. Getting this wrong sends replies into
// the void, silently — which is why Kind travels with every message rather than
// being re-derived at reply time.
type Source struct {
	Kind           string `json:"kind"` // chat | channel | email
	ConversationID string `json:"conversationId"`
	Name           string `json:"name"`
	ThreadID       string `json:"threadId,omitempty"`
}

// Message is the canonical envelope of ARCHITECTURE.md §5.2. Every channel is
// flattened into this shape before anything else in the system sees it; that
// flattening is the only reason the agent-facing half is portable.
//
// Text is always a string, never null. Raw is the untouched channel payload and
// is never discarded — you will need a field you did not anticipate.
type Message struct {
	ID         string          `json:"id"`
	At         string          `json:"at"`
	From       Author          `json:"from"`
	Text       string          `json:"text"`
	HTML       *string         `json:"html"`
	Source     Source          `json:"source"`
	ReplyToID  *string         `json:"replyToId"`
	MentionsMe bool            `json:"mentionsMe"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}

// Target is where an outbound message goes. It is derived from a Message's
// Source by ReplyTarget, so a caller supplies a message id and the routing is
// the tool's problem, not the agent's (ARCHITECTURE.md §6.3).
type Target struct {
	ConversationID string
	ThreadID       string
	Kind           string
}

// ReplyTarget routes a reply from the message being answered: a DM goes back to
// the conversation, a channel post goes back to the thread it started.
func ReplyTarget(m Message) Target {
	t := Target{ConversationID: m.Source.ConversationID, Kind: m.Source.Kind}
	if m.Source.Kind == "channel" {
		t.ThreadID = m.Source.ThreadID
		if t.ThreadID == "" {
			t.ThreadID = m.ID
		}
	}
	return t
}
