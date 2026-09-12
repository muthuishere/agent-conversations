package convo

import (
	"errors"
	"fmt"
	"time"
)

// Exit codes. The first five are INTERFACES.md §1 verbatim — a Go CLI that
// invented its own would break every shell that already branches on them. The
// last two are new, and they are new for the same reason 64 exists: a caller
// must be able to tell "I refused for safety" from "the thing is broken"
// without parsing an error string.
const (
	ExitOK            = 0  // success
	ExitError         = 1  // unexpected error
	ExitTimeout       = 64 // nothing arrived / a bounded wait aged out. NOT an error.
	ExitNotConfigured = 65 // bad args, unknown id, nothing configured
	ExitConflict      = 66 // someone else holds this (consumer lock, delivery in flight)
	ExitUnavailable   = 69 // the host or the target is dead, stale or absent
	ExitSelfDelivery  = 70 // refused: the target resolves to our own pane
	ExitBlocked       = 75 // the target is blocked; a human is needed, message held
)

// Err is a machine-matchable error. Code is stable and is what a caller should
// branch on; Message is for a human and may change. Same shape as the Node
// reference's `{"error":{"code":…,"message":…}}` so the two are interchangeable
// on the wire.
type Err struct {
	Code    string
	Message string
	Exit    int
	wrapped error
}

func (e *Err) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }
func (e *Err) Unwrap() error { return e.wrapped }

func newErr(code, msg string, exit int) *Err {
	return &Err{Code: code, Message: msg, Exit: exit}
}

// Sentinels. Compare with errors.Is; the concrete values carry the exit code.
var (
	// ErrSelfDelivery — the resolved target is the pane we are running in.
	// An agent prompting its own pane is an instant self-loop: it wakes itself,
	// answers itself, wakes itself again. This is refused by construction and
	// is never a warning.
	ErrSelfDelivery = newErr("selfDelivery",
		"refusing to deliver to our own pane — an agent prompting itself is a self-loop",
		ExitSelfDelivery)

	// ErrAgentBlocked — the target sits on a prompt a human must clear. The
	// host enforces this too (it rejects the submission before any input is
	// sent), so our job is detect-and-report, not prevent (CROSS-SESSION.md §11).
	ErrAgentBlocked = newErr("agentBlocked",
		"target is blocked and needs a human; message held, nothing was sent",
		ExitBlocked)

	// ErrTargetAbsent — no such agent, or it is done/unknown. Fall back; never
	// restart a session you did not start.
	ErrTargetAbsent = newErr("targetAbsent",
		"no such agent — fall back, do not resurrect", ExitUnavailable)

	// ErrHostUnavailable — the agent host itself is not reachable.
	ErrHostUnavailable = newErr("hostUnavailable",
		"agent host is not reachable", ExitUnavailable)

	// ErrNotInHost — we are not running inside a host pane, and the command
	// needed that context.
	ErrNotInHost = newErr("notInHost",
		"not running inside an agent host pane", ExitNotConfigured)

	// ErrNotConfigured — bad arguments, unknown id, missing configuration.
	ErrNotConfigured = newErr("notConfigured", "not configured", ExitNotConfigured)

	// ErrTimeout — a bounded wait aged out with nothing to show. Not an error.
	ErrTimeout = newErr("timeout", "timed out with nothing to deliver", ExitTimeout)

	// ErrConflict — another consumer or another delivery holds this.
	ErrConflict = newErr("conflict", "another consumer holds this", ExitConflict)

	// ErrInternal — an unexpected failure with no better category.
	ErrInternal = newErr("error", "unexpected error", ExitError)
)

// Wrap produces a copy of a sentinel carrying extra detail, still matching the
// original under errors.Is.
func Wrap(sentinel *Err, format string, args ...any) *Err {
	return &Err{
		Code:    sentinel.Code,
		Message: fmt.Sprintf(format, args...),
		Exit:    sentinel.Exit,
		wrapped: sentinel,
	}
}

// Is makes a wrapped copy match its sentinel.
func (e *Err) Is(target error) bool {
	t, ok := target.(*Err)
	return ok && t.Code == e.Code
}

// ExitCode maps any error to the process exit code it should produce.
// An untyped error is 1 — "unexpected", which is the honest answer.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var e *Err
	if errors.As(err, &e) {
		return e.Exit
	}
	return ExitError
}

// Code extracts the stable machine-readable code, or "error".
func Code(err error) string {
	var e *Err
	if errors.As(err, &e) {
		return e.Code
	}
	return "error"
}

// Retryable is implemented by an error a channel considers transient — a 429,
// a 5xx, a gateway that blinked. It is an interface rather than a sentinel so
// an adapter can keep its own error type and still tell the ingest loop "back
// off, do not hammer". RetryAfter is the server's own suggestion, or zero.
type Retryable interface {
	error
	Retryable() bool
	RetryAfter() time.Duration
}

// IsRetryable reports whether err, anywhere in its chain, is a Retryable that
// says so.
func IsRetryable(err error) bool {
	var r Retryable
	return errors.As(err, &r) && r.Retryable()
}

// RetryAfter returns the delay the error's origin suggested, or zero.
func RetryAfter(err error) time.Duration {
	var r Retryable
	if errors.As(err, &r) {
		return r.RetryAfter()
	}
	return 0
}
