# Hybrid retrieval

One query, five retrievers, one fused ranking. Retrieval reads only from projections, never
from the ledger, so it can be made fast without making the ledger complicated (AGENTS.md
section 25).

```
POST /v1/graph-spaces/{graph_space_id}/query
```

```json
{
  "query": "who supplies industrial fasteners",
  "valid_at": "2026-03-01T00:00:00Z",
  "limit": 10,
  "explain": true
}
```

Everything but `query` is optional. With no `modes` the planner picks them.

## The retrievers

| Mode | Finds | Backed by |
|---|---|---|
| `lexical` | Fuzzy word overlap | `pg_trgm` similarity |
| `exact` | The phrase, verbatim | `ILIKE` containment |
| `vector` | Semantic similarity | pgvector cosine distance |
| `entity` | A named subject | canonical and alias name match |
| `graph` | Neighbours of a matched entity | assertion edges, bounded depth |

`graph` never runs alone. It expands from entity seeds the other retrievers produced, so a
query that matches no entity produces no expansion — which is correct, not a failure.

## The planner

Modes are chosen from the shape of the query, and the choice is reported rather than hidden.

- A query that looks like an identifier — mixed digits and letters, uppercase with
  separators, or four or more bare digits (`SKU-4471`, `INV 88213`, `12345`) — gets `exact`
  weighted heavily. Fuzzy matching on identifiers is worse than useless: a near miss on an
  invoice number is a wrong answer wearing the costume of a right one.
- A short query of six fields or fewer that reads like a name gets `entity` and `graph`.
- Long natural-language queries get `vector` and `lexical`.
- `vector` is skipped entirely when no embedder is configured, and `Explain` says so instead
  of silently returning fewer results.

## Fusion

Results are combined by weighted Reciprocal Rank Fusion, scaled by how good each match was
within its own retriever, with weak tails dropped relative to the top score and at least one
result reserved per surface. The reasoning, the weights, and the measured numbers are in
[ADR 0012](../adr/0012-weighted-rrf-with-quality-scaling.md).

Two consequences worth knowing at the API surface:

- **A record found by several retrievers outranks one found by a single retriever**, even
  when the single retriever ranked it first. Agreement is the strongest signal available.
- **Fused scores are only comparable within one response.** They are not a probability, not a
  similarity, and not stable across queries. Rank order is the meaningful output.

## Filters

| Field | Effect |
|---|---|
| `valid_at` | What held in the world at that instant (half-open: `[from, to)`) |
| `known_at` | What the system believed at that instant |
| `active_at` | What was in scope at that instant |
| `surfaces` | `chunk`, `episode`, `entity`, `assertion` |
| `statuses` | `asserted`, `superseded`, `retracted`, `disputed` |
| `classifications`, `memory_kinds`, `predicates` | Narrow before ranking |
| `min_confidence` | Floor on extraction confidence |

Filters are applied inside each retriever's SQL, not after fusion, so a filtered query still
returns a full result set instead of a decimated one. Graph space scoping is not a filter and
cannot be disabled — it is a predicate on every query in every retriever.

## Explain

With `"explain": true` each result carries the retrievers that found it, its rank within
each, and the raw signals behind those ranks; the response also carries the plan, including
modes that were skipped and why.

```
$ cgctl search --graph-space $GS --query "who supplies fasteners" --explain

SCORE    SURFACE    FOUND BY               CONTENT
0.04127  assertion  lexical+vector+entity  acme corporation supplies industrial fasteners
0.02341  chunk      lexical+vector         Acme has supplied our fastener stock since ...
0.01502  entity     entity                 Acme Corporation (organization)

3 result(s) from 47 candidate(s)

plan:
  lexical: always runs: matches the words as written (18 candidates, 7ms)
  vector: finds wording the query did not use (15 candidates, 11ms)
  entity: short query, so it may name an entity directly (2 candidates, 3ms)
  graph: expands from entities the other retrievers found (12 candidates, 5ms)
  exact: skipped, the query does not look like an identifier
```

Use it when a result is surprising. A record ranked high by one retriever and absent from the
others usually means the query was ambiguous; a fused score of exactly `0.00000` on every
result means fusion is misconfigured, not that nothing matched.
