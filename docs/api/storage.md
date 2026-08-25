# Storage backends

Which storage each concern uses, what can be swapped, and how a swap is proven safe
(AGENTS.md phase 15, section 3).

## Current backends

| Concern | Port | Backends | Selected by |
|---|---|---|---|
| Canonical ledger | — | PostgreSQL 16 | not swappable; see below |
| Raw archive | `blob.Store` | filesystem, S3-compatible | `CG_BLOB_BACKEND` |
| Vector projection | `index.Vectors` | pgvector (HNSW) | `index.Set` |
| Lexical projection | `index.Lexical` | PostgreSQL `tsvector` + pg_trgm | `index.Set` |
| Graph projection | `index.Graph` | PostgreSQL `graph_edges` | `index.Set` |

The ledger is deliberately not swappable. Multi-temporal reconciliation, supersession, and
the transactional outbox all depend on one database committing them together
([ADR 0001](../adr/0001-postgresql-canonical-ledger.md), section 28.1). The projections are
rebuildable and therefore replaceable in principle; whether they should be replaced is a
question [performance.md](performance.md) has opened and not yet answered.

## Raw archive

```bash
CG_BLOB_BACKEND=fs                       # default
CG_BLOB_DIR=./.data/blobs

CG_BLOB_BACKEND=s3
CG_S3_BUCKET=strata-artifacts
CG_S3_PREFIX=production                  # namespaces keys when deployments share a bucket
CG_S3_ENDPOINT=http://127.0.0.1:19000    # empty uses AWS's own endpoints
CG_S3_REGION=us-east-1
CG_S3_PATH_STYLE=true                    # required by most self-hosted stores
```

Credentials may be omitted entirely, in which case the ambient chain is used — environment,
shared config, instance role — so a deployment on AWS holds no credentials in its
configuration. When set, they are never logged: the redacted config prints the bucket,
prefix, and endpoint only.

**Any S3-compatible store works.** MinIO, Cloudflare R2, Ceph, and Backblaze address the same
API; the endpoint and path-style options are what make that true rather than aspirational.
The tests run against MinIO for exactly this reason — an adapter claiming compatibility and
tested only against AWS has an untested claim.

**Why an object store.** The filesystem has no versioning and no replication of its own, so
protecting the bytes every claim is cited to becomes the deployment's problem. A
`source_events` row whose artifact is missing is a claim that can no longer be walked back to
its source: the provenance chain ends in a 404, which is worse than the claim being absent
because it still looks answerable. See
[backup-and-recovery.md](backup-and-recovery.md).

**The bucket is not created for you.** Bucket lifecycle, versioning, and replication are the
deployment's to configure — they are the whole reason to use an object store. An adapter that
created buckets silently would make a typo in configuration look like a working deployment,
with the bytes going somewhere nobody backs up.

### Switching backends

`Artifact.Storage` has recorded which backend wrote each row since phase 1, so
filesystem-era and object-store-era artifacts coexist and are distinguishable. Changing
`CG_BLOB_BACKEND` changes where *new* artifacts go; it does not move existing ones, and
reading an artifact written by a backend that is no longer configured will fail. Moving them
is a separate operation and there is no tool for it yet.

## The conformance suite

Two backends compiling against one interface prove nothing about behaviour — the compiler
checks signatures and says nothing about what the methods do. So `blob.RunConformance` is
exported from the non-test file and run by both backends' tests, covering the parts that
genuinely differ between a filesystem and an object store:

- absence is `blob.ErrNotFound` from **both** `Get` and `Stat`
- `Put` is idempotent, because keys are content addresses and replays are normal
- `Delete` is idempotent, so cleanup need not distinguish "gone now" from "gone already"
- empty content round trips, so a zero-byte document is not indistinguishable from a missing
  artifact
- `Healthy` reports the truth, and `Name()` is non-empty because artifact rows depend on it

A third backend added later inherits the same bar rather than a subset somebody
reimplements. See [ADR 0020](../adr/0020-one-blob-port-two-backends-one-conformance-suite.md).

## Running the object store locally

```bash
./scripts/dev-minio.sh start     # port 19000, credentials strata / strata-secret
CG_REQUIRE_S3=1 go test ./...
```

Port 19000 rather than 9000, for the same reason the PostgreSQL harness avoids 5432 and the
NATS one avoids 4222: a developer's own server should not have this project's buckets created
and dropped in it. `CG_REQUIRE_S3=1` turns a missing store into a failure rather than a skip,
which is how CI avoids losing the coverage silently.

## The index ports

Three ports, one per projection, in `internal/store/index`
([ADR 0021](../adr/0021-three-index-ports-not-one-store.md)). A deployment builds an
`index.Set` and hands the same one to the projector and the retriever:

```go
indexes := index.Set{
    Vectors: myVectorBackend,
    Lexical: store.Indexes().Lexical,   // still PostgreSQL
    Graph:   store.Indexes().Graph,
}
```

Three ports rather than one, because a deployment putting its vectors elsewhere must not be
made to implement full-text search and graph traversal to do it. Read and write stay
together in each port: the fields a record is written with are the fields a query filters
on, and no round-trip conformance test can span two interfaces a backend might implement
inconsistently.

A partially configured set degrades rather than panics. The projector writes the projections
it has, and the retriever skips a mode whose index is absent — so a deployment can move one
projection without losing the other two while it does so.

Each port has an exported conformance suite — `index.RunVectorConformance`,
`index.RunLexicalConformance` — run by PostgreSQL as well as by any substitute. A suite run
only by the newcomer encodes whatever its author assumed the incumbent did.

### What is substitutable, and what is not

Four of the five retrieval modes: **lexical**, **exact**, **vector**, and **graph**.
`SearchVectors` and `SearchLexical` are single-table queries joining nothing canonical, and
`Expand` reports identifiers only — the retriever names them in one batched canonical read,
so a graph backend needs to hold nothing but edges.

**Entity resolution is not.** `FindEntitiesByName` joins `entities` to `entity_aliases` —
canonical tables — so a name-to-identity lookup stays on the ledger however the indexes are
configured. It is on `retrieval.Ledger`, not on any index port.

Two consequences of traversal reporting identifiers, both deliberate. A walk seeded with an
identifier from another workspace returns nothing rather than echoing it back, since edges
are scoped and a foreign root can reach nothing — the tenancy check the removed canonical
join used to provide. And an edge pointing at an entity the ledger no longer has is dropped
during hydration, which PostgreSQL's foreign keys make impossible but an eventually-consistent
backend does not.

## What phase 15 has not built

Backends behind the three ports other than PostgreSQL. The ports exist and the projections
are rebuildable, so adding one is now possible without implementing the ledger; whether it is
needed is the open question in [performance.md](performance.md) — the measured finding there
is that the vector index is correctly built and simply not reached by scoped queries, which
points at a query-shape fix rather than at a different database.
