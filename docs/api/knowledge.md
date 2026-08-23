# Knowledge API

Entities, predicates, assertions, evidence, and provenance. Authentication, error codes,
and scope rules are the same as [ingest.md](ingest.md): tenancy comes from the graph space
in the path, and another tenant's resource reports `404`, never `403`.

## The model in one paragraph

An **entity** is a stable identity; facts about it are never columns on it. An
**assertion** is one immutable claim: subject, predicate, typed object, and four
independent layers of time. Corrections never edit a claim - they create a new one and mark
the old one superseded, which changes what the system believes without rewriting what it
believed before or when the fact held in the world. **Evidence** ties a claim to the exact
source material behind it. A **derivation** explains a claim that was reasoned rather than
observed.

## Predicates

| Method | Path | Requires |
|---|---|---|
| POST | `/v1/workspaces/{workspace_id}/predicates` | workspace `admin` |
| GET | `/v1/workspaces/{workspace_id}/predicates` | workspace `reader` |

Predicate semantics are what let the system tell a contradiction from two facts that simply
coexist. Names are normalized to `UPPER_SNAKE_CASE`, so `worksAt`, `works at`, and
`works_at` are one predicate.

```bash
curl -X POST "localhost:8080/v1/workspaces/$WS/predicates" \
  -H "Authorization: Bearer $CRED" \
  -d '{"name":"current_plan","functional":true,"conflict_policy":"manual",
       "temporal_policy":"stateful","object_kinds":["symbol"]}'
```

| Field | Meaning |
|---|---|
| `functional` | Only one value may hold at a time |
| `conflict_policy` | `coexist`, `latest_wins`, `highest_authority`, `manual` |
| `temporal_policy` | `stateful` (replaced over time), `event` (a moment), `immutable` |
| `default_memory_kind` | Applied to claims that do not state one |
| `sensitivity` | Classification floor for claims using this predicate |

A predicate used by a claim but never defined is registered automatically as a
**candidate**, non-functional and coexisting. That default is deliberate: assuming an
invented predicate is exclusive would manufacture contradictions that do not exist.

Changing semantics bumps `version`. Assertions record the version they were validated
under, so tightening a predicate later cannot silently reinterpret older claims.

## Entities

| Method | Path | Requires |
|---|---|---|
| POST | `/v1/graph-spaces/{graph_space_id}/entities` | workspace `writer` |
| GET | `/v1/graph-spaces/{graph_space_id}/entities` | workspace `reader` |
| GET | `/v1/entities/{entity_id}` | a grant on the owning workspace |

```bash
curl -X POST "localhost:8080/v1/graph-spaces/$GS/entities" \
  -H "Authorization: Bearer $CRED" \
  -d '{"canonical_name":"Acme Corporation","entity_type":"organization",
       "aliases":["Acme","Acme Corp"]}'
```

`GET .../entities?name=acme%20corp` resolves through aliases. It can legitimately return
several identities: deciding which one is meant is entity resolution's job in phase 4, and
guessing here would silently merge two different organizations.

## Asserting knowledge

```
POST /v1/graph-spaces/{graph_space_id}/assertions        (workspace writer)
```

Every claim must name the `source_event_id` it came from. Knowledge with no traceable
origin is exactly what this architecture refuses to hold.

```bash
curl -X POST "localhost:8080/v1/graph-spaces/$GS/assertions" \
  -H "Authorization: Bearer $CRED" \
  -d '{
    "source_event_id": "'$EVENT'",
    "claims": [{
      "subject":       {"name": "Alice Chen", "type": "person"},
      "predicate":     "role_at",
      "object_entity": {"name": "Acme", "type": "organization"},
      "scope_key":     "CEO",
      "valid_from":    "2025-01-01T00:00:00Z",
      "valid_to":      "2026-04-01T00:00:00Z",
      "evidence": [{"episode_id": "'$EPISODE'", "chunk_id": "'$CHUNK'",
                    "extracted_text": "Alice Chen served as CEO through March 31st."}]
    }]
  }'
```

### Objects keep their type

Use `object_entity` for a relation, or `object` with a `kind` for a literal:

| Kind | Field | Example |
|---|---|---|
| `string`, `uri`, `symbol` | `text` | `{"kind":"symbol","text":"ENTERPRISE"}` |
| `integer` | `integer` | `{"kind":"integer","integer":1234}` |
| `decimal` | `decimal` | `{"kind":"decimal","decimal":"12345678.90"}` |
| `boolean` | `boolean` | `{"kind":"boolean","boolean":true}` |
| `timestamp` | `timestamp` | RFC 3339 |
| `date` | `date` | `"1998-07-14"` |
| `duration` | `duration` | `"4h"` |
| `geo` | `latitude`, `longitude` | |
| `json` | `json` | any document |
| `entity` | `entity_id` | |

Decimals travel as strings and are stored as `numeric`, so a source's figure is never
rounded by a float. Values compare by a canonical key, so `12.50` and `12.5000` are one
value and JSON key order does not matter.

### The four clocks

| Layer | Fields | Question it answers |
|---|---|---|
| World | `event_time`, `valid_from`, `valid_to`, `effective_from`, `effective_to` | When was this true? |
| Knowledge | `observed_at`, `recorded_at`, `superseded_at` | When did we believe it? |
| Source | `source_time`, `source_commit_time`, `source_sequence`, `source_version` | What did upstream say, and in what order? |
| Lifecycle | `active_from`, `active_until`, `decay_starts_at`, `expires_at` | Is it relevant context now? |

Knowledge time is set by the system and cannot be supplied by a caller. Validity intervals
are half-open, so a claim ending exactly when the next begins is a clean handover rather
than a contradiction.

### Corrections

Pass `supersedes` with the claims the new one replaces. The correction and the supersession
commit together, so the ledger never shows one without the other:

```json
{"predicate": "role_at", "valid_to": "2026-03-20T00:00:00Z",
 "supersedes": ["01a02cda-f761-79f2-993e-e0b68547c20b"]}
```

The superseded claim is not edited or deleted. Its world validity stands as it was
recorded; only its knowledge-time window closes.

### Derived claims

A claim with `provenance_mode` of `inferred` or `derived` must be accompanied by a
`derivation` naming the method and the assertions it reasoned from. Without one the request
is rejected: inference is never presented as direct observation.

```json
{"derivation": {"method": "rule_inference", "rule_name": "recurring_customer",
                "rule_version": "1", "input_assertion_ids": ["..."]},
 "claims": [{"predicate": "is_recurring_customer", "provenance_mode": "inferred", ...}]}
```

### Response

```json
{"assertions": [...], "duplicates": 0, "superseded": ["..."], "conflicts": ["..."]}
```

`duplicates` counts claims already present by fingerprint, which is the normal result of
reprocessing an event rather than an error. `conflicts` names conflict sets created because
the claim contradicted an existing one.

## Querying

```
POST /v1/graph-spaces/{graph_space_id}/assertions/query   (workspace reader)
```

| Filter | Meaning |
|---|---|
| `subject_ids`, `predicates`, `object_entity_ids`, `scope_key` | Structural |
| `memory_kinds`, `statuses`, `min_confidence` | Selection |
| `valid_at` | What held in the world at this instant |
| `valid_between` | Claims whose validity overlaps a range |
| `known_at` | What the system believed at this instant, including claims since replaced |
| `event_between` | When the described event occurred |
| `active_at` | What is relevant context, which is not the same as what is true |
| `include_superseded`, `limit`, `offset` | Paging and history |

The two that matter most are independent. `valid_at` moves along world time; `known_at`
moves along knowledge time. Together they answer the question a single timestamp cannot
express at all:

```bash
# What do we believe now about March 25?
-d '{"predicates":["role_at"],"valid_at":"2026-03-25T00:00:00Z"}'

# What did we believe on April 10 about March 25?
-d '{"predicates":["role_at"],"valid_at":"2026-03-25T00:00:00Z",
     "known_at":"2026-04-10T00:00:00Z"}'
```

With `known_at` set, status is not filtered: a claim superseded today was current belief
then, and excluding it would answer the wrong question. Without it, the default is current
belief - `active` and `disputed`. Disputed claims are included because a contested fact is
still believed, and hiding it would present uncertainty as settled.

Results are capped at 100 by default and 1000 at most; unbounded reads are not offered.

## Reading and withdrawing

| Method | Path | Purpose |
|---|---|---|
| GET | `/v1/assertions/{assertion_id}` | One claim, with all four temporal layers |
| GET | `/v1/assertions/{assertion_id}/provenance` | The full walk back to source |
| POST | `/v1/assertions/{assertion_id}/retract` | Withdraw a claim, `reason` required |

Retraction withdraws a claim without replacing it. Like supersession it is a knowledge-time
event: the claim leaves current belief but a query as of an earlier instant still returns
it, and it stays readable. Retraction is not deletion.

## Provenance

```json
{
  "assertion": { ... },
  "subject":   {"id": "...", "canonical_name": "Acme Corporation"},
  "evidence_chain": [{
    "evidence":     {"extracted_text": "...", "confidence": 0.9},
    "chunk":        {"id": "...", "content": "...", "char_start": 0, "char_end": 47},
    "episode":      {"id": "...", "sequence": 0, "content": "...", "locator": {...}},
    "artifact":     {"id": "...", "content_hash": "sha256...", "blob_key": "..."},
    "source_event": {"id": "...", "operation": "upsert", "recorded_at": "..."},
    "source":       {"name": "crm", "kind": "database", "trust_level": "high"}
  }]
}
```

Every hop is present, ending at the content-addressed bytes the claim came from. A derived
claim additionally carries `derivation`, including the assertions it reasoned from. A fact
that cannot be walked back this way is a fact the system should not be asserting.

## Reconciliation

When a new claim overlaps an existing one for a predicate that does not permit multiple
values, the reconciler decides what happens. Two questions are asked in order.

**First: has the source already moved past this?** Source ordering is read from sequence or
LSN, then version, then commit time, then source time. If the source says this claim
describes an older state of the record, it is *superseded on arrival* — recorded, because it
is what the source said, but never current belief. Arrival order is irrelevant: a CDC stream
delivering update 102 before 101 converges to exactly the state in-order delivery would have
produced. Positions from different sources are never compared, since they are not on one
timeline. See [ADR 0010](../adr/0010-source-order-over-arrival-order.md).

**Then: what does the predicate's policy say?**

| Policy | Behavior |
|---|---|
| `coexist` | Nothing to reconcile; both values stand |
| `latest_wins` | The newer claim supersedes what it replaces |
| `highest_authority` | The more trusted source wins; equal trust produces a conflict |
| `manual` | Both claims kept, disagreement recorded |

Authority comes from the registered source's `trust_level`, so it is configuration rather
than a guess. Equal authority is deliberately a conflict: picking a winner between two
equally trusted systems would be arbitrary, and an arbitrary answer is an invisible wrong
answer.

Claims whose validity intervals do not actually overlap never conflict. A tenancy ending the
day the next begins is a clean handover, because intervals are half-open.

`AssertResult` reports the outcome: `superseded` for claims this one replaced,
`superseded_on_arrival` when the new claim was the stale one, and `conflicts` for
disagreements that were recorded rather than resolved.

## Conflicts

| Method | Path | Requires |
|---|---|---|
| GET | `/v1/graph-spaces/{graph_space_id}/conflicts` | workspace `reader` |
| POST | `/v1/conflicts/{conflict_id}/resolve` | workspace `writer` |

When two claims genuinely overlap for a predicate that does not permit multiple values,
both are kept, marked `disputed`, and joined to a conflict set. Nothing is deleted, because
deleting one arbitrarily would destroy information and hide the disagreement.

Resolving takes `resolution` of `resolved_by_source`, `resolved_by_human`, or
`resolved_by_policy`, and returns surviving claims to `active`. Add `?status=all` to
`GET .../conflicts` to include closed ones.

Automatic authority-weighted resolution arrives with the full reconciler in phase 5; see
[ADR 0006](../adr/0006-conflicts-recorded-not-resolved.md).

## Command line

```bash
cgctl predicate define --workspace acme --name role_at --functional --conflict-policy manual
cgctl assert --graph-space $GS --source-event $EV --episode $EP \
    --subject "Alice Chen" --subject-type person --predicate role_at \
    --object-entity Acme --scope-key CEO --valid-to 2026-04-01T00:00:00Z
cgctl ask --graph-space $GS --predicate role_at --valid-at 2026-03-25T00:00:00Z
cgctl ask --graph-space $GS --predicate role_at --valid-at 2026-03-25T00:00:00Z \
    --known-at 2026-04-10T00:00:00Z
cgctl provenance --assertion $ID
cgctl conflicts --graph-space $GS
```

## Not yet available

Extraction that produces these claims automatically (phase 3), entity resolution beyond
exact-name matching (phase 4), the full reconciler (phase 5), and retrieval and context
assembly (phases 6 to 8). See [AGENTS.md](../../AGENTS.md) section 36.
