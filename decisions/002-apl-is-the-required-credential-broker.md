# 002 — apl is the required credential broker

**Status:** accepted · **Decider:** project owner · **Reversibility:** two-way door
(the transport is one seam; a channel can fall back to a raw token if it must)

## Context
Every channel needs credentials: Graph tokens for Teams, a paired device for WhatsApp,
OAuth for Google. `apl` already holds all of them as named handles and exposes two verbs —
`apl call <handle> <METHOD> <url>` for HTTP and `apl with <handle> -- <cmd>` for CLIs.
The alternative is each channel adapter managing its own secret.

## Options

### A. Each channel handles its own credential
Self-contained; no broker to install. **Against:** the token exists as a string in the
process, and therefore in flags, env, `ps`, logs and shell history. Every adapter re-solves
refresh, scopes and expiry. Adding a channel means adding an auth implementation.

### B. apl as the required broker
The process **never sees a credential**: apl mints and injects it at the point of the call.
Adding a channel means writing the API mapping only. A missing scope fails loudly with the
exact `apl login … --scope …` line to fix it. **Against:** one more required install; apl's
verbs shape what a channel can do (HTTP or a wrapped CLI — nothing else).

### C. apl optional, raw token as fallback
Flexible. **Against:** two auth paths to test and document, and the insecure one is the one
people copy from examples.

## The axis that dominates
**Whether a secret can ever appear in a place it shouldn't.** Only B makes that structurally
impossible rather than a matter of discipline.

## Decision
**B.** Channels authenticate through apl handles. The raw-token flag is kept only for the
local simulator, is documented as such, and is not to be used against a real tenant.

**Accepting:** apl is a prerequisite alongside Herdr, and a channel that apl cannot front
(no HTTP, no CLI) cannot be built here.
**Not deciding:** anything about apl's own internals or how handles are provisioned.

## What would change our mind
- A channel arrives that apl cannot front and that we genuinely need → add a second broker
  seam, not a raw-token path.
- apl gains a streaming/webhook verb → revisit push transports.
