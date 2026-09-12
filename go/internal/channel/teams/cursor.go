package teams

import (
	"encoding/base64"
	"encoding/json"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// cursor is what this channel hides behind convo.Channel's OPAQUE cursor
// string, and it is the single most interesting thing about this adapter.
//
// Teams does not have one position per conversation. It has three shapes of
// position, because the two kinds of conversation page differently:
//
//   - `…/channels/{id}/messages/delta` + `$deltatoken` — new TOP-LEVEL channel
//     messages. Measured against the Graph-shaped mock: a threaded reply does
//     NOT appear in this stream, and `…/messages/{id}/replies/delta` does not
//     exist (404). So a listener that only follows the delta token is deaf to
//     every reply — including replies to its own posts, which is where its
//     conversations actually happen.
//   - a per-thread watermark — the newest reply timestamp we have already
//     emitted for each root message we are scanning.
//   - a per-chat watermark — because CHATS HAVE NO DELTA AT ALL. Measured on a
//     live tenant, not on the simulator: `…/chats/{id}/messages/delta` answers
//     HTTP 400 "Change tracking is not supported against
//     'microsoft.graph.chatMessage'". The simulator implemented that endpoint,
//     so every chat worked locally and every chat failed in production. A chat
//     is read newest-first from the plain listing and walked back until the
//     watermark is passed (fetchChat).
//
// convo.Channel gives us exactly one string per conversation to carry that in,
// and that turned out to be enough: the cursor is opaque by contract, so a
// composite one is legal and nothing above the seam notices. The interface did
// not have to change. What it costs is stated plainly in fetchChannel and
// fetchChat.
type cursor struct {
	V int `json:"v"`
	// Delta is the `$deltatoken` value for the top-level stream.
	Delta string `json:"d,omitempty"`
	// Threads maps a root message id to the newest reply timestamp already
	// emitted for it. Bounded by the reply-scan window, so it cannot grow
	// without limit.
	Threads map[string]string `json:"t,omitempty"`
	// ChatAt is the newest createdDateTime already emitted for a chat, kept as
	// the exact string Graph sent. ChatIDs are the message ids that share that
	// timestamp, so a second message landing in the same millisecond is
	// neither skipped nor re-emitted forever; the set is as small as one
	// millisecond of a chat. Both are meaningless for a channel.
	ChatAt  string   `json:"ca,omitempty"`
	ChatIDs []string `json:"ci,omitempty"`
}

func newCursor() cursor {
	return cursor{V: cursorVersion, Threads: map[string]string{}}
}

// parseCursor decodes what we handed out last time. An empty cursor means
// "from wherever you consider the beginning", per the interface contract.
//
// An UNREADABLE cursor is a loud error, not a silent restart: quietly treating
// it as empty would replay the entire backlog into the journal and look like a
// flood of new messages.
func parseCursor(s string) (cursor, error) {
	c := newCursor()
	if s == "" {
		return c, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, convo.Wrap(convo.ErrNotConfigured, "unreadable cursor %q: %v", s, err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, convo.Wrap(convo.ErrNotConfigured, "unreadable cursor %q: %v", s, err)
	}
	if c.V != cursorVersion {
		return c, convo.Wrap(convo.ErrNotConfigured,
			"cursor version %d, this build writes %d — refusing to guess", c.V, cursorVersion)
	}
	if c.Threads == nil {
		c.Threads = map[string]string{}
	}
	return c, nil
}

// encode is base64 of compact JSON: one token, no separators to escape, and
// unmistakably opaque so nobody above the seam is tempted to parse it.
func (c cursor) encode() (string, error) {
	// encoding/json sorts map keys, so an unchanged cursor encodes to an
	// unchanged string and a test can compare two of them directly.
	raw, err := json.Marshal(c)
	if err != nil {
		return "", convo.Wrap(convo.ErrInternal, "encoding cursor: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// prune keeps only the threads still inside the scan window, so a long-lived
// listener's cursor stays a constant size instead of accumulating one entry per
// conversation thread forever.
func (c *cursor) prune(keep []string) {
	if len(c.Threads) == 0 {
		return
	}
	set := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		set[k] = struct{}{}
	}
	for id := range c.Threads {
		if _, ok := set[id]; !ok {
			delete(c.Threads, id)
		}
	}
}
