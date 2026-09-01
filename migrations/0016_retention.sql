-- Retention for the operational tables (AGENTS.md section 28, issue #3).
--
-- Nothing in this system deleted anything. Every table grew forever, and the ones that
-- grew with *traffic* rather than with knowledge were the problem: an outbox row per
-- event, an audit row per action, and a retrieval trace per query. A deployment serving a
-- million queries a day accumulated a million trace rows a day, none of which described
-- anything anybody knew.
--
-- What is prunable and what is not is the whole design here.
--
--   Prunable, because it records an operation: retrieval_traces, audit_events,
--   pipeline_runs and their stage rows, and outbox rows that have reached a terminal
--   state. Deleting these loses history about the system's own behaviour and no knowledge.
--
--   Never prunable, because it records provenance: assertions, source_events, evidence,
--   derivations, and model_runs. evidence and derivations carry model_run_id under
--   ON DELETE SET NULL, so pruning model_runs would not fail loudly — it would quietly
--   null the link from a claim to the model interaction that proposed it, which is the
--   exact trace this system exists to keep.
--
-- retrieval_traces is partitioned here and the others are not, for a reason worth stating.
-- Range partitioning requires the partition key inside every unique constraint, and
-- outbox_events carries UNIQUE (workspace_id, dedupe_key) — the guard that stops the same
-- logical work being enqueued twice. Partitioning it by time would scope that guarantee to
-- a partition, so the same work could enqueue once per month. A queue with retention stays
-- small anyway; a trace table does not. So traces get partitions, and the rest get
-- bounded deletes.

-- ---------------------------------------------------------------------------------------
-- retrieval_traces becomes range-partitioned on created_at.
-- ---------------------------------------------------------------------------------------
--
-- The primary key gains created_at because PostgreSQL requires the partition key in it.
-- Nothing references retrieval_traces, so no foreign key has to learn the new shape.

ALTER TABLE retrieval_traces RENAME TO retrieval_traces_unpartitioned;
ALTER INDEX retrieval_traces_workspace_idx RENAME TO retrieval_traces_unpart_workspace_idx;
ALTER INDEX retrieval_traces_principal_idx RENAME TO retrieval_traces_unpart_principal_idx;
-- The primary key index too, or the new table's key is named retrieval_traces_pkey1 for
-- the life of the database because the name was still taken when it was created.
ALTER INDEX retrieval_traces_pkey RENAME TO retrieval_traces_unpart_pkey;

CREATE TABLE retrieval_traces (
    id             uuid NOT NULL,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    graph_space_id uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE CASCADE,

    -- The hash is always stored; the text only when policy permits. That way "which queries
    -- ran often" stays answerable even where the words themselves may not be retained.
    query_hash text NOT NULL,
    query_text text NOT NULL DEFAULT '',
    redacted   boolean NOT NULL DEFAULT false,

    principal_id   text NOT NULL DEFAULT '',
    principal_kind text NOT NULL DEFAULT '',
    purpose        text NOT NULL DEFAULT '',

    action          text NOT NULL DEFAULT 'read',
    policy_version  integer NOT NULL DEFAULT 0,
    policy_rule     text NOT NULL DEFAULT '',
    policy_filters  jsonb NOT NULL DEFAULT '{}'::jsonb,

    filters       jsonb NOT NULL DEFAULT '{}'::jsonb,
    candidate_refs jsonb NOT NULL DEFAULT '[]'::jsonb,
    selected_refs  jsonb NOT NULL DEFAULT '[]'::jsonb,

    latency_ms bigint NOT NULL DEFAULT 0,
    query_time timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

-- Monthly partitions covering whatever is already here, plus a window forward so writes
-- have somewhere to land before anything runs. The application extends the window; this
-- only has to survive until it does.
DO $$
DECLARE
    month_start date;
    last_month  date;
    partition   text;
BEGIN
    SELECT date_trunc('month', COALESCE(min(created_at), now()))::date
      INTO month_start FROM retrieval_traces_unpartitioned;
    last_month := (date_trunc('month', now()) + interval '3 months')::date;

    WHILE month_start <= last_month LOOP
        partition := 'retrieval_traces_' || to_char(month_start, 'YYYYMM');
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF retrieval_traces FOR VALUES FROM (%L) TO (%L)',
            partition, month_start, month_start + interval '1 month');
        month_start := (month_start + interval '1 month')::date;
    END LOOP;
END $$;

-- A row outside every range would otherwise be rejected. It lands here instead, which
-- keeps writes working; the cost is that a partition cannot later be created for a range
-- the default already holds, so the application treats a non-empty default as a warning.
CREATE TABLE retrieval_traces_default PARTITION OF retrieval_traces DEFAULT;

INSERT INTO retrieval_traces (
    id, workspace_id, graph_space_id, query_hash, query_text, redacted,
    principal_id, principal_kind, purpose, action, policy_version, policy_rule,
    policy_filters, filters, candidate_refs, selected_refs, latency_ms, query_time, created_at)
SELECT
    id, workspace_id, graph_space_id, query_hash, query_text, redacted,
    principal_id, principal_kind, purpose, action, policy_version, policy_rule,
    policy_filters, filters, candidate_refs, selected_refs, latency_ms, query_time, created_at
FROM retrieval_traces_unpartitioned;

DROP TABLE retrieval_traces_unpartitioned;

-- Partitioned indexes: PostgreSQL creates and maintains the per-partition copies.
CREATE INDEX retrieval_traces_workspace_idx
    ON retrieval_traces (workspace_id, graph_space_id, query_time DESC);
CREATE INDEX retrieval_traces_principal_idx
    ON retrieval_traces (workspace_id, principal_id, query_time DESC);

-- Pruning the unpartitioned tables is a bounded delete, which needs the cutoff column
-- indexed or every sweep is a sequential scan over history.
CREATE INDEX IF NOT EXISTS outbox_events_terminal_idx
    ON outbox_events (completed_at)
    WHERE status IN ('succeeded', 'dead');
CREATE INDEX IF NOT EXISTS audit_events_created_idx ON audit_events (created_at);
CREATE INDEX IF NOT EXISTS pipeline_runs_created_idx ON pipeline_runs (created_at);
