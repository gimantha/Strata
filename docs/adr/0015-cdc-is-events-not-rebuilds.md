# ADR 0015: A row change is an event, and a deleted row is a retraction

Status: accepted
Date: 2026-08-24

## Context

Phase 10 asks for a generic CDC contract, a reference adapter, source checkpoints, tombstone
semantics, and late-event handling — with two acceptance criteria: row updates must not
require a graph rebuild, and replay from a checkpoint must be idempotent.

The tempting implementation of "keep the graph in sync with a table" is to delete everything
derived from a row and re-derive it whenever the row changes. It is easy, it is obviously
correct on the surface, and it destroys the thing the ledger exists for: the assertion that
recorded a customer's tier last March would be deleted and replaced by an identical-looking
one, and every reference to it — evidence, conflict sets, derivations, a context block
someone cited in a report — would point at a claim that no longer exists.

## Decision

**A change is a source event, and knowledge is derived, never rebuilt.** The connector
records what the upstream said; the pipeline states what the row now says. Claims whose
values did not move collide on their fingerprint and are left exactly as they were, down to
the assertion id. Claims whose values did move are superseded, keeping their history. A
hundred unrelated column edits leave a subject's other facts untouched, and the test measures
that by assertion identity rather than by counting rows — counting would pass even if the
whole subgraph were rewritten.

**Mapping is deterministic, with no model involved.** A database row is already structured:
the columns say what the predicates are and the primary key says what the subject is. Asking
a model to rediscover that would be slower, more expensive, and less reliable than reading a
mapping — the same argument the phase 1 stages make about conversation turns and markdown
headings. It also means CDC works with no model provider configured at all.

**A mapped column is functional by default.** That is what a column is: one current value.
Declaring it makes an update supersede rather than accumulate, and registering the mapping
writes those semantics into the predicate registry rather than leaving them to be guessed at
first use.

**A source correcting itself is not a conflict.** This was a defect CDC exposed in the phase 5
reconciler. Staleness was handled in one direction — an older claim from a source loses to a
newer one — but the symmetric case was not, so an ordinary column update between two values
the source never held simultaneously was recorded as a contradiction. Now, when the same
source's own ordering puts a competing claim earlier, the new claim supersedes it directly:
no policy, no authority weighting, no conflict set. Disagreement between *different* sources
still goes to authority resolution, which refuses to pick between equals (ADR 0006).

**A tombstone retracts; it does not erase.** A deleted row means the source stopped claiming
the record, not that the record was never true (AGENTS.md section 11.3). The claims keep
their evidence and stay answerable as of any earlier knowledge time. Privacy erasure is a
different workflow with different authorization, and conflating the two would make "delete
this row" a data-destruction primitive that any upstream could fire.

**A record's changes are applied one at a time.** Deciding whether a column changed means
reading what is believed and then writing, and workers claim outbox items independently — so
two changes to one row were being applied concurrently, each seeing the state before the
other, and each asserting the same unchanged columns. An advisory lock per record serializes
them. Per record, not per stream: rows are independent, and serializing a whole table would
make a busy stream single-threaded for no reason.

**Checkpoints advance after events are durable, never before.** A crash between accepting an
event and saving the checkpoint replays that event, and replaying is free: the gateway keys
on the change's idempotency key and returns the original. The opposite ordering would silently
lose changes, which is the one outcome a CDC pipeline cannot recover from. Idempotency comes
from the keys, not from the bookmark — which is why replaying a whole log with checkpointing
disabled also changes nothing, and why rebuilding a workspace from an archive works.

**The reference adapter is a JSONL change log.** AGENTS.md allows an adapter or a fixture,
and the fixture is the more useful of the two to build first. A live adapter needs a
replication slot, a running upstream, and a network; a change log is a file, which means the
ordering, tombstone, and replay behavior everything else depends on can be tested exactly and
reproduced from a bug report. Every real CDC pipeline can export to it, so "replay the
customer's change log" is a supported way to reproduce a problem rather than an exercise.

**World validity comes from the row's columns, or from nowhere.** An earlier version stamped
the commit time as `valid_from`, which is inventing information: a row touched in February
does not mean a company's legal name became true in February. It also broke deduplication,
because the same unchanged value fingerprinted differently on every touch. Knowledge time
already records when we learned something; world time is only what the source states.

## Consequences

The archived payload is the whole change envelope — before image, transaction id, offset,
schema version — because that is what makes a disputed fact explicable a year later. None of
it belongs in a search index, so the normalizer renders the row itself as `column: value`
lines and only that reaches the projections. A projected chunk reads like the record rather
than like the plumbing that carried it.

Changes that arrive before anyone has written a mapping are still ingested and archived; only
the interpretation is missing. Registering a mapping later and re-running the stage picks
them up. Failing the ingest instead would discard data nobody can re-fetch, to avoid a
configuration gap that takes a minute to close.

A stream's mapping and its checkpoint live in one row because they are the same unit of
configuration. Re-registering a mapping deliberately does not touch the checkpoint: changing
how rows are interpreted is a different decision from deciding where to read from.

## Alternatives considered

**Delete and re-derive the subject's subgraph per change.** Simple, and it forfeits assertion
identity, supersession history, evidence links, and every external reference to a claim. The
acceptance criterion exists to forbid exactly this.

**Let a model read the row.** Costs a provider call per row, produces nondeterministic
predicates from deterministic data, and makes replay non-reproducible.

**Checkpoint before processing.** Faster to write and loses events on any crash. At-least-once
delivery plus an idempotent consumer is the standard answer, and this system already had the
idempotent consumer.

**Treat a delete as a hard delete.** Matches the naive reading of "delete" and destroys
history the ledger is built to keep. Retention and privacy erasure need their own workflow
with their own authorization, which is where that operation belongs.
