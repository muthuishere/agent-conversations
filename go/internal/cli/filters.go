package cli

import (
	"regexp"
	"strings"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// stringList is a repeatable flag. `--from alice --from bob` collects both, and
// the values OR together — that is the composition rule of INTERFACES.md §1 and
// it is the only one a user can predict.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	if v = strings.TrimSpace(v); v != "" {
		*l = append(*l, v)
	}
	return nil
}

// filterKinds is the closed set of source kinds. A typo here is the worst kind
// of filter bug: `--kind chanel` would match nothing, deliver nothing, and look
// exactly like a quiet channel. So an unknown kind is exit 65, not silence.
var filterKinds = map[string]bool{"chat": true, "channel": true, "email": true}

// filter builds the delivery filter from the parsed flags.
//
// A bad regexp fails LOUDLY here rather than being treated as a literal string:
// silently matching nothing is indistinguishable from nobody having written,
// which is the silent deafness this repo exists to prevent.
func (o options) filter() (convo.Filter, error) {
	f := convo.Filter{
		MentionsMe:  o.mentionsMe,
		From:        o.from,
		ExcludeFrom: o.excludeFrom,
		In:          o.in,
		Kind:        strings.ToLower(strings.TrimSpace(o.kind)),
	}
	if f.Kind != "" && !filterKinds[f.Kind] {
		return f, convo.Wrap(convo.ErrNotConfigured,
			"unknown --kind %q: expected chat, channel or email", o.kind)
	}
	if o.match != "" {
		re, err := regexp.Compile(o.match)
		if err != nil {
			return f, convo.Wrap(convo.ErrNotConfigured, "bad --match regexp: %v", err)
		}
		f.Text = re
	}
	return f, nil
}
