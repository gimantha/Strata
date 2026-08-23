# ADR 0007: pgvector for the vector projection

Status: accepted
Date: 2026-08-23

## Context

AGENTS.md section 17 requires a vector projection over several semantic surfaces - chunks,
entity names, assertion text, episodes, summaries, procedural memory - with every record
retaining its scope, canonical record id, embedding model and version, provenance, and the
temporal metadata needed for pre-filtering. Phase 6 builds it; phase 7 retrieves through
it.

ADR 0001 already chose PostgreSQL as the canonical ledger and said projections would start
there too. This decision confirms the vector half of that and settles a question left open
during phase 0: the development environment at the time had no pgvector available, and the
choice was deferred rather than worked around.

## Decision

pgvector is the vector projection backend for phase 6, running in PostgreSQL alongside the
canonical ledger.

CI and local development use the `pgvector/pgvector:pg16` image, which is the stock
`postgres:16` image with the extension available. That switch is made now, before any
vector code exists, so phase 6 requires no CI change and nobody discovers the extension is
missing halfway through building the projection.

The extension is not enabled yet. A phase 6 migration will run `CREATE EXTENSION IF NOT
EXISTS vector` when there is a table that needs it.

Go talks to the type through its text form. A `[]float32` is formatted as `[1,0,0,0]` and
passed with an explicit `$n::vector` cast; results are read with `embedding::text` and
parsed back. No new dependency is required.

## What was verified before deciding

Against `pgvector/pgvector:pg16` (PostgreSQL 16.15, pgvector 0.8.6):

- `CREATE EXTENSION vector` succeeds inside a transaction, so the existing migration runner
  handles it with no change.
- A scoped projection table with `vector(n)` columns, an HNSW index using
  `vector_cosine_ops`, and cosine-distance ordering all work, and `EXPLAIN` confirms the
  planner uses the index rather than falling back to a scan.
- A workspace-filtered nearest-neighbour query returns only that tenant's rows, which is
  the shape phase 6 and the phase 11 isolation tests need.
- With plain pgx: writing via a text cast works and reading via `embedding::text` works.
  Passing a `[]float32` directly does **not** - pgx encodes it as a PostgreSQL array,
  `{0.1,0.2}`, which pgvector rejects - and scanning directly into a `[]float32` fails for
  the mirror-image reason. Hence the explicit format-and-parse helpers.

## Alternatives

- **A dedicated vector database** (Qdrant, Weaviate, Milvus). Rejected for now, and
  explicitly allowed later by phase 15. AGENTS.md section 3 requires a developer to run a
  meaningful complete system with PostgreSQL and a model provider alone, and a second
  datastore breaks that. The `VectorIndex` interface in section 27.3 is what keeps this
  reversible: the projection is rebuildable, so switching backends is a re-index, not a
  migration.
- **The `pgvector/pgvector-go` package.** It registers the type with pgx so `[]float32`
  binds directly, and supports the binary wire format. Not adopted yet: AGENTS.md section 3
  says not to add a dependency to avoid a trivial helper, and formatting a float slice is
  trivial. It stays the obvious answer if benchmarks show the text encoding matters - a
  1536-dimension vector is roughly 15 KB as text against 6 KB binary, which could matter on
  bulk re-embedding rather than on query.
- **Exact search over a `real[]` column with a hand-written distance function.** Rejected:
  it works at small scale and then quietly stops working, with no index to fall back on.
- **Keeping `postgres:16` in CI and installing the extension per run.** Rejected as slower
  and one more thing to break, for no benefit over an image that already has it.

## Trade-offs

pgvector will not match a specialized engine at very large scale, and index build time on
HNSW is significant for big collections. Both are acceptable at the scale AGENTS.md section
39 targets, and neither is locked in.

Using the text wire format costs bandwidth and some parsing on both ends. It is the right
default until measured otherwise, and switching to binary later means adding one dependency
and changing two helpers, with no schema or semantic change.

Local development without Docker needs pgvector installed separately (`brew install
pgvector` on macOS, `postgresql-16-pgvector` on Debian and Ubuntu). Until phase 6 nothing
requires it, and the test harness's ephemeral cluster keeps working without it.

## Migration impact

None today: no schema, code, or behavior changes, only the CI image. Phase 6 adds the
extension and the projection tables in its own migration.

Changing embedding model or version later is a projection rebuild, never a canonical
migration - vector records carry their model and version, and the ledger they derive from
is untouched (AGENTS.md section 17).
