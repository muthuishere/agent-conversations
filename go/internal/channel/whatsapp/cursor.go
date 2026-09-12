package whatsapp

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

const cursorVersion = 1

// cursor is this adapter's answer to a platform that offers no cursor at all.
//
// `teams` could hide a delta token behind convo.Channel's opaque string.
// `wacli` has nothing to hide: `messages list` takes `--after <RFC3339>`,
// `--asc` and `--limit`, and the store underneath is a database, not a stream.
// So the position is manufactured here, out of the only monotonic thing on
// offer — the message timestamp — plus the ids seen at its boundary.
//
//	TS   the newest timestamp already emitted for this conversation
//	Seen the ids at (or just before) that second, so the deliberate one-second
//	     re-ask does not re-emit them
//
// The one-second overlap is the whole design. WhatsApp timestamps are
// second-granular, so two messages routinely share one; asking for `> TS` drops
// the second of them and nothing ever notices. Asking for `>= TS - 1s` and
// deduplicating by id costs one repeated query row and loses nothing.
type cursor struct {
	V    int      `json:"v"`
	TS   string   `json:"ts,omitempty"`
	Seen []string `json:"s,omitempty"`

	seen map[string]bool
}

// parseCursor decodes what we handed out last time. Empty means "from the
// beginning" per the interface contract; UNREADABLE is a loud error, never a
// silent restart — quietly starting over replays an entire chat history into
// the journal and looks exactly like a flood of new messages.
func parseCursor(s string) (cursor, error) {
	c := cursor{V: cursorVersion, seen: map[string]bool{}}
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
	c.seen = make(map[string]bool, len(c.Seen))
	for _, id := range c.Seen {
		c.seen[id] = true
	}
	return c, nil
}

// advance returns the cursor to hand back: the new watermark, and the ids that
// have to survive the next overlapping query. Bounded by seenCap so a
// long-lived listener's cursor stays a constant size.
func (c cursor) advance(newest string, emitted []string) cursor {
	next := cursor{V: cursorVersion, TS: newest}
	if newest == "" {
		next.TS = c.TS
	}
	keep := append(append([]string{}, c.Seen...), emitted...)
	if len(keep) > seenCap {
		keep = keep[len(keep)-seenCap:]
	}
	next.Seen = keep
	return next
}

func (c cursor) encode() (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", convo.Wrap(convo.ErrInternal, "encoding cursor: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// overlapFrom is the watermark minus one second — the deliberate re-ask window.
// An unparseable watermark is returned untouched rather than dropped: asking
// from where we were is worse than asking from one second earlier, and both are
// far better than asking from the beginning of time.
func overlapFrom(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Add(-time.Second).UTC().Format(time.RFC3339)
}
