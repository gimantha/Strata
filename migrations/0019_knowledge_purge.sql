-- Audited physical retention for old, inactive knowledge.
--
-- Supersession, retraction, and deactivation preserve history. This migration adds the
-- separate mechanism an operator uses after policy permits that history to be destroyed.
-- Audit rows contain only identifiers and aggregate counts so they can survive the data.

CREATE TABLE knowledge_holds (
    id              uuid PRIMARY KEY,
    workspace_id    uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    graph_space_id  uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE CASCADE,
    source_event_id uuid NULL,
    reason          text NOT NULL,
    created_by_id   text NOT NULL,
    created_by_kind text NOT NULL DEFAULT '',
    expires_at      timestamptz NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX knowledge_holds_scope_idx
    ON knowledge_holds (workspace_id, graph_space_id, source_event_id);
CREATE INDEX knowledge_holds_expiry_idx ON knowledge_holds (expires_at)
    WHERE expires_at IS NOT NULL;

CREATE TABLE knowledge_purge_runs (
    id                uuid PRIMARY KEY,
    workspace_id      uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    graph_space_id    uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE CASCADE,
    cutoff            timestamptz NOT NULL,
    dry_run           boolean NOT NULL DEFAULT true,
    status            text NOT NULL,
    reason            text NOT NULL,
    requested_by_id   text NOT NULL,
    requested_by_kind text NOT NULL DEFAULT '',
    eligible_counts   jsonb NOT NULL DEFAULT '{}'::jsonb,
    deleted_counts    jsonb NOT NULL DEFAULT '{}'::jsonb,
    blob_keys         jsonb NOT NULL DEFAULT '[]'::jsonb,
    last_error        text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now(),
    completed_at      timestamptz NULL,
    CONSTRAINT knowledge_purge_status CHECK
        (status IN ('planned', 'canonical_deleted', 'completed', 'failed'))
);

CREATE INDEX knowledge_purge_runs_scope_idx
    ON knowledge_purge_runs (workspace_id, graph_space_id, created_at DESC);
