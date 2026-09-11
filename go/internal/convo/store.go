package convo

// Store is the SEAM FOR DURABILITY: the journal, the delivery position, and
// the processed marks.
//
// The three are separate methods because they are three separate positions,
// and conflating any two of them is the most-copied bug in this class of
// system (ARCHITECTURE.md §5.5, INTERFACES.md §2.3–2.4):
//
//	Append   — write BEFORE any consumer sees the message, so a crash between
//	           "received" and "delivered" loses nothing.
//	Journal  — look around. READING IS NOT CONSUMING: this must never move the
//	           read cursor, or an audit destroys the delivery state.
//	Next     — hand messages to a consumer. THIS, and only this, advances the
//	           read cursor, and it persists it AFTER the hand-off succeeds, so
//	           an interrupted consumer redelivers rather than drops.
//	Ack      — an explicit "I finished with this". Ack is NOT what prevents
//	           redelivery; the read cursor does that unconditionally. Ack exists
//	           so Unacked can show what nobody has completed — which is exactly
//	           the drain CROSS-SESSION.md §11 found missing: a message declined
//	           by a handler is never offered again by the wake path, so
//	           something must come back for it.
//
// One implementation (store/file) is enough; the interface exists so the
// semantics above are stated once, in one place, and can be tested.
type Store interface {
	// Append writes messages to the durable log, oldest first.
	Append(msgs []Message) error

	// Journal returns the whole log. Never advances anything.
	Journal() ([]Message, error)

	// Next hands up to limit messages to a consumer and advances the read
	// cursor past them. limit <= 0 means "everything outstanding".
	Next(limit int) ([]Message, error)

	// Ack marks ids processed.
	Ack(ids []string) error

	// Unacked returns journalled messages that were never acked — the drain.
	Unacked() ([]Message, error)
}
