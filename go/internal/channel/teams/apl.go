package teams

import (
	"net/http"
	"strings"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
	apltransport "github.com/muthuishere/agent-conversations/go/internal/transport/apl"
)

// Doer is the one thing this adapter needs from a transport.
//
// It was `*http.Client` until there was a second transport, and that is the
// only reason it is an interface now: `*http.Client` satisfies it unchanged, so
// nothing that already worked had to move. The adapter above — paging, delta
// cursors, threading, retry, path encoding — does not know or care whether the
// bytes came from net/http or from a broker subprocess.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// GraphBaseURL is the real thing. When authentication comes from apl there is
// no simulator to point at, so this is the sensible default rather than a
// required flag.
const GraphBaseURL = "https://graph.microsoft.com/v1.0"

// GraphReadScopes are the delegated scopes a read-only Teams listener needs.
//
// They are NOT declared by default, and that is a measured decision rather than
// an oversight. Declaring a scope makes apl pre-flight the call against its own
// record of the stored grant, which is the better error when the grant really
// is short — it names the scope and the exact `apl login` that repairs it,
// locally, before anything leaves the machine.
//
// But apl's record can understate the token. On a live tenant this handle was
// refused for `Chat.Read` while `GET /me/chats` served fourteen chats through
// the same handle with no `--scope` flag: the token could do it, apl's ledger
// did not say so. A transport that refuses a call the backend would have served
// is worse than one that lets the backend answer, so the default is to declare
// nothing and let Graph's own 403 be the authority. Opt in with
// `--teams-apl-scope` when you want the local pre-flight.
var GraphReadScopes = []string{
	"User.Read",
	"Team.ReadBasic.All",
	"Channel.ReadBasic.All",
	"ChannelMessage.Read.All",
	"Chat.Read",
}

// NewViaAPL builds the channel with apl as its transport.
//
// The credential handling here is subtraction, not addition: cfg.Authorization
// is cleared, unconditionally. If a caller has both an apl handle and a bearer
// token configured, honouring the token would mean this process is holding a
// credential it was specifically arranged not to hold — so the handle wins and
// the token is dropped. The CLI says so out loud; this makes it true.
func NewViaAPL(cfg Config, handle string, scopes ...string) (*Channel, error) {
	tr, err := apltransport.New(handle)
	if err != nil {
		return nil, convo.Wrap(convo.ErrNotConfigured, "%v", err)
	}
	if len(scopes) > 0 {
		tr.Scopes = scopes
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = GraphBaseURL
	}
	cfg.Authorization = ""
	// x-user-name is a simulator affordance for skipping OAuth. Going through
	// apl IS the OAuth, so carrying it would be noise at best and a confusing
	// second identity claim at worst.
	cfg.UserName = ""
	cfg.HTTPClient = tr
	return New(cfg)
}
