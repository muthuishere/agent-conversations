// Package memory is an in-process fake channel: no network, no account, no
// token, no credentials.
//
// It exists so the whole system above the transport seam — normalisation,
// self-echo suppression, the journal, cursors, acks and the host hand-off —
// can be exercised offline and in a test. It is also the reference for what a
// MINIMAL correct convo.Channel looks like: four methods, an opaque cursor, and
// an envelope.
//
// It is a TEST DOUBLE. The storage is a slice behind a mutex; do not copy this
// part into anything real.
package memory

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// Channel is the fake. The zero value is not usable; call New.
type Channel struct {
	mu    sync.Mutex
	seq   int
	convs []convo.Conversation
	msgs  []convo.Message
	self  convo.Identity
	now   func() time.Time
}

// New builds a fake channel with two conversations — a public room and a DM —
// because those are the two routing cases that behave differently on every real
// channel, and a fake with only one hides the bug.
func New() *Channel {
	return &Channel{
		convs: []convo.Conversation{
			{ID: "c-general", Kind: "channel", Name: "#general"},
			{ID: "c-dm-alice", Kind: "chat", Name: "dm:alice"},
		},
		self: convo.Identity{ID: "u-agent", Name: "agent"},
		now:  time.Now,
	}
}

// SetNow makes timestamps deterministic in tests.
func (c *Channel) SetNow(f func() time.Time) { c.now = f }

// Post injects a message as if a person had typed it. This is the test's way
// of making traffic happen.
func (c *Channel) Post(convID, fromID, fromName, text string) convo.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.appendLocked(convID, fromID, fromName, text)
}

func (c *Channel) appendLocked(convID, fromID, fromName, text string) convo.Message {
	c.seq++
	id := fmt.Sprintf("m-%d", c.seq)
	kind, name := "chat", convID
	for _, cv := range c.convs {
		if cv.ID == convID {
			kind, name = cv.Kind, cv.Name
		}
	}
	m := convo.Message{
		ID:   id,
		At:   c.now().UTC().Format(time.RFC3339Nano),
		From: convo.Author{ID: fromID, Name: fromName},
		Text: text,
		// A mention is an ID match, never a display-name match: a name is text
		// anyone on a real channel may be able to choose, so "@agent" from a
		// stranger must not make a message look addressed to us. The fake
		// spells a mention "@u-agent" for exactly that reason.
		MentionsMe: strings.Contains(text, "@"+c.self.ID),
		Source: convo.Source{
			Kind: kind, ConversationID: convID, Name: name, ThreadID: id,
		},
	}
	c.msgs = append(c.msgs, m)
	return m
}

// Conversations lists the rooms.
func (c *Channel) Conversations(ctx context.Context) ([]convo.Conversation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]convo.Conversation, len(c.convs))
	copy(out, c.convs)
	return out, nil
}

// Fetch returns messages after an opaque cursor. The cursor here is a decimal
// index; that it is readable is an accident of the fake, and nothing above this
// package may assume anything about its shape.
//
// Self-echo suppression happens HERE, by identity, before a message can reach a
// journal: anything we sent is dropped. Without it the agent's own reply wakes
// it to reply again, forever.
func (c *Channel) Fetch(ctx context.Context, convID, cursor string) ([]convo.Message, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	from := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil {
			return nil, cursor, convo.Wrap(convo.ErrNotConfigured, "bad cursor %q", cursor)
		}
		from = n
	}
	var out []convo.Message
	i := from
	for ; i < len(c.msgs); i++ {
		m := c.msgs[i]
		if m.Source.ConversationID != convID {
			continue
		}
		if m.From.ID == c.self.ID {
			continue // self-echo: dropped before anyone sees it
		}
		out = append(out, m)
	}
	return out, strconv.Itoa(i), nil
}

// Send posts as us and returns the new id. A channel message is threaded under
// the target thread; a DM goes to the conversation. Routing is the channel's
// job, never the agent's.
func (c *Channel) Send(ctx context.Context, target convo.Target, text string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	known := false
	for _, cv := range c.convs {
		if cv.ID == target.ConversationID {
			known = true
		}
	}
	if !known {
		return "", convo.Wrap(convo.ErrNotConfigured,
			"unknown conversation %q — failing loudly rather than sending into the void",
			target.ConversationID)
	}
	m := c.appendLocked(target.ConversationID, c.self.ID, c.self.Name, text)
	if target.ThreadID != "" {
		c.msgs[len(c.msgs)-1].Source.ThreadID = target.ThreadID
		m.Source.ThreadID = target.ThreadID
	}
	return m.ID, nil
}

// Identity is who we are. Self-echo suppression compares against this.
func (c *Channel) Identity(ctx context.Context) (convo.Identity, error) {
	return c.self, nil
}

// All returns every message including our own — the test's way of reading the
// channel back. Note that replies are threaded, so verifying by reading the
// room and not the thread is how a successful run looks like a total failure.
func (c *Channel) All() []convo.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]convo.Message, len(c.msgs))
	copy(out, c.msgs)
	return out
}

var _ convo.Channel = (*Channel)(nil)
