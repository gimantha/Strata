# Purging old knowledge

Strata normally preserves history. Supersession, retraction, expiry, and memory deactivation
change what retrieval treats as current while retaining the assertion and its provenance.
Knowledge purge is a separate administrator operation for retention policies that require
old data to be physically destroyed.

## Eligibility

A purge works on complete source-event provenance units within one workspace and graph
space. An event is eligible only when:

- its ledger `recorded_at` is before the cutoff;
- pipeline status is terminal (`processed`, `failed`, or `quarantined`);
- no active knowledge hold covers the graph space or event;
- every assertion from the event is quarantined, old and retracted/superseded/deactivated,
  or has a world/context end before the cutoff; and
- no retained assertion still cites it as evidence, supersedes one of its assertions, or
  depends on one through a derivation.

An old assertion with no validity or lifecycle end stays protected. A source event is never
partly deleted because that would leave assertions with incomplete evidence.

## Preview and execute

The CLI requires workspace-admin access through an API key. Cutoffs must be at least 30
days old. Use a policy identifier for `--reason`; do not put source content in audit fields.

```bash
cgctl purge plan --graph-space <uuid> --before 2025-01-01 \
  --reason RETENTION-7Y --key-id <key-id>

cgctl purge run --graph-space <uuid> --before 2025-01-01 \
  --reason RETENTION-7Y --key-id <key-id>
```

The HTTP equivalent defaults to a dry run. Physical deletion happens only with
`"execute": true`:

```http
POST /v1/graph-spaces/{graph_space_id}/knowledge-purge
Content-Type: application/json

{
  "before": "2025-01-01T00:00:00Z",
  "reason": "RETENTION-7Y",
  "execute": true
}
```

Canonical rows are deleted transactionally in dependency order. Unshared archived blobs are
then deleted and all configured graph, vector, and lexical projections for the workspace are
rebuilt from the surviving ledger. The full rebuild makes the operation correct for external
index backends as well as PostgreSQL, but it can be expensive on a large workspace.

If blob deletion or projection rebuilding fails after the canonical transaction commits,
the run status is `failed` and the response error names its purge ID. Retry cleanup without
repeating canonical deletion:

```bash
cgctl purge retry --graph-space <uuid> --run <purge-uuid> --key-id <key-id>
```

or `POST /v1/graph-spaces/{graph_space_id}/knowledge-purge/{purge_id}/retry`.
Persisted blob keys and aggregate deletion counts make this retry idempotent.

## Holds

A hold with no event protects the entire graph space. Supplying an event limits it to that
source event. Optional expiry automatically stops the hold from affecting later plans.

```bash
cgctl purge hold --graph-space <uuid> --event <source-event-uuid> \
  --reason CASE-123 --expires-at 2027-01-01T00:00:00Z --key-id <key-id>

cgctl purge release --graph-space <uuid> --hold <hold-uuid> --key-id <key-id>
```

Plans, executions, cleanup results, hold creation, and hold release are written to the audit
ledger. Purge-run records contain identifiers and aggregate counts; they do not copy source
text or deleted facts.
