-- Model-run tracking (AGENTS.md section 13.2).
--
-- Every interaction with a model is recorded, including the ones whose output was
-- rejected. A run that produced unusable output is exactly the run an operator needs to
-- see, so it is stored rather than dropped.
--
-- Request and response are stored as hashes, not content. The prompt embeds source
-- material, and copying that here would scatter sensitive text outside the archive that
-- deliberately holds it. Provider credentials are never stored.

CREATE TABLE model_runs (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    graph_space_id uuid NULL REFERENCES graph_spaces (id) ON DELETE SET NULL,

    provider        text NOT NULL,
    model           text NOT NULL,
    prompt_template text NOT NULL DEFAULT '',
    prompt_version  int  NOT NULL DEFAULT 1,

    request_hash  text NOT NULL,
    response_hash text NOT NULL DEFAULT '',

    prompt_tokens     int NOT NULL DEFAULT 0,
    completion_tokens int NOT NULL DEFAULT 0,
    total_tokens      int NOT NULL DEFAULT 0,
    -- Cost in millionths of a currency unit, so accounting never rounds through a float.
    cost_micros       bigint NOT NULL DEFAULT 0,

    latency_ms int NOT NULL DEFAULT 0,
    status     text NOT NULL,

    validation_error text NOT NULL DEFAULT '',
    -- A bounded sample of rejected output, kept only for invalid runs so the shape of the
    -- failure is diagnosable without storing whole responses.
    response_excerpt text NOT NULL DEFAULT '',

    source_event_id uuid NULL REFERENCES source_events (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX model_runs_workspace_created_idx ON model_runs (workspace_id, created_at DESC);
CREATE INDEX model_runs_source_event_idx ON model_runs (source_event_id);
-- Failures and rejections are what get reviewed, so keep them cheap to find.
CREATE INDEX model_runs_problem_idx ON model_runs (workspace_id, created_at DESC)
    WHERE status <> 'succeeded';

-- Evidence and derivations already carry model_run_id. Now that the table exists, tie
-- them to it so a claim can be traced to the exact model interaction that proposed it.
ALTER TABLE evidence
    ADD CONSTRAINT evidence_model_run_fk
    FOREIGN KEY (model_run_id) REFERENCES model_runs (id) ON DELETE SET NULL;

ALTER TABLE derivations
    ADD CONSTRAINT derivations_model_run_fk
    FOREIGN KEY (model_run_id) REFERENCES model_runs (id) ON DELETE SET NULL;
