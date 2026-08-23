# Architecture overview

This describes what exists after phases 0 through 2, and how each part enforces the
invariants in [AGENTS.md](../../AGENTS.md) section 2. Later phases extend this picture
without changing its shape.

## The shape of the system

```text
        Sources: chat, documents, database CDC, API, code, JSON, tool events
                                     |
                                     v
  INGESTION GATEWAY   authenticate -> resolve scope -> archive raw -> SourceEvent
                                                    + outbox row, one transaction
                                     |
                                     v
                          WORK FABRIC (transactional outbox)
                                     |
                                     v
        PIPELINE   normalize -> segment -> chunk        [phase 1 stages]
                   -> extract -> resolve entities -> reconcile temporally   [later]
                                     |
                                     v
  CANONICAL LEDGER   Source | SourceEvent | Artifact | Episode | Chunk
                     Entity | Assertion | Evidence | Derivation | ConflictSet
                                     |
                                     v
        PROJECTIONS   graph | vector | lexical | summaries                  [later]
                                     |
                                     v
        RETRIEVAL and CONTEXT ASSEMBLY                                      [later]
```

Everything down to and including the ledger is implemented. The projections and retrieval
below it are deliberately absent rather than stubbed: an empty stage that recorded success
would make replay state lie.

Claims currently enter the ledger through the assertion API rather than from extraction.
Phase 3 adds extraction stages to the pipeline, which will call the same
`knowledge.Assert` path the API uses.

## Where each invariant is enforced

| Invariant | Where |
|---|---|
| Raw source is preserved | The gateway archives content-addressed bytes **before** committing the event; `artifacts` holds the address, and the pipeline fails loudly if the bytes are gone |
| The canonical ledger is authoritative | Episodes and chunks are derived: deleting them and reprocessing reproduces them exactly, which an integration test asserts |
| All ingestion is event-shaped | Every route, including batch and raw-document upload, calls the same `Gateway.Accept` |
| Temporal semantics are first-class | `source_events` and `assertions` store world, knowledge, source, and lifecycle clocks as distinct columns; corrections supersede rather than overwrite, so `valid_at` and `known_at` are independently queryable |
| Assertions are immutable | Only knowledge-time markers change after commit: status, `superseded_at`, `retracted_at`, `conflict_set_id`. Content and world validity are never edited |
| Provenance is walkable | Every claim reaches its evidence, chunk, episode, artifact, source event, and source in one query; reasoned claims also name their derivation and inputs |
| Contradictions are visible | Overlapping claims for a functional predicate become a conflict set with both sides kept and marked disputed |
| Tenant scope is mandatory | Every canonical table carries `workspace_id`, and every query takes it as an argument; scope is resolved from identity, never from a request body |
| Idempotency is mandatory | Unique indexes on the idempotency key and on upstream identity; episodes and chunks keyed by sequence; the outbox keyed by dedupe key; stage runs keyed by stage and version |
| Observability is part of correctness | Every ingest and stage is traced, W3C trace context is carried on outbox rows across the async boundary, and queue lag is an observable gauge |
| No hidden global graph | There is no unscoped list or read path anywhere, including administrative listings |
| Provider independence | `internal/domain` imports nothing but the standard library and a UUID package, enforced by `scripts/check-domain-deps.sh` in CI |

## Ingestion, step by step

1. **Authenticate.** An API key resolves to a principal with its current workspace
   grants, read per request so a revocation takes effect immediately.
2. **Resolve scope.** The graph space in the URL is looked up, and *its* workspace is the
   one used. A caller naming a different workspace changes nothing. Denials on another
   tenant's resource are reported as absence, because existence is itself information.
3. **Archive.** The payload is hashed and written to blob storage under
   `<workspace>/sha256/<aa>/<bb>/<hash>`. Identical bytes are stored once. Writes are
   atomic, so a crash cannot leave a partial object that later reads as complete source.
4. **Commit, once.** One transaction inserts the artifact row, the source event, its
   pipeline run, its outbox work item, and an audit record. There is no "commit then
   publish" window in either direction.
5. **Acknowledge.** The caller gets a receipt as soon as that transaction commits.
   Extraction is not on the request path.

Replay behavior is the interesting part. The idempotency key is either supplied by the
caller or derived from `(source, external id, source version, content hash)`. A resend of
identical content returns the original event with `duplicate: true`. The same key with
*different* content is a `source_event_conflict`, because accepting it would silently
discard one of the two payloads. An upstream row that changes without a version number
still appends a new event, since an update is new knowledge rather than a duplicate.

## The work fabric

Work is claimed with `FOR UPDATE SKIP LOCKED`, so many workers share one queue without
locking each other out or double-processing an item. Each claim carries a lease that its
worker heartbeats while working. A worker that dies stops renewing; the reaper returns
the expired claim to the queue and another worker finishes it. That is the whole
mechanism by which accepted work cannot be lost, and it is covered by a test that
abandons a lease and asserts the item still completes exactly once.

Failures are classified rather than retried uniformly. Transient faults, rate limits, and
storage conflicts back off with equal jitter. Malformed input, schema violations, and
policy rejections go straight to the dead-letter state, because waiting will not make
them valid. Nothing is discarded: dead items keep their cause and can be revived once it
is fixed.

## The pipeline

Stages are keyed by `(workspace, source event, pipeline version, stage name, stage
version)`. A stage that already succeeded is skipped, so redelivery is nearly free.
Stages read from the ledger rather than from each other, which costs one cheap read and
buys the ability to re-run any single stage, years later, from durable state alone.
Incrementing a stage version re-runs it for events that passed the previous version.

The three phase-1 stages are deliberately model-free. Structure already present in the
source is read directly: conversation turns from a message array, sections from markdown
headings, records from a JSON array, primary keys and timestamps from the fields that
carry them. An LLM is not used to rediscover what the source already states.

Every derived unit keeps positional provenance. Chunk offsets reproduce their episode's
bytes exactly, and an episode's locator reproduces its position in the normalized
artifact text. A fuzz test holds that contract, because provenance that cannot reproduce
a quote is not provenance.

## Consistency

Ingestion is acknowledged before processing runs, so a freshly accepted event will
briefly report zero episodes. `GET /v1/events/{id}/status` exposes exactly where an event
is, including per-stage state and queued work. The phase-15 consistency modes
(`wait_for_event`, `ledger_only`) are not implemented yet; today the honest answer is that
processing is asynchronous and its state is queryable.

## Knowledge

An entity is a stable identity; facts about it are assertions, never columns on it. An
assertion is one immutable claim carrying a typed object and four independent layers of
time.

A correction creates a new claim and marks the old one superseded in the same transaction.
That changes knowledge time without touching world validity, which is what makes all three
of these different questions answerable from the same rows: what holds now, what held on a
past date, and what the system believed on a past date about a past date.

Objects keep their types in typed columns, with a canonical key for equality
([ADR 0005](../adr/0005-typed-object-columns-for-assertions.md)). Contradictions are
recorded as conflict sets rather than resolved by guesswork
([ADR 0006](../adr/0006-conflicts-recorded-not-resolved.md)); the full reconciler, with
authority weighting and out-of-order handling, is phase 5.

## What comes next

Phase 3 adds extraction: a provider-independent LLM interface, model-run tracking, and
pipeline stages that turn chunks into candidate claims. Those candidates commit through the
same `knowledge.Assert` path, so everything above - evidence, supersession, conflict
detection, provenance - applies unchanged to extracted knowledge.
