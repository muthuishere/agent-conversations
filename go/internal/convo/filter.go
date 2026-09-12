package convo

import (
	"regexp"
	"strings"
)

// Filter is DELIVERY POLICY: which of the messages in the journal this consumer
// is willing to be handed right now.
//
// It lives here, next to the envelope, because it is the one piece of policy
// every consumer needs and no channel is allowed to have. A channel that
// filters is a channel that filters DIFFERENTLY from the next one, and the
// question "who gets answered" is then answered in as many places as there are
// transports (channel/README.md §7).
//
// Two rules give the whole thing its meaning, and both are properties of where
// a filter is applied rather than of what it matches:
//
//   - A FILTER IS NOT A DELETE. It applies at delivery and never to the
//     journal. Everything the channel ingested is still on disk and still
//     visible through `convo journal --all`, so narrowing who the agent answers
//     is reversible, auditable, and cannot lose traffic.
//   - A FILTERED-OUT MESSAGE IS HELD, NOT DROPPED. It stays deliverable, so
//     widening the filter later hands it over instead of silently having
//     destroyed it. The store owns that half; see store/file.
//
// Composition is fixed and is the same as the Node reference implementation
// (INTERFACES.md §1): repeats of one flag OR together, different flags AND
// together. Anything else and a user cannot predict what a second flag does.
type Filter struct {
	// MentionsMe keeps only messages whose envelope says they address us. The
	// envelope field is set by the channel against Identity().ID — a display
	// name is text anyone may be able to choose, and is never the comparison.
	MentionsMe bool

	// From keeps messages from any of these senders (OR). Each needle matches
	// the author id exactly, or the author name case-insensitively by
	// substring: on a real channel a human knows "alice", not "u-3f9c".
	From []string

	// ExcludeFrom rejects messages from any of these senders, matched the same
	// way. Exclusion wins over inclusion: an explicit "not this one" is a
	// stronger statement than a broad "these ones".
	ExcludeFrom []string

	// Text keeps only messages whose text matches. nil means "no text filter",
	// which is NOT the same as a regexp that matches everything.
	Text *regexp.Regexp

	// In keeps only messages from one conversation, by exact id or by a
	// case-insensitive substring of its name. This is the CONSUMER-side twin of
	// the ingest `--in`: that one decides what is polled and journalled at all,
	// this one decides what is handed over. They are deliberately separate —
	// narrowing ingest loses history, narrowing delivery does not.
	In string

	// Kind keeps only chat, channel or email.
	Kind string
}

// Active reports whether this filter constrains anything. An inactive filter
// allows everything, which is what makes "drop the filter" mean "deliver what
// was held".
func (f Filter) Active() bool {
	return f.MentionsMe || len(f.From) > 0 || len(f.ExcludeFrom) > 0 ||
		f.Text != nil || f.In != "" || f.Kind != ""
}

// Allows reports whether this message may be delivered now.
func (f Filter) Allows(m Message) bool {
	if f.MentionsMe && !m.MentionsMe {
		return false
	}
	if len(f.ExcludeFrom) > 0 && matchesAuthor(f.ExcludeFrom, m.From) {
		return false
	}
	if len(f.From) > 0 && !matchesAuthor(f.From, m.From) {
		return false
	}
	if f.Text != nil && !f.Text.MatchString(m.Text) {
		return false
	}
	if f.Kind != "" && !strings.EqualFold(f.Kind, m.Source.Kind) {
		return false
	}
	if f.In != "" && !matchesConversation(f.In, m.Source) {
		return false
	}
	return true
}

// matchesAuthor is the OR within one repeatable flag.
func matchesAuthor(needles []string, a Author) bool {
	for _, n := range needles {
		if n == "" {
			continue
		}
		if a.ID == n {
			return true
		}
		if a.Name != "" && strings.Contains(strings.ToLower(a.Name), strings.ToLower(n)) {
			return true
		}
		if a.ID != "" && strings.EqualFold(a.ID, n) {
			return true
		}
	}
	return false
}

func matchesConversation(needle string, s Source) bool {
	if s.ConversationID == needle {
		return true
	}
	if s.Name != "" && strings.Contains(strings.ToLower(s.Name), strings.ToLower(needle)) {
		return true
	}
	return strings.Contains(strings.ToLower(s.ConversationID), strings.ToLower(needle))
}
