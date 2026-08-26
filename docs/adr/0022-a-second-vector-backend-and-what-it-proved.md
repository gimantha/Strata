# ADR 0022 — A second vector backend, and what it proved

Status: accepted
Date: 2026-08-26
Phase: 15 (advanced storage adapters)

## Context

[ADR 0021](0021-three-index-ports-not-one-store.md) split the projections into three ports
and said the point was substitution. It could not demonstrate that. One implementation
behind an interface is a shape, not a contract, and the suite that came with it turned out
to be weaker than the claim it was supporting: it exercised seven of `VectorQuery`'s fourteen
fields and none of `PolicyFilters`' thirteen. A backend that leaked restricted
classifications would have passed.

Phase 15 asks for a dedicated vector backend. The reason to build one here is not
performance — [performance.md](../api/performance.md) shows PostgreSQL reaching HNSW from
about a thousand records and meeting every section 39 target. It is that a port with one
implementation is an assertion, and the way to find out whether it is true is to write the
second one.

## Decision

**Qdrant behind `index.Vectors`, selected by `CG_VECTOR_BACKEND`, holding the vector
projection while the ledger, the lexical index and the graph stay in PostgreSQL.**

Three decisions carry the adapter, and each is a place a naive port goes wrong.

**Point identity is derived, not stored.** Qdrant addresses points by UUID or integer while
the projection's key is a five-part tuple, so the id is a UUIDv5 over exactly the columns of
`vector_records_key`. That is what makes an upsert converge on replay — a rebuild replays
from the beginning every time — and it makes tenancy structural: another workspace asking for
the same record computes a different id and finds nothing. Deliberately excluding
`graph_space_id`, so a record re-derived into another graph space lands on the same point and
updates it.

**Absent timestamps are values, not missing keys.** PostgreSQL's temporal clauses read "NULL
means unbounded", which needs two branches per column. Writing ±8e15 microseconds instead
collapses each to one range comparison, keeps every condition individually indexable so
Qdrant can plan the filtered traversal, and — the part that is not merely tidy — is the only
encoding under which `RefreshMetadata` can clear a timestamp, because `set_payload` merges
and cannot remove a key it is not told about.

**Writes wait.** Qdrant acknowledges before a write is searchable unless asked otherwise, and
the port requires a purge to be visible to the next read: a rebuild purges and immediately
starts writing. That is correctness rather than tuning, so it is not configurable.

## What it proved, and what it found

The adapter passes both suites — nine shape cases and twenty-six filter cases — against a
real Qdrant, unmodified. The port needed no change to accommodate it. That is the
demonstration ADR 0021 wanted and could not give.

Getting there required strengthening the contract first, and doing so found two defects that
had nothing to do with Qdrant:

- **Collection-scoped policy was not applied at all.** `AllowedCollections` and
  `DeniedCollections` had been populated since phase 11 and enforced nowhere. Enumerating
  every field for the filter mapping is what surfaced it.
- **The upserts left columns out of their SET lists**, so a projection could keep a scope the
  ledger had changed. Harmless with one backend and a real divergence with two.

Neither was findable from the Qdrant side. Both were findable by asking what a second
implementation would have to reproduce, which is the argument for writing the second
implementation.

## Consequences

`index.Set` mixes backends freely: vectors in Qdrant, lexical and graph in PostgreSQL, one
ledger underneath. Nothing above the ports knows. The recovery drill iterates the configured
indexes and names the backend serving each, so a rebuild reaches Qdrant and a drill reports
against it rather than against an empty PostgreSQL table.

**A second datastore is now in the restore path.** The vector projection is derived, so a
rebuild reconstructs it — losing Qdrant costs a replay, not data. But
[backup-and-recovery.md](../api/backup-and-recovery.md)'s classification of canonical and
derived now spans two systems, and the drill is the thing that keeps that honest.

**Referential integrity is lost on this leg.** PostgreSQL has foreign keys from
`vector_records` to workspaces, sources and collections; Qdrant has payload values. A
dangling reference is now possible where it was not. That is the same shape as the stale
graph edge ADR 0021 anticipated, and it is handled the same way: retrieval hydrates through
the ledger and drops what it cannot resolve.

**Startup fails when a configured Qdrant is unreachable**, rather than falling back to
PostgreSQL. A deployment that asked for one store and silently got another would discover it
as a retrieval-quality problem, which is the hardest kind to trace.

## Alternatives considered

**A collection per workspace**, with purge implemented as dropping the collection. Rejected:
it fails workspace isolation on a workspace nothing has been written to yet, since the
collection does not exist and every read errors instead of returning nothing.

**`hnsw.iterative_scan` for filtered search.** Not needed — and measured not to help the
PostgreSQL side either.

**`overwrite_payload` for `RefreshMetadata`.** It replaces the whole payload and would erase
`content_hash`, so the next rebuild would re-embed everything ever deactivated. No subtest
could have caught it; the cost would have shown up as a provider bill.

**Moving all three projections at once.** Rejected: the lexical and graph ports have no
second implementation and no evidence that they need one, and moving what is not measured to
be a problem is how a system acquires operational surface it cannot justify.
