package file

import (
	"path/filepath"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
)

// HeldCap bounds the hold list. See Hold() for what dropping the oldest entry
// actually costs — the journal is still complete either way.
const HeldCap = 5000

// The hold list: cursor/<tag>.held.json
//
// WHY THIS FILE EXISTS AT ALL
//
// The read cursor is a byte offset and it means "a consumer has been handed
// everything below this". Add a delivery filter and that sentence stops being
// true: a filtered-out message sits below the cursor having been handed to
// nobody. Two obvious fixes are both wrong:
//
//   - Advance the cursor anyway. The message is then gone from the delivery
//     path forever — a filter silently destroys traffic the agent never saw,
//     which is exactly the class of bug the cursor-vs-ack split exists to
//     prevent.
//   - Stall the cursor at the first rejected message. Nothing is lost, but one
//     permanently-rejected message at the head blocks the cursor forever, so
//     every message behind it is redelivered on every single call. Head-of-line
//     blocking that degrades into infinite redelivery is not a fix.
//
// So the delivery position becomes TWO things instead of one, and the invariant
// is stated positively:
//
//	NO MESSAGE LEAVES THE DELIVERABLE SET WITHOUT BEING HANDED TO A CONSUMER.
//
// The read cursor advances past a rejected message ONLY because that message's
// id is durably written to the hold list in the same operation, and it is
// written FIRST. The pair — cursor plus hold list — is the delivery position;
// neither half means anything alone. `next` re-evaluates the hold list against
// the CURRENT filter before it looks at anything new, so relaxing or dropping a
// filter delivers what was held, in order, ahead of fresh traffic.
//
// Cost, stated honestly: an id in the hold list is re-checked on every `next`,
// so a filter that rejects a lot accumulates work proportional to what it
// rejected. HeldCap bounds that at the price of the oldest ids falling out of
// the DELIVERY path — they remain in the journal, visible to
// `convo journal --all`, which is the trade this file chooses over unbounded
// growth. Nothing is deleted from disk, ever.

type heldFile struct {
	HeldIDs   []string `json:"heldIds"`
	UpdatedAt string   `json:"updatedAt"`
}

// HeldPath is where the hold list lives. It is a consumer-owned file, beside
// the read cursor and the acks, because it is part of the same position.
func (s *Store) HeldPath() string {
	return filepath.Join(s.Home, "cursor", s.Tag+".held.json")
}

// Held lists the ids currently held back by a filter, oldest first.
func (s *Store) Held() ([]string, error) {
	var body heldFile
	if err := readJSON(s.HeldPath(), &body); err != nil {
		return nil, err
	}
	return body.HeldIDs, nil
}

func (s *Store) writeHeld(ids []string) error {
	if len(ids) > HeldCap {
		ids = ids[len(ids)-HeldCap:]
	}
	return writeJSONAtomic(s.HeldPath(), heldFile{HeldIDs: ids, UpdatedAt: nowISO()})
}

// NextMatching hands up to limit messages that `allow` accepts to a consumer,
// and advances the delivery position past exactly what it EXAMINED — delivering
// what passed and holding what did not.
//
// allow == nil means "everything", which makes NextMatching a superset of Next:
// an unfiltered call drains the hold list first and then the journal, which is
// what "the filtered-out messages become deliverable again when the filter is
// dropped" means in code.
//
// Write order is load-bearing. The hold list is persisted BEFORE the cursor, so
// a crash between the two re-examines messages that are already held (the hold
// list dedupes) instead of advancing past messages nothing recorded. Delivered
// messages can be delivered twice; at-least-once is the correct side to fail on
// and is the same promise Next has always made.
func (s *Store) NextMatching(limit int, allow func(convo.Message) bool) ([]convo.Message, error) {
	if allow == nil {
		allow = func(convo.Message) bool { return true }
	}

	held, err := s.Held()
	if err != nil {
		return nil, err
	}
	before := len(held)
	heldChanged := false
	var out []convo.Message

	// 1. The hold list first, re-evaluated against the CURRENT filter, in
	//    journal order. A held message is older than anything new by
	//    definition, and answering the newest first is how the person who
	//    waited longest gets ignored.
	if len(held) > 0 {
		all, err := s.Journal()
		if err != nil {
			return nil, err
		}
		byID := make(map[string]convo.Message, len(all))
		for _, m := range all {
			byID[m.ID] = m
		}
		stillHeld := make([]string, 0, len(held))
		for i, id := range held {
			m, ok := byID[id]
			if !ok {
				// The journal was truncated or replaced under us. There is
				// nothing to deliver and nothing to hold.
				continue
			}
			if limit > 0 && len(out) >= limit {
				stillHeld = append(stillHeld, held[i:]...)
				break
			}
			if allow(m) {
				out = append(out, m)
				continue
			}
			stillHeld = append(stillHeld, id)
		}
		held = stillHeld
		if len(held) != before {
			heldChanged = true
		}
	}

	// 2. Then whatever is new, from the read cursor.
	var newOffset int64 = -1
	if limit <= 0 || len(out) < limit {
		cur, err := s.readCursor()
		if err != nil {
			return nil, err
		}
		msgs, offsets, err := s.readFrom(cur)
		if err != nil {
			return nil, err
		}
		inHold := make(map[string]bool, len(held))
		for _, id := range held {
			inHold[id] = true
		}
		for i, m := range msgs {
			if limit > 0 && len(out) >= limit {
				break
			}
			if allow(m) {
				out = append(out, m)
			} else if !inHold[m.ID] {
				held = append(held, m.ID)
				inHold[m.ID] = true
				heldChanged = true
			}
			// Examined: delivered or held. Either way the position moves,
			// because the hold list is what carries the rejected one forward.
			newOffset = offsets[i]
		}
	}

	if len(out) == 0 && newOffset < 0 && !heldChanged {
		return nil, nil
	}
	// Hold list first. See the comment above: this order is the whole
	// at-least-once argument.
	if heldChanged {
		if err := s.writeHeld(held); err != nil {
			return nil, err
		}
	}
	if newOffset >= 0 {
		if err := s.writeCursor(newOffset); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
