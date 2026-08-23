-- Retrieval projections: vector, lexical, and graph.
--
-- Everything here is derived. Each table can be dropped entirely and rebuilt from the
-- canonical ledger, and that is the property the whole architecture rests on: the ledger is
-- authoritative, these are views of it that happen to be materialized
-- (AGENTS.md sections 2.3, 15.2).
--
-- Because they are rebuildable, they carry no history and no provenance of their own. They
-- point back at the canonical record and copy only what a query needs to filter on before
-- ranking.

CREATE EXTENSION IF NOT EXISTS vector;

-- Where each projection has got to. Replay resumes from here, and a projection that has
-- fallen behind is visible rather than merely slow (AGENTS.md section 15.2).
CREATE TABLE projection_checkpoints (
    workspace_id     uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    projection       text NOT NULL,
    -- The last canonical position consumed. Recorded time is the cursor because it is
    -- monotonic in the ledger and survives identifiers being unordered.
    last_recorded_at timestamptz NULL,
    last_record_id   uuid NULL,
    records_projected bigint NOT NULL DEFAULT 0,
    last_error       text NOT NULL DEFAULT '',
    rebuilt_at       timestamptz NULL,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, projection)
);

-- Vector projection (AGENTS.md section 17).
--
-- Several semantic surfaces are embedded, not only chunks: a question about an entity's name
-- and a question about a fact are different searches, and collapsing them into chunk text
-- loses both.
CREATE TABLE vector_records (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    graph_space_id uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE CASCADE,

    -- Which surface this vector represents, and the canonical row it came from.
    surface   text NOT NULL,
    record_id uuid NOT NULL,

    -- A vector is only comparable with others from the same model. Storing the model and
    -- version means a re-embedding can run alongside the old vectors and swap atomically.
    embedding_model   text NOT NULL,
    embedding_version int  NOT NULL,
    embedding         vector(1536) NOT NULL,

    -- Copied from the canonical record so retrieval can filter before ranking rather than
    -- ranking everything and filtering after.
    valid_from     timestamptz NULL,
    valid_to       timestamptz NULL,
    status         text NOT NULL DEFAULT '',
    classification text NOT NULL,
    memory_kind    text NOT NULL DEFAULT '',
    source_event_id uuid NULL REFERENCES source_events (id) ON DELETE CASCADE,

    content_hash text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- One vector per surface, record, and model version.
CREATE UNIQUE INDEX vector_records_key
    ON vector_records (workspace_id, surface, record_id, embedding_model, embedding_version);
CREATE INDEX vector_records_scope_idx ON vector_records (workspace_id, graph_space_id, surface);
CREATE INDEX vector_records_source_event_idx ON vector_records (source_event_id);

-- HNSW on cosine distance. Vectors are stored normalized, so cosine and inner product agree.
CREATE INDEX vector_records_embedding_idx ON vector_records
    USING hnsw (embedding vector_cosine_ops);

-- Lexical projection (AGENTS.md section 18).
--
-- Vectors are weak exactly where identifiers, error codes, product numbers, and rare terms
-- live, which is a large share of what anyone actually searches for.
CREATE TABLE lexical_records (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    graph_space_id uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE CASCADE,

    surface   text NOT NULL,
    record_id uuid NOT NULL,

    content text NOT NULL,
    -- Generated rather than maintained: a stored tsvector that the application forgets to
    -- update is worse than no index at all.
    search_vector tsvector GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,

    valid_from      timestamptz NULL,
    valid_to        timestamptz NULL,
    status          text NOT NULL DEFAULT '',
    classification  text NOT NULL,
    memory_kind     text NOT NULL DEFAULT '',
    source_event_id uuid NULL REFERENCES source_events (id) ON DELETE CASCADE,

    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX lexical_records_key ON lexical_records (workspace_id, surface, record_id);
CREATE INDEX lexical_records_scope_idx ON lexical_records (workspace_id, graph_space_id, surface);
CREATE INDEX lexical_records_search_idx ON lexical_records USING gin (search_vector);
-- Trigram alongside full text: stemming helps with prose and actively hurts with an error
-- code or a part number, which is precisely what lexical retrieval is here for.
CREATE INDEX lexical_records_trgm_idx ON lexical_records USING gin (content gin_trgm_ops);
CREATE INDEX lexical_records_source_event_idx ON lexical_records (source_event_id);

-- Graph projection (AGENTS.md section 16).
--
-- Only entity-to-entity assertions become edges. A literal-valued assertion is not an edge
-- in any useful sense, and forcing one would either invent nodes for values or flatten the
-- typed object the ledger works hard to preserve. Each edge carries its assertion id, so the
-- full claim with all its metadata is one lookup away.
CREATE TABLE graph_edges (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    graph_space_id uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE CASCADE,

    subject_id       uuid NOT NULL REFERENCES entities (id) ON DELETE CASCADE,
    predicate        text NOT NULL,
    object_entity_id uuid NOT NULL REFERENCES entities (id) ON DELETE CASCADE,
    assertion_id     uuid NOT NULL REFERENCES assertions (id) ON DELETE CASCADE,

    valid_from     timestamptz NULL,
    valid_to       timestamptz NULL,
    status         text NOT NULL,
    confidence     double precision NOT NULL DEFAULT 1.0,
    classification text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX graph_edges_assertion_key ON graph_edges (assertion_id);
-- Traversal goes both ways, so both directions are indexed.
CREATE INDEX graph_edges_out_idx ON graph_edges (workspace_id, graph_space_id, subject_id, predicate);
CREATE INDEX graph_edges_in_idx ON graph_edges (workspace_id, graph_space_id, object_entity_id, predicate);
-- Expansion almost always wants current edges only, so keep that a partial index.
CREATE INDEX graph_edges_current_idx ON graph_edges (workspace_id, graph_space_id, subject_id)
    WHERE status IN ('active', 'disputed');
