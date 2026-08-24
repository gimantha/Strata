# Change data capture

A database row change is a source event like any other (AGENTS.md sections 10.1 and 11).
It goes through the same gateway as a document upload, which is what keeps idempotency,
archival, and the outbox commit identical for every source.

Knowledge is **derived from changes, never rebuilt on them**. An update supersedes the claims
whose values moved and leaves every other claim exactly as it was — same assertion id, still
active. The reasoning is in [ADR 0015](../adr/0015-cdc-is-events-not-rebuilds.md).

## The change contract

One JSON object per change, in the shape every CDC pipeline already carries:

```json
{
  "stream": "public.customers",
  "operation": "update",
  "key": {"id": 42},
  "before": {"id": 42, "name": "Acme Corporation", "tier": "STANDARD"},
  "after":  {"id": 42, "name": "Acme Corporation", "tier": "PREMIUM"},
  "commit_time": "2026-02-01T00:00:00Z",
  "offset": "0/2000",
  "sequence": "102",
  "transaction": "tx-9",
  "schema_version": "3"
}
```

| Field | Why it matters |
|---|---|
| `stream` | The unit that carries a mapping and a checkpoint |
| `key` | Ties every change about one row together; without it an update looks like a new row |
| `operation` | `insert`, `update`, `delete`, `snapshot` |
| `before` / `after` | The row images. A delete usually has only `before` |
| `offset` | The connector's position — LSN, file offset, Kafka offset. What a checkpoint stores |
| `sequence` | Orders events within a stream. Ordering uses this, never arrival order |
| `commit_time` | Source time, not our time |

Only `stream`, `operation`, and `key` are required, plus an image for anything but a delete.

## Mapping rows to knowledge

A mapping says how a stream's columns become claims. Deterministic, no model involved — a row
is already structured, and reading the columns beats asking a model to rediscover them.

```json
{
  "subject_type": "organization",
  "subject_name_column": "name",
  "identifier_namespace": "erp.customer_id",
  "valid_from_column": "effective_from",
  "columns": [
    {"column": "name", "predicate": "LEGAL_NAME"},
    {"column": "tier", "predicate": "TIER", "object_kind": "symbol"},
    {"column": "credit_limit", "predicate": "CREDIT_LIMIT", "object_kind": "integer"},
    {"column": "region", "predicate": "REGION", "skip_empty": true},
    {"column": "owner", "predicate": "OWNED_BY", "object_entity_type": "person"}
  ]
}
```

- `identifier_namespace` registers the primary key as a stable identifier. This is rung one of
  the resolution ladder and the reason CDC identities do not drift: two rows with the same key
  are the same subject however the name is spelled today.
- `object_entity_type` makes a column a relation rather than a literal.
- `skip_empty` omits the claim when a column is null. A null usually means "not recorded", not
  "recorded as nothing".
- Columns are **functional by default** — one column, one current value — which is what makes
  an update supersede the old value instead of accumulating beside it. A value that will not
  parse as its declared kind falls back to a string rather than failing the ingest.

Registering a mapping writes its predicates into the registry with those semantics, so
nothing has to be guessed at first use.

```bash
cgctl stream register --graph-space $GS --source erp --stream public.customers \
    --mapping customers.json
cgctl stream ls --graph-space $GS
```

## Getting changes in

**Pull**, from a JSONL change log — the reference adapter:

```bash
cgctl stream replay --graph-space $GS --source erp --stream public.customers \
    --file changes.jsonl
```

**Push**, for upstreams that can only write to you:

```
POST /v1/graph-spaces/{graph_space_id}/changes
{"source_name": "erp", "stream": "public.customers", "changes": [ ... ]}
```

Returns `202` with counts: the changes are durable, and turning them into knowledge happens on
the pipeline.

## Checkpoints and replay

A checkpoint advances only **after** its events are durably accepted. A crash in between
replays them, and replaying is free — the gateway keys on each change's idempotency key and
returns the original event. Checkpointing first would silently lose changes, which is the one
failure a CDC pipeline cannot recover from.

Idempotency comes from the keys, not from the bookmark. Replaying a whole log with `--resume=false`
changes nothing either, which is what makes rebuilding a workspace from an archive safe.

## Deletes are retractions

A deleted row means the source stopped claiming the record, not that the record was never true
(AGENTS.md section 11.3). Every current claim from that row is retracted with a reason; the
claims keep their evidence and stay answerable as of any earlier knowledge time.

Privacy erasure is a separate workflow with separate authorization. Conflating the two would
make "delete this row" a data-destruction primitive any upstream could fire.

## Out-of-order changes

Ordering uses the source's own sequence, never arrival. A stream that delivers 102 before 101
converges to the same state either way: whichever arrives second is compared against what is
already known, and the older one is recorded but never becomes current belief.

A source correcting itself is not a conflict. When the same source's ordering puts a competing
claim earlier, the new claim supersedes it directly — no policy, no authority weighting, no
conflict set. Disagreement between *different* sources still goes to authority resolution,
which refuses to pick between equals.

## What gets indexed

The archived payload is the whole envelope — before image, transaction, offset, schema version
— because that is what makes a disputed fact explicable later. None of it belongs in a search
index, so the row itself is rendered as `column: value` lines and only that is projected:

```
public.customers update public.customers:id=42
credit_limit: 50000
id: 42
name: Acme Corporation
tier: PREMIUM
```
