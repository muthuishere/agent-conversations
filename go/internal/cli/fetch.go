package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
	filestore "github.com/muthuishere/agent-conversations/go/internal/store/file"
)

// SeenCap bounds the replay guard. The journal is the truth; this list only has
// to be long enough to cover the overlap a delta token or a timestamp watermark
// can hand back after a restart.
const SeenCap = 5000

// fetchCursor is `cursor/<tag>.fetch.json` of INTERFACES.md §2.2 — how far we
// have pulled FROM THE CHANNEL. It is written by the ingest path and by nothing
// else.
//
// Two fields, for two different jobs:
//
//   - Tokens is one OPAQUE cursor per conversation. Whatever the channel handed
//     back — a delta token, a timestamp, a composite — travels through here
//     untouched. Nothing in this file interprets it.
//   - SeenIDs is a bounded replay guard, and dedupe is BY ID, never by clock.
//     Conversations are polled one after another, so a valid message from a
//     quiet DM routinely arrives after a newer one from a busy channel; a
//     timestamp high-water mark across conversations would silently drop it.
type fetchCursor struct {
	Tokens    map[string]string `json:"tokens"`
	SeenIDs   []string          `json:"seenIds"`
	UpdatedAt string            `json:"updatedAt"`

	seen map[string]bool
}

func fetchCursorPath(st *filestore.Store) string {
	return filepath.Join(st.Home, "cursor", st.Tag+".fetch.json")
}

// loadFetchCursor treats a missing or unreadable cursor as empty. That is the
// honest reading — a position we cannot read has not been reached — and it
// fails towards re-fetching, which the id dedupe then absorbs.
func loadFetchCursor(path string) *fetchCursor {
	fc := &fetchCursor{Tokens: map[string]string{}, seen: map[string]bool{}}
	b, err := os.ReadFile(path)
	if err != nil {
		return fc
	}
	_ = json.Unmarshal(b, fc)
	if fc.Tokens == nil {
		fc.Tokens = map[string]string{}
	}
	for _, id := range fc.SeenIDs {
		fc.seen[id] = true
	}
	return fc
}

func (fc *fetchCursor) markSeen(id string) {
	fc.seen[id] = true
	fc.SeenIDs = append(fc.SeenIDs, id)
	if len(fc.SeenIDs) > SeenCap {
		drop := fc.SeenIDs[:len(fc.SeenIDs)-SeenCap]
		for _, d := range drop {
			delete(fc.seen, d)
		}
		fc.SeenIDs = append([]string(nil), fc.SeenIDs[len(fc.SeenIDs)-SeenCap:]...)
	}
}

func (fc *fetchCursor) save(path string) error {
	fc.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(fc)
	if err != nil {
		return convo.Wrap(convo.ErrInternal, "encoding fetch cursor: %v", err)
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return convo.Wrap(convo.ErrNotConfigured, "cannot write %s: %v", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return convo.Wrap(convo.ErrNotConfigured, "cannot rename %s: %v", path, err)
	}
	return nil
}

// primer is the OPTIONAL half of a channel: jump a fresh cursor to NOW without
// emitting anything. It is deliberately not on convo.Channel — a channel that
// cannot do it is still a perfectly good channel, and widening the interface to
// hold an optional capability is how interfaces rot.
type primer interface {
	PrimeCursor(ctx context.Context, convID string) (string, error)
}

// pass is what one ingest pass reports back. The counts feed the heartbeat and
// the backoff tier, so they are returned rather than printed.
type pass struct {
	Ingested      int            `json:"ingested"`
	Conversations int            `json:"conversations"`
	Identity      convo.Identity `json:"identity"`
}

// cmdFetch is ONE ingest pass, and it is the half of this CLI that was missing:
// without it nothing ever calls Store.Append, so the journal stays empty and
// `journal`, `next`, `ack` and `respond` all have nothing to work on.
//
// It is idempotent by construction — run it in a loop, run it twice by mistake,
// run it after a crash mid-write. The fetch cursor moves it forward, the id
// dedupe absorbs whatever the cursor hands back twice, and the journal is
// append-only, so the worst case is a no-op.
func (a *App) cmdFetch(ctx context.Context, o options) error {
	ch, err := a.channel(o)
	if err != nil {
		return err
	}
	st, err := a.store(o)
	if err != nil {
		return err
	}
	p, err := a.fetchOnce(ctx, o, ch, st)
	if err != nil {
		return err
	}
	return a.print(o, fmt.Sprintf("ingested %d from %d conversations", p.Ingested, p.Conversations), p)
}

// fetchOnce pulls every conversation once, normalises, suppresses our own echo,
// journals, and advances the per-conversation cursor.
//
// The ORDER of the last two is the whole correctness argument: append first,
// persist the cursor after. A crash in between re-fetches messages that are
// already journalled — which the id dedupe swallows — whereas the other order
// loses them outright. At-least-once is the correct side to fail on.
func (a *App) fetchOnce(ctx context.Context, o options, ch convo.Channel, st *filestore.Store) (pass, error) {
	var p pass

	// Identity BEFORE anything else. Self-echo suppression compares against
	// this and nothing else (ARCHITECTURE.md §5.3); running without it means
	// our own reply is journalled, wakes the agent, and it replies again — on a
	// real channel, for real money. So an identity we cannot establish is a
	// hard stop, never a warning we carry on past.
	self, err := ch.Identity(ctx)
	if err != nil {
		return p, err
	}
	p.Identity = self

	convs, err := ch.Conversations(ctx)
	if err != nil {
		return p, err
	}

	path := fetchCursorPath(st)
	fc := loadFetchCursor(path)

	var failed []string
	var firstErr error
	for _, cv := range convs {
		if !matchesIn(o.in, cv) {
			continue
		}
		p.Conversations++

		cur := fc.Tokens[cv.ID]
		if cur == "" && o.prime {
			// First attach. A cursorless Fetch replays the entire history of
			// the room, which looks exactly like a flood of new traffic and
			// wakes an agent once per message anyone ever sent.
			//
			// A channel that cannot prime falls through to that ordinary
			// cursorless Fetch, and does so openly: --prime is a request, not
			// a promise convo.Channel makes.
			if pr, ok := ch.(primer); ok {
				tok, perr := pr.PrimeCursor(ctx, cv.ID)
				if perr != nil {
					failed, firstErr = note(failed, firstErr, cv, perr)
					continue
				}
				fc.Tokens[cv.ID] = tok
				continue
			}
		}

		msgs, next, ferr := ch.Fetch(ctx, cv.ID, cur)
		if ferr != nil {
			// One unreachable room must not blind us to the others: keep
			// going, keep the cursors that did advance, and report at the end.
			// Swallowing it instead is the silent deafness this design exists
			// to prevent.
			failed, firstErr = note(failed, firstErr, cv, ferr)
			continue
		}

		fresh := make([]convo.Message, 0, len(msgs))
		for _, m := range msgs {
			if m.ID == "" {
				continue
			}
			if m.From.ID != "" && m.From.ID == self.ID {
				// SELF-ECHO, by identity. The channel is expected to do this
				// too; doing it here as well costs a string compare and means
				// a channel that forgets cannot start a reply loop.
				continue
			}
			if fc.seen[m.ID] {
				continue
			}
			if m.Source.Name == "" {
				// Backfill the room's display name from discovery. A channel
				// that leaves it empty is not broken — routing uses the id and
				// the thread — but the compact format of INTERFACES.md §3 is
				// what a model reads with its eyes, and `[]` where `[#general]`
				// belongs makes every message look like it came from nowhere.
				// Filling it here means one forgetful adapter cannot degrade
				// the output of every consumer.
				m.Source.Name = cv.Name
			}
			fc.markSeen(m.ID)
			fresh = append(fresh, m)
		}

		if err := st.Append(fresh); err != nil {
			return p, err
		}
		fc.Tokens[cv.ID] = next
		p.Ingested += len(fresh)
	}

	if err := fc.save(path); err != nil {
		return p, err
	}
	if firstErr != nil {
		return p, convo.Wrap(errSentinel(firstErr),
			"%d of %d conversations failed (%s): %s",
			len(failed), p.Conversations, strings.Join(failed, ", "), errMessage(firstErr))
	}
	return p, nil
}

func note(failed []string, first error, cv convo.Conversation, err error) ([]string, error) {
	if first == nil {
		first = err
	}
	return append(failed, cv.Name), first
}

// errSentinel recovers the typed sentinel from an error so a partial failure
// keeps the exit code the underlying failure earned — 69 for an unreachable
// transport stays 69, and does not get flattened into a generic 1.
func errSentinel(err error) *convo.Err {
	var e *convo.Err
	if errors.As(err, &e) {
		return e
	}
	return convo.ErrInternal
}

// matchesIn is the conversation selector: empty means everything, otherwise a
// conversation matches on its exact id or a substring of its display name.
// Case-insensitive on the name, because `#General` and `#general` are the same
// room to everyone except a string compare.
func matchesIn(needle string, cv convo.Conversation) bool {
	if needle == "" {
		return true
	}
	if cv.ID == needle {
		return true
	}
	return strings.Contains(strings.ToLower(cv.Name), strings.ToLower(needle))
}
