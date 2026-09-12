package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// Parallel-fetch defaults. Measured on a live Teams tenant through apl: ~1.6s
// per Graph call, because every call is a process spawn, so one sequential
// pass over 37 conversations took 60–160s. Six workers is enough to bring that
// under the active poll interval without looking like a burst to Graph's
// per-app throttle.
const (
	defaultFetchParallel = 6

	// throttleRetries bounds how many times ONE worker re-asks ONE throttled
	// room inside a single pass. Past that the room is reported failed and
	// re-tried on the next poll — the next poll is coming anyway.
	throttleRetries = 3
	throttleBase    = 2 * time.Second
	throttleMax     = 30 * time.Second
)

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

// fetchJob is one conversation's worth of work handed to a worker: the room
// and the cursor it starts from, snapshotted BEFORE the worker runs so no
// worker ever reads the shared cursor map.
type fetchJob struct {
	cv  convo.Conversation
	cur string
}

// fetchResult is what a worker hands back. Exactly one of these per job, in
// completion order, and the worker touches nothing else — journal, cursor
// map and seen-set are written by the collecting goroutine alone.
type fetchResult struct {
	cv     convo.Conversation
	msgs   []convo.Message
	next   string
	primed bool
	err    error
}

// fetchOnce pulls every conversation once, normalises, suppresses our own echo,
// journals, and advances the per-conversation cursor.
//
// The ORDER of the last two is the whole correctness argument: append first,
// persist the cursor after. A crash in between re-fetches messages that are
// already journalled — which the id dedupe swallows — whereas the other order
// loses them outright. At-least-once is the correct side to fail on.
//
// Conversations are fetched by a BOUNDED WORKER POOL (--fetch-parallel, default
// 6), because on a brokered transport every call is a process spawn and a
// sequential pass over a few dozen rooms takes minutes. The pool changes where
// the work runs and nothing about what is guaranteed:
//
//   - each conversation keeps its own cursor, snapshotted into its job before
//     any worker starts, so workers never share a read of the cursor map;
//   - a room that fails is reported and the others still land — one blind
//     room must not blind the pass;
//   - the journal is appended by ONE goroutine as results come in, and the
//     cursor file is written once, after every append, so append-before-cursor
//     holds exactly as it did sequentially;
//   - within one conversation the journal order is the adapter's order.
//     ACROSS conversations it is completion order, which interleaves and is
//     not deterministic — a busy channel's batch may land after a quiet DM
//     that was fetched later. Consumers already had to live with that
//     (INTERFACES.md §2.2: dedupe by id, never by clock), and now it is
//     stated rather than accidental.
//
// A worker whose room answers with a throttle (convo.IsRetryable — a 429 or a
// 5xx the adapter already retried) backs itself off, honouring Retry-After when
// the server sent one, and re-asks that room a bounded number of times before
// giving up on it for this pass. Moving straight on to the next room instead
// is how a throttle becomes an app-wide quota cut.
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

	var jobs []fetchJob
	for _, cv := range convs {
		if !matchesIn(o.in, cv) {
			continue
		}
		jobs = append(jobs, fetchJob{cv: cv, cur: fc.Tokens[cv.ID]})
	}
	p.Conversations = len(jobs)

	workers := o.fetchParallel
	if workers <= 0 {
		workers = defaultFetchParallel
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}

	pr, canPrime := ch.(primer)
	results := make(chan fetchResult)
	queue := make(chan fetchJob)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				results <- fetchOne(ctx, ch, pr, canPrime, o.replayHistory, j)
			}
		}()
	}
	go func() {
		for _, j := range jobs {
			queue <- j
		}
		close(queue)
		wg.Wait()
		close(results)
	}()

	var failed []string
	var firstErr error
	var appendErr error
	for r := range results {
		if appendErr != nil {
			// The journal is not writable; nothing more can land, and moving a
			// cursor past messages that never landed would lose them. Drain the
			// workers so they exit, then fail the pass.
			continue
		}
		if r.err != nil {
			// One unreachable room must not blind us to the others: keep
			// going, keep the cursors that did advance, and report at the end.
			// Swallowing it instead is the silent deafness this design exists
			// to prevent.
			failed, firstErr = note(failed, firstErr, r.cv, r.err)
			continue
		}
		if r.primed {
			fc.Tokens[r.cv.ID] = r.next
			continue
		}

		fresh := make([]convo.Message, 0, len(r.msgs))
		for _, m := range r.msgs {
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
				m.Source.Name = r.cv.Name
			}
			fc.markSeen(m.ID)
			fresh = append(fresh, m)
		}

		if err := st.Append(fresh); err != nil {
			appendErr = err
			continue
		}
		fc.Tokens[r.cv.ID] = r.next
		p.Ingested += len(fresh)
	}
	if appendErr != nil {
		return p, appendErr
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

// fetchOne is the unit of work a worker runs: one conversation, one cursor,
// no shared state. It touches the channel and nothing else.
//
// A conversation with NO CURSOR is one we have never looked at — a first
// attach, or a room discovery only just surfaced (measured on WhatsApp: the
// top-N chat listing drifts between polls, and a chat that drops in later
// arrives cursorless). The default is to PRIME it at now and ingest nothing
// from its past: the journal is what happens while listening (ADR-005), and
// filters still see everything from that point. Replaying instead wakes an
// agent once for every message anyone ever sent in that room — measured as
// 447 months-old messages from 17 late-discovered chats in one pass.
// --replay-history is the explicit opt-in to the archive behaviour, and a
// channel that cannot prime falls through to it openly.
func fetchOne(ctx context.Context, ch convo.Channel, pr primer, canPrime, replayHistory bool, j fetchJob) fetchResult {
	r := fetchResult{cv: j.cv}
	if j.cur == "" && !replayHistory && canPrime {
		tok, err := withThrottle(ctx, func() (string, error) {
			return pr.PrimeCursor(ctx, j.cv.ID)
		})
		r.next, r.primed, r.err = tok, err == nil, err
		return r
	}
	type fetched struct {
		msgs []convo.Message
		next string
	}
	f, err := withThrottle(ctx, func() (fetched, error) {
		msgs, next, err := ch.Fetch(ctx, j.cv.ID, j.cur)
		return fetched{msgs, next}, err
	})
	r.msgs, r.next, r.err = f.msgs, f.next, err
	return r
}

// withThrottle runs one channel call and, if the channel says the failure was
// a throttle, waits and re-asks — Retry-After verbatim when the server sent
// one, an exponential ladder from throttleBase otherwise, capped at
// throttleMax and at throttleRetries attempts. Any other error, and a
// cancelled context, come straight back.
func withThrottle[T any](ctx context.Context, call func() (T, error)) (T, error) {
	var zero T
	var last error
	for attempt := 0; ; attempt++ {
		v, err := call()
		if err == nil {
			return v, nil
		}
		last = err
		if !convo.IsRetryable(err) || attempt >= throttleRetries {
			return zero, last
		}
		delay := convo.RetryAfter(err)
		if delay <= 0 {
			delay = throttleBase << attempt
		}
		if delay > throttleMax {
			delay = throttleMax
		}
		select {
		case <-ctx.Done():
			return zero, last
		case <-throttleSleep(delay):
		}
	}
}

// throttleSleep is the one clock the backoff waits on, swapped in tests so a
// throttled fixture does not make the suite sleep for real.
var throttleSleep = func(d time.Duration) <-chan time.Time { return time.After(d) }

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
