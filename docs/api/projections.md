# Retrieval projections

Vector, lexical, and graph indexes over canonical knowledge. All three are **derived**:
drop them entirely, replay from the ledger, and nothing is lost. That property is what keeps
the ledger authoritative rather than merely first, and it is exercised by a test rather than
asserted (AGENTS.md sections 2.3, 15.2, scenario I).

## What gets projected

| Surface | Vector | Lexical | Graph |
|---|---|---|---|
| `chunk` | ✓ | ✓ | |
| `assertion` | ✓ | ✓ | ✓ when the object is an entity |
| `entity` | ✓ | ✓ | as nodes |

Episodes are not indexed separately: their text is covered by their chunks, and indexing
both would return the same passage twice under different surfaces.

Assertions are rendered as sentences — "acme corporation supplies industrial fasteners" —
so a claim is findable by meaning, not only by structure. Superseded and retracted claims
are projected too, because retrieval filters by status and knowledge time, and a projection
holding only current belief could not answer a question about what was believed last month.

Only entity-to-entity assertions become graph edges. A literal-valued claim is not an edge
in any useful sense, and forcing one would either invent nodes for values or flatten the
typed object the ledger works to preserve. Each edge carries its assertion id, so the full
claim is one lookup away.

## Configuration

| Variable | Meaning |
|---|---|
| `CG_EMBEDDING_PROVIDER` | `none` (default), `mock`, or `openai` |
| `CG_EMBEDDING_BASE_URL` | Any OpenAI-compatible endpoint |
| `CG_EMBEDDING_MODEL` | Required for `openai` |
| `CG_EMBEDDING_API_KEY` | Held in memory only, never stored or logged |

**An embedder is optional.** Without one, the lexical and graph projections still work: text
search and traversal need no model. Vector search is simply unavailable, and a later rebuild
fills it in once a provider is configured.

The projection schema holds **1536-dimension** vectors. A model of another width is rejected
at startup rather than at first use, because discovering the mismatch when the first
document is indexed is far worse than refusing to start. Changing the width is an additive
migration plus a rebuild — never a change to canonical knowledge.

## Vector retrieval

Filters are applied **before** ranking, not after. Ranking the whole table and discarding
afterwards would return fewer results than asked for, and would let another tenant's vectors
influence which of ours came back.

Available filters: graph space, surface, embedding model and version, `valid_at`, status,
classification, and memory kind.

`MinScore` matters more than it looks. Nearest-neighbour search always returns its *k*
nearest, however far away they are, so without a floor an unrelated question still gets
confident-looking answers.

Vectors from different models are never mixed in one search: they are not comparable, and
ranking one against the other is ranking noise against signal.

## Lexical retrieval

Two modes, because they fail in opposite places:

- **Full text** (default) — stemmed `tsvector` matching ranked by `ts_rank_cd`, which weighs
  term proximity so a passage discussing the terms together outranks one that merely
  mentions them. Good for prose.
- **Exact** (`exact: true`) — trigram-backed substring matching. This is what finds
  `ERR_7731X` and `AF-2291-B`, which stemming mangles and vectors have no useful
  neighbourhood for (AGENTS.md section 18).

## Graph expansion

Bounded breadth-first traversal with a visited set, so cycles terminate and each entity is
reported at the shallowest depth it was reached.

Both bounds are mandatory and the depth ceiling is **5**. A well-connected graph reaches
everything within a few hops, so an unbounded traversal is not a slow answer but a useless
one. A request for more depth is clamped rather than honoured.

Traversal follows edges in **both directions**: "who supplies Initech" and "what does Acme
supply" are the same graph seen from opposite ends. Every hit records the assertion and
predicate that connected it, so a path can be explained rather than merely reported.

## Rebuilding

```bash
cgctl projections status  --workspace acme
cgctl projections rebuild --workspace acme
```

A rebuild drops every projected record for the workspace and replays from the ledger. It
uses **the same code path** as incremental projection — a rebuild is not a second
implementation that might drift, it is the normal path run over everything.

Checkpoints are saved per page rather than only at the end, so an interrupted rebuild
resumes instead of starting over. Each checkpoint records how far it consumed, how many
records it wrote, and when it was last rebuilt.

Incremental re-projection **skips text that has not changed**, comparing content hashes
before embedding. Embedding is the expensive part of a rebuild, and paying a provider twice
for an identical result is pure waste. A full rebuild deletes first, so it genuinely
re-embeds.

## Searching from the command line

```bash
cgctl search --graph-space $GS --query "industrial fasteners"
cgctl search --graph-space $GS --query "ERR_7731X" --mode exact
cgctl search --graph-space $GS --query "who supplies Acme" --mode vector
```

## Not yet available

These are the retrieval *primitives*. Fusing them — a planner that decides which retrievers
to run, reciprocal-rank fusion, graph expansion seeded from vector hits, and result
explanations — is phase 7. Context assembly within a token budget is phase 8.
