# Security and multi-tenancy

Three layers, each doing one job (AGENTS.md section 22):

1. **Authentication** resolves an API key to a principal.
2. **Grants** decide which workspaces that principal may touch at all.
3. **Policy** decides which of a workspace's contents they may see.

The third is the one this page is mostly about. A grant is a gate; policy is a filter.

## What a decision returns

Not a boolean. A decision carries the narrowing every query must apply:

```json
{
  "allowed": true,
  "rule": "support tier",
  "policy_version": 3,
  "filters": {
    "max_classification": "confidential",
    "permitted_classifications": ["public", "internal", "confidential"],
    "denied_sources": ["hr-system"],
    "denied_predicates": ["SALARY"]
  }
}
```

Those filters go into each retriever's `WHERE` clause. Unauthorized rows are never read into
memory, never ranked against authorized ones, and never counted — which is what AGENTS.md
section 22.4 requires and what a boolean cannot deliver. The reasoning is in
[ADR 0016](../adr/0016-policy-returns-filters-not-verdicts.md).

## Writing a policy

```
POST /v1/graph-spaces/{graph_space_id}/policies
```

```json
{
  "name": "support access",
  "activate": true,
  "default_clearance": "internal",
  "rules": [
    {
      "name": "support may see confidential for support work",
      "effect": "allow",
      "actions": ["read"],
      "roles": ["reader"],
      "purposes": ["customer-support"],
      "max_classification": "confidential"
    },
    {
      "name": "no hr material for readers",
      "effect": "deny",
      "actions": ["read"],
      "roles": ["reader"],
      "source_ids": ["01a0..."]
    },
    {
      "name": "contractor suspended",
      "effect": "deny",
      "principal_ids": ["contractor-42"]
    }
  ]
}
```

A rule matches when every condition it states matches; an unstated condition means "any".

| Condition | Matches on |
|---|---|
| `principal_ids`, `principal_kinds`, `roles` | Who is asking |
| `purposes` | Why, when the caller states it via `X-Strata-Purpose` |
| `residencies` | Where the request is served from |
| `not_before`, `not_after` | When, for temporary access |
| `graph_space_ids`, `collection_ids`, `source_ids` | Which material |
| `entity_types`, `predicates`, `classifications`, `memory_kinds` | Which knowledge |
| `max_classification` | A ceiling this rule grants, rather than a match condition |

**The two kinds of deny matter.** A deny naming resources *narrows* — the third rule above
suspends a contractor entirely, while the second only removes HR sources from what readers
see. A rule that made every query fail would be routed around, and a system people route
around protects nothing.

**Resolution is boring on purpose**: deny wins, then an explicit allow, then the role
baseline. No priorities, no most-specific-match, no first-match ordering — those are where
policy bugs live.

**Every condition that names material narrows the query itself**, never the results
(section 22.4). Sources, collections, entity types, predicates, memory kinds, and the
classification ceiling all become SQL predicates in the vector, lexical, and graph searches,
so unauthorized material is not fetched and then discarded.

A record that is not scoped by a condition is not what that condition is about, and an allow
rule does not hide it. A passage ingested into no collection survives a rule allowing one
collection; a chunk has no entity type, so a rule allowing certain entity types does not
remove every passage. The alternative — treating "unscoped" as "not permitted" — makes one
narrow rule quietly delete most of the corpus.

## Roles are the baseline

| Role | read | write | export | admin |
|---|---|---|---|---|
| reader | ✓ | | | |
| writer | ✓ | ✓ | | |
| admin | ✓ | ✓ | ✓ | ✓ |

Export is deliberately not implied by read. Being able to look something up and being able to
walk out with everything are different powers.

## Classification and clearance

Labels propagate source → episode/chunk → assertion and may only be raised, never quietly
lowered (section 22.3): `public`, `internal`, `confidential`, `restricted`, `secret`.

A principal's ceiling is the lower of the policy's and their grant's:

```bash
cgctl policy clearance --graph-space $GS --principal analyst-7 --max-classification confidential
```

A clearance only ever narrows. If it could raise the policy ceiling, granting workspace
access would quietly hand over everything in it.

## No policy configured

That is a named state, not a gap: role-based access with an `internal` ceiling, recorded in
audit as policy version 0. `cgctl policy ls` says so explicitly rather than printing nothing.

## Checking before you commit

```bash
cgctl policy explain --graph-space $GS --principal analyst-7 --action read --purpose customer-support
```

```
analyst-7 may read: allowed
  reason: allowed by rule support tier
  policy version 3
  may see: public, internal, confidential
  excluded sources: [hr-system]
```

Hypotheticals are not recorded as decisions — filling the audit log with questions nobody
acted on makes it harder to read when it matters.

## Audit

Every decision is recorded, refusals included, as `policy.read`, `policy.write`,
`policy.export`, or `policy.admin` (section 22.6). Audit rows carry who asked, for what
purpose, what was decided, and which rule decided it.

**They never contain source content.** A log that quoted the material it was guarding would
be a second copy of the protected data, in a table people grant broad read access to precisely
because it is "just metadata".

## Traces

Retrieval records what was asked, under which policy, what was considered, and what came
back:

```
GET /v1/traces/{trace_id}
GET /v1/graph-spaces/{graph_space_id}/traces
```

Set `CG_REDACT_QUERY_TEXT=true` to store the query's hash without its words, for deployments
where what people asked is itself sensitive. "Which queries run often" stays answerable
either way.

A trace names records in a workspace, so it is resolved through the workspaces the caller
actually holds. Serving one by id across tenants would be a leak with extra steps.

## Export

```
GET /v1/graph-spaces/{graph_space_id}/export
```

Streams JSONL, requires the `export` action, is audited, and applies the same filters as any
other read — narrowing the query, then re-checking each row on the way out. Export is the one
path where a missed filter hands over a complete copy rather than merely showing too much.

## Cross-workspace isolation

Enforced by scope on every query in every path, and tested rather than asserted. The
acceptance test in `internal/policy/isolation_integration_test.go` seeds two tenants with
distinctive words and then tries to reach one from the other through lexical, exact, vector,
entity, graph, context assembly, canonical reads, provenance, export, and traces — including
asking graph expansion directly for the other tenant's entity as a traversal root.
