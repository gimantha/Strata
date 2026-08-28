# Performance

Measured targets from [AGENTS.md section 39](../../AGENTS.md), the harness that produces
them, and the invariants that are checked on every commit.

Section 39 is deliberately modest: *do not prematurely promise enterprise-scale numbers, but
design for measurable targets*. What follows is a baseline on one machine, not a promise
about yours. Every figure states the conditions it was measured under, because a number
without them is not a measurement.

## The two kinds of target

Section 39 lists six things. Three are speeds and three are invariants, and they are handled
differently on purpose.

| Kind | Items | How it is checked |
|---|---|---|
| Speeds | ingest acceptance, retrieval p95, projection lag | `scripts/benchmark.sh`, run deliberately |
| Invariants | bounded traversal, no unbounded context, no full scan for semantic search | Ordinary tests, run in CI |

An invariant either holds or the system is wrong, and no amount of fast hardware fixes it.
Leaving those to a benchmark somebody runs quarterly would mean discovering a regression
long after it shipped, so they are tests in
[`internal/benchmarks/guards_test.go`](../../internal/benchmarks/guards_test.go).

## Running the benchmarks

```bash
./scripts/dev-postgres.sh start
./scripts/benchmark.sh                # everything
./scripts/benchmark.sh Retrieval      # one pattern
```

Targets are configurable, so a deployment holds itself to its own numbers rather than the
ones this machine happens to meet:

```bash
CG_BENCH_RETRIEVAL_P95=250ms CG_BENCH_PROJECTION_LAG_P95=2s ./scripts/benchmark.sh
CG_BENCH_ITERATIONS=1000 ./scripts/benchmark.sh
```

A benchmark that exceeds its configured target fails. The defaults are section 39's own:
retrieval p95 under a second, projection lag in seconds.

## Baseline

**Conditions.** 500 documents of ~140 words, each mentioning 4 entities drawn from a
vocabulary of 120, plus the entity-to-entity claims those mentions imply. Apple M1 Pro,
10 CPU, macOS, PostgreSQL 16 in Docker. pgvector HNSW (`vector_cosine_ops`) + GIN over
`tsvector` + pg_trgm (migration 0008). Embedding model `hashing-bow-v1`, 1536 dimensions.
200 iterations per measurement.

| Measurement | Result | Section 39 target |
|---|---|---|
| Ingest acceptance, one caller | 66 events/sec — p50 14ms, p95 23ms | tens to hundreds/sec per node |
| Ingest acceptance, 10 callers | **207 events/sec** — p50 46ms, p95 68ms | tens to hundreds/sec per node |
| Retrieval, entity lookup | p50 15ms, p95 21ms | p95 under 1s |
| Retrieval, common term | p50 16ms, p95 20ms | p95 under 1s |
| Retrieval, rare term | p50 9ms, p95 14ms | p95 under 1s |
| Retrieval, relational | p50 11ms, p95 15ms | p95 under 1s |
| Context assembly, 2000-token budget | p50 31ms, p95 35ms | — |
| Projection lag, accept to retrievable | p50 87ms, p95 106ms | normally seconds |

One run, not an average. Repeated runs on this machine moved every figure by under 15%
— ingest between 59 and 66 events/sec single-caller, 207 to 215 concurrent — which is the
precision to read these at.

**Query mix.** Four shapes rather than one repeated query, reported separately. A pooled p95
over entity lookups and semantic questions describes neither, and hides the case where one
shape is slow because it is missing an index.

- *entity-lookup* — a person and an organization by name
- *common-term* — phrases appearing in most documents
- *rare-term* — a marker appearing in one
- *relational* — a question whose answer is an edge

### Reading these numbers

**The single-caller ingest figure is a latency measurement wearing a throughput label.** It
reports how fast one request completes. Most of an acceptance is waiting on PostgreSQL, so
the concurrent figure — 213/sec across ten callers — is the one that describes a node.

**Projection lag here is the synchronous pipeline, not the worker fleet.** It measures accept
→ process → retrievable with no queue in between, which is the floor. A deployment adds its
poll interval on top of that, or close to nothing with a broker configured; see
[distributed.md](distributed.md).

**The embedding model is local.** `hashing-bow-v1` computes feature hashes with no network
call, so these numbers measure Strata rather than somebody's API. A deployment using a hosted
embedder should expect projection lag dominated by that provider, and should say which model
when reporting — that is exactly what section 39's reporting requirement is for.

**The corpus is synthetic and deterministic.** A benchmark whose input changes between runs
measures the input. The generator is in
[`corpus.go`](../../internal/benchmarks/corpus.go); scale it up for a deployment-sized run
and state the size.

## The invariants

### Bounded graph traversal

`MaxGraphDepth` is 5 and `DefaultGraphDepth` is 2. A request beyond the ceiling is clamped
during normalization, not honoured, so a caller cannot raise the bound by asking. The
traversal is breadth-first with a visited set, so a cycle terminates rather than looping.

The guard asserts both: that an over-large depth does not survive normalization, and that no
returned hit exceeds the ceiling. It runs against a corpus where entities recur across
documents, because a traversal over a graph with no cycles proves nothing.

### No unbounded context generation

The token budget is a hard ceiling: assembly drops content rather than exceeding it. The
guard checks a range of budgets from `MinTokenBudget` (100) upward, because a ceiling that
holds at 2000 tokens and fails at 150 fails exactly where it matters — a small budget is
where scaffolding alone can overrun. A budget above `MaxTokenBudget` is clamped.

### No full scan for ordinary semantic search

This is a claim about query plans, so the guard reads one. `ExplainVectorSearch` and
`ExplainLexicalSearch` render the search the retriever would run and return PostgreSQL's
plan without executing it. They run `EXPLAIN`, never `EXPLAIN ANALYZE` — describing a query
should not cost as much as running it twice — and they are available to an operator
diagnosing a slow query on a live deployment, not only to tests.

Checking `pg_indexes` instead would be easier and would prove nothing. Indexes can exist and
go unused, and that turns out to be exactly what happens here.

## The scoped-search finding, resolved

An earlier version of this document reported that scoped semantic search never used the HNSW
or GIN index, and listed three candidate fixes. That finding was measured at 500 records and
was wrong, in an instructive way. The correction is kept here rather than deleted, because
the mistake is the useful part.

**What is actually true.** A scoped vector search uses the HNSW index from roughly a thousand
records onward. At 200 records a sequential scan genuinely is cheapest and PostgreSQL is right
to choose it. The original measurement sat at 500 — close enough to the crossover to look like
a rule.

**What made it briefly true anyway.** A deterministic tiebreak was later added to the
retrievers so that two records at the same distance could not come back in different orders on
different machines. Written as `ORDER BY embedding <=> $q, record_id`, it made the index
unusable at *every* size: an HNSW index scan can only satisfy an ordering that matches the
indexed expression exactly, so a second sort key forces PostgreSQL to sort the whole candidate
set. Measured at 200,000 records in one workspace — still a sequential scan.

**The fix is the shape of the query, not the database.** The distance ordering and the limit
go in an inner query, where the index can serve them; the deterministic tiebreak goes outside,
over the twenty rows that survive:

```sql
SELECT * FROM (
    SELECT ... FROM vector_records v WHERE ...
    ORDER BY v.embedding <=> $q          -- HNSW can satisfy this
    LIMIT $k
) ranked
ORDER BY ranked.score DESC, ranked.record_id   -- and this makes the order stable
```

Neither `hnsw.iterative_scan = relaxed_order` nor `strict_order` helped; both still produced a
sequential scan. Partitioning and scope-carrying indexes were not needed. This is what the
argument for measuring before phase 15 was about: a team seeing slow scoped search and no
index usage could reasonably conclude they needed a dedicated vector database, and the answer
was eleven lines of SQL.

What remains undetermined is which records sit exactly on the limit boundary when distances
tie there. That is inherent to approximate nearest-neighbour search and not worth a full scan
to remove.

### A planner caveat worth knowing

Whether PostgreSQL *chooses* the index is less stable than whether it *can*. The same 2,000
records produced an HNSW scan on one run and a sequential scan on another, and 20,000 records
chose the scan. The cause is that pgvector stores vectors out of line, so the main table looks
tiny to the planner — a 2,000-row scan is costed at about a hundred pages — and detoasting is
not costed at all.

Disabling sequential scans is not enough either, which CI demonstrated by failing a first
version of the guard that did exactly that: with a sort still available the planner can prefer
a bitmap scan on the scope index and a sort, and whether it does depends on the machine.

So the guard disables sorting too, which turns the question from a cost comparison into a
structural one: with neither a sequential scan nor a sort available, the only plans left are
those where an index supplies the ordering. Either the ORDER BY is something HNSW can serve,
or it is not — and that does not depend on hardware or row count. The way this property breaks
is not the planner changing its mind; it is a query the index cannot serve.

### What the guards actually assert

Given the above, the CI guard deliberately does **not** assert that the retriever's scoped
query avoids a sequential scan — that would encode today's behaviour as correct, or fail on
a fixture whose size is the real variable. Instead:

- `TestIntegrationProjectionIndexesAreUsable` proves each index can be chosen for the search
  shape it exists to serve, with sequential scans disabled so the planner has to reveal
  whether an index path exists at all. A changed distance operator, a dropped index, or a
  generated column that stops matching its operator class all fail here.
- `TestIntegrationScopedSemanticSearchUsesItsIndex` seeds two thousand vectors and asserts
  that an index can supply the ordering for the retriever's own scoped query, with sequential
  scans and sorting both disabled. This is the one that would have caught the tiebreak
  regression: reintroducing it fails this test with the reason and the plan.

## What is not measured yet

Honest gaps, so nobody reads absence as a passing grade:

- **Scale.** 500 documents is a laptop-sized corpus. Index *usability* is now measured up to
  200,000 vectors, but end-to-end latency is not: the throughput and p95 figures above are
  still small-corpus numbers.
- **Concurrent retrieval.** Ingest is measured under concurrency; queries are not.
- **A hosted embedding model.** Every figure here uses the local hashing embedder.
- **The alternative backends.** Every figure here is PostgreSQL. Qdrant and OpenSearch are
  known to satisfy their ports; nothing has measured whether either is faster, and the port
  split exists so that question can be answered later rather than assumed now.
- **Sustained load.** These are short runs, so they say nothing about connection-pool
  exhaustion, index bloat, or autovacuum behaviour over days.
