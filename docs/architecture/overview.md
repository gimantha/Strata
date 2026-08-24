# Architecture overview

This describes what exists after phases 0 through 10, and how each part enforces the
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
        PIPELINE   normalize -> segment -> chunk -> map changes -> extract
                   -> resolve entities -> validate against ontology
                   -> reconcile temporally -> project
                                     |
                                     v
  CANONICAL LEDGER   Source | SourceEvent | Artifact | Episode | Chunk
                     Entity | Assertion | Evidence | Derivation | ConflictSet
                     OntologyVersion | CDCStream
                                     |
                                     v
        PROJECTIONS   graph | vector | lexical                 summaries   [later]
                                     |
                                     v
        HYBRID RETRIEVAL   plan -> five retrievers -> weighted RRF
                                     |
                                     v
        CONTEXT ASSEMBLY   select under budget -> cite -> fence untrusted text
```

Everything in this diagram is implemented except summary projections, which are deliberately
absent rather than stubbed: an empty stage that recorded success would make replay state lie.

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
| Row updates never rebuild a subgraph | A change states what the row now says; unchanged claims collide on their fingerprint and keep their assertion id, and only moved values are superseded. A test asserts identity, not counts |
| Invalid schema candidates are never silently committed | A guided graph space validates every claim; a caller gets `ontology_violation`, a model's candidate is committed as `quarantined` with its reasons, and quarantined claims are neither projected nor reconciled |

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
([ADR 0006](../adr/0006-conflicts-recorded-not-resolved.md)).

Claims from extraction commit through the same `knowledge.Assert` path the API uses, so
evidence, supersession, conflict detection, and provenance apply unchanged to extracted
knowledge.

## Extraction, resolution, reconciliation

Extraction reads chunks and returns candidate claims with quoted spans. Untrusted document
text is fenced with a random nonce, the model is constrained to a schema, every claim must
quote the chunk it came from, and an instruction found inside a quoted span quarantines that
span rather than the document
([ADR 0008](../adr/0008-defense-in-depth-for-prompt-injection.md)). No single one of those is
sufficient, which is the point.

Resolution climbs an evidence ladder: an external identifier is decisive, an exact canonical
name is strong, and fuzzy similarity only ever produces a candidate for review
([ADR 0009](../adr/0009-fuzzy-matching-generates-candidates-only.md)) — measured on real
data, the similarity of a typo and the similarity of two different people overlap. A merge
redirects rather than collapses, so undoing one clears a pointer instead of reconstructing
history.

Reconciliation orders claims by the source's own clock rather than by arrival
([ADR 0010](../adr/0010-source-order-over-arrival-order.md)), so a CDC stream delivering
updates out of order converges to the same state either way. Predicate policy decides what
happens when claims overlap; equal authority produces a conflict rather than a coin flip.

## Projections and retrieval

Projections carry no state of their own and are rebuilt through the same code path that
maintains them incrementally
([ADR 0011](../adr/0011-projections-hold-no-history.md)). Dropping every projection and
replaying produces equivalent retrieval results, which a test asserts.

A query is planned from its shape — identifiers get exact matching, names get entity and
graph, prose gets vector and lexical — then fused by weighted Reciprocal Rank Fusion scaled
by match quality ([ADR 0012](../adr/0012-weighted-rrf-with-quality-scaling.md)). Agreement
between retrievers is the dominant signal, and it is why hybrid measurably beats every
individual mode on the evaluation corpus rather than merely being assumed to.

## Context assembly

A block is selected greedily against a hard token budget, with redundancy reduction, per
section shares, and a citation reserved for every item before that item is written
([ADR 0013](../adr/0013-selection-under-a-hard-token-budget.md)). Quoted source text is
fenced with a per-block random nonce and labeled untrusted, so the boundary between what a
document says and what the graph asserts survives concatenation into a prompt.

The budget is a ceiling rather than a target: content is dropped rather than exceeding it,
and what was dropped is reported, because absence is the hardest thing to debug in an
assembled prompt.

## Ontology modes

A graph space is either open — extraction invents entity types and predicate names, and the
registry normalizes them — or guided, where every claim is validated against an immutable
schema version ([ADR 0014](../adr/0014-two-ontology-modes-and-two-dispositions.md)). The
setting lives on the graph space so the same source can be processed both ways and compared.

What a schema refuses never becomes knowledge quietly. A caller stating a claim gets
`ontology_violation` and can fix it; a model proposing one has no such loop, so the claim is
committed as quarantined, carrying its reasons, and held out of belief — not projected, not
retrievable, not reconciled.

## Change data capture

A row change is a source event like any other, and knowledge is derived from it rather than
rebuilt on it ([ADR 0015](../adr/0015-cdc-is-events-not-rebuilds.md)). An update supersedes
the claims whose values moved and leaves every other assertion untouched, which is what keeps
identity, history, and every reference to a claim intact across a hundred column edits.

Mapping is deterministic: a row is already structured, so the columns say what the predicates
are and the primary key says what the subject is. CDC therefore works with no model provider
configured at all.

A checkpoint advances only after its events are durable; a crash in between replays them, and
replaying is free because the gateway keys on each change. A deleted row retracts its claims
rather than erasing them — the source stopped saying it, which is not the same as it never
having been true.

## What comes next

Phase 11 adds security and multi-tenancy in depth: authentication, ABAC policy evaluation,
and classification-aware filtering applied before ranking rather than after.
