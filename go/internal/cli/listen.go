package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/muthuishere/agent-conversations/go/internal/convo"
	filestore "github.com/muthuishere/agent-conversations/go/internal/store/file"
)

// Adaptive-backoff defaults, ARCHITECTURE.md §5.6 and the Node reference's
// INTERVAL_DEFAULTS, to the second. A Go build that invented its own numbers
// would make the two implementations behave differently under the same flags.
const (
	defaultActive = 15 * time.Second
	defaultMid    = 60 * time.Second
	defaultIdle   = 5 * time.Minute
	defaultIdle1  = 2 * time.Minute
	defaultIdle2  = 10 * time.Minute

	// beatEvery bounds how long the heartbeat can go unwritten, INDEPENDENT of
	// the poll interval. At the idle tier a poll is five minutes apart, and a
	// heartbeat that only ticked with the poll would read as stale for four of
	// those minutes — a healthy listener that every consumer refuses to trust.
	beatEvery = 2 * time.Second
)

// heartbeat is `heartbeat.<tag>.json` of INTERFACES.md §2.5 — the only cure for
// silent deafness. Never report "listening" from memory; read this file.
type heartbeat struct {
	TS            string         `json:"ts"`
	StartedAt     string         `json:"startedAt"`
	PollCount     int            `json:"pollCount"`
	LastMessageAt *string        `json:"lastMessageAt"`
	IntervalMS    int64          `json:"intervalMs"`
	Conversations int            `json:"conversations"`
	Identity      convo.Identity `json:"identity"`
	PID           int            `json:"pid"`
	LastError     string         `json:"lastError,omitempty"`
}

func heartbeatPath(st *filestore.Store) string {
	return filepath.Join(st.Home, "heartbeat."+st.Tag+".json")
}
func listenerPIDPath(st *filestore.Store) string {
	return filepath.Join(st.Home, "listener."+st.Tag+".pid")
}
func listenerLogPath(st *filestore.Store) string {
	return filepath.Join(st.Home, "listener."+st.Tag+".log")
}

// write is tmp-then-rename, so a concurrent reader sees the old heartbeat or
// the new one and never a half-written one. A torn heartbeat parses as absent,
// which reads as a dead listener — a false alarm is worse than no alarm.
func (h heartbeat) write(path string) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// claimListenerLock enforces one listener per tag.
//
// Two listeners on one tag both write the fetch cursor, so each advances past
// messages the other never journalled, and the overlap is lost for good. The
// lock is a pid file: a pid that is still alive is a conflict (exit 66); a pid
// that is not is stale and is claimed silently, because a crashed listener must
// not need a human to clean up after it before the system can run again.
func claimListenerLock(st *filestore.Store) (release func(), err error) {
	path := listenerPIDPath(st)
	if b, rerr := os.ReadFile(path); rerr == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && pid > 0 && pid != os.Getpid() {
			if alive := syscall.Kill(pid, 0) == nil; alive {
				return nil, convo.Wrap(convo.ErrConflict,
					"listener pid %d already holds tag %q", pid, st.Tag)
			}
		}
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return nil, convo.Wrap(convo.ErrNotConfigured, "cannot write %s: %v", path, err)
	}
	return func() { _ = os.Remove(path) }, nil
}

// logLine appends one `[iso] message` line to the listener log. The log is how
// a human watches a running listener without a dashboard, and it is plain text
// on purpose: `tail -f` is the interface.
func logLine(st *filestore.Store, format string, args ...any) {
	f, err := os.OpenFile(listenerLogPath(st), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

// cmdListen runs the ingest loop IN THIS PROCESS, and that placement is the
// entire point of the command.
//
// The alternative — an agent session running its own poll loop — burns a model
// turn per poll, stops the moment the agent is doing anything else, and dies
// with the terminal. A loop belongs in a program. The agent is woken by what
// this loop finds; it never watches for it.
//
// Everything else here exists to keep the loop honest: a heartbeat so silence
// can be told from deafness, a single-instance lock so two loops cannot shred
// one cursor, and backoff tiers so a quiet channel costs nearly nothing.
func (a *App) cmdListen(ctx context.Context, o options) error {
	ch, err := a.channel(o)
	if err != nil {
		return err
	}
	st, err := a.store(o)
	if err != nil {
		return err
	}

	// --once is one pass, and takes no lock beyond the pass itself: it is the
	// unit a cron entry or a test runs, and identical in effect to `fetch`.
	if o.once {
		release, err := claimListenerLock(st)
		if err != nil {
			return err
		}
		defer release()
		p, ferr := a.fetchOnce(ctx, o, ch, st)
		a.beat(st, o, p, time.Now(), time.Now(), 1, nil, ferr)
		logLine(st, "once: ingested %d from %d conversations", p.Ingested, p.Conversations)
		if ferr != nil {
			return ferr
		}
		return a.print(o, fmt.Sprintf("ingested %d from %d conversations", p.Ingested, p.Conversations), p)
	}

	release, err := claimListenerLock(st)
	if err != nil {
		return err
	}
	defer release()

	// Ctrl-C and SIGTERM must unwind through the same path as a clean exit, so
	// the lock and the final heartbeat are not left behind for the next run to
	// puzzle over.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	tiers := resolveTiers(o)
	startedAt := time.Now()
	var lastMessageAt *time.Time
	pollCount := 0
	total := 0

	logLine(st, "listening: channel=%s tag=%s active=%s mid=%s idle=%s",
		o.channel, st.Tag, tiers.active, tiers.mid, tiers.idle)

	for {
		p, ferr := a.fetchOnce(ctx, o, ch, st)
		pollCount++
		total += p.Ingested
		if p.Ingested > 0 {
			now := time.Now()
			lastMessageAt = &now
			logLine(st, "poll %d: ingested %d", pollCount, p.Ingested)
		}
		if ferr != nil {
			// A failing poll is LOGGED AND SURVIVED, never fatal. Losing the
			// listener because one request 500'd is exactly the outage this
			// design exists to prevent; the heartbeat below carries the error
			// so nobody mistakes a broken transport for a quiet one.
			logLine(st, "poll %d failed: %s", pollCount, errMessage(ferr))
		}

		interval := tiers.pick(startedAt, lastMessageAt, time.Now())
		a.beat(st, o, p, startedAt, time.Now(), pollCount, lastMessageAt, ferr)
		if o.asJSON {
			// stdout carries data only, so the per-poll status goes out as one
			// JSON object per line — parseable, not prose.
			enc := json.NewEncoder(a.Stdout)
			_ = enc.Encode(map[string]any{
				"poll": pollCount, "ingested": p.Ingested,
				"conversations": p.Conversations, "intervalMs": interval.Milliseconds(),
				"error": errString(ferr),
			})
		}

		if err := a.sleepBeating(ctx, st, o, interval, p, startedAt, pollCount, lastMessageAt, ferr); err != nil {
			logLine(st, "stopping: %d polls, %d messages ingested", pollCount, total)
			return a.print(o, fmt.Sprintf("stopped after %d polls, %d ingested", pollCount, total),
				map[string]any{"polls": pollCount, "ingested": total})
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return errMessage(err)
}

// beat writes the heartbeat. It is called on every poll AND on every 2s tick of
// the sleep, because the heartbeat is a statement about the PROCESS, not about
// the channel: "I am alive and this is when I last looked".
func (a *App) beat(st *filestore.Store, o options, p pass, startedAt, now time.Time,
	pollCount int, lastMessageAt *time.Time, ferr error) {
	h := heartbeat{
		TS:            now.UTC().Format(time.RFC3339Nano),
		StartedAt:     startedAt.UTC().Format(time.RFC3339Nano),
		PollCount:     pollCount,
		IntervalMS:    resolveTiers(o).pick(startedAt, lastMessageAt, now).Milliseconds(),
		Conversations: p.Conversations,
		Identity:      p.Identity,
		PID:           os.Getpid(),
		LastError:     errString(ferr),
	}
	if lastMessageAt != nil {
		s := lastMessageAt.UTC().Format(time.RFC3339Nano)
		h.LastMessageAt = &s
	}
	_ = h.write(heartbeatPath(st))
}

// sleepBeating waits out the poll interval in short steps, writing a heartbeat
// on each one, and returns a non-nil error when the context is done.
func (a *App) sleepBeating(ctx context.Context, st *filestore.Store, o options, d time.Duration,
	p pass, startedAt time.Time, pollCount int, lastMessageAt *time.Time, ferr error) error {
	deadline := time.Now().Add(d)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		step := beatEvery
		if remaining < step {
			step = remaining
		}
		t := time.NewTimer(step)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
			a.beat(st, o, p, startedAt, time.Now(), pollCount, lastMessageAt, ferr)
		}
	}
}

// tiers is the adaptive backoff of ARCHITECTURE.md §5.6.
type tiers struct {
	active, mid, idle time.Duration
	idle1, idle2      time.Duration
}

func resolveTiers(o options) tiers {
	t := tiers{active: o.pollActive, mid: o.pollMid, idle: o.pollIdle, idle1: o.idle1, idle2: o.idle2}
	if t.active <= 0 {
		t.active = defaultActive
	}
	if t.idle < t.active {
		t.idle = max(t.active, defaultIdle)
	}
	if t.mid < t.active || t.mid > t.idle {
		t.mid = min(max(t.active, defaultMid), t.idle)
	}
	if t.idle1 <= 0 {
		t.idle1 = defaultIdle1
	}
	if t.idle2 <= 0 {
		t.idle2 = defaultIdle2
	}
	return t
}

// pick chooses the next interval from how long it has been quiet.
//
// The snap-back is implicit and that is the nice part: any traffic moves
// lastMessageAt to now, so `quiet` collapses to zero and the very next interval
// is `active` again. There is no separate "wake up" path to forget to call.
//
// The known trade, stated out loud: at the idle tier you cannot notice traffic
// sooner than `idle`. The first message after a long silence pays full idle
// latency — precisely when a human is opening a conversation. Lower --poll-idle
// while someone is attached if that matters.
func (t tiers) pick(startedAt time.Time, lastMessageAt *time.Time, now time.Time) time.Duration {
	since := startedAt
	if lastMessageAt != nil {
		since = *lastMessageAt
	}
	quiet := now.Sub(since)
	switch {
	case quiet >= t.idle2:
		return t.idle
	case quiet >= t.idle1:
		return t.mid
	default:
		return t.active
	}
}
