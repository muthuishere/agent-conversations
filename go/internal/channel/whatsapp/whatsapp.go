// Package whatsapp implements convo.Channel on top of two binaries the user
// already has: `wacli` (a local WhatsApp CLI over a synced SQLite store) and
// `apl` (the identity broker that says WHICH WhatsApp account a command runs
// as).
//
// It is the THIRD Channel in this repo and the first one that is not an HTTP
// API, which is the point of writing it: `teams` proved the seam holds across
// two REST-shaped transports, and this proves it holds across a transport that
// is a subprocess and a local database. convo.Channel did not change.
//
// The split between the two binaries is deliberate and is not symmetric:
//
//	READS  ->  wacli --account <label> …            (local store, cheap, safe)
//	WRITES ->  apl with whatsapp:<label> -- wacli … (the broker owns identity)
//
// A read is a query against a database on this machine. A send leaves the
// machine, under a real person's phone number, into somebody's chat — so it
// goes through the broker that owns that identity rather than through a label
// this package assembled for itself. apl injects `--account <label>` and execs;
// it copies no credential onto the command line (measured with `apl with
// --json --dry-run`).
//
// THE HONEST CAVEAT, and it is not small: `wacli` reads a LOCAL store that a
// separate `wacli sync` fills in, and sync can silently miss the newest
// messages. This adapter never runs sync — syncing is a long-running write and
// belongs to whoever owns the account, not to a fetch — so freshness is
// somebody else's job and a read here is NOT authoritative. A quiet Fetch means
// "the local store has nothing new", which is not the same as "nobody wrote".
// That is the price of a poll-first adapter on a platform whose only real
// inbound path is a webhook (channel/README.md §9), and it is stated here
// rather than discovered later.
package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// JID suffixes. These are the whole of this platform's routing taxonomy, and
// they travel INSIDE the conversation id — so unlike Teams this adapter needs
// no composite id: a JID already says what kind of thing it addresses, which
// means Send can CHECK the routing instead of inferring it.
const (
	suffixGroup = "@g.us"           // a group -> convo kind "channel"
	suffixUser  = "@s.whatsapp.net" // a 1:1 chat -> convo kind "chat"
)

const (
	defaultChatLimit    = 200
	defaultMessageLimit = 50
	seenCap             = 500
)

// Config is everything channel-specific.
type Config struct {
	// Handle is the apl handle for the account, e.g. "whatsapp:personal".
	// A bare label ("personal") is accepted and normalised.
	Handle string

	// WacliPath and AplPath are the binaries. They default to the bare names
	// "wacli" and "apl" so PATH resolution — and apl's own handle-to-account
	// mapping, which keys off the tool it is asked to run — work normally.
	// Tests point them at scripts in a temp dir.
	WacliPath string
	AplPath   string

	// ChatLimit is how many chats one discovery pass lists (default 200).
	ChatLimit int

	// MessageLimit is how many messages one Fetch pulls per conversation
	// (default 50). It is a ceiling per poll, not a window: whatever is left
	// over is picked up by the next poll because the cursor only advances over
	// what was actually returned.
	MessageLimit int

	// Run is the subprocess seam, injectable so tests need no PATH games.
	// It returns stdout; stderr is folded into the error.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)

	// Now is injectable for deterministic tests.
	Now func() time.Time
}

// Channel is the adapter. The zero value is not usable; call New.
type Channel struct {
	cfg     Config
	account string

	mu   sync.Mutex
	self *convo.Identity // cached: identity does not change mid-process
}

// New builds the channel and performs no I/O — a constructor that shells out
// cannot be used in a test, and a channel that cannot be constructed offline
// will not be.
func New(cfg Config) (*Channel, error) {
	h := strings.TrimSpace(cfg.Handle)
	if h == "" {
		return nil, convo.Wrap(convo.ErrNotConfigured,
			"whatsapp channel needs an apl handle, e.g. whatsapp:personal")
	}
	account := h
	if i := strings.IndexByte(h, ':'); i >= 0 {
		account = h[i+1:]
		if !strings.EqualFold(h[:i], "whatsapp") {
			return nil, convo.Wrap(convo.ErrNotConfigured,
				"handle %q is not a whatsapp handle", h)
		}
	} else {
		h = "whatsapp:" + h
	}
	if strings.TrimSpace(account) == "" {
		return nil, convo.Wrap(convo.ErrNotConfigured, "handle %q names no account", cfg.Handle)
	}
	cfg.Handle = h
	if cfg.WacliPath == "" {
		cfg.WacliPath = "wacli"
	}
	if cfg.AplPath == "" {
		cfg.AplPath = "apl"
	}
	if cfg.ChatLimit <= 0 {
		cfg.ChatLimit = defaultChatLimit
	}
	if cfg.MessageLimit <= 0 {
		cfg.MessageLimit = defaultMessageLimit
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Run == nil {
		cfg.Run = execRun
	}
	return &Channel{cfg: cfg, account: account}, nil
}

var _ convo.Channel = (*Channel)(nil)

// execRun is the real subprocess. stderr is captured and folded into the error
// rather than inherited, so a broken invocation reports what went wrong instead
// of scribbling on the CLI's own stderr — which is one JSON object by contract.
func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return out.Bytes(), fmt.Errorf("%s: %v: %s", name, err, msg)
	}
	return out.Bytes(), nil
}

// wacli runs a READ against the local store, as the bound account.
func (c *Channel) wacli(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"--account", c.account}, args...)
	return c.cfg.Run(ctx, c.cfg.WacliPath, full...)
}

// envelope is wacli's uniform JSON wrapper: {"success":…,"data":…,"error":…}.
// The data shape differs per command; only this outer layer is shared.
type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   json.RawMessage `json:"error"`
}

// decode unwraps the envelope into v. A command that reports failure is a hard
// error: a channel that treats "the store is locked" as "no messages" is a
// silently deaf listener, which is the failure mode this whole repo is about.
func decode(raw []byte, v any) error {
	var env envelope
	if err := json.Unmarshal(bytes.TrimSpace(raw), &env); err != nil {
		return convo.Wrap(convo.ErrInternal, "wacli returned unparseable JSON: %v", err)
	}
	if !env.Success {
		detail := strings.TrimSpace(string(env.Error))
		if detail == "" || detail == "null" {
			detail = "no detail"
		}
		return convo.Wrap(convo.ErrTargetAbsent, "wacli reported failure: %s", detail)
	}
	if v == nil || len(env.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		return convo.Wrap(convo.ErrInternal, "unexpected wacli payload shape: %v", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

type authStatus struct {
	Authenticated bool   `json:"authenticated"`
	LinkedJID     string `json:"linked_jid"`
	Phone         string `json:"phone"`
}

// Identity is the account this handle is bound to.
//
// An account that is not authenticated is exit 69 (unavailable), not a warning:
// without an identity there is no self-echo suppression, and an adapter that
// carries on without one journals its own replies and wakes the agent to answer
// itself, forever, in a real person's chat.
func (c *Channel) Identity(ctx context.Context) (convo.Identity, error) {
	c.mu.Lock()
	if c.self != nil {
		id := *c.self
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	raw, err := c.wacli(ctx, "auth", "status", "--json")
	if err != nil {
		return convo.Identity{}, convo.Wrap(convo.ErrTargetAbsent,
			"cannot read whatsapp auth status for account %q: %v", c.account, err)
	}
	var st authStatus
	if err := decode(raw, &st); err != nil {
		return convo.Identity{}, err
	}
	if !st.Authenticated || st.LinkedJID == "" {
		return convo.Identity{}, convo.Wrap(convo.ErrTargetAbsent,
			"whatsapp account %q is not linked — refusing to run with no identity "+
				"to suppress our own echo against", c.account)
	}
	id := convo.Identity{ID: normalizeJID(st.LinkedJID), Name: st.Phone}
	if id.Name == "" {
		id.Name = id.ID
	}
	c.mu.Lock()
	c.self = &id
	c.mu.Unlock()
	return id, nil
}

// normalizeJID strips the device suffix WhatsApp appends to a linked account
// ("<number>:<device>@s.whatsapp.net"). The same human is a different string on
// every device, and comparing the raw value is how self-echo suppression fails
// after a relink.
func normalizeJID(jid string) string {
	at := strings.IndexByte(jid, '@')
	if at < 0 {
		return jid
	}
	user, domain := jid[:at], jid[at:]
	if i := strings.IndexByte(user, ':'); i >= 0 {
		user = user[:i]
	}
	return user + domain
}

// jidUser is the phone-number part of a JID — the token a mention is spelled
// with in WhatsApp message text ("@1234567890").
func jidUser(jid string) string {
	if i := strings.IndexByte(jid, '@'); i >= 0 {
		jid = jid[:i]
	}
	if i := strings.IndexByte(jid, ':'); i >= 0 {
		jid = jid[:i]
	}
	return jid
}

// ---------------------------------------------------------------------------
// Conversations
// ---------------------------------------------------------------------------

type chatRow struct {
	JID           string `json:"jid"`
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	LastMessageTS string `json:"last_message_ts"`
}

// Conversations lists the chats worth polling.
//
// The kind comes from the JID SUFFIX, not from wacli's own `kind` field, and
// that is on purpose: the suffix is WhatsApp's own routing fact and it is the
// same string Send later validates against, so discovery and delivery cannot
// disagree. wacli's "group"/"dm" wording is a second source of truth for the
// same thing, and two sources of truth is one too many.
//
// Anything that is neither a group nor a user JID — status broadcasts,
// newsletters/channels — is skipped. It is not conversation, and every one
// passed through costs an agent turn.
func (c *Channel) Conversations(ctx context.Context) ([]convo.Conversation, error) {
	raw, err := c.wacli(ctx, "chats", "list", "--limit", strconv.Itoa(c.cfg.ChatLimit), "--json")
	if err != nil {
		return nil, convo.Wrap(convo.ErrTargetAbsent, "wacli chats list failed: %v", err)
	}
	var rows []chatRow
	if err := decode(raw, &rows); err != nil {
		return nil, err
	}
	out := make([]convo.Conversation, 0, len(rows))
	for _, r := range rows {
		kind, ok := kindFor(r.JID)
		if !ok {
			continue
		}
		name := r.Name
		if name == "" {
			name = r.JID
		}
		out = append(out, convo.Conversation{ID: r.JID, Kind: kind, Name: name})
	}
	return out, nil
}

// kindFor maps a JID to a convo kind. The second return is false for anything
// this adapter refuses to treat as a conversation.
func kindFor(jid string) (string, bool) {
	switch {
	case strings.HasSuffix(jid, suffixGroup):
		return "channel", true
	case strings.HasSuffix(jid, suffixUser):
		return "chat", true
	default:
		return "", false
	}
}

// ---------------------------------------------------------------------------
// Fetch
// ---------------------------------------------------------------------------

type messageRow struct {
	ChatJID      string `json:"ChatJID"`
	ChatName     string `json:"ChatName"`
	MsgID        string `json:"MsgID"`
	SenderJID    string `json:"SenderJID"`
	SenderName   string `json:"SenderName"`
	Timestamp    string `json:"Timestamp"`
	FromMe       bool   `json:"FromMe"`
	Text         string `json:"Text"`
	DisplayText  string `json:"DisplayText"`
	ReactionToID string `json:"ReactionToID"`
	MediaType    string `json:"MediaType"`
	MediaCaption string `json:"MediaCaption"`
	Revoked      bool   `json:"Revoked"`
	DeletedForMe bool   `json:"DeletedForMe"`
}

type messagePage struct {
	Messages []json.RawMessage `json:"messages"`
}

// Fetch returns everything new in one conversation since an opaque cursor.
//
// THE CURSOR IS INVENTED HERE, because wacli has none. `messages list` offers
// `--after <RFC3339>`, `--asc` and `--limit`, and nothing monotonic: the store
// is a database, not a stream. So the cursor is a TIMESTAMP WATERMARK plus the
// ids already emitted at the boundary, and it costs exactly what you would
// expect:
//
//   - WhatsApp timestamps are second-granular, so two messages can share one.
//     A bare `ts >` watermark drops the second of them silently. The adapter
//     therefore re-asks from one second BEFORE the watermark and deduplicates
//     by message id — overlap is cheap, loss is not.
//   - The id list is bounded (seenCap), so the cursor cannot grow forever. A
//     burst larger than that within one second at the boundary could redeliver;
//     at-least-once is the correct side to fail on.
//   - Freshness is NOT ours. See the package comment: the local store is filled
//     by a separate `wacli sync` that can miss the newest messages, so an empty
//     Fetch means "nothing new locally", never "nobody wrote".
//
// Self-echo suppression happens HERE, before anything is returned, by identity
// — plus wacli's own FromMe flag, because two cheap checks are worth more than
// one when the failure is a reply loop in a real person's chat.
func (c *Channel) Fetch(ctx context.Context, convID, cur string) ([]convo.Message, string, error) {
	kind, ok := kindFor(convID)
	if !ok {
		return nil, cur, convo.Wrap(convo.ErrNotConfigured,
			"%q is not a whatsapp conversation id (expected a JID ending %s or %s)",
			convID, suffixGroup, suffixUser)
	}
	self, err := c.Identity(ctx)
	if err != nil {
		return nil, cur, err
	}
	cr, err := parseCursor(cur)
	if err != nil {
		return nil, cur, err
	}

	args := []string{"messages", "list", "--chat", convID, "--asc",
		"--limit", strconv.Itoa(c.cfg.MessageLimit), "--json"}
	if cr.TS != "" {
		args = append(args, "--after", overlapFrom(cr.TS))
	}
	raw, err := c.wacli(ctx, args...)
	if err != nil {
		return nil, cur, convo.Wrap(convo.ErrTargetAbsent, "wacli messages list failed: %v", err)
	}
	var page messagePage
	if err := decode(raw, &page); err != nil {
		return nil, cur, err
	}

	mentionToken := "@" + jidUser(self.ID)
	var out []convo.Message
	newest := cr.TS
	var boundary []string
	for _, rawRow := range page.Messages {
		var r messageRow
		if err := json.Unmarshal(rawRow, &r); err != nil {
			continue // one malformed row must not wedge the conversation
		}
		if r.MsgID == "" || cr.seen[r.MsgID] {
			continue
		}
		if r.Timestamp > newest {
			newest = r.Timestamp
		}
		boundary = append(boundary, r.MsgID)
		if r.FromMe || normalizeJID(r.SenderJID) == self.ID {
			continue // self-echo, by identity, before anyone sees it
		}
		if r.Revoked || r.DeletedForMe || r.ReactionToID != "" {
			continue // deleted, and reactions are not conversation
		}
		text := messageText(r)
		if text == "" {
			continue
		}
		name := r.SenderName
		if name == "" {
			name = jidUser(r.SenderJID)
		}
		convName := r.ChatName
		if convName == "" {
			convName = convID
		}
		out = append(out, convo.Message{
			ID:   r.MsgID,
			At:   r.Timestamp,
			From: convo.Author{ID: normalizeJID(r.SenderJID), Name: name},
			Text: text,
			Source: convo.Source{
				Kind:           kind,
				ConversationID: convID,
				Name:           convName,
				// WhatsApp is FLAT: there are no threads, so a message's thread
				// is itself. Quoting is a per-message reference, not a
				// container, and it is carried by Send's --reply-to.
				ThreadID: r.MsgID,
			},
			// A mention is the sender typing our phone number, which IS our
			// identity id — not a display name anyone could copy.
			MentionsMe: strings.Contains(text, mentionToken),
			Raw:        json.RawMessage(rawRow),
		})
	}

	next, err := cr.advance(newest, boundary).encode()
	if err != nil {
		return nil, cur, err
	}
	return out, next, nil
}

// messageText picks the best human-readable body. A photo with a caption IS
// conversation and must not be dropped; a message with no body at all is not,
// and passing it through costs an agent turn for nothing.
func messageText(r messageRow) string {
	for _, s := range []string{r.Text, r.MediaCaption, r.DisplayText} {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	if r.MediaType != "" {
		return "[" + r.MediaType + "]"
	}
	return ""
}

// PrimeCursor positions a fresh cursor at NOW without emitting anything, so a
// first attach does not replay a person's entire WhatsApp history into the
// journal and wake an agent once per message anyone ever sent.
//
// It is a package method, NOT part of convo.Channel — same as teams. The
// interface has no vocabulary for "position me at the present", and widening it
// to carry an optional capability is how interfaces rot.
func (c *Channel) PrimeCursor(ctx context.Context, convID string) (string, error) {
	if _, ok := kindFor(convID); !ok {
		return "", convo.Wrap(convo.ErrNotConfigured, "%q is not a whatsapp conversation id", convID)
	}
	return cursor{V: cursorVersion, TS: c.cfg.Now().UTC().Format(time.RFC3339)}.encode()
}

// ---------------------------------------------------------------------------
// Send — the only outward-facing method in this package
// ---------------------------------------------------------------------------

// Send posts text and returns the new message's id.
//
// Two facts, both measured, both of which break a send that ignores them:
//
//   - `--message` is a FLAG, not a positional. `wacli send text --to X "hi"`
//     fails outright.
//   - the recipient is a JID, and the suffix says what kind of thing it is. A
//     Target whose Kind disagrees with its JID is a routing bug upstream, and
//     sending anyway is how a reply lands in a stranger's DM. It is refused.
//
// It goes through `apl with whatsapp:<label> --`, not through a bare wacli
// call, because a send leaves this machine under a real person's number and the
// broker is what owns that identity.
func (c *Channel) Send(ctx context.Context, target convo.Target, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", convo.Wrap(convo.ErrNotConfigured, "refusing to send an empty message")
	}
	jid := strings.TrimSpace(target.ConversationID)
	kind, ok := kindFor(jid)
	if !ok {
		return "", convo.Wrap(convo.ErrNotConfigured,
			"%q is not a whatsapp recipient (expected a JID ending %s or %s)",
			jid, suffixGroup, suffixUser)
	}
	if target.Kind != "" && target.Kind != kind {
		return "", convo.Wrap(convo.ErrNotConfigured,
			"target kind %q disagrees with recipient %s (which is a %s) — refusing to "+
				"send into the wrong conversation", target.Kind, jid, kind)
	}

	args := []string{"with", c.cfg.Handle, "--", c.cfg.WacliPath,
		"send", "text", "--to", jid, "--message", text, "--json"}
	// WhatsApp has no threads, so ReplyTarget's ThreadID means "quote this
	// message". Quoting is what makes an answer legible in a busy group, and
	// the group's own message id is exactly what --reply-to wants.
	//
	// KNOWN GAP: wacli documents --reply-to-sender as required to quote inside
	// a group whose history is not synced locally. convo.Target carries no
	// author, so this adapter cannot supply it. The failure mode is a send that
	// is rejected loudly, not one that goes to the wrong place.
	if kind == "channel" && target.ThreadID != "" {
		args = append(args, "--reply-to", target.ThreadID)
	}

	raw, err := c.cfg.Run(ctx, c.cfg.AplPath, args...)
	if err != nil {
		return "", convo.Wrap(convo.ErrTargetAbsent, "whatsapp send failed: %v", err)
	}
	id, err := sentID(raw)
	if err != nil {
		return "", err
	}
	return id, nil
}

// sentID digs the new message id out of the send result.
//
// It accepts several spellings on purpose. This is the one code path that could
// not be exercised against the real binary — sending is outward-facing and real
// — so it is written to be liberal about WHERE the id is and absolutely strict
// about whether there is one. A success with no id is NOT a success: reporting
// a message as sent when you cannot name the artifact is the failure this repo
// exists to prevent.
func sentID(raw []byte) (string, error) {
	var env envelope
	if err := json.Unmarshal(bytes.TrimSpace(raw), &env); err != nil {
		return "", convo.Wrap(convo.ErrInternal,
			"whatsapp send returned unparseable JSON: %v", err)
	}
	if !env.Success {
		detail := strings.TrimSpace(string(env.Error))
		if detail == "" || detail == "null" {
			detail = "no detail"
		}
		return "", convo.Wrap(convo.ErrTargetAbsent, "whatsapp send reported failure: %s", detail)
	}
	var body map[string]any
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &body)
	}
	for _, key := range []string{"msg_id", "message_id", "messageID", "id"} {
		if s, ok := body[key].(string); ok && strings.TrimSpace(s) != "" {
			return s, nil
		}
	}
	return "", convo.Wrap(convo.ErrInternal,
		"whatsapp send reported success but named no message id — refusing to report "+
			"a message as sent when the artifact cannot be pointed at")
}
