-- Canonical ingestion ledger: sources, artifacts, source events, episodes, chunks.
--
-- Source events are immutable apart from processing status. Corrections and updates
-- arrive as new events; nothing here is ever overwritten (AGENTS.md sections 2.1,
-- 6.3, 11.2).

CREATE TABLE sources (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    kind           text NOT NULL,
    name           text NOT NULL,
    uri            text NOT NULL DEFAULT '',
    trust_level    text NOT NULL,
    classification text NOT NULL,
    metadata       jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX sources_workspace_name_key ON sources (workspace_id, name);

-- Artifacts are content-addressed and workspace-scoped: identical bytes ingested
-- twice, or into two graph spaces, reuse one archived blob. They carry no
-- graph_space_id for exactly that reason - raw material is shared, knowledge is not.
CREATE TABLE artifacts (
    id           uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    content_hash text NOT NULL,
    media_type   text NOT NULL DEFAULT '',
    size_bytes   bigint NOT NULL,
    blob_key     text NOT NULL,
    storage      text NOT NULL,
    metadata     jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT artifacts_size_nonnegative CHECK (size_bytes >= 0)
);

CREATE UNIQUE INDEX artifacts_workspace_content_key ON artifacts (workspace_id, content_hash);

CREATE TABLE source_events (
    id                 uuid PRIMARY KEY,
    workspace_id       uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    graph_space_id     uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE RESTRICT,
    collection_id      uuid NULL REFERENCES collections (id) ON DELETE SET NULL,
    source_id          uuid NOT NULL REFERENCES sources (id) ON DELETE RESTRICT,
    external_id        text NOT NULL DEFAULT '',
    event_type         text NOT NULL DEFAULT '',
    operation          text NOT NULL,
    content_hash       text NOT NULL,
    idempotency_key    text NOT NULL,

    -- World and source clocks, as reported upstream (sections 7.1, 11.1).
    event_time         timestamptz NULL,
    source_time        timestamptz NULL,
    source_commit_time timestamptz NULL,
    source_sequence    text NOT NULL DEFAULT '',
    source_version     text NOT NULL DEFAULT '',

    -- Knowledge clocks, set by this system.
    observed_at        timestamptz NOT NULL,
    recorded_at        timestamptz NOT NULL,

    raw_artifact_id    uuid NOT NULL REFERENCES artifacts (id) ON DELETE RESTRICT,
    media_type         text NOT NULL DEFAULT '',
    status             text NOT NULL,
    classification     text NOT NULL,
    created_by_id      text NOT NULL DEFAULT '',
    created_by_kind    text NOT NULL DEFAULT '',
    created_by_name    text NOT NULL DEFAULT '',
    metadata           jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- Primary idempotency guard: a replayed event cannot become a second event.
CREATE UNIQUE INDEX source_events_idempotency_key
    ON source_events (workspace_id, source_id, idempotency_key);

-- Upstream identity guard, applied only when the source supplies BOTH an external
-- id and a version. A CDC stream that reports successive updates to one row without
-- versions must still be able to append new events (section 11.2), so those rows are
-- intentionally excluded from this constraint.
CREATE UNIQUE INDEX source_events_external_version_key
    ON source_events (workspace_id, source_id, external_id, source_version)
    WHERE external_id <> '' AND source_version <> '';

CREATE INDEX source_events_graph_space_recorded_idx
    ON source_events (workspace_id, graph_space_id, recorded_at DESC);
CREATE INDEX source_events_source_sequence_idx
    ON source_events (workspace_id, source_id, source_sequence);
CREATE INDEX source_events_status_idx ON source_events (status) WHERE status <> 'processed';
CREATE INDEX source_events_artifact_idx ON source_events (raw_artifact_id);

-- Episodes are the smallest semantically meaningful ingestion unit (section 6.5).
CREATE TABLE episodes (
    id              uuid PRIMARY KEY,
    workspace_id    uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    graph_space_id  uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE RESTRICT,
    source_event_id uuid NOT NULL REFERENCES source_events (id) ON DELETE CASCADE,
    artifact_id     uuid NULL REFERENCES artifacts (id) ON DELETE SET NULL,
    sequence        bigint NOT NULL,
    content         text NOT NULL,
    content_type    text NOT NULL DEFAULT '',
    event_time      timestamptz NULL,
    observed_at     timestamptz NOT NULL,
    recorded_at     timestamptz NOT NULL,
    locator         jsonb NOT NULL DEFAULT '{}'::jsonb,
    classification  text NOT NULL,
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- Deterministic ordering per event makes segmentation replay idempotent.
CREATE UNIQUE INDEX episodes_event_sequence_key
    ON episodes (workspace_id, source_event_id, sequence);
CREATE INDEX episodes_graph_space_idx ON episodes (workspace_id, graph_space_id);

-- Chunks are retrieval and extraction units, not facts (section 6.6). Offsets are
-- relative to the parent episode's content; locator carries artifact-level position.
CREATE TABLE chunks (
    id              uuid PRIMARY KEY,
    workspace_id    uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    graph_space_id  uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE RESTRICT,
    source_event_id uuid NOT NULL REFERENCES source_events (id) ON DELETE CASCADE,
    episode_id      uuid NOT NULL REFERENCES episodes (id) ON DELETE CASCADE,
    artifact_id     uuid NULL REFERENCES artifacts (id) ON DELETE SET NULL,
    sequence        bigint NOT NULL,
    content         text NOT NULL,
    content_type    text NOT NULL DEFAULT '',
    token_count     int NOT NULL DEFAULT 0,
    char_start      int NOT NULL DEFAULT 0,
    char_end        int NOT NULL DEFAULT 0,
    byte_start      int NOT NULL DEFAULT 0,
    byte_end        int NOT NULL DEFAULT 0,
    locator         jsonb NOT NULL DEFAULT '{}'::jsonb,
    classification  text NOT NULL,
    metadata        jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT chunks_offsets_ordered CHECK (char_end >= char_start AND byte_end >= byte_start)
);

CREATE UNIQUE INDEX chunks_episode_sequence_key ON chunks (workspace_id, episode_id, sequence);
CREATE INDEX chunks_source_event_idx ON chunks (workspace_id, source_event_id);
