# ADR 0026 — What grows forever, and what must never be deleted

Status: accepted
Date: 2026-09-01
Phase: 16 (operability), issue #3

## Context

Nothing in this system deleted anything. Every table was append-only in the strict sense —
no `DELETE` existed for any of them — and the tables that grew with *traffic* rather than
with knowledge grew without bound: an outbox row per event, an audit row per action, a
pipeline run per document, and a retrieval trace per query. A deployment serving a million
queries a day accumulated a million trace rows a day, none of which described anything
anybody knew.

This surfaced while auditing issue #3, which was filed against a more abstract problem: the
ledger is one PostgreSQL and cannot be sharded. That remains true and remains a deliberate
design choice — a single transaction covering the assertion, the outbox row and the
checkpoint is what makes the temporal guarantees cheap. But it is not the thing a deployment
hits first. Unbounded operational growth is.

## Decision

**The tables divide in two, and the division is the whole design.**

*Records of what the system did* may be pruned: `retrieval_traces`, `audit_events`,
`pipeline_runs` and their stage rows by cascade, and outbox rows that have reached a
terminal state. Deleting these loses history about the system's own behaviour and no
knowledge.

*Records of what the system knows, and how it knows it* are never pruned by any policy:
assertions, source events, episodes, chunks, evidence, derivations — and `model_runs`.

`model_runs` is the trap, and it is why this needed writing down. It looks operational: a
log of calls to a provider. It is not. `evidence` and `derivations` carry `model_run_id`
under `ON DELETE SET NULL`, so pruning a run would not fail — it would quietly cut the link
from a claim to the model interaction that proposed it. That link is the thing this system
exists to keep, and its loss would be silent, permanent, and invisible until someone asked
where a claim came from.

**Every retention setting defaults to keeping records forever.** Deleting an operator's
records is their decision. A system whose subject is provenance should not begin making it
on their behalf because it was upgraded.

**`retrieval_traces` is range-partitioned by month; nothing else is.** Expiring a month of
traces is then a `DROP TABLE` on one partition rather than millions of deletes and the bloat
they leave behind. The others use bounded deletes.

The asymmetry is not arbitrary. Range partitioning requires the partition key inside every
unique constraint, and `outbox_events` carries `UNIQUE (workspace_id, dedupe_key)` — the
guard that stops the same logical work being enqueued twice. Partitioning it by time would
scope that guarantee to a partition, so the same work could enqueue once per month. A queue
with retention stays small anyway; a trace table does not.

**Every process sweeps, and an advisory lock decides which one does the work.** The same
reasoning as the outbox reaper: recovery should not depend on one designated process being
alive, and leader election is a large amount of machinery for a job whose worst failure mode
is running twice.

## Consequences

An operator can now bound the tables that grow with traffic, and cannot bound the ones that
carry provenance. Both halves are asserted: a test seeds a model run with evidence and a
derivation pointing at it, sets every retention window to one second, moves the clock ten
years forward, and requires that all of it survives — including the `model_run_id` link
rather than only the row counts, since `SET NULL` would leave both tables intact and the
trace cut.

Three things worth recording that were found by writing the tests rather than by reasoning.

The first is that a row in the default partition is unreachable by partition drops. It gets
there when partition creation has fallen behind — a paused environment, a fleet scaled to
zero — which is exactly when nobody is watching, and it would then have been the one record
in the system no retention policy could ever remove. Expired rows there are deleted
individually.

The second is that the backup classification test failed the moment partitions existed,
because it walks the live schema and demands every table be classified as canonical or
derived. It was right to fail and the fix was to teach it that a partition is storage for a
table already classified, not a table anyone classifies separately — otherwise next month's
partition would silently make the runbook incomplete.

The third is that the first version of the provenance test proved nothing. It compared row
counts before and after for six tables, and the fixture creates no `evidence`, no
`derivations` and no `model_runs`, so for the three tables that mattered most it was
asserting that zero equals zero. A test written to protect a guarantee, passing without
touching it. The fixture now seeds them, and the test fails if they are absent.

What this does not address is the rest of issue #3. The ledger is still one PostgreSQL, the
knowledge tables still have no partitioning, and ingest is still bounded by one node. Those
remain open, and the ceiling is now documented rather than implied.
