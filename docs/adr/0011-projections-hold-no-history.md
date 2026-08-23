# ADR 0011: Projections hold no history and are rebuilt through the normal path

Status: accepted
Date: 2026-08-23

## Context

AGENTS.md section 15.2 requires that derived stores be reconstructable exclusively from
canonical records, and scenario I demands that dropping every projection and replaying
produce equivalent retrieval results.

That is easy to claim and easy to lose. A projection accumulates conveniences — a
denormalized field nobody rewrites, a counter incremented in place, a record kept because
deleting it seemed wasteful — and one day the rebuild produces something subtly different
from what incremental updates produced. Nobody notices until a rebuild is needed, which is
exactly when it must work.

## Decision

**Projections carry no state of their own.** Every column is either a pointer to a canonical
record or a copy of a canonical field used for pre-filtering. There is no history, no
provenance, no counter, and nothing that could not be recomputed.

**A rebuild is the normal path run over everything.** `Rebuild` deletes the workspace's
projections and then calls the same `ProjectEvent` that the pipeline stage calls, event by
event. There is no separate bulk implementation to drift from the incremental one.

**Filter fields are duplicated deliberately.** Validity interval, status, classification, and
memory kind are copied onto each projected record so retrieval can narrow before ranking.
Joining back to the ledger for every candidate would defeat the point of having an index.
The duplication is safe precisely because the projection is disposable: a stale copy is
fixed by rebuilding, not by a repair script.

## Alternatives

- **Incremental invalidation with a separate bulk loader.** Faster to write, and it is the
  arrangement that produces divergence: two code paths that must agree, exercised at very
  different frequencies.
- **Projecting only current belief.** Rejected. Retrieval filters by knowledge time, so a
  projection holding only what is true now could not answer what was believed last month —
  which is a large part of why the temporal model exists.
- **Views or materialized views.** Attractive for the guarantee, rejected for the mechanics:
  vector and trigram indexes over a materialized view are refreshed wholesale, and the
  embedding step is not expressible in SQL at all.
- **Keeping provenance in the projection.** Rejected. Provenance belongs to the ledger; a
  second copy is a second thing to keep correct, and the projection can always point at the
  first.

## Consequences

A rebuild costs a full re-embed of the workspace, which is real money against a hosted
provider. Incremental re-projection mitigates it by comparing content hashes and skipping
unchanged text, but a deliberate rebuild after deletion pays in full. That is the honest
price of the guarantee, and it is why the embedding model and version are stored per record:
a model change can be rolled out as a second set of vectors alongside the first rather than
as a stop-the-world migration.

Because projections are disposable, they are also safe to drop under pressure. A workspace
with a corrupt index is repaired by deletion, and losing the projection tables entirely
costs time rather than knowledge.

## Migration impact

None to canonical data, by construction. Changing what gets projected means bumping
`ProjectStage.Version()`, which re-runs the stage for events that already passed the earlier
version, or running a rebuild.
