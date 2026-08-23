# ADR 0010: Reconciliation follows source order, never arrival order

Status: accepted
Date: 2026-08-23
Supersedes the deferral in [ADR 0006](0006-conflicts-recorded-not-resolved.md) for the
`highest_authority` policy.

## Context

AGENTS.md section 11.4 is a single sentence with wide consequences: "Never assume
arrival/recorded order equals event order." Scenario B in section 37 makes it concrete -
receive source sequence 102 before 101, and converge correctly once 101 arrives.

This matters more than it first appears. A CDC stream can deliver updates out of order after
a partition heals. A webhook retries an hour late. A backfill replays a year of history in
minutes, with every event arriving "now". In each case the order things were recorded says
nothing about the order they happened, and a reconciler that assumes otherwise will let a
stale update overwrite current truth - silently, because nothing about it looks like an
error.

ADR 0006 handled the unambiguous cases and deferred authority-weighted resolution to this
phase. That deferral ends here.

## Decision

**Ordering comes before policy.** Before any conflict policy runs, the reconciler asks
whether the source's own ordering already places this claim behind one it knows about. If it
does, the new claim is superseded on arrival: recorded, because it is what the source said,
but never current belief. No policy can override this, however authoritative the source of
the late claim.

Order is read from the strongest evidence the source supplied, in this order:

1. **sequence or LSN** — the source's own monotonic counter, definitive;
2. **version**;
3. **commit time**;
4. **source time**, weakest, because clocks skew and records share timestamps.

Sequences compare numerically when both parse as integers, so `9` precedes `10`, using
arbitrary precision so a 64-bit counter cannot overflow into a wrong answer. Non-numeric
markers such as `0/16B3748` fall back to lexicographic comparison, which is what those
formats are designed for.

Two positions from **different sources** are never ordered against each other. They are not
on one timeline, and comparing them would be numerology.

When nothing is comparable the answer is `OrderUnknown` — concurrent, not equal — and the
claims fall through to policy.

**Policy then applies**, per predicate:

| Policy | Behavior |
|---|---|
| `coexist` | Nothing to reconcile |
| `latest_wins` | The newer claim supersedes what it replaces |
| `highest_authority` | The more trusted source wins; **equal trust is a conflict, not a coin flip** |
| `manual` | Both kept, disagreement recorded |

## Alternatives

- **Ordering by `recorded_at`.** The status quo in most systems and simply wrong under
  replay, retry, or backfill.
- **Ordering by world time (`valid_from`).** Rejected: world time says when a fact held, not
  when the source learned it. A correction issued today about last year has an old world
  time and a new source position, and treating it as stale would discard exactly the
  corrections that matter most.
- **Rejecting late events.** Rejected. The late event is real data; refusing it loses what
  the source said and leaves a gap in the record.
- **Letting the highest-authority source win regardless of order.** Rejected. An
  authoritative source's *old* value is still old. Authority decides between disagreeing
  sources, not between two states of one record.
- **Breaking equal-authority ties by recency or confidence.** Rejected for the reason ADR
  0006 gives: an arbitrary winner is an invisible wrong answer, where a conflict set is a
  visible question.

## Consequences

A late claim occupies a knowledge-time window that opens and closes at the same instant. It
was recorded, it was never believed, and an as-of query at any time returns the same answer
it would have without it. That is what "converge correctly" means here: delivery order
cannot be observed in the result.

Sources that emit no ordering information get no ordering guarantees, and their overlapping
updates become conflicts rather than supersessions. This is the correct outcome and also a
useful pressure: it makes the value of passing through a sequence number visible.

Authority resolution depends on trust levels being set thoughtfully. A source registered at
the default `standard` alongside another at `standard` will produce conflict sets rather
than resolutions - correct, but it means trust configuration is now operationally
meaningful rather than decorative.

## Migration impact

None to the schema: source sequence, version, commit time, and source time were already
columns on both source events and assertions from phase 1. What changed is that the
reconciler now reads them.

A bug surfaced while implementing this and is worth recording. Policy-driven supersession
was applied by re-committing the superseding claim with a `SupersedesIDs` list. The claim
already existed, so the commit collided on its fingerprint, was correctly treated as a
replay, and skipped the supersession along with it. The path had no test until phase 5
exercised `latest_wins`. Supersession is now applied directly rather than through a commit.
