# ADR 0024 — The last port, and what three backends showed

Status: accepted
Date: 2026-08-28
Phase: 15 (advanced storage adapters)

## Context

`index.Graph` was the last of the three ports without a second implementation, and the one
where the two engines are least alike. A recursive CTE and a Cypher variable-length pattern
are not dialects of one idea. Passing the same suite therefore says something the other two
backends could not: that the port describes traversal, rather than describing PostgreSQL's
way of doing it.

## Decision

**Neo4j behind `index.Graph`, selected by `CG_GRAPH_BACKEND`.** It passes all twenty-three
cases unmodified.

The port writes edges and never entities, so nodes are created as a side effect of writing an
edge and carry nothing but an identifier and a workspace. Names, types and everything else
stay canonical. That is only possible because ADR 0021 moved name resolution out of the index
and into the retriever — a change made for the shape of the port, which turns out to be
exactly what a graph backend needs.

Three details carry the traversal.

**Shallowest depth with matching provenance.** The reference reports each entity once, at the
shallowest depth it was reached, with the edge that produced *that* depth. Cypher returns
every path, so the query orders by depth and keeps the first, taking the via triple from the
same row — depth and provenance cannot disagree, which they could if computed separately.

**Filters sit inside the pattern.** They apply to the walk, not to its results: an edge that
fails one does not merely hide an entity, it can make everything behind it unreachable and
change how deep something else is reported. `all(edge IN rels WHERE ...)` is Cypher's way of
saying what the reference says by putting the clause inside the recursive term.

**The depth bound is interpolated.** Cypher will not take a parameter in a variable-length
pattern. It is safe because `Normalize` has already clamped the value to between one and the
ceiling, so it can only be a small integer — but it is the one place in any adapter where a
query is built by formatting rather than binding, and it is worth knowing that.

Absent timestamps are nulls here rather than sentinels, as in OpenSearch and unlike Qdrant.
Cypher compares against null the way SQL does, so the clauses are the reference's almost
verbatim. Three adapters, three encodings of "unbounded", one answer.

## What three backends showed

Phase 15 asked for four storage adapters. The more useful result is what building them did to
the contract, because every one of them found something the reference had got wrong or the
suite had failed to say.

- **S3** — nothing in the reference, but it established the pattern: one port, two backends,
  one conformance suite ([ADR 0020](0020-one-blob-port-two-backends-one-conformance-suite.md)).
- **Qdrant** — found that collection-scoped policy was enforced nowhere, and that the
  projection upserts let a record keep a scope the ledger had changed. Neither was visible
  from the Qdrant side; both were found by asking what a second implementation would have to
  reproduce.
- **OpenSearch** — found that the exact-match contract tested PostgreSQL's wildcard alphabet
  and not OpenSearch's, so an adapter escaping nothing would have passed; and that nothing
  distinguished a full-text query requiring every term from one accepting any.
- **Neo4j** — found nothing new, which is itself the result worth recording. By the time the
  hardest port got its second implementation, the contract was strong enough that a
  fundamentally different engine passed it unmodified on the first run.

The pattern across all four: **a filter that exists in the domain and nowhere in the query
path fails silently, in the direction of disclosure.** Three of the four gaps found this way
were policy holes. The conformance suites are the structural answer, and the reason they run
against PostgreSQL as well as against the newcomer.

## Consequences

`index.Set` mixes four backends: vectors in Qdrant, text in OpenSearch, graph in Neo4j,
ledger in PostgreSQL. Nothing above the ports knows.

**Every projection can now leave PostgreSQL, and none of them should without a reason.**
[performance.md](../api/performance.md) shows PostgreSQL meeting every section 39 target at
the scales measured, and no backend here was built to fix a bottleneck. What they buy is the
option, and the evidence that the option is real.

**Referential integrity is gone on all three projection legs.** PostgreSQL has foreign keys
where the others have properties on documents, points and relationships. The retriever
already drops a graph hit it cannot resolve, which was designed for a stale edge and covers
this too.

**The restore path spans four systems.** All three projections are derived, so a rebuild
reconstructs them and losing any one costs a replay rather than data — but
[backup-and-recovery.md](../api/backup-and-recovery.md)'s classification now spans more than
one database, and the drill is what keeps it honest.

## Alternatives considered

**Memgraph**, which is Bolt-compatible, lighter, and would have used the same driver.
Rejected because Neo4j is the reference implementation of Cypher, and the point of the
exercise was to test the port against the engine least likely to share PostgreSQL's
assumptions rather than the one easiest to run.

**Denormalising entity types onto edges** so the graph leg could enforce entity-type policy
itself. Rejected in ADR 0021 and still rejected: a type belongs to the entity, and a copy on
the edge would go stale on a rename until reprojection.
