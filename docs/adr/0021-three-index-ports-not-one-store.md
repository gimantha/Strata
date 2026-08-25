# ADR 0021 — Three index ports, not one store

Status: accepted
Date: 2026-08-25
Phase: 15 (advanced storage adapters)

## Context

[ADR 0001](0001-postgresql-canonical-ledger.md) said each retrieval projection "sits behind
its own interface so a dedicated backend can replace it." That was the intent. It was not
what the code said.

`projection.Store` was one interface with fifteen methods: six canonical reads, three
projection writes, a hash lookup, a compound delete, and checkpoints. `retrieval.Store` had
five, mixing two index searches with two canonical reads. To put vectors in a dedicated
store, an implementer had to supply `QueryAssertions` — against a vector database that has
no assertions, no temporal reconciliation, and no supersession.

Phase 15 says *add only after interfaces stabilize*. For the blob port that was already
true; it was written small in phase 1 and the S3 adapter needed no change to it
([ADR 0020](0020-one-blob-port-two-backends-one-conformance-suite.md)). For the projections
it was not.

## Decision

**Three ports, one per index, in `internal/store/index` alongside `internal/store/blob`.**

`index.Vectors`, `index.Lexical`, `index.Graph`. Never a combined `index.Store`: a
deployment putting its vectors elsewhere must not be made to implement full-text search and
graph traversal to do it.

**Read and write stay together in each port.** Splitting them into a writer and a searcher
reads tidily and makes the contract unstateable — the fields a record is written with are
the fields a query filters on, and no round-trip conformance test can span two interfaces a
backend might implement inconsistently.

**No `CanonicalReader`.** Two consumer-declared interfaces instead: `projection.Ledger` with
six reads and `retrieval.Ledger` with one. They overlap in nothing, and Go convention is
that the consumer declares what it needs. `retrieval.Ledger` is small on purpose —
`FindEntitiesByName` joins `entities` to `entity_aliases` and touches no projection, so
resolving a name to an identity stays canonical however the indexes are configured.

**Checkpoints are their own port**, not per-index and not on the reader. The high-water-mark
rules — a position only moves forward, counts accumulate unless a rebuild resets them — are
bookkeeping about the ledger rather than index data, and making every backend reimplement
them would be asking each to get the same subtlety right alone.

**Each port has an exported conformance suite**, run by PostgreSQL as well as by any
substitute. A suite written after the fact and run only by the newcomer encodes whatever its
author assumed the incumbent did.

## Consequences

Three of the five retrieval modes become substitutable: lexical, exact, and vector. Entity
resolution stays canonical, and graph does not move — see below. That is a smaller claim
than "the projections are replaceable", and it is the true one.

A partially configured `index.Set` degrades rather than panics, the way a nil embedder
already did. A deployment moving one projection should not lose the other two mid-migration,
and the retriever skips a mode whose index is absent rather than failing the query.

**`DeleteProjections` is gone, and its hidden choice is now explicit.** It deleted four
tables in one transaction, so nobody had to decide what order a rebuild clears things in.
Across separate backends that transaction cannot exist, and the ordering matters: a
checkpoint left standing over an emptied index claims work that is no longer there, while a
cleared checkpoint over a populated index only causes a replay. Checkpoints are cleared
first, and the comment says why.

Nothing was taken apart that was ever atomic. Each projection write already opened its own
transaction, `saveCheckpoints` already wrote three rows as three statements, and retrieval
already crossed the canonical/index boundary mid-query with no snapshot tying it together.
The refactor is safe because the atomicity it appears to remove was not there.

The recovery drill iterates the configured indexes rather than deleting known tables, and
reports which backend serves each. Otherwise a deployment that had moved its vectors would
get a green drill proving only that the PostgreSQL half of its derived data is disposable.

## The graph seam, which does not hold

`index.Graph` exists and is honest about being incomplete. `ExpandGraph` reads canonical
`entities` twice: the recursive CTE seeds its roots there, and the final projection joins it
for each hit's `canonical_name`. With `graph_edges` completely empty it still returns
depth-0 rows. A backend holding only edges cannot satisfy it.

Making it substitutable means returning edge-shaped hits and hydrating names in the
retriever through one batched canonical read. That changes what `Expand` returns, so it is a
change of behaviour rather than a change of shape, and it gets its own commit and its own
test rather than being smuggled into a refactor that claims to change nothing.

The alternative — denormalising `canonical_name` onto `graph_edges` — was rejected. A rename
or a merge would leave traversal reporting a stale name until reprojection, where today's
join is fresh by construction.

## Verification

The claim is that behaviour is unchanged, and a green suite cannot support it: the existing
tests assert properties that survive a quiet reordering. So the ranked output of forty-eight
queries — eight questions against six mode combinations — was captured before the split and
compared after. It is byte-identical.

That golden was itself checked both ways before being trusted: three runs against fresh
databases with fresh UUIDs produce the same file, and a five-percent change to the vector
weight fails it with a readable diff.

## Alternatives considered

**One combined `index.Store`.** Reintroduces exactly the coupling this removes.

**`VectorWriter` / `VectorSearcher` and siblings.** No round-trip conformance test can be
written across them, and `ExistingHashes` has no honest side.

**Checkpoints per index.** Every backend reimplements the monotonic high-water mark.

**Defining the ports inside `internal/projection`.** Puts the write package on the read
package's dependency path for types both merely share.
