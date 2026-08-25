# Backup and recovery

What a backup must contain, what it may safely omit, and how to prove the difference
([AGENTS.md section 40](../../AGENTS.md)).

The whole plan rests on one property: **the canonical ledger is authoritative and every
derived index is rebuildable from it.** That is not a claim in a runbook — it is enforced by
`TestIntegrationScenarioIProjectionRebuild`, checked against the live schema by
`TestIntegrationEveryTableIsClassifiedForBackup`, and provable against any deployment with
`cgctl recovery drill`.

## What to back up

```bash
cgctl recovery classify
```

Printed from the running binary rather than copied here, because a list of tables in a
document goes stale the first time somebody adds one and the failure surfaces during a
restore. The command exits non-zero if the schema contains a table the classification does
not cover.

### Canonical — restore these; nothing regenerates them

Scope and access (`workspaces`, `graph_spaces`, `collections`, `principals`,
`principal_workspaces`, `policy_sets`, `sources`), the ingestion ledger (`artifacts`,
`source_events`, `episodes`, `chunks`, `pipeline_runs`, `pipeline_stage_runs`), knowledge
(`entities`, `entity_aliases`, `entity_identifiers`, `predicates`, `assertions`, `evidence`,
`derivations`, `derivation_inputs`, `conflict_sets`, `ontology_versions`), and the records
that cannot be recomputed (`model_runs`, `resolution_candidates`, `resolution_decisions`,
`audit_events`, `retrieval_traces`, `cdc_streams`, `outbox_events`).

Three of those are worth justifying, because they look derived and are not:

- **`audit_events`** records what was disclosed to whom. A disclosure that happened cannot be
  re-derived from data that survived, and losing the record does not undo the disclosure.
- **`retrieval_traces`** explains answers already given. If an answer is questioned six
  months later, the trace is the only account of why it was returned.
- **`outbox_events`** holds work accepted but not yet processed. A caller was told their
  event was durable; dropping the queue makes that untrue (sections 28.1, 40.5).

### Derived — back up only for recovery speed

`vector_records`, `lexical_records`, `graph_edges`, `projection_checkpoints`.

Include them if replaying a year of events would take longer than your recovery objective
allows. Never rely on them: a restore must always be able to choose the replay.

## The five requirements

**1. PostgreSQL with point-in-time recovery.** Continuous archiving (`archive_mode = on`
with a WAL archive) plus periodic base backups, or your provider's equivalent. The recovery
point objective is whatever your WAL archive interval is; the recovery time objective is the
base backup restore plus WAL replay, and a projection rebuild only if you chose not to back
up the derived tables.

**2. Blob storage with versioning and replication.** The artifact store holds the raw bytes
every claim is ultimately cited to, and `Artifact.ContentHash` is the integrity check. A
`source_events` row whose artifact is missing is a claim that can no longer be walked back to
its source — the provenance chain ends in a 404, which is worse than the claim being absent.
The filesystem backend has no versioning of its own, so this is the deployment's
responsibility until the S3-compatible backend lands in phase 15.

**3. Ontology, configuration, and secret references.** Ontology versions live in the database
and travel with it. Configuration is environment-first (`configs/dev.env` documents every
key), so back up the deployment's environment definition — not its secret values. Secrets
belong in a secret manager with its own recovery story; back up the *references*, and confirm
the manager itself is covered by somebody's plan.

**4. Derived indexes are rebuildable.** See the classification above.

**5. Event-bus retention.** The JetStream stream is a delivery optimization, not a durability
boundary: the ledger holds every work item and workers fall back to polling when the broker
is gone ([ADR 0019](../adr/0019-the-broker-carries-notice-the-ledger-carries-work.md)). So
the bus needs no backup at all — a rebuilt broker starts empty and the fleet keeps working.
This is one of the concrete payoffs of keeping durability in one place.

## Testing the restore

Section 40 asks for restore and rebuild to be tested regularly. A backup nobody has restored
is a hypothesis.

```bash
CG_DRILL_WORKSPACE=acme scripts/restore-drill.sh postgres://.../production
```

The script dumps the source, restores into a throwaway database, checks the classification
covers the restored schema, then drops every derived record and rebuilds from the canonical
ledger alone. Nothing is written to the source; the drill works on the copy, and the copy is
dropped afterwards unless `CG_DRILL_KEEP=1`.

Against a deployment you already have — including a restored copy made another way:

```bash
cgctl recovery classify
cgctl recovery drill --workspace acme --confirm
```

`--confirm` is required because the drill deletes every derived index in the workspace and
rebuilds it, which makes retrieval unavailable while it runs. It is safe on a restored copy
and should not be a routine command in production.

A passing drill looks like this:

```
before:  graph 12  lexical 480  vector 480
dropping every derived record...
rebuilding from the canonical ledger alone...
after:   graph 12  lexical 480  vector 480  (replayed 500 event(s))
```

A rebuild that comes back short means this backup's canonical tables are not sufficient to
reconstruct its indexes, which is the finding the drill exists to produce — before an
incident rather than during one.

## Recovering

1. **Restore PostgreSQL** to the target point in time.
2. **Restore blob storage** to a consistent-or-later point. Later is safe: an artifact with
   no `source_events` row is unreferenced, while an event with no artifact breaks provenance.
   Restore blobs *after* the database's point in time, never before.
3. **Apply migrations** if the binary is newer than the backup: `cgctl migrate`.
4. **Rebuild projections** if the derived tables were not backed up:
   `cgctl projections rebuild --workspace <slug>`.
5. **Drain the outbox.** Restored work items are `pending` or `claimed`; claimed ones whose
   leases have expired are reaped automatically by any running worker, so starting the fleet
   is the whole step.
6. **Verify** with `cgctl recovery drill` on a copy, and a real query against the restored
   deployment.

### What a restore cannot recover

Anything accepted after the recovery point. Idempotency keys make replaying those events safe
— a re-sent event returns the original rather than duplicating it (see
[ingest.md](ingest.md)) — so the recovery procedure for a gap is to re-send the window from
the upstream source. For CDC streams, reset the checkpoint and let the connector re-read;
source ordering means out-of-order redelivery converges correctly
([ADR 0010](../adr/0010-source-order-over-arrival-order.md)).
