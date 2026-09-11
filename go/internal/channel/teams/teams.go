// Package teams implements convo.Channel against a Microsoft Graph-shaped
// Teams API: standard library only, poll-first, one file per concern.
//
// It is the SECOND real Channel in this repo, and that is its main job. The
// agent host is fixed — Herdr is a required dependency — so the channel is the
// axis the product actually grows along, and an interface with one
// implementation is not an interface. `channel/memory` is an in-process fake;
// this one talks HTTP to a real API surface with real pagination, real delta
// tokens, real threading and real throttling, and it was written WITHOUT
// changing convo.Channel. That is the evidence the seam is in the right place.
//
// It was developed and measured against a Graph-shaped simulator on localhost.
// Porting to a live tenant is a base URL and a bearer token: every path,
// payload and paging rule below is Graph v1.0 as documented, and the two places
// where the simulator is NOT Graph are called out where they occur (the
// `x-user-name` identity header, and `/mock/*`, which this package never uses).
//
// What this package does NOT do, because the layers above already do it
// (ARCHITECTURE.md §7): journal, coalesce, filter, back off between polls,
// decide anything, or call a model. It fetches, normalises, suppresses its own
// echo, and sends.
package teams

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// Config is everything channel-specific. Nothing above the seam sees any of it.
type Config struct {
	// BaseURL is the Graph root, WITHOUT a trailing slash, e.g.
	// "https://graph.microsoft.com/v1.0" or "http://127.0.0.1:4000/v1.0".
	BaseURL string

	// Authorization is the full header value, e.g. "Bearer eyJ0…". A real
	// tenant needs it. Read it from the environment at run time; never bake a
	// token into code or a config file in the repo.
	Authorization string

	// UserName sets the `x-user-name` header. This is a SIMULATOR affordance —
	// the Graph-shaped mock resolves identity from it so tests need no OAuth.
	// A real tenant ignores it. Leave it empty in production.
	UserName string

	// HTTPClient is injectable so a test can point the adapter at an
	// httptest server serving recorded fixtures. Defaults to
	// http.DefaultClient.
	HTTPClient *http.Client

	// ReplyScanDepth is how many of the newest top-level messages per channel
	// are scanned for new threaded replies on each Fetch. Default 10.
	//
	// This is the one real knob, and it is a straight trade: Graph has no delta
	// for replies, so replies cost one request per scanned thread. Too small
	// and a conversation in an older thread goes unheard; too large and every
	// poll is expensive. Chats have no threads and ignore it entirely.
	ReplyScanDepth int

	// Now is injectable for deterministic tests.
	Now func() time.Time
}

// Channel is the adapter. The zero value is not usable; call New.
type Channel struct {
	cfg Config

	mu   sync.Mutex
	self *convo.Identity // cached: identity does not change mid-process
}

// New builds the channel. It performs no I/O — a constructor that reaches the
// network cannot be used in a test, and a channel that cannot be constructed
// offline will not be.
func New(cfg Config) (*Channel, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, convo.Wrap(convo.ErrNotConfigured, "teams channel needs a BaseURL")
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.ReplyScanDepth <= 0 {
		cfg.ReplyScanDepth = defaultScan
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Channel{cfg: cfg}, nil
}

var _ convo.Channel = (*Channel)(nil)

// ---------------------------------------------------------------------------
// Conversation ids
//
// convo.Conversation carries ONE id string, and a Teams channel needs two
// (team + channel). So the id is a composite with an explicit kind prefix:
//
//	channel:<teamId>/<channelId>
//	chat:<chatId>
//
// The prefix is not decoration. A conversation id reaches Send through a
// journalled message written possibly days earlier, and a reply routed to the
// wrong KIND of target returns HTTP 200 and vanishes (ARCHITECTURE.md §6.3).
// Carrying the kind in the id itself means that can be checked, not inferred.
// ---------------------------------------------------------------------------

const (
	kindChannel = "channel"
	kindChat    = "chat"
)

func channelConvID(teamID, channelID string) string {
	return kindChannel + ":" + teamID + "/" + channelID
}

func chatConvID(chatID string) string { return kindChat + ":" + chatID }

type convRef struct {
	kind      string
	teamID    string
	channelID string
	chatID    string
}

func parseConvID(id string) (convRef, error) {
	switch {
	case strings.HasPrefix(id, kindChat+":"):
		rest := strings.TrimPrefix(id, kindChat+":")
		if rest == "" {
			return convRef{}, convo.Wrap(convo.ErrNotConfigured, "empty chat id in %q", id)
		}
		return convRef{kind: kindChat, chatID: rest}, nil
	case strings.HasPrefix(id, kindChannel+":"):
		rest := strings.TrimPrefix(id, kindChannel+":")
		team, ch, ok := strings.Cut(rest, "/")
		if !ok || team == "" || ch == "" {
			return convRef{}, convo.Wrap(convo.ErrNotConfigured,
				"malformed channel id %q — want channel:<teamId>/<channelId>", id)
		}
		return convRef{kind: kindChannel, teamID: team, channelID: ch}, nil
	default:
		return convRef{}, convo.Wrap(convo.ErrNotConfigured,
			"unrecognised conversation id %q — refusing to guess where this belongs", id)
	}
}

// escSeg percent-encodes ONE path segment for Graph.
//
// url.PathEscape is not enough, and this is the single most common way to break
// a Teams integration: a channel id is `19:abc@thread.tacv2`, and `:` and `@`
// are legal in a path segment, so PathEscape leaves them alone. Graph then
// answers 404 for every request and the listener is silently deaf while looking
// perfectly healthy. The offline test asserts the encoding for exactly this
// reason — it caught this bug before the live server did.
func escSeg(s string) string {
	return strings.NewReplacer(":", "%3A", "@", "%40").Replace(url.PathEscape(s))
}

// ---------------------------------------------------------------------------
// Conversations
// ---------------------------------------------------------------------------

// Conversations lists every channel of every team we are a member of, plus
// every chat we are in.
//
// Discovery is re-run periodically on purpose: a team adds a channel, someone
// opens a DM, a bot is removed from a team. A listener that discovers once at
// boot goes quietly deaf to everything created afterwards, and looks perfectly
// healthy doing it.
func (c *Channel) Conversations(ctx context.Context) ([]convo.Conversation, error) {
	var out []convo.Conversation

	teams, _, err := getCollection[graphTeam](ctx, c, c.cfg.BaseURL+"/me/joinedTeams")
	if err != nil {
		return nil, err
	}
	for _, t := range teams {
		chans, _, err := getCollection[graphChannel](ctx, c,
			fmt.Sprintf("%s/teams/%s/channels", c.cfg.BaseURL, escSeg(t.ID)))
		if err != nil {
			return nil, err
		}
		for _, ch := range chans {
			out = append(out, convo.Conversation{
				ID:   channelConvID(t.ID, ch.ID),
				Kind: kindChannel,
				Name: "#" + ch.DisplayName,
			})
		}
	}

	self, err := c.Identity(ctx)
	if err != nil {
		return nil, err
	}
	chats, _, err := getCollection[graphChat](ctx, c, c.cfg.BaseURL+"/me/chats?%24expand=members")
	if err != nil {
		return nil, err
	}
	for _, ch := range chats {
		out = append(out, convo.Conversation{
			ID:   chatConvID(ch.ID),
			Kind: kindChat,
			Name: chatName(ch, self.ID),
		})
	}
	return out, nil
}

// chatName is display text, never an identifier. A 1:1 chat has no topic, so
// the useful name is the other person.
func chatName(ch graphChat, selfID string) string {
	if ch.Topic != nil && *ch.Topic != "" {
		return *ch.Topic
	}
	var others []string
	for _, m := range ch.Members {
		if m.UserID != selfID && m.DisplayName != "" {
			others = append(others, m.DisplayName)
		}
	}
	if len(others) == 0 {
		return "dm"
	}
	sort.Strings(others)
	return "dm:" + strings.Join(others, ",")
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

// Identity is who we are on this channel. Self-echo suppression compares
// against this and nothing else (ARCHITECTURE.md §5.3): without it our own
// reply is fetched back, wakes the agent, and it replies again — on a real
// tenant, in a real room, for real money.
func (c *Channel) Identity(ctx context.Context) (convo.Identity, error) {
	c.mu.Lock()
	if c.self != nil {
		id := *c.self
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	var me graphUser
	if err := c.do(ctx, "GET", c.cfg.BaseURL+"/me", nil, &me); err != nil {
		return convo.Identity{}, err
	}
	if me.ID == "" {
		return convo.Identity{}, convo.Wrap(convo.ErrNotConfigured,
			"/me returned no id — refusing to run without an identity to suppress")
	}
	id := convo.Identity{ID: me.ID, Name: me.DisplayName}

	c.mu.Lock()
	c.self = &id
	c.mu.Unlock()
	return id, nil
}

// ---------------------------------------------------------------------------
// Fetch
// ---------------------------------------------------------------------------

// Fetch returns everything new in one conversation since an opaque cursor.
//
// Messages come back OLDEST FIRST, whatever order the API used — Graph returns
// channel listings newest-first, and a journal appended in that order replays a
// conversation backwards.
func (c *Channel) Fetch(ctx context.Context, convID, cur string) ([]convo.Message, string, error) {
	ref, err := parseConvID(convID)
	if err != nil {
		return nil, cur, err
	}
	cs, err := parseCursor(cur)
	if err != nil {
		return nil, cur, err
	}
	self, err := c.Identity(ctx)
	if err != nil {
		return nil, cur, err
	}

	var msgs []graphMessage
	switch ref.kind {
	case kindChat:
		msgs, cs, err = c.fetchChat(ctx, ref, cs)
	case kindChannel:
		msgs, cs, err = c.fetchChannel(ctx, ref, cs)
	}
	if err != nil {
		return nil, cur, err
	}

	out := make([]convo.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.DeletedDateTime != nil && *m.DeletedDateTime != "" {
			continue
		}
		if m.MessageType != "" && m.MessageType != "message" {
			// systemEventMessage and friends: a join/leave notice is not
			// conversation, and waking an agent for one costs a turn.
			continue
		}
		if m.From == nil || m.From.User == nil {
			// No human author: a bot card, an event. Nothing to reply to.
			continue
		}
		if m.From.User.ID == self.ID {
			// SELF-ECHO, dropped before it can reach a journal.
			continue
		}
		out = append(out, c.toEnvelope(m, ref, convID, self))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })

	next, err := cs.encode()
	if err != nil {
		return nil, cur, err
	}
	return out, next, nil
}

// fetchChat is the simple half: chats are flat, so one delta token is the whole
// position.
func (c *Channel) fetchChat(ctx context.Context, ref convRef, cs cursor) ([]graphMessage, cursor, error) {
	base := fmt.Sprintf("%s/chats/%s/messages/delta", c.cfg.BaseURL, escSeg(ref.chatID))
	msgs, token, err := c.delta(ctx, base, cs.Delta)
	if err != nil {
		return nil, cs, err
	}
	if token != "" {
		cs.Delta = token
	}
	return msgs, cs, nil
}

// fetchChannel is the interesting half, and the finding worth writing down.
//
// `…/channels/{id}/messages/delta` returns only TOP-LEVEL messages. A threaded
// reply never appears in it, and `…/messages/{id}/replies/delta` does not exist
// — measured, not assumed: the simulator answers 404, and Graph v1.0 offers no
// such endpoint either. Since `convo respond` puts our answers in a THREAD, a
// listener following the delta token alone hears the first message of every
// conversation and nothing anyone says back.
//
// So this does two things and folds both positions into the one opaque cursor
// the interface allows:
//
//  1. the delta token, for new top-level messages;
//  2. a bounded scan of the newest ReplyScanDepth threads, each with its own
//     timestamp watermark, for new replies.
//
// The cost is explicit: 2 + ReplyScanDepth requests per channel poll. The limit
// is explicit too — a reply landing on a thread that has fallen out of the scan
// window is NOT seen. Raise the depth, or accept that very old threads go cold.
func (c *Channel) fetchChannel(ctx context.Context, ref convRef, cs cursor) ([]graphMessage, cursor, error) {
	chPath := fmt.Sprintf("%s/teams/%s/channels/%s",
		c.cfg.BaseURL, escSeg(ref.teamID), escSeg(ref.channelID))

	// 1. new top-level messages
	msgs, token, err := c.delta(ctx, chPath+"/messages/delta", cs.Delta)
	if err != nil {
		return nil, cs, err
	}
	if token != "" {
		cs.Delta = token
	}

	// 2. the reply scan. The roots list is newest-first and top-level only.
	var roots collection[graphMessage]
	rootsURL := fmt.Sprintf("%s/messages?%%24top=%d", chPath, c.cfg.ReplyScanDepth)
	if err := c.do(ctx, "GET", rootsURL, nil, &roots); err != nil {
		return nil, cs, err
	}
	scanned := make([]string, 0, len(roots.Value))
	for _, root := range roots.Value {
		if root.ID == "" {
			continue
		}
		scanned = append(scanned, root.ID)
		watermark, tracked := cs.Threads[root.ID]
		replies, _, err := getCollection[graphMessage](ctx, c,
			fmt.Sprintf("%s/messages/%s/replies", chPath, escSeg(root.ID)))
		if err != nil {
			return nil, cs, err
		}
		newest := watermark
		for _, r := range replies {
			if r.CreatedDateTime > newest {
				newest = r.CreatedDateTime
			}
			if tracked && r.CreatedDateTime <= watermark {
				continue
			}
			// An untracked thread emits every reply it has: they are all new
			// to us. A duplicate across a restart is handled where it belongs
			// — dedupe by message id (ARCHITECTURE.md §10).
			msgs = append(msgs, r)
		}
		cs.Threads[root.ID] = newest
	}
	cs.prune(scanned)
	return msgs, cs, nil
}

// delta runs one delta request and returns the messages plus the NEXT token.
//
// An empty token means "from the beginning", and Graph obliges by returning the
// whole backlog. That is faithful and it is also a foot-gun on first attach:
// see PrimeCursor.
func (c *Channel) delta(ctx context.Context, base, token string) ([]graphMessage, string, error) {
	u := base
	if token != "" {
		u = base + "?%24deltatoken=" + url.QueryEscape(token)
	}
	msgs, deltaLink, err := getCollection[graphMessage](ctx, c, u)
	if err != nil {
		return nil, "", err
	}
	return msgs, deltaTokenOf(deltaLink), nil
}

// deltaTokenOf pulls `$deltatoken` out of a deltaLink. Only the token is kept,
// never the whole link: the link is absolute and carries the server's own idea
// of its hostname, which is wrong the moment anything sits in front of it.
func deltaTokenOf(link string) string {
	if link == "" {
		return ""
	}
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return u.Query().Get("$deltatoken")
}

// PrimeCursor returns a cursor positioned at NOW without emitting anything.
//
// It is not part of convo.Channel — it is the answer to a real operational
// problem the interface does not need to know about. A first Fetch with an
// empty cursor replays the entire history of a conversation into the journal,
// which on a busy room looks exactly like a flood of new traffic and wakes an
// agent for every message anyone ever sent. Prime once when attaching a new
// listener, store the cursor, and start from there.
func (c *Channel) PrimeCursor(ctx context.Context, convID string) (string, error) {
	ref, err := parseConvID(convID)
	if err != nil {
		return "", err
	}
	cs := newCursor()
	switch ref.kind {
	case kindChat:
		_, cs, err = c.fetchChat(ctx, ref, cs)
	case kindChannel:
		_, cs, err = c.fetchChannel(ctx, ref, cs)
	}
	if err != nil {
		return "", err
	}
	return cs.encode()
}

// ---------------------------------------------------------------------------
// Send
// ---------------------------------------------------------------------------

// Send posts text and returns the new message's id.
//
// Routing is decided HERE, from the target's kind, exactly as `convo respond`
// derives that target from the message being answered. The agent never picks
// between "post in the room" and "reply in the thread": it gets that wrong, the
// API returns 200, and the reply is invisible to the person who asked.
//
//	chat                       -> POST /chats/{id}/messages
//	channel, with a thread id  -> POST /teams/{t}/channels/{c}/messages/{id}/replies
//	channel, no thread id      -> POST /teams/{t}/channels/{c}/messages   (new thread)
func (c *Channel) Send(ctx context.Context, target convo.Target, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", convo.Wrap(convo.ErrNotConfigured, "refusing to send an empty message")
	}
	ref, err := parseConvID(target.ConversationID)
	if err != nil {
		return "", err
	}
	if target.Kind != "" && target.Kind != ref.kind {
		// The envelope and the id disagree about what this conversation is.
		// That is a routing bug upstream, and sending anyway is how a reply
		// ends up in a stranger's DM.
		return "", convo.Wrap(convo.ErrNotConfigured,
			"target kind %q does not match conversation %q — refusing to route on a guess",
			target.Kind, target.ConversationID)
	}

	body := map[string]any{"body": map[string]string{"contentType": "text", "content": text}}
	var endpoint string
	switch ref.kind {
	case kindChat:
		endpoint = fmt.Sprintf("%s/chats/%s/messages", c.cfg.BaseURL, escSeg(ref.chatID))
	case kindChannel:
		base := fmt.Sprintf("%s/teams/%s/channels/%s/messages",
			c.cfg.BaseURL, escSeg(ref.teamID), escSeg(ref.channelID))
		if target.ThreadID != "" {
			endpoint = base + "/" + escSeg(target.ThreadID) + "/replies"
		} else {
			endpoint = base
		}
	}

	var sent graphMessage
	if err := c.do(ctx, "POST", endpoint, body, &sent); err != nil {
		return "", err
	}
	if sent.ID == "" {
		// A 2xx with no id is the platform saying "fine" while having done
		// nothing useful. Verify the artifact, never the status code.
		return "", convo.Wrap(convo.ErrInternal,
			"send returned no message id — cannot confirm the message exists")
	}
	return sent.ID, nil
}

// ---------------------------------------------------------------------------
// Normalisation
// ---------------------------------------------------------------------------

var tagRE = regexp.MustCompile(`<[^>]*>`)

// toEnvelope flattens a Graph message into the canonical envelope of
// ARCHITECTURE.md §5.2. Everything portable above the seam depends on this
// being the only shape that escapes the package.
func (c *Channel) toEnvelope(m graphMessage, ref convRef, convID string, self convo.Identity) convo.Message {
	text, htmlBody := bodyText(m.Body)

	src := convo.Source{Kind: ref.kind, ConversationID: convID}
	if ref.kind == kindChannel {
		// THREADING. A reply's thread is its root; a root's thread is itself.
		// Carry it on the message rather than re-deriving it at reply time —
		// by then the information is gone and the reply lands in the room,
		// where the person who asked will never look.
		if m.ReplyToID != nil && *m.ReplyToID != "" {
			src.ThreadID = *m.ReplyToID
		} else {
			src.ThreadID = m.ID
		}
	}

	env := convo.Message{
		ID:         m.ID,
		At:         m.CreatedDateTime,
		From:       convo.Author{ID: m.From.User.ID, Name: m.From.User.DisplayName},
		Text:       text,
		HTML:       htmlBody,
		Source:     src,
		ReplyToID:  m.ReplyToID,
		MentionsMe: mentionsUser(m, self.ID),
	}
	// Raw is the untouched payload. Never discard it: the field you did not
	// anticipate is always in there.
	env.Raw = m.raw
	return env
}

// bodyText gives Text (always a string, never null) and the original HTML when
// there was one. The original is never discarded: a stripped body loses links,
// mentions and code blocks, and something downstream will want them.
func bodyText(b graphBody) (string, *string) {
	if strings.EqualFold(b.ContentType, "html") {
		original := b.Content
		plain := tagRE.ReplaceAllString(b.Content, "")
		plain = html.UnescapeString(plain)
		return strings.TrimSpace(plain), &original
	}
	return b.Content, nil
}

func mentionsUser(m graphMessage, selfID string) bool {
	for _, mention := range m.Mentions {
		if mention.Mentioned != nil && mention.Mentioned.User != nil &&
			mention.Mentioned.User.ID == selfID {
			return true
		}
	}
	return false
}
