# Entity resolution

How a mention becomes an identity, and how to fix it when that goes wrong.

Resolution is the highest-risk component here. Merging two identities that are not the same
thing corrupts every fact about both and produces no error, so the design prefers leaving
things separate: a duplicate is visible and mergeable, a wrong merge is neither.

## The ladder

Evidence is used in strength order and the first rung that gives an unambiguous answer wins.
Evidence is never blended into one score, because a very similar name should not be able to
outvote the absence of a matching key.

| Rung | Evidence | Resolves automatically? |
|---|---|---|
| 1 | Upstream system's own primary key | **Yes**, confidence 1.0 |
| 2 | Configured business key (email, tax id, ticker) | **Yes**, confidence 0.99 |
| 3 | Exact name match after normalization | **Yes**, confidence 0.9 |
| 5 | Similar name (trigram) | **No** — records candidates only |
| 6, 7 | Embedding and graph-neighbourhood candidates | Not built; arrive with phase 6 projections |
| 8 | Adjudication of close candidates | Not built |

Rung 4 of the contract's ladder, structured attribute matching, is served by domain keys: a
configured attribute that identifies a record *is* a domain key here.

**Fuzzy matching never resolves on its own.** Measured with `pg_trgm`, a transposition typo
scores 0.619 while two entirely different people score 0.571 — no threshold separates them.
See [ADR 0009](../adr/0009-fuzzy-matching-generates-candidates-only.md).

## What happens to a mention

```
carries an upstream key that matches      -> that identity
carries a business key that matches       -> that identity
name matches exactly, one candidate       -> that identity
name matches exactly, several candidates  -> new identity, candidates recorded
name is merely similar                    -> new identity, candidates recorded
nothing matches                           -> new identity
```

Whenever a mention resolves, the names and keys it carried are recorded against the identity.
That is how the ladder improves with use: a record first seen by name becomes matchable by
its primary key the moment a source supplies one.

An unmatched key also *excludes* candidates. If a mention carries key `cust-002` and an
identity already holds `cust-001` in the same namespace, the source has told us those are two
records, and a shared name cannot override that.

## Providing identity evidence

Claims carry it on the entity reference:

```json
{
  "subject": {
    "name": "Acme Corporation",
    "type": "organization",
    "external_id": "cust-001",
    "source_id": "...",
    "domain_keys": [{"namespace": "email", "value": "ap@acme.example"}],
    "aliases": ["Acme", "Acme Corp"]
  }
}
```

Supplying `external_id` or a domain key is worth far more than any matching heuristic. A
source that passes through its own primary keys resolves perfectly and never reaches the
fuzzy rung.

## Inspecting an identity

```
GET /v1/entities/{entity_id}/identity        (workspace reader)
```

```json
{
  "entity_id": "...",
  "canonical_entity_id": "...",
  "merged": true,
  "cluster": ["...", "..."],
  "identifiers": [{"kind": "source_identifier", "namespace": "...", "value": "cust-001"}]
}
```

`cluster` is every identity that resolves to the same thing. Queries expand through it, so
asking about either side of a merge reaches the facts recorded under both.

## Merging and unmerging

| Method | Path | Requires |
|---|---|---|
| POST | `/v1/graph-spaces/{graph_space_id}/entities/merge` | workspace `admin` |
| POST | `/v1/entities/{entity_id}/split` | workspace `admin` |
| GET | `/v1/graph-spaces/{graph_space_id}/resolution-decisions` | workspace `reader` |

```bash
curl -X POST ".../entities/merge" -H "Authorization: Bearer $CRED" \
  -d '{"from_entity_id":"...","into_entity_id":"...","reason":"same company, different spelling"}'
```

A reason is required. This is the most damaging operation available if it is wrong, and the
decision ledger is only worth reading if it says why.

**Nothing is deleted by a merge.** The redirected identity keeps its row, its names, its
keys, and every assertion that referenced it. Only a pointer changes — which is exactly why
a split is possible:

```bash
curl -X POST "/v1/entities/{id}/split" -d '{"reason":"different subsidiaries after all"}'
```

After a split each identity holds its own facts again, and the ledger shows both the merge
(marked `reverted_at`) and the split. Reversing a merge does not erase the record that it
happened.

## The decision ledger

Every resolution is recorded, not just the interesting ones: a merge that turns out to be
wrong is investigated through this ledger, and a decision that cannot be found cannot be
reversed.

Each entry holds the mention, the method, the chosen identity, the confidence, the resolver
version, every candidate considered with its score and features, and — for human actions —
the actor and reason.

`?review=true` narrows it to what needs attention: ambiguous outcomes the resolver refused to
guess, and merges or splits someone performed.

## Command line

```bash
cgctl entity identity --entity $ID          # canonical form, cluster, keys, names
cgctl entity merge --graph-space $GS --from $A --into $B --reason "same company"
cgctl entity split --entity $A --reason "wrong merge"
cgctl resolutions --graph-space $GS         # the review queue
cgctl resolutions --graph-space $GS --all   # every resolution
```

## Not yet available

Embedding and graph-neighbourhood candidate generation (rungs 6 and 7) arrive with the phase
6 projections; adjudication of close candidates (rung 8) after them. Until then the ambiguous
middle is a human's decision, which is why the ledger records it so carefully.
