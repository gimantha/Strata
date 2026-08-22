-- Work fabric: the transactional outbox and durable pipeline execution state.
--
-- Any canonical mutation that needs downstream work writes its outbox row in the
-- SAME transaction as the mutation (AGENTS.md section 28.1). There is no
-- "commit then publish" path anywhere in this system.

CREATE TABLE outbox_events (
    id              uuid PRIMARY KEY,
    workspace_id    uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    graph_space_id  uuid NULL REFERENCES graph_spaces (id) ON DELETE SET NULL,
    source_event_id uuid NULL REFERENCES source_events (id) ON DELETE CASCADE,
    topic           text NOT NULL,
    event_type      text NOT NULL,
    schema_version  int NOT NULL,
    payload         jsonb NOT NULL,

    -- Publication is idempotent: re-running a producer for the same logical work
    -- collides here instead of enqueueing duplicate work.
    dedupe_key      text NOT NULL,

    status          text NOT NULL,
    attempts        int NOT NULL DEFAULT 0,
    max_attempts    int NOT NULL,
    visible_at      timestamptz NOT NULL DEFAULT now(),

    -- Leasing: a worker that dies leaves an expired claim, which the reaper returns
    -- to pending. This is what makes accepted work impossible to lose.
    claimed_by       text NOT NULL DEFAULT '',
    claim_expires_at timestamptz NULL,

    last_error      text NOT NULL DEFAULT '',
    error_class     text NOT NULL DEFAULT '',

    -- W3C trace context captured at publish time, so worker spans join the ingest
    -- request's trace across the asynchronous boundary (section 30.1).
    trace_parent    text NOT NULL DEFAULT '',

    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    completed_at    timestamptz NULL,
    CONSTRAINT outbox_max_attempts_positive CHECK (max_attempts > 0)
);

CREATE UNIQUE INDEX outbox_events_dedupe_key ON outbox_events (workspace_id, dedupe_key);

-- The claim path is the hottest query in the system; keep it a partial index scan.
CREATE INDEX outbox_events_claimable_idx ON outbox_events (visible_at, created_at)
    WHERE status = 'pending';
CREATE INDEX outbox_events_lease_idx ON outbox_events (claim_expires_at)
    WHERE status = 'claimed';
CREATE INDEX outbox_events_status_idx ON outbox_events (status);
CREATE INDEX outbox_events_source_event_idx ON outbox_events (source_event_id);

-- One run per (source event, pipeline version): a replay reuses the run rather than
-- forking a second one (section 10.4).
CREATE TABLE pipeline_runs (
    id               uuid PRIMARY KEY,
    workspace_id     uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    graph_space_id   uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE RESTRICT,
    source_event_id  uuid NOT NULL REFERENCES source_events (id) ON DELETE CASCADE,
    pipeline_version int NOT NULL,
    status           text NOT NULL,
    attempts         int NOT NULL DEFAULT 0,
    last_error       text NOT NULL DEFAULT '',
    error_class      text NOT NULL DEFAULT '',
    started_at       timestamptz NULL,
    finished_at      timestamptz NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX pipeline_runs_event_version_key
    ON pipeline_runs (workspace_id, source_event_id, pipeline_version);
CREATE INDEX pipeline_runs_status_idx ON pipeline_runs (status) WHERE status <> 'succeeded';

-- The durable stage execution key (section 10.4). A succeeded stage returns its
-- recorded output instead of re-executing.
CREATE TABLE pipeline_stage_runs (
    id               uuid PRIMARY KEY,
    pipeline_run_id  uuid NOT NULL REFERENCES pipeline_runs (id) ON DELETE CASCADE,
    workspace_id     uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    source_event_id  uuid NOT NULL REFERENCES source_events (id) ON DELETE CASCADE,
    stage_name       text NOT NULL,
    stage_version    int NOT NULL,
    status           text NOT NULL,
    attempts         int NOT NULL DEFAULT 0,
    output_ref       jsonb NOT NULL DEFAULT '{}'::jsonb,
    last_error       text NOT NULL DEFAULT '',
    error_class      text NOT NULL DEFAULT '',
    started_at       timestamptz NULL,
    finished_at      timestamptz NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX pipeline_stage_runs_key
    ON pipeline_stage_runs (pipeline_run_id, stage_name, stage_version);
CREATE INDEX pipeline_stage_runs_event_idx ON pipeline_stage_runs (workspace_id, source_event_id);
