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


## Query planning

Something has to decide *how* to look before anything looks. That step is query planning, and
Strata offers two.

```bash
CG_QUERY_PLANNER=heuristic     # default
CG_QUERY_PLANNER=llm
CG_QUERY_PLANNER_TIMEOUT=5s
```

**Heuristic** reads the query's *shape*. Is it short enough to be a name? Does it look like an
identifier — `ERR_7731X`, `AF-2291-B` — which stemming and embeddings both destroy? Is an
embedder configured? Then it fires the matching retrievers and fuses them. It costs nothing,
never fails, and never sends the question anywhere.

What it cannot do is read meaning. In *"which context graph implementation supports
episodes?"*, the phrase `supports episodes` is a **requirement**, and the heuristic planner
has no way to know that. Embeddings are weak here too: a document about context graphs that
mentions episodes and one that does not sit close together in vector space.

**LLM** reads meaning. It chooses retrievers with a stated reason, and may reshape the
question into up to four searches:

| Kind | What it is |
|---|---|
| `original` | The question as asked. Always present, always the real text. |
| `decomposed` | One part of a question that asked for several things. |
| `hypothetical` | A sentence that reads like the answer would, searched instead of the question — a question and its answer are worded differently, so the answer's shape often lands closer to the passage containing it. |

Each search runs across every planned retriever, and the results all fuse together: a record
found by two searches earns two contributions and outranks one found by either alone.

### What it can and cannot do

The planner may choose **which retrievers run** and **what strings they search for**. That is
the whole of its output space. It cannot name a workspace, relax a policy, raise a limit, or
reach a surface the caller could not already reach — those come from the request and the
access decision and are never read back from the model.

This is what makes it safe to let a model influence retrieval over text a stranger wrote. A
question engineered to hijack the planner can, at worst, make it run every retriever — which
is close to what the heuristic does anyway. The prompt also states that the question is data
rather than instruction, the same defence extraction uses, but the type system is the part
that actually holds.

### It always falls back

Any failure — timeout, provider outage, output that does not validate — returns the heuristic
plan with a note saying why, and logs it. Section 19.4 requires retrieval to work without a
model at query time, and a system that *usually* has one does not satisfy that.

The model also never decides what you asked. Whatever it returns labelled `original` is
replaced with the actual question, so a rewrite can add searches but never silently
substitute one.

### Reading what it did

`Explain: true` returns the plan: which planner ran, every search issued and why, and the
per-mode reasons and counts. A result that came back for a rewritten question is not evidence
about the original unless someone can compare them.

### The same question plans the same way

Planning requests temperature 0 and a fixed seed, so a question asked twice does not produce
two different retrieval plans and two different result sets. A provider that ignores the seed
weakens this; one that ignores temperature 0 breaks it, and nothing here can detect that from
the outside.

Determinism is asserted where it can be — the request sent to the provider is checked to
carry both. It once was not: temperature 0 shared a representation with "unset" and was
dropped before the wire, which affected extraction too ([ADR
0025](../adr/0025-two-model-bugs-that-degraded-instead-of-failing.md)).

### One combination is refused

`CG_QUERY_PLANNER=llm` with `CG_REDACT_QUERY_TEXT=true` fails at startup. Redaction exists so
the words of a question stay out of traces; planning sends those same words to a provider.
Accepting both would defeat a compliance control silently.

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
