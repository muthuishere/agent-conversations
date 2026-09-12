# 001 — Herdr is the required agent host

**Status:** accepted · **Decider:** project owner · **Reversibility:** one-way door in practice
(the host is behind an interface, but every operator workflow, doc and test assumes it)

## Context
An agent cannot be pushed to; something must invoke it. Four mechanisms were built and
measured before choosing: a background task exiting and re-invoking the session, a daemon
spawning a fresh agent per message, keystroke injection into a live terminal, and a stop-hook
long-poll. Each worked. Each had a defect the others didn't. Supporting all four meant four
code paths, four failure modes, and an operator who had to know which applied to which runtime.

## Options

### A. Support every mechanism, pick per runtime
Maximum reach. **Against:** four delivery paths to keep working; the failure that recurred
throughout the spike — a listener that looks healthy while the wake path is severed — was hit
in *four distinct ways*, one per mechanism.

### B. Spawn a fresh agent per message (the portable floor)
Works everywhere, survives session death. **Against:** cold start on every message, and no
way to reach a session a human is already working in.

### C. Herdr as the single required host
Typed agent states (`idle/working/blocked/done`), stable addressing by name, the pane id
exported into the environment so an agent can tell which one it *is*. **Against:** a hard
third-party dependency and a single point of failure for delivery.

### Rejected early
- Raw `tmux send-keys` — no state; cannot tell "finished" from "parked on a permission prompt".

## The axis that dominates
**Whether a delivery can silently fail.** Only C exposes `blocked`, and only C refuses to
deliver into it. Every other option can report success on a message nobody will ever read.

## Decision
**C.** Delivery into an agent is always through Herdr. The `Host` interface stays — it is
what lets the system be tested with a fake host binary and nothing running — but the product
ships one host.

**Accepting:** a hard dependency, and that nothing works without a Herdr server up.
**Not deciding:** whether Herdr is the right *terminal* for humans; only that it is the host.

## What would change our mind
- Herdr stops exporting `HERDR_PANE_ID` / `HERDR_SOCKET_PATH`, or drops typed states → revisit.
- An agent runtime ships a documented, stable push-into-live-session API → B's objection
  disappears and this becomes a choice rather than a necessity.
