// Package file implements convo.Store on the on-disk layout of
// INTERFACES.md §2 — the portable interface, the one anything in any language
// can read with no CLI and no runtime.
//
//	$AGENT_CONVERSATIONS_HOME/
//	  journal/<tag>.ndjson       append-only log   writer appends · anyone reads
//	  cursor/<tag>.read.json     delivery position consumer writes
//	  cursor/<tag>.ack.json      processed marks   consumer writes
//
// The read cursor is a BYTE OFFSET into the journal, not a message count,
// because a consumer must be able to stop mid-batch (a --count satisfied) and
// resume at exactly the right line; anything coarser silently drops messages at
// a batch boundary.
//
// Two rules this package exists to enforce, and the tests assert both:
//
//   - READING IS NOT CONSUMING. Journal() never touches the cursor.
//   - THE CURSOR ADVANCES ON DELIVERY, NOT ON ACK, and is persisted AFTER the
//     hand-off, so an interrupted consumer redelivers rather than drops.
//     At-least-once is the correct side to fail on.
package file

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// AckCap bounds the ack list; the journal itself remains the truth.
const AckCap = 5000

// Store is one tag's durable state.
type Store struct {
	Home string
	Tag  string
}

// Home resolves the state directory: $AGENT_CONVERSATIONS_HOME, else
// ~/.config/agent-conversations. Every path derives from one variable so a test
// can run fully hermetically.
func Home() string {
	if h := os.Getenv("AGENT_CONVERSATIONS_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".agent-conversations"
	}
	return filepath.Join(home, ".config", "agent-conversations")
}

// New opens (and creates) a store.
func New(home, tag string) (*Store, error) {
	if home == "" {
		home = Home()
	}
	if tag == "" {
		tag = "default"
	}
	s := &Store{Home: home, Tag: tag}
	for _, d := range []string{filepath.Join(home, "journal"), filepath.Join(home, "cursor")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, convo.Wrap(convo.ErrNotConfigured, "cannot create %s: %v", d, err)
		}
	}
	return s, nil
}

func (s *Store) JournalPath() string {
	return filepath.Join(s.Home, "journal", s.Tag+".ndjson")
}
func (s *Store) ReadCursorPath() string {
	return filepath.Join(s.Home, "cursor", s.Tag+".read.json")
}
func (s *Store) AckPath() string {
	return filepath.Join(s.Home, "cursor", s.Tag+".ack.json")
}

// Append writes messages to the durable log, one JSON object per line, with a
// single O_APPEND write per line so concurrent appends interleave between lines
// and never inside one.
//
// This happens BEFORE any consumer, filter or handler sees the message: a crash
// between "received" and "delivered" must lose nothing.
func (s *Store) Append(msgs []convo.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	f, err := os.OpenFile(s.JournalPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return convo.Wrap(convo.ErrNotConfigured, "cannot open journal: %v", err)
	}
	defer f.Close()
	for _, m := range msgs {
		line, err := json.Marshal(m)
		if err != nil {
			return convo.Wrap(convo.ErrInternal, "cannot encode message %s: %v", m.ID, err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return convo.Wrap(convo.ErrNotConfigured, "cannot append to journal: %v", err)
		}
	}
	return nil
}

// Journal returns the whole log. It NEVER advances the read cursor: a
// look-around, an audit or a dashboard must not consume anything.
func (s *Store) Journal() ([]convo.Message, error) {
	msgs, _, err := s.readFrom(0)
	return msgs, err
}

// Next hands up to limit outstanding messages to a consumer and advances the
// read cursor past exactly those. limit <= 0 means everything outstanding.
//
// The cursor is written AFTER the messages are returned to the caller's slice,
// which is the closest this API can get to "after the hand-off succeeded".
func (s *Store) Next(limit int) ([]convo.Message, error) {
	cur, err := s.readCursor()
	if err != nil {
		return nil, err
	}
	msgs, offsets, err := s.readFrom(cur)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	if limit > 0 && len(msgs) > limit {
		msgs = msgs[:limit]
		offsets = offsets[:limit]
	}
	newOffset := offsets[len(offsets)-1]
	if err := s.writeCursor(newOffset); err != nil {
		return nil, err
	}
	return msgs, nil
}

// Ack marks ids processed. Ack is NOT what stops redelivery — the read cursor
// does that, unconditionally. Ack exists so Unacked can show what nobody has
// finished with, which is the drain a declined message needs.
func (s *Store) Ack(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	acked, err := s.Acked()
	if err != nil {
		return err
	}
	seen := make(map[string]bool, len(acked))
	for _, id := range acked {
		seen[id] = true
	}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			acked = append(acked, id)
		}
	}
	if len(acked) > AckCap {
		acked = acked[len(acked)-AckCap:]
	}
	return writeJSONAtomic(s.AckPath(), map[string]any{
		"ackedIds":  acked,
		"updatedAt": nowISO(),
	})
}

// Acked lists the ids marked processed.
func (s *Store) Acked() ([]string, error) {
	var body struct {
		AckedIDs []string `json:"ackedIds"`
	}
	if err := readJSON(s.AckPath(), &body); err != nil {
		return nil, err
	}
	return body.AckedIDs, nil
}

// Unacked returns journalled messages nobody has acked — including ones already
// delivered. Delivered-but-unacked is exactly the state a failed answer leaves
// behind, and the only way it is ever seen again.
func (s *Store) Unacked() ([]convo.Message, error) {
	acked, err := s.Acked()
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(acked))
	for _, id := range acked {
		set[id] = true
	}
	all, err := s.Journal()
	if err != nil {
		return nil, err
	}
	var out []convo.Message
	for _, m := range all {
		if !set[m.ID] {
			out = append(out, m)
		}
	}
	return out, nil
}

// ReadOffset exposes the delivery position for inspection.
func (s *Store) ReadOffset() (int64, error) { return s.readCursor() }

// readFrom parses the journal from a byte offset, returning each message and
// the byte offset immediately AFTER its line.
//
// A trailing line with no newline is a partial write caught mid-flight: it is
// skipped, not parsed, and the cursor is not advanced past it. The writer will
// finish it and the next call will pick it up.
func (s *Store) readFrom(offset int64) ([]convo.Message, []int64, error) {
	data, err := os.ReadFile(s.JournalPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, convo.Wrap(convo.ErrNotConfigured, "cannot read journal: %v", err)
	}
	if offset > int64(len(data)) {
		// The journal was truncated or replaced under us. Re-reading from the
		// start is at-least-once; pretending the cursor is still valid is loss.
		offset = 0
	}
	rest := data[offset:]
	var msgs []convo.Message
	var ends []int64
	pos := offset
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break // partial final line — leave it for next time
		}
		line := rest[:i]
		pos += int64(i) + 1
		rest = rest[i+1:]
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m convo.Message
		if err := json.Unmarshal(line, &m); err != nil {
			continue // a corrupt line must not wedge the whole log
		}
		msgs = append(msgs, m)
		ends = append(ends, pos)
	}
	return msgs, ends, nil
}

func (s *Store) readCursor() (int64, error) {
	var body struct {
		ReadOffset int64 `json:"readOffset"`
	}
	if err := readJSON(s.ReadCursorPath(), &body); err != nil {
		return 0, err
	}
	return body.ReadOffset, nil
}

func (s *Store) writeCursor(offset int64) error {
	return writeJSONAtomic(s.ReadCursorPath(), map[string]any{
		"readOffset": offset,
		"updatedAt":  nowISO(),
	})
}

// writeJSONAtomic is tmp-then-rename: a concurrent reader sees the old file or
// the new one, never a half-written one.
func writeJSONAtomic(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
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

// readJSON treats a missing or unparseable state file as absent, which is the
// correct reading: a cursor we cannot read has not advanced.
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	_ = json.Unmarshal(b, v)
	return nil
}

func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

var _ convo.Store = (*Store)(nil)
