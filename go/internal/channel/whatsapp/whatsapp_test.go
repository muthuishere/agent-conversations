package whatsapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// EVERY JID, NAME AND NUMBER IN THIS FILE IS SYNTHETIC. This repo is public,
// and a fixture captured from a real account would publish somebody's phone
// number and somebody's chat forever.
const (
	selfJID  = "1234567890@s.whatsapp.net"
	aliceJID = "1555000111@s.whatsapp.net"
	bobJID   = "1555000222@s.whatsapp.net"
	groupJID = "1111111111-2222222222@g.us"
)

// fakes writes a fake `wacli` and a fake `apl` into a temp dir and returns a
// Config pointed at them, plus the path of the file every invocation appends
// its argv to.
//
// The fake apl is three lines and does the one thing the real one does that
// matters here: it strips `with <handle> --` and execs the rest. So the
// wacli argv this package builds for a SEND is executed for real, by the real
// shell, through the real broker shape — with nothing leaving the machine.
//
// NOTHING IN THIS TEST FILE CAN SEND A WHATSAPP MESSAGE. That is the point of
// it: sending is outward-facing and real, and the only honest way to test the
// argv is to build it and then run something that is not WhatsApp.
func fakes(t *testing.T, wacliBody string) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")

	wacli := filepath.Join(dir, "wacli")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" + wacliBody
	if err := os.WriteFile(wacli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	apl := filepath.Join(dir, "apl")
	// `apl with <handle> -- cmd…` -> exec cmd…
	aplScript := "#!/bin/sh\nprintf 'apl %s\\n' \"$*\" >> " + log + `
shift 2      # drop "with" and the handle
[ "$1" = "--" ] && shift
exec "$@"
`
	if err := os.WriteFile(apl, []byte(aplScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return Config{Handle: "whatsapp:testacct", WacliPath: wacli, AplPath: apl}, log
}

// wacliDispatch is the body of the fake: a case over the sub-command, each arm
// echoing one recorded-shape JSON envelope. The shapes are wacli's real ones
// (an outer {success,data,error}, snake_case chat rows, PascalCase message
// rows) with synthetic content.
const wacliDispatch = `
case "$*" in
  *"auth status"*)
    echo '{"success":true,"data":{"authenticated":true,"linked_jid":"1234567890:12@s.whatsapp.net","phone":"+1234567890"},"error":null}' ;;
  *"chats list"*)
    echo '{"success":true,"data":[
      {"jid":"` + groupJID + `","kind":"group","name":"#general","last_message_ts":"2026-09-12T10:00:05Z"},
      {"jid":"` + aliceJID + `","kind":"dm","name":"alice","last_message_ts":"2026-09-12T10:00:01Z"},
      {"jid":"1234567890@newsletter","kind":"newsletter","name":"a newsletter","last_message_ts":"2026-09-12T09:00:00Z"},
      {"jid":"status@broadcast","kind":"status","name":"status","last_message_ts":"2026-09-12T09:00:00Z"}
    ],"error":null}' ;;
  *"messages list"*)
    echo '{"success":true,"data":{"fts":false,"messages":[
      {"ChatJID":"` + groupJID + `","ChatName":"#general","MsgID":"AAA1","SenderJID":"` + aliceJID + `","SenderName":"alice","Timestamp":"2026-09-12T10:00:01Z","FromMe":false,"Text":"standup in five"},
      {"ChatJID":"` + groupJID + `","ChatName":"#general","MsgID":"AAA2","SenderJID":"` + bobJID + `","SenderName":"bob","Timestamp":"2026-09-12T10:00:02Z","FromMe":false,"Text":"@1234567890 is the deploy red?"},
      {"ChatJID":"` + groupJID + `","ChatName":"#general","MsgID":"AAA3","SenderJID":"1234567890:12@s.whatsapp.net","SenderName":"me","Timestamp":"2026-09-12T10:00:03Z","FromMe":true,"Text":"looking now"},
      {"ChatJID":"` + groupJID + `","ChatName":"#general","MsgID":"AAA4","SenderJID":"` + aliceJID + `","SenderName":"alice","Timestamp":"2026-09-12T10:00:04Z","FromMe":false,"Text":"","ReactionToID":"AAA2","ReactionEmoji":"thumbsup"},
      {"ChatJID":"` + groupJID + `","ChatName":"#general","MsgID":"AAA5","SenderJID":"` + bobJID + `","SenderName":"bob","Timestamp":"2026-09-12T10:00:05Z","FromMe":false,"Text":"","MediaType":"image","MediaCaption":"the graph"}
    ]},"error":null}' ;;
  *"send text"*)
    echo '{"success":true,"data":{"msg_id":"SENT1","to":"x","timestamp":"2026-09-12T10:01:00Z"},"error":null}' ;;
  *) echo '{"success":false,"data":null,"error":{"message":"unsupported"}}'; exit 1 ;;
esac
`

func newFake(t *testing.T, body string) (*Channel, string) {
	t.Helper()
	cfg, log := fakes(t, body)
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, log
}

func argv(t *testing.T, log string) string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("nothing was executed: %v", err)
	}
	return string(b)
}

// The handle is the only configuration, and it is mandatory: an adapter that
// defaults to "whichever account wacli calls default" can start answering from
// the wrong phone number, and nobody finds out until a message has gone out.
func TestHandleIsRequiredAndNormalised(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("a channel with no handle was accepted")
	}
	if _, err := New(Config{Handle: "slack:whatever"}); err == nil {
		t.Fatal("a non-whatsapp handle was accepted")
	}
	c, err := New(Config{Handle: "personal"}) // bare label
	if err != nil {
		t.Fatal(err)
	}
	if c.account != "personal" || c.cfg.Handle != "whatsapp:personal" {
		t.Fatalf("handle normalised to %q / account %q", c.cfg.Handle, c.account)
	}
}

// Identity strips the device suffix. The same human is a different string on
// every linked device, and comparing the raw value is how self-echo
// suppression quietly stops working after a relink.
func TestIdentityIsTheLinkedAccountWithoutItsDevice(t *testing.T) {
	c, _ := newFake(t, wacliDispatch)
	id, err := c.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id.ID != selfJID {
		t.Fatalf("identity = %q, want %q", id.ID, selfJID)
	}
}

// An unlinked account is exit 69, not a warning we carry on past: with no
// identity there is no self-echo suppression, and the adapter would journal its
// own replies and wake the agent to answer itself.
func TestUnlinkedAccountIsAHardFailure(t *testing.T) {
	c, _ := newFake(t, `echo '{"success":true,"data":{"authenticated":false,"linked_jid":"","phone":""},"error":null}'`)
	_, err := c.Identity(context.Background())
	if err == nil {
		t.Fatal("an unlinked account was accepted")
	}
	if convo.ExitCode(err) != convo.ExitUnavailable {
		t.Fatalf("exit code %d, want %d", convo.ExitCode(err), convo.ExitUnavailable)
	}
}

// The JID suffix is the routing taxonomy: @g.us is a group (kind "channel"),
// @s.whatsapp.net is a 1:1 (kind "chat"), and anything else — newsletters,
// status broadcasts — is not conversation and is skipped.
func TestConversationsMapGroupAndDMFromTheJIDSuffix(t *testing.T) {
	c, log := newFake(t, wacliDispatch)
	convs, err := c.Conversations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 2 {
		t.Fatalf("got %d conversations, want 2 (newsletters and status are not conversation): %+v", len(convs), convs)
	}
	if convs[0].ID != groupJID || convs[0].Kind != "channel" {
		t.Fatalf("group mapped to %+v", convs[0])
	}
	if convs[1].ID != aliceJID || convs[1].Kind != "chat" {
		t.Fatalf("dm mapped to %+v", convs[1])
	}
	if !strings.Contains(argv(t, log), "--account testacct") {
		t.Fatalf("a read did not run as the bound account: %s", argv(t, log))
	}
}

func TestFetchNormalisesSuppressesAndThreads(t *testing.T) {
	c, log := newFake(t, wacliDispatch)
	msgs, next, err := c.Fetch(context.Background(), groupJID, "")
	if err != nil {
		t.Fatal(err)
	}
	// AAA3 is ours (self-echo) and AAA4 is a reaction: neither is conversation.
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3: %+v", len(msgs), msgs)
	}
	if msgs[0].ID != "AAA1" || msgs[2].ID != "AAA5" {
		t.Fatalf("not oldest-first: %s … %s", msgs[0].ID, msgs[2].ID)
	}
	for _, m := range msgs {
		if m.From.ID == selfJID {
			t.Fatal("our own message was returned — a reply loop waiting to happen")
		}
		if m.Source.Kind != "channel" || m.Source.ConversationID != groupJID {
			t.Fatalf("bad source routing: %+v", m.Source)
		}
		if m.Source.ThreadID != m.ID {
			t.Fatalf("whatsapp is flat: a message's thread is itself, got %q", m.Source.ThreadID)
		}
		if len(m.Raw) == 0 {
			t.Fatal("raw payload discarded")
		}
	}
	// A mention is our NUMBER in the text — which is our identity id, not a
	// display name anyone could type.
	if msgs[0].MentionsMe || !msgs[1].MentionsMe {
		t.Fatalf("mention detection wrong: %v / %v", msgs[0].MentionsMe, msgs[1].MentionsMe)
	}
	// A photo with a caption IS conversation.
	if msgs[2].Text != "the graph" {
		t.Fatalf("captioned media lost its text: %q", msgs[2].Text)
	}
	if next == "" {
		t.Fatal("no cursor handed back")
	}
	// First fetch has no watermark, so it must not pass --after.
	if strings.Contains(argv(t, log), "--after") {
		t.Fatalf("a cursorless fetch narrowed the window: %s", argv(t, log))
	}
}

// The cursor is this adapter's own invention, so its two properties are
// asserted directly: it re-asks from one second BEFORE the watermark (because
// whatsapp timestamps are second-granular and two messages share one), and it
// deduplicates the resulting overlap by message id.
func TestCursorOverlapsBySecondAndDedupesByID(t *testing.T) {
	c, log := newFake(t, wacliDispatch)
	_, next, err := c.Fetch(context.Background(), groupJID, "")
	if err != nil {
		t.Fatal(err)
	}
	msgs, _, err := c.Fetch(context.Background(), groupJID, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("the same page was re-emitted: %+v", msgs)
	}
	if !strings.Contains(argv(t, log), "--after 2026-09-12T10:00:04Z") {
		t.Fatalf("the second poll did not overlap by one second: %s", argv(t, log))
	}
}

// An unreadable cursor is a loud error, never a silent restart: quietly
// starting over replays an entire chat history into the journal and looks
// exactly like a flood of new traffic.
func TestUnreadableCursorFailsLoudly(t *testing.T) {
	c, _ := newFake(t, wacliDispatch)
	if _, _, err := c.Fetch(context.Background(), groupJID, "!!!not-base64!!!"); err == nil {
		t.Fatal("a corrupt cursor was silently treated as empty")
	}
}

func TestFetchRefusesSomethingThatIsNotAJID(t *testing.T) {
	c, _ := newFake(t, wacliDispatch)
	if _, _, err := c.Fetch(context.Background(), "c-general", ""); err == nil {
		t.Fatal("a non-JID conversation id was accepted")
	}
}

// The send argv, built for real and executed against a fake. `--message` is a
// FLAG: `wacli send text --to X "hi"` fails, and a channel that gets this wrong
// looks fine until the first reply.
func TestSendGoesThroughAplAndUsesTheMessageFlag(t *testing.T) {
	c, log := newFake(t, wacliDispatch)
	id, err := c.Send(context.Background(),
		convo.Target{ConversationID: aliceJID, Kind: "chat"}, "on it")
	if err != nil {
		t.Fatal(err)
	}
	if id != "SENT1" {
		t.Fatalf("send returned id %q", id)
	}
	got := argv(t, log)
	if !strings.Contains(got, "apl with whatsapp:testacct --") {
		t.Fatalf("the send did not go through the identity broker: %s", got)
	}
	if !strings.Contains(got, "send text --to "+aliceJID+" --message on it") {
		t.Fatalf("wrong send argv: %s", got)
	}
	if strings.Contains(got, "--reply-to") {
		t.Fatalf("a flat 1:1 send quoted something: %s", got)
	}
}

// A group reply QUOTES the message it answers. In a busy group an unquoted
// answer is unreadable, and ReplyTarget already carries the id to quote.
func TestGroupReplyQuotesWithReplyTo(t *testing.T) {
	c, log := newFake(t, wacliDispatch)
	src := convo.Message{ID: "AAA2", Source: convo.Source{
		Kind: "channel", ConversationID: groupJID, ThreadID: "AAA2"}}
	if _, err := c.Send(context.Background(), convo.ReplyTarget(src), "looking"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(argv(t, log), "--reply-to AAA2") {
		t.Fatalf("a group reply did not quote: %s", argv(t, log))
	}
}

// A Target whose Kind disagrees with its JID is a routing bug upstream, and
// sending anyway is how a reply lands in a stranger's DM. Nothing is executed.
func TestSendRefusesAKindMismatchAndEmptyText(t *testing.T) {
	c, log := newFake(t, wacliDispatch)
	if _, err := c.Send(context.Background(),
		convo.Target{ConversationID: aliceJID, Kind: "channel"}, "hello"); err == nil {
		t.Fatal("a chat JID was accepted as a channel target")
	}
	if _, err := c.Send(context.Background(),
		convo.Target{ConversationID: "not-a-jid", Kind: "chat"}, "hello"); err == nil {
		t.Fatal("a non-JID recipient was accepted")
	}
	if _, err := c.Send(context.Background(),
		convo.Target{ConversationID: aliceJID, Kind: "chat"}, "   "); err == nil {
		t.Fatal("an empty message was accepted")
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("a refused send still executed something")
	}
}

// A success with no message id is NOT a success. Reporting a message as sent
// when the artifact cannot be pointed at is the exact failure this repo is
// about, and it is the one send shape that could not be checked against the
// real binary — so it is checked here.
func TestSendRefusesASuccessWithNoID(t *testing.T) {
	c, _ := newFake(t, `echo '{"success":true,"data":{"to":"x"},"error":null}'`)
	if _, err := c.Send(context.Background(),
		convo.Target{ConversationID: aliceJID, Kind: "chat"}, "hi"); err == nil {
		t.Fatal("an id-less success was reported as sent")
	}
}

// A failing wacli is a hard error, not an empty result. A channel that reads
// "the store is locked" as "no messages" is a silently deaf listener.
func TestWacliFailureIsNotSilence(t *testing.T) {
	c, _ := newFake(t, `echo '{"success":false,"data":null,"error":{"message":"store is locked"}}'; exit 0`)
	if _, err := c.Conversations(context.Background()); err == nil {
		t.Fatal("a reported failure was read as an empty chat list")
	}
}

// PrimeCursor positions at NOW without emitting anything, so a first attach
// does not replay a person's whole history into the journal.
func TestPrimeCursorEmitsNothing(t *testing.T) {
	c, log := newFake(t, wacliDispatch)
	cur, err := c.PrimeCursor(context.Background(), groupJID)
	if err != nil {
		t.Fatal(err)
	}
	if cur == "" {
		t.Fatal("empty primed cursor")
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("priming ran a command; it should be free")
	}
	parsed, err := parseCursor(cur)
	if err != nil || parsed.TS == "" {
		t.Fatalf("primed cursor = %+v (%v)", parsed, err)
	}
}

// The envelope this adapter produces is the canonical one, round-trippable
// through the journal.
func TestEnvelopeRoundTrips(t *testing.T) {
	c, _ := newFake(t, wacliDispatch)
	msgs, _, err := c.Fetch(context.Background(), groupJID, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(msgs[1])
	if err != nil {
		t.Fatal(err)
	}
	var back convo.Message
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ID != "AAA2" || !back.MentionsMe || back.Source.ConversationID != groupJID {
		t.Fatalf("round trip lost something: %+v", back)
	}
}
