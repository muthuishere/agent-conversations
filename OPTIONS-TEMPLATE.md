# Writing a decision doc with options

A method and a template for the document you write when there is a real choice to make and
someone has to live with it. Works for architecture, vendors, process, hiring shapes —
anything where "it depends" is true but unhelpful.

The job of this document is **not** to survey the space. It is to make a decision legible:
what was chosen, what it cost, and what would overturn it.

---

## 1. Frame the decision before listing anything

Most bad options docs are bad before the options start, because the decision was never
pinned down. Answer these first, in the doc, in under ten lines:

| | |
|---|---|
| **The decision** | One sentence, phrased as a choice. Not "investigate X" — *"which X do we use for Y"*. |
| **Forcing function** | Why now? What is blocked until this is decided? If nothing is blocked, you may not need this document. |
| **Decider** | A name. "The team" decides nothing. |
| **Deadline** | A date. Options docs rot; a stale one gets re-litigated from scratch. |
| **Reversibility** | One-way or two-way door? This sets how much rigour is warranted. |

**Reversibility is the highest-leverage line in the document.** A two-way door deserves a
fast decision and a short doc — the cost of being wrong is one afternoon. A one-way door
(a data model, a public API, a vendor with an export problem) deserves the full treatment.
Spending a week on a two-way door is itself a bad decision.

---

## 2. Enumerate options that are actually real

Rules, in order of how often they are broken:

1. **Include "do nothing" / "keep the status quo".** It is always available and often wins.
   If it can't win, say explicitly why — that argument *is* your forcing function.
2. **Steel-man every option, especially the one you dislike.** Write each as if its
   strongest advocate wrote it. A reader who can tell which one you prefer from the *framing*
   rather than the *evidence* will stop trusting the document.
3. **No strawmen.** If an option exists only to be dismissed, delete it. Three genuine
   options beat five where two are padding.
4. **Name the option you rejected early and why.** Readers will think of it; pre-empt the
   "did you consider…" round trip. A one-line "rejected: reason" list is enough.
5. **Options must be mutually exclusive** — or you're deciding several things at once.
   If two options can both be true, you have two decisions. Split the document.
6. **Watch for the false binary.** "Build or buy" usually hides "buy now, build later",
   "build the 20% that differentiates", "buy and wrap". The hybrid is frequently correct
   and is frequently missing.

**Test:** if a reasonable colleague would add an option you haven't listed, the list is
incomplete. Ask one before you write the recommendation, not after.

---

## 3. Choose axes that discriminate

The comparison table is where these documents usually go wrong. Two failure modes:

- **Axes where every option scores the same.** They feel thorough and carry zero
  information. Delete them.
- **Axes nobody will act on.** If you would not change the decision based on a row, it is
  decoration.

Good axes discriminate *and* map to something someone cares about. Typical ones:

| axis | ask |
|---|---|
| Cost | Not just money — engineering time, ongoing attention, cognitive load |
| Time to first value | When does anyone benefit? |
| Risk | What is the failure mode, how likely, how bad |
| Reversibility | Cost of backing out *of this option specifically* |
| Operational burden | Who carries the pager, and for what |
| Fit to existing stack | What we already run and already know |
| Ceiling | Does it stop working at 10×? |

**Do not score by counting ticks.** A table with five axes where option A wins three is not
an argument for A — the axes are not equally weighted, and pretending they are hides the
actual judgement. Say which one or two axes dominate, and why. That sentence is usually the
real content of the entire document.

---

## 4. Separate what you measured from what you assume

Mark every material claim:

- **Measured** — you ran it. Give the number and the conditions.
- **Reported** — a doc or a vendor says so. Link it.
- **Assumed** — you believe it. Say so plainly.

This matters more than it sounds. In one recent decision, official documentation
contradicted itself on whether a mechanism worked at all; the question was only settled by a
ten-minute experiment. **If a claim is load-bearing and cheap to test, test it** — and if you
didn't, label it `assumed` so the reader can weigh it properly.

An options doc where everything is asserted with equal confidence is a document that cannot
be checked, and therefore cannot be trusted.

---

## 5. Land the recommendation

- **Recommend exactly one option.** A document that presents three and recommends none has
  moved the work, not done it. If you genuinely cannot choose, the blocker is a missing fact
  — name that fact and what it would take to get it. That is a finding, not a failure.
- **Give the reason in one sentence**, tied to the dominant axis. "B, because operational
  burden dominates here and B is the only one we can run without adding a pager rotation."
- **State the cost you are accepting.** Every choice loses something. A recommendation with
  no acknowledged downside reads as advocacy and invites someone to go looking for the
  catch you hid.
- **Say what you're not deciding.** A scope fence stops the document sprawling and stops the
  decision being blamed for things it never covered.

---

## 6. What would change our mind

The section that separates a decision doc from an argument. List the **falsifiable triggers**
that would reopen this:

> - If throughput exceeds ~5k/day, option B's ceiling becomes the binding constraint — revisit.
> - If the vendor's export API is still missing in 6 months, the lock-in risk we accepted is real — revisit.
> - If more than two people need to operate this, the ops burden we waved off stops being cheap — revisit.

Each trigger should be **observable** — a number, a date, an event — not a vibe. This turns
a decision into something that can be revisited on evidence rather than on whoever complains
loudest, and it is what makes the doc still useful a year later.

---

## 7. Anti-patterns

| Anti-pattern | What it looks like | Fix |
|---|---|---|
| **Strawman option** | An option nobody would pick, present to make the favourite look good | Delete it, or steel-man it |
| **False binary** | "Build or buy" with no hybrid | Look for the 80/20 split |
| **Tick-counting** | "A wins 3 of 5 axes" | Name the dominant axis |
| **Hidden default** | The doc is written to justify a decision already made | Write the options before the recommendation |
| **Everything asserted** | No distinction between measured and assumed | Label each claim |
| **No recommendation** | Survey with a shrug | Choose, or name the missing fact |
| **Unfalsifiable** | "We'll revisit if it becomes a problem" | Give a number and a date |
| **Sunk cost** | "We already built half of A" | Sunk cost is not an axis; today's cost-to-finish is |
| **Analysis paralysis on a two-way door** | A week of work on a reversible call | Decide in an hour; the doc is one paragraph |

---

## 8. The template

```markdown
# Decision: <the choice, phrased as a choice>

**Status:** proposed | accepted | superseded by <link>
**Decider:** <name>   **Date:** <date>   **Deadline:** <date>
**Reversibility:** two-way door (cheap to undo) | one-way door (expensive)

## Context
What is true today, and what is blocked until this is decided. Three sentences.
No history lessons — only what bears on the choice.

## Options

### A. <name>
<Steel-manned description.>
- **For:** …
- **Against:** …
- **Cost:** <time / money / ongoing attention>
- **Evidence:** measured: … | reported: <link> | assumed: …

### B. <name>
…

### C. Do nothing
<Always present. Why it does or doesn't win.>

### Rejected early
- <option> — <one-line reason>

## Comparison

| | A | B | C |
|---|---|---|---|
| <dominant axis> | | | |
| <second axis> | | | |
| Reversibility | | | |

**The axis that dominates:** <which, and why>.

## Recommendation
**<Option>**, because <one sentence tied to the dominant axis>.

**What we're accepting:** <the real cost of this choice>.
**What we're not deciding:** <scope fence>.

## What would change our mind
- <observable trigger> → revisit
- <observable trigger> → revisit

## Open questions
- <fact we lack, and what it would take to get it>
```

---

## 9. Worked example

A decision from a real project, compressed to show the shape:

> **Decision:** how does an external message reach a running agent?
> **Reversibility:** two-way door (the transport is behind one interface).
>
> **Options:** (A) poll from inside the agent — a turn per tick; (B) blocked process that
> exits and re-invokes the session; (C) an always-on daemon that spawns a fresh agent per
> message; (D) inject into a live terminal session.
>
> **Dominant axes:** idle cost, and portability across runtimes.
>
> **Evidence:** B *measured* at 0.31s pickup and zero idle cost — but only on one runtime,
> and its own docs contradicted each other, so it was tested rather than trusted. C *measured*
> as the only one working on every runtime tried. A *measured* at ~120 turns per idle hour.
>
> **Recommendation:** C as the floor, B as an optimisation where the runtime supports it.
> **Accepting:** C pays a cold start per message, and loses conversational context unless the
> spawn resumes an existing session.
> **What would change our mind:** if a runtime ships a documented, stable push into a live
> session, B's portability objection disappears and it becomes the default.

Note what makes it work: an option was **disqualified on a measurement**, the recommendation
**names its own cost**, and the reopening trigger is **observable** rather than aspirational.
