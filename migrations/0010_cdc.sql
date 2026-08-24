-- CDC streams: how rows become knowledge, and where a connector has got to
-- (AGENTS.md section 11).
--
-- Mapping and checkpoint live in one row because they are the same unit of configuration.
-- A stream nobody can interpret is not worth checkpointing, and a mapping with no position
-- cannot be resumed.

CREATE TABLE cdc_streams (
    id           uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    source_id    uuid NOT NULL REFERENCES sources (id) ON DELETE CASCADE,
    -- The table or topic: "public.customers".
    stream       text NOT NULL,

    -- Deterministic column-to-predicate mapping. JSON because it is read and written whole
    -- and never queried by its interior, the same reasoning as ontology versions.
    mapping jsonb NOT NULL DEFAULT '{}'::jsonb,

    -- The connector's position. Advanced only after an event is durably accepted, so a
    -- crash between the two replays the event rather than skipping it — which is safe
    -- because ingestion is keyed by idempotency key (AGENTS.md sections 10.2, 11.1).
    last_offset      text NOT NULL DEFAULT '',
    last_sequence    text NOT NULL DEFAULT '',
    last_commit_time timestamptz NULL,
    events_consumed  bigint NOT NULL DEFAULT 0,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    UNIQUE (workspace_id, source_id, stream)
);

CREATE INDEX cdc_streams_source_idx ON cdc_streams (workspace_id, source_id);

-- Which upstream record a source event describes, so a later change to the same row can
-- find the claims the earlier one produced.
--
-- external_id already holds this, but it is only unique per source by convention; the index
-- is what makes "every claim from this row" a lookup rather than a scan. Tombstones need
-- exactly that query (AGENTS.md section 11.3).
CREATE INDEX source_events_record_idx
    ON source_events (workspace_id, source_id, external_id)
    WHERE external_id <> '';
