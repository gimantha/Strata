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

Phases 0 and 1 of the contract are implemented: the architecture skeleton and the
canonical ingestion ledger.

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

**Not built yet** (later phases, in order): entities and assertions, extraction, entity
resolution, temporal reconciliation, retrieval projections, hybrid retrieval, context
assembly, ontology mode, CDC connectors, ABAC policy, memory lifecycle, MCP, and
distributed operation.

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

Integration tests use real PostgreSQL. They pick up `TEST_DATABASE_URL` if set, otherwise
boot a throwaway cluster with `initdb`, and skip if neither is possible. Set
`CG_REQUIRE_PG=1` to turn that skip into a failure, as CI does.

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
internal/ingest     The one path by which knowledge enters
internal/normalize  Deterministic decode, segment, chunk
internal/pipeline   Stage orchestration and durable stage state
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
- [docs/api/ingest.md](docs/api/ingest.md) — HTTP API, idempotency semantics, error codes
- [docs/adr/](docs/adr/) — decisions, alternatives, and trade-offs
