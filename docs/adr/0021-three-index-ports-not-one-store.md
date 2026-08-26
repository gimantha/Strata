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

That principle was applied too narrowly at first. The original vector suite covered the shape
of the port — write, read, converge, purge — and exercised seven of `VectorQuery`'s fourteen
fields and none of `PolicyFilters`' thirteen. A backend that leaked restricted
classifications, ignored denied sources, or hid every passage behind an entity-type rule
would have passed it. `RunVectorFilterConformance` closes that: twenty-six cases declaring a
query and the exact set of records it must return, because a filter that admits too much
fails in the direction that matters. Writing it found a policy hole in the reference, which
is what a suite the incumbent runs is for.

## Consequences

Four of the five retrieval modes become substitutable: lexical, exact, vector, and — once
the seam below is closed — graph. Entity resolution stays canonical, because resolving a
name to an identity joins `entities` to `entity_aliases` and touches no projection.

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

## The graph seam

`index.Graph` initially could not hold. `ExpandGraph` read canonical `entities` twice — the
recursive CTE seeded its roots there, and the final projection joined it for each hit's
`canonical_name` — so a backend holding only edges could not satisfy it. That was recorded
here as an open seam rather than hidden, and closed in the following commit.

**Traversal now reports identifiers, and the retriever names them** in one batched
`GetEntities` call. `domain.GraphHit` has no `Name` field: naming an entity is a canonical
read, and a graph backend that had to perform one would have to hold the entity table to be
a graph backend at all.

**Roots are no longer returned.** They were only ever discarded by the single caller, and
dropping them inside the traversal buys the tenancy property that the removed join used to
provide: a walk seeded with an identifier from another workspace now comes back empty rather
than echoing that identifier, because edges are scoped and a foreign root can reach nothing.
Returning even the bare root would tell one tenant that an entity with that id exists in
another. The filter is applied after the limit, not before, because the limit has always
counted roots and moving it would quietly widen every traversal.

Hydration introduces one case the join handled silently: an edge pointing at an entity the
ledger does not have. PostgreSQL cannot produce one — `graph_edges` has foreign keys to
`entities` with `ON DELETE CASCADE` — but a substituted backend has no such guarantee, and
holding a stale edge briefly is what an eventually-consistent index does. The retriever drops
the hit rather than returning a nameless entity.

The change was expected to alter behaviour and did not: the golden is byte-identical, because
the retriever already discarded depth-0 hits and the hydrated names match what the join
produced. What changed is the contract, not the output.

The alternative — denormalising `canonical_name` onto `graph_edges` — was rejected. A rename
or a merge would leave traversal reporting a stale name until reprojection, where the join
was fresh by construction.

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
