# 004 — One binary, one daemon, three commands an operator needs

**Status:** accepted · **Decider:** project owner · **Reversibility:** two-way door

## Context
The spike produced a great deal: a mock, a Node CLI, a Go CLI, a polling responder, a
session router, a supervisor, a cross-session injector, four wake mechanisms and their
comparison. Most of it was necessary to *learn* what to build. Very little of it is what a
teammate should have to understand to *use* it. A system that needs its author present is
not finished.

## Options

### A. Ship everything; document the map
Complete. **Against:** the map is the problem. A new teammate has to choose between the Node
reference, the Go CLI, three wake modes and two hosts before sending one message.

### B. One binary, one daemon, three commands
`convo listen` (the daemon), `convo next` (what's for me), `convo respond <id>` (answer it).
Everything else is either internal to those three or a diagnostic (`self`, `journal`,
`host list`). **Against:** some capability becomes less visible; the research artefacts
become history rather than product.

### C. A framework with plugins for everything
Maximum flexibility. **Against:** it is the thing nobody wanted to build the first time.

## The axis that dominates
**Time for a teammate to send their first reply through the system, unaided.** B is under
ten minutes; A is an afternoon of reading.

## Decision
**B.** The operator surface is three commands plus `self` for orientation. Prerequisites
are exactly two installs: Herdr (001) and apl (002). The Node reference implementation,
the polling responder and the wake-mechanism research stay in the repo as **evidence and
history**, clearly labelled, and are not the recommended path.

**Accepting:** less visible surface; anyone who needs the research has to look for it.
**Not deciding:** the internal package layout, which is free to change behind the three verbs.

## What would change our mind
- A teammate needs a fourth command in their first week → it was a real gap; promote it.
- The three commands need flags a teammate can't remember → the surface is wrong, not the docs.
