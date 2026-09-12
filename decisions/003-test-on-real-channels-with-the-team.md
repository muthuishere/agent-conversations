# 003 — Test on real channels, with the real team

**Status:** accepted · **Decider:** project owner · **Reversibility:** two-way door

## Context
Everything so far was proven against a local simulator: hundreds of assertions, all green,
and not one against a live tenant. The simulator caught real bugs — a URL-encoding fault that
made a listener silently deaf, a branched transcript that ate a turn — but the highest-value
findings of the whole spike came from *running the real thing*: documentation that contradicted
itself, a `--help` that described flags that didn't exist, a flag that only worked in one order.
Reading never found those. Running did.

The team is four people on the same channels. That is a test bed with a known, bounded
blast radius.

## Options

### A. Mock-first; real channels only after sign-off
Safe. **Against:** the class of bug we most need to find — the difference between what a
system is documented to do and what it does — is invisible in a mock by construction.

### B. Real channels with the team from the start; the mock is for CI
Finds the real bugs. Breakage lands on people who know it's an experiment.
**Against:** a bad send goes to a real person; a runaway loop is a real nuisance.

### C. Real channels, but a dedicated test group nobody reads
Safer than B. **Against:** nobody reads it, so nobody notices the reply that vanished — which
is exactly the failure that matters.

## The axis that dominates
**Whether a test can detect "looks delivered, wasn't".** Only a human on the other end can.

## Decision
**B.** Test against the team's real channels. Breaking something there is the point; it is
how the system learns. The simulator remains the regression suite for CI.

Rules that keep this from being reckless:
- Sends go to the team's own channels; anything reaching outside the team still gets a
  human check first.
- Self-echo suppression and the per-conversation lock are verified in the simulator **before**
  a real session is attached — those two are what stop a loop.
- Every real run is journaled; the journal is how we reconstruct what happened when it breaks.

**Accepting:** the team will occasionally be spammed by a bot under test.
**Not deciding:** anything about external users or production traffic.

## What would change our mind
- A run reaches someone outside the team → tighten to a dedicated group until the cause is fixed.
- The team stops reading the channel → the bed is no longer live; find another.
