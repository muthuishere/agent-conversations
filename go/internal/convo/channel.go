package convo

import "context"

// Channel is the SEAM FOR TRANSPORT: where messages come from and go back to.
//
// This is the only interface in the system that is allowed to know a channel's
// API, its auth, its pagination, its delta tokens and its rate limits. Teams,
// Slack, Telegram, IMAP and an in-process fake all implement these four
// methods, and nothing above this line changes when you swap one for another.
// That is the test of whether the split is right: porting to a new channel must
// touch exactly one file (ARCHITECTURE.md §5.1, §11).
//
// Two fetch styles live behind the same interface. Poll (a delta token, a
// `since` timestamp, an id watermark) is always available and is what you build
// first; push (websocket, SSE, webhook) is a latency optimisation not every
// tenant grants you.
//
// Implementations must NOT: journal, filter, coalesce, suppress self-echo,
// decide anything, or call a model. Those belong above.
type Channel interface {
	// Conversations lists the rooms, DMs and mailboxes worth polling.
	// Discovery is re-run periodically: rooms appear and disappear.
	Conversations(ctx context.Context) ([]Conversation, error)

	// Fetch returns messages for one conversation since an OPAQUE cursor, plus
	// the cursor to use next time. The cursor's format belongs entirely to the
	// implementation; nothing above interprets it. An empty cursor means
	// "from wherever you consider the beginning".
	Fetch(ctx context.Context, convID string, cursor string) (msgs []Message, next string, err error)

	// Send posts text to a target and returns the new message's id.
	// The target is produced by ReplyTarget, so the caller supplies a message
	// id and never has to choose between "send to chat" and "reply in thread" —
	// that choice is where replies silently vanish.
	Send(ctx context.Context, target Target, text string) (id string, err error)

	// Identity is who we are on this channel. Self-echo suppression compares
	// against this and nothing else: without it the agent's own reply wakes it
	// to reply again, forever, on a real channel, for real money.
	Identity(ctx context.Context) (Identity, error)
}
