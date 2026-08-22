# HTTP API

Base path `/v1`. Requests and responses are JSON. Every error carries a stable machine-
readable code; raw database and provider errors are never returned.

## Authentication

Send an API key as a bearer token:

```
Authorization: Bearer <key_id>.<secret>
```

Mint one with `cgctl keygen` and store the printed entry in the file named by
`CG_API_KEYS_FILE`. Only a SHA-256 digest of the secret is stored.

Authenticated identity decides what a caller can reach. A request cannot widen its own
scope: the workspace is always derived from the graph space in the path, never from the
body. Access to another tenant's resource is reported as `404`, not `403`, because
confirming existence would itself leak information.

Roles are hierarchical: `reader` < `writer` < `admin` < `owner`. Workspace access comes
from a grant; the `system_role` in the key file only gates operations that exist outside
any workspace, such as creating one.

## Health

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/healthz` | none | Liveness. Touches no dependency, so a database outage does not cause healthy processes to be killed |
| GET | `/readyz` | none | Readiness. Checks the database, that the applied schema matches this binary's migrations, and that the blob store is writable |

`/readyz` returns `503` with per-check detail when any check fails.

## Scope administration

| Method | Path | Requires |
|---|---|---|
| POST | `/v1/workspaces` | `admin` system role |
| GET | `/v1/workspaces` | any principal (returns only granted workspaces) |
| POST | `/v1/workspaces/{workspace_id}/graph-spaces` | workspace `admin` |
| GET | `/v1/workspaces/{workspace_id}/graph-spaces` | workspace `reader` |
| POST | `/v1/workspaces/{workspace_id}/sources` | workspace `admin` |
| GET | `/v1/workspaces/{workspace_id}/sources` | workspace `reader` |
| POST | `/v1/workspaces/{workspace_id}/grants` | workspace `owner` |
| POST | `/v1/graph-spaces/{graph_space_id}/collections` | workspace `admin` |

Creating a workspace grants its creator ownership in the same transaction, so no
workspace can exist that nobody may administer.

```bash
curl -X POST localhost:8080/v1/workspaces \
  -H "Authorization: Bearer $CRED" \
  -d '{"slug":"acme","name":"Acme Corp"}'
```

Registering a source declares how much its content should be trusted and how sensitive it
is. Both default conservatively: `trust_level` to `standard`, `classification` to
`internal`.

```bash
curl -X POST "localhost:8080/v1/workspaces/$WS/sources" \
  -H "Authorization: Bearer $CRED" \
  -d '{"kind":"database","name":"crm","uri":"postgres://crm/customers",
       "trust_level":"high","classification":"confidential"}'
```

## Ingestion

Three routes, one path through the system. All require workspace `writer`.

| Method | Path | Body |
|---|---|---|
| POST | `/v1/graph-spaces/{graph_space_id}/events` | one event object, or an array of them |
| POST | `/v1/graph-spaces/{graph_space_id}/documents` | the raw document, with its type in `Content-Type` |
| POST | `/v1/graph-spaces/{graph_space_id}/episodes` | one already-segmented unit |

### Event fields

| Field | Notes |
|---|---|
| `source_id` or `source_name` | Required. Must belong to the resolved workspace |
| `content` or `content_json` | Required, exactly one. `content_json` avoids escaping structured payloads |
| `media_type` | `application/json`, `text/markdown`, or `text/plain`. Anything else is treated as plain text |
| `operation` | `upsert` (default), `delete`, `append`, `snapshot`, `correction` |
| `external_id`, `source_version`, `source_sequence` | Upstream identity and ordering |
| `event_time`, `source_time`, `source_commit_time` | World and source clocks, RFC 3339. Knowledge time is set by the system, never by the caller |
| `idempotency_key` | Also accepted as the `Idempotency-Key` header |
| `classification` | May raise the source's sensitivity, never lower it |
| `collection_id`, `event_type`, `metadata` | Optional |

Unknown fields are rejected rather than ignored: silently dropping a misspelled field is
how a caller ends up believing it set a temporal bound that never took effect.

### Idempotency

An idempotency key is either supplied or derived from
`(source, external_id, source_version, content_hash)`.

| Situation | Status | Meaning |
|---|---|---|
| New submission | `202 Accepted` | Durably committed with its work queued |
| Replay, identical content | `200 OK`, `duplicate: true` | The original event, not a second one |
| Same key, different content | `409 source_event_conflict` | Refused, because accepting it would discard one payload |
| Same `(external_id, source_version)`, different content | `409 source_event_conflict` | The source reused a version for changed data |
| Upstream update with no version | `202 Accepted` | A new event: an update is new knowledge, not a duplicate |

`202` means the event and its work item are committed, not that processing has finished.

### Batch

An array on `/events` runs each item through the identical single-event path. Items
succeed or fail independently, so one malformed record cannot discard the rest.

```json
{
  "accepted": 2,
  "failed": 1,
  "results": [
    {"index": 0, "receipt": {"source_event_id": "...", "duplicate": false}},
    {"index": 1, "error": {"code": "not_found", "message": "source not found"}},
    {"index": 2, "receipt": {"source_event_id": "...", "duplicate": false}}
  ]
}
```

The response is `207 Multi-Status` when any item failed; a single overall status would
hide either the failures or the durable successes. An `Idempotency-Key` header is not
applied to batch items, since one key across many events would collapse them into one.

### Documents

```bash
curl -X POST "localhost:8080/v1/graph-spaces/$GS/documents?source_name=uploads&external_id=handbook.md" \
  -H "Authorization: Bearer $CRED" \
  -H "Content-Type: text/markdown" \
  -H "Idempotency-Key: handbook-v1" \
  --data-binary @handbook.md
```

Query parameters mirror the event fields: `source_id`, `source_name`, `external_id`,
`event_type`, `operation`, `classification`, `source_version`, `source_sequence`,
`collection_id`.

### Episodes

For callers that have already segmented their input - one conversation turn, one tool
result. It becomes an ordinary source event with `operation: append`; only segmentation is
skipped, so provenance and replay behave exactly as they do elsewhere.

## Processing status

```
GET /v1/events/{event_id}/status
```

Requires workspace `reader`. The workspace is resolved from the caller's grants, so an
identifier from another tenant reports `404`.

```json
{
  "source_event_id": "01a026c2-8473-78b8-b3c2-4032104eda7f",
  "status": "processed",
  "operation": "upsert",
  "content_hash": "fc891f...",
  "episodes": 4,
  "chunks": 4,
  "pipeline": {
    "version": 1,
    "status": "succeeded",
    "stages": [
      {"name": "normalize", "version": 1, "status": "succeeded", "attempts": 1,
       "output": {"media_type": "application/json", "segment_count": 4}},
      {"name": "segment", "version": 1, "status": "succeeded", "attempts": 1,
       "output": {"episode_count": 4}},
      {"name": "chunk", "version": 1, "status": "succeeded", "attempts": 1,
       "output": {"chunk_count": 4, "token_total": 90}}
    ]
  },
  "work": [{"id": "...", "status": "succeeded", "attempts": 1}]
}
```

Event status describes processing progress only. It never asserts that the content is
true.

Lifecycle: `accepted` → `processing` → `processed`, or `failed` when a stage fails
permanently. A stage that failed transiently stays retryable and the event remains
`processing`; a permanently invalid payload leaves its stage `dead` and the event
`failed`, with the source event and its archived bytes still intact.

## Errors

```json
{"error": {"code": "source_event_conflict", "message": "...", "request_id": "01a0..."}}
```

| Code | Status |
|---|---|
| `invalid_argument` | 400 |
| `unauthenticated` | 401 |
| `permission_denied` | 403 |
| `not_found`, `workspace_not_found`, `graph_space_not_found` | 404 |
| `conflict`, `source_event_conflict` | 409 |
| `ontology_violation`, `temporal_conflict` | 422 |
| `rate_limited` | 429 |
| `internal` | 500 |
| `projection_not_ready`, `provider_unavailable` | 503 |

Every response carries `X-Request-Id`, echoed from the request when supplied, and the same
value appears in logs and in error bodies.

## Not yet available

Retrieval (`POST /v1/query`), context assembly (`POST /v1/context`), entity and assertion
reads, retrieval traces, and the MCP surface arrive in later phases. See
[AGENTS.md](../../AGENTS.md) section 36.
