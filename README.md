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

Phases 0 through 6 of the contract are implemented: the architecture skeleton, the
canonical ingestion ledger, the assertion-first knowledge model, schema-constrained
extraction, conservative entity resolution, multi-temporal reconciliation, and rebuildable
retrieval projections.

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

**Not built yet** (later phases, in order): hybrid retrieval, context assembly, ontology mode, CDC connectors, ABAC policy, memory lifecycle,
MCP, and distributed operation.

## Quickstart

Requirements: Go 1.26 (a `toolchain` directive fetches it automatically) and PostgreSQL
16. No Docker needed.

```bash
# 1. A local PostgreSQL cluster
./scripts/dev-postgres.sh start

# 2. Configuration
set -a && source configs/dev.env && set +a
export CG_DATABASE_URL="postgres://postgres@127.0.0.1:55432/strata"

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
```

Over HTTP:

```bash
go run ./cmd/contextgraphd

curl -s localhost:8080/readyz

curl -s -X POST "localhost:8080/v1/graph-spaces/$GS/episodes" \
  -H "Authorization: Bearer <key_id>.<secret>" \
  -H "Idempotency-Key: turn-1" \
  -d '{"source_name":"support-chat","content":"Alice Chen confirmed the renewal."}'
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

Integration tests use real PostgreSQL. They pick up `TEST_DATABASE_URL` if set,
otherwise boot a throwaway cluster from a local installation (Debian packages, Homebrew,
or Postgres.app; override the location with `PG_BINDIR`), and skip if neither is
possible. **Set `CG_REQUIRE_PG=1` to turn that skip into a failure** - without it a run
with no database reports every package green while quietly skipping the integration
tests, which is exactly the false confidence to avoid. CI sets it.

With no local PostgreSQL, a container works:

```bash
docker run -d --name strata-pg -e POSTGRES_PASSWORD=postgres -p 55432:5432 pgvector/pgvector:pg16
export TEST_DATABASE_URL="postgres://postgres:postgres@127.0.0.1:55432/postgres"
export CG_REQUIRE_PG=1
go test ./...
```

That image is the stock `postgres:16` with the pgvector extension available, which is what
CI uses. Nothing needs the extension yet; it is there so the vector projection in phase 6
does not require changing how anyone runs the tests. Plain `postgres:16` also works today.
For a local installation without Docker, `brew install pgvector` or
`apt install postgresql-16-pgvector` adds it. See
[ADR 0007](docs/adr/0007-pgvector-for-the-vector-projection.md).

Segmentation and chunk boundaries are covered by golden files. When a change to them is
intended:

```bash
go test ./internal/normalize -update    # then review the diff
```

## Layout

```
cmd/contextgraphd   HTTP API server, optionally with an in-process worker
cmd/cgworker        Outbox consumer and pipeline runner
cmd/cgctl           Administration and development CLI
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
  retrieval, and rebuilding them
- [docs/adr/](docs/adr/) — decisions, alternatives, and trade-offs
