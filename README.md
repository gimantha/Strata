# Strata

A context graph built around an immutable, multi-temporal assertion ledger.

Strata ingests conversations, documents, database changes, code, and tool events through
one semantic pipeline, records what it learned as immutable canonical knowledge, and
serves hybrid retrieval and token-budgeted context to agents and applications. The
canonical ledger is authoritative; graph, vector, and lexical stores are rebuildable
projections of it.

The full implementation contract is [AGENTS.md](AGENTS.md). It takes precedence over
this file.

## Status

Phases 0 through 14 of the contract are implemented: the architecture skeleton, the
canonical ingestion ledger, the assertion-first knowledge model, schema-constrained
extraction, conservative entity resolution, multi-temporal reconciliation, rebuildable
retrieval projections, hybrid retrieval and context assembly, ontology modes, change data
capture, attribute-based policy, memory lifecycle, portable packages and MCP, and
distributed production mode.

**Working today**

- Workspace, graph space, and collection scope, with API-key identity and grant-based
  authorization
- Source registration with trust levels and sensitivity classification
- Event-shaped ingestion: HTTP JSON events (single or batch), raw documents, and
  pre-segmented episodes, all converging on one `SourceEvent` path
- Content-addressed archival of every raw payload before its event is committed
- Idempotent ingestion: a replay returns the original event, and the same key with
  different content is a reported conflict rather than a silent overwrite
- A transactional outbox with leased claims, crash recovery, class-based retry, and
  dead-lettering
- A durable, replayable pipeline: `normalize` → `segment` → `chunk`, producing episodes
  and chunks that keep exact positional provenance
- Processing status, structured logs, OpenTelemetry traces and metrics, and a security
  audit trail
- Entities as stable identities, with aliases, and a predicate registry carrying the
  semantics that make contradiction handling something other than guesswork
- Immutable assertions with typed objects across twelve kinds, four independent layers of
  time, evidence, and derivations for anything reasoned rather than observed
- Corrections by supersession and withdrawal by retraction, both knowledge-time events
  that never edit or delete what was believed before
- Temporal queries combining `valid_at`, `known_at`, `valid_between`, `event_between`, and
  `active_at`, so "what did we believe on April 10 about March 25" is answerable
- Contradictions recorded as conflict sets rather than resolved by guesswork
- A provenance walk from any fact back to the archived bytes it came from
- Optional model-driven extraction: a provider-independent interface, an OpenAI-compatible
  adapter for any such endpoint, and a scripted provider so CI never needs a live model
- Model-run tracking for every interaction, successes and failures alike, storing hashes
  rather than prompts and never a credential
- Layered injection defenses: unforgeable delimiters, schema-constrained output validated
  locally, verbatim quote grounding, and quarantine for claims planted inside instructions
- Entity resolution up an evidence ladder: upstream primary keys, configured business keys,
  then exact names. Similar names generate candidates for review and never resolve on their
  own, because measured similarity cannot separate a typo from a different person
- Reversible merges: a merged identity is redirected, never collapsed, so a mistaken merge
  is undone by clearing a pointer rather than reconstructing history
- A decision ledger recording every resolution with its candidates, scores, and reasons
- Reconciliation that follows the source's own ordering rather than arrival order, so a CDC
  stream delivering updates out of order converges to the same state either way
- Predicate conflict policies driven automatically, including authority-weighted resolution
  from registered source trust, with equal authority producing a conflict rather than a
  coin flip
- Vector, lexical, and graph projections over chunks, claims, and identities, with filters
  applied before ranking and traversal bounded by depth. Embeddings are one retrieval path,
  not the comprehension step: documents become entities and assertions through extraction,
  so running without an embedder costs paraphrase matching rather than understanding
- Projections that hold no history: dropping all of them and replaying from the ledger
  produces equivalent retrieval results, which a test checks rather than assumes
- Hybrid retrieval over all five paths, fused by weighted RRF and planned from the shape of
  the query, with the plan and per-retriever ranks reported rather than hidden. On the
  evaluation corpus hybrid beats every individual mode on both recall@5 and MRR — a measured
  assertion in the test suite, not a claim

- Context assembly under a hard token budget: greedy selection with redundancy reduction,
  per-section shares, conflict annotation, and a citation for every rendered item — reserved
  before the item is written, so a block never carries a marker whose reference did not fit.
  Against top-k at an equal budget it covers more distinct facts with less than half the
  repetition, measured rather than asserted

- Two ontology modes per graph space: open mode invents and normalizes vocabulary, guided
  mode validates every claim against an immutable schema version. What the schema refuses is
  returned to a caller who can fix it, and quarantined — visible, cited, out of belief — when
  a model proposed it. Neither outcome is a silent commit

- Change data capture as first-class ingestion: a generic change contract, a JSONL replay
  adapter, per-stream checkpoints, and deterministic column-to-claim mapping with no model
  involved. A row update supersedes only what moved and leaves every other assertion
  untouched — measured by assertion identity, not by counting rows — and a deleted row is
  retracted rather than erased

- Attribute-based policy that returns filters rather than verdicts: a decision carries the
  classification ceiling, source, predicate, and type narrowing that every retriever pushes
  into its own SQL, so unauthorized rows are never read, never ranked, and never counted.
  Cross-workspace isolation is tested through lexical, vector, graph, entity, context,
  provenance, export, and trace paths rather than asserted

- Memory lifecycle: expiry that takes a claim out of active context while leaving it true,
  cited, and answerable as of an earlier instant; decay that reorders results and never
  removes them; consolidation that turns repeated observation into a derived fact naming
  every observation behind it; and forgetting as four named operations rather than one
  ambiguous delete flag

- An MCP tool surface an agent can call — search, context, ingest, entity, assertion, explain,
  temporal query — carrying canonical ids so an agent follows references instead of asking for
  bigger payloads, and running under the same policy as every other caller
- Portable context packages: a streamed, chain-digested JSONL format that rebuilds knowledge in
  an empty instance. An importer trusts nothing in it — identifiers are provenance rather than
  identity, knowledge time is the importer's own, and nothing is committed until the digest
  over every record verifies

- Distributed operation: a worker fleet sharing one queue without duplicate delivery,
  optional push delivery through NATS JetStream that carries notice rather than authority,
  partition keys that keep successive versions of one upstream record in order, per-worker
  rate limiting, and monotonic projection checkpoints that name the worker that advanced them

**Not built yet** (phase 15): dedicated graph, vector, lexical, and S3-compatible blob
storage adapters behind the interfaces the earlier phases stabilized.

## Quickstart

Requirements: Go 1.26 (a `toolchain` directive fetches it automatically) and PostgreSQL 16
with pgvector — either installed locally or through Docker. `scripts/dev-postgres.sh` uses
whichever it finds.

```bash
# 1. A local PostgreSQL cluster
./scripts/dev-postgres.sh start

# 2. Configuration
set -a && source configs/dev.env && set +a
export CG_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:55432/strata?sslmode=disable"

# 3. A credential. Save the printed JSON to $CG_API_KEYS_FILE.
go run ./cmd/cgctl keygen --principal ops-admin --system-role admin

# 4. Schema
go run ./cmd/cgctl migrate

# 5. Scope and a source
go run ./cmd/cgctl workspace create --slug acme --name "Acme Corp"
go run ./cmd/cgctl graph-space create --workspace acme --slug main --name Main
go run ./cmd/cgctl source register --workspace acme --kind chat --name support-chat
GS=$(go run ./cmd/cgctl graph-space ls --workspace acme | awk 'NR==2 {print $1}')

# 6. Ingest, twice, with the same key
go run ./cmd/cgctl ingest file --graph-space "$GS" --source support-chat \
    --file testdata/chat/session-01.json --idempotency-key demo-1
go run ./cmd/cgctl ingest file --graph-space "$GS" --source support-chat \
    --file testdata/chat/session-01.json --idempotency-key demo-1   # duplicate: true

# 7. Process it
go run ./cmd/cgworker &     # or set CG_EMBEDDED_WORKER=true on the server
go run ./cmd/cgctl event status --id <source_event_id>

# Run several workers to scale out; they share one queue and never take the same item
# twice. For push delivery instead of polling, start a broker and point them at it:
#   ./scripts/dev-nats.sh start
#   export CG_NATS_URL=nats://127.0.0.1:14222

# 8. Ask it something
go run ./cmd/cgctl search --graph-space "$GS" --query "who confirmed the renewal" --explain

# 9. Get context for a prompt, with citations, inside a token budget
go run ./cmd/cgctl context --graph-space "$GS" --query "who confirmed the renewal" --budget 800
```

Over HTTP:

```bash
go run ./cmd/contextgraphd

curl -s localhost:8080/readyz

curl -s -X POST "localhost:8080/v1/graph-spaces/$GS/episodes" \
  -H "Authorization: Bearer <key_id>.<secret>" \
  -H "Idempotency-Key: turn-1" \
  -d '{"source_name":"support-chat","content":"Alice Chen confirmed the renewal."}'

curl -s -X POST "localhost:8080/v1/graph-spaces/$GS/query" \
  -H "Authorization: Bearer <key_id>.<secret>" \
  -d '{"query":"who confirmed the renewal","explain":true}'

curl -s -X POST "localhost:8080/v1/graph-spaces/$GS/context" \
  -H "Authorization: Bearer <key_id>.<secret>" \
  -d '{"query":"who confirmed the renewal","token_budget":800}'
```

See [docs/api/ingest.md](docs/api/ingest.md) for the full surface.

## Development

```bash
gofmt -w .
go vet ./...
./scripts/check-domain-deps.sh    # internal/domain must stay infrastructure-free
go test ./...
go test -race ./...
```

Integration tests use real PostgreSQL, resolved in this order:

1. `TEST_DATABASE_URL`, if set.
2. A throwaway cluster booted from a local installation (Debian packages, Homebrew, or
   Postgres.app; override the location with `PG_BINDIR`).
3. A server already listening on `127.0.0.1:55432` or `127.0.0.1:55433` — the ports
   `scripts/dev-postgres.sh` uses — provided it offers pgvector and pg_trgm. A server
   without those extensions is passed over rather than used and failed against.

So on a machine with Docker and no PostgreSQL installation, `scripts/dev-postgres.sh start`
followed by `go test ./...` works with nothing exported. The default 5432 is deliberately
never probed: the harness creates and drops databases, and on most machines that port is
someone's real database.

**Set `CG_REQUIRE_PG=1` to turn a skip into a failure** - without it a run with no database
reports every package green while quietly skipping the integration tests, which is exactly
the false confidence to avoid. CI sets it.

To point at a specific server instead:

```bash
export TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"
export CG_REQUIRE_PG=1
go test ./...
```

pgvector is required, not optional — the projections migration creates the extension, so
plain `postgres:16` connects fine and then fails every integration test. The
`pgvector/pgvector:pg16` image that `scripts/dev-postgres.sh` and CI use has it; for a local
installation, `brew install pgvector` or `apt install postgresql-16-pgvector` adds it. See
[ADR 0007](docs/adr/0007-pgvector-for-the-vector-projection.md).

Segmentation and chunk boundaries are covered by golden files. When a change to them is
intended:

```bash
go test ./internal/normalize -update    # then review the diff
```

Performance targets are benchmarked separately, because they load a corpus and take minutes:

```bash
./scripts/benchmark.sh
```

The invariants that go with them — bounded traversal, bounded context, no full scan for
semantic search — are ordinary tests and run with everything else. See
[docs/api/performance.md](docs/api/performance.md) for the current baseline and the
conditions it was measured under.

## Layout

```
cmd/contextgraphd   HTTP API server, optionally with an in-process worker
cmd/cgworker        Outbox consumer and pipeline runner
cmd/cgctl           Administration and development CLI
cmd/cgmcp           MCP server over stdio
internal/domain     Pure canonical model: typed ids, enums, temporal coordinates
internal/embedding  Provider-independent embeddings for the vector projection
internal/extraction Source material into candidate knowledge, safely
internal/ingest     The one path by which knowledge enters
internal/knowledge  Claims into committed canonical knowledge
internal/llm        Provider-independent model interface and adapters
internal/normalize  Deterministic decode, segment, chunk
internal/resolution Which identity a mention refers to
internal/pipeline   Stage orchestration and durable stage state
internal/projection Rebuildable vector, lexical, and graph indexes
internal/eventbus   Work fabric: the transactional outbox
internal/identity   Authentication and scope resolution
internal/store      Canonical ledger and blob storage
internal/api/http   HTTP surface
migrations          Explicit SQL, applied in order, checksum-verified
docs/adr            Architecture decision records
```

`internal/domain` depends on nothing but the standard library and a UUID package. Ports
are declared by the services that consume them; implementations live under `store`,
`eventbus`, and the other infrastructure packages.

## Documentation

- [docs/architecture/overview.md](docs/architecture/overview.md) — how the pieces fit and
  which invariants each one enforces
- [docs/api/ingest.md](docs/api/ingest.md) — ingestion API, idempotency semantics, error codes
- [docs/api/knowledge.md](docs/api/knowledge.md) — entities, assertions, temporal queries,
  provenance
- [docs/api/extraction.md](docs/api/extraction.md) — model configuration, injection
  defenses, model runs
- [docs/api/resolution.md](docs/api/resolution.md) — the evidence ladder, reversible
  merges, the decision ledger
- [docs/api/projections.md](docs/api/projections.md) — vector, lexical, and graph
  projections, and rebuilding them
- [docs/api/retrieval.md](docs/api/retrieval.md) — query planning, the five retrievers,
  fusion, filters, and explain
- [docs/api/context.md](docs/api/context.md) — prompt-ready blocks, the token budget,
  citations, and the injection boundary
- [docs/api/ontology.md](docs/api/ontology.md) — schema versions, open and guided modes,
  validation, and what happens to what a schema refuses
- [docs/api/cdc.md](docs/api/cdc.md) — the change contract, mappings, checkpoints,
  tombstones, and out-of-order handling
- [docs/api/security.md](docs/api/security.md) — grants, ABAC policy, clearances, audit,
  traces, and export
- [docs/api/memory.md](docs/api/memory.md) — expiry, decay, consolidation, and the four
  ways of forgetting
- [docs/api/mcp.md](docs/api/mcp.md) — the MCP tool surface, and portable packages with
  their integrity manifests
- [docs/api/distributed.md](docs/api/distributed.md) — running a worker fleet, push
  delivery, partition keys, backpressure, and distributed checkpoints
- [docs/api/performance.md](docs/api/performance.md) — measured targets, the benchmark
  harness, and the invariants checked on every commit
- [docs/api/backup-and-recovery.md](docs/api/backup-and-recovery.md) — what a backup must
  contain, what rebuilds, and how to prove the difference
- [docs/adr/](docs/adr/) — decisions, alternatives, and trade-offs
