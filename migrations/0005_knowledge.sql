-- Canonical knowledge: predicates, entities, assertions, evidence, derivations.
--
-- Assertions are immutable. The only columns that change after commit are the
-- knowledge-time markers (status, superseded_at, retracted_at, conflict_set_id), which
-- record what the system believes now without rewriting what it believed before or what
-- was true in the world (AGENTS.md sections 2.1, 6.9, 14.3).

-- Predicate registry. Predicates carry the semantics that make contradiction handling
-- something other than guesswork (AGENTS.md section 8). Open mode registers unknown
-- names as candidates; ontology mode in phase 9 constrains them.
CREATE TABLE predicates (
    id                  uuid PRIMARY KEY,
    workspace_id        uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    name                text NOT NULL,
    description         text NOT NULL DEFAULT '',
    subject_types       text[] NOT NULL DEFAULT '{}',
    object_types        text[] NOT NULL DEFAULT '{}',
    object_kinds        text[] NOT NULL DEFAULT '{}',
    functional          boolean NOT NULL DEFAULT false,
    inverse_functional  boolean NOT NULL DEFAULT false,
    is_symmetric        boolean NOT NULL DEFAULT false,
    transitive          boolean NOT NULL DEFAULT false,
    temporal_policy     text NOT NULL,
    conflict_policy     text NOT NULL,
    default_memory_kind text NOT NULL,
    sensitivity         text NOT NULL,
    status              text NOT NULL,
    version             int NOT NULL DEFAULT 1,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT predicates_version_positive CHECK (version >= 1)
);

CREATE UNIQUE INDEX predicates_workspace_name_key ON predicates (workspace_id, name);

-- Entities are stable identities. Facts about them live in assertions, never as
-- mutable columns here (AGENTS.md section 6.7).
CREATE TABLE entities (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    graph_space_id uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE RESTRICT,
    canonical_name text NOT NULL,
    entity_type    text NOT NULL,
    metadata       jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    retired_at     timestamptz NULL
);

CREATE INDEX entities_graph_space_type_idx ON entities (workspace_id, graph_space_id, entity_type);
-- Name lookup within a graph space. Not unique: two distinct people may share a name,
-- and deciding whether they are the same identity is entity resolution's job (phase 4),
-- not a database constraint's.
CREATE INDEX entities_name_idx ON entities (workspace_id, graph_space_id, lower(canonical_name));

CREATE TABLE entity_aliases (
    id           uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    entity_id    uuid NOT NULL REFERENCES entities (id) ON DELETE CASCADE,
    alias        text NOT NULL,
    normalized   text NOT NULL,
    source_id    uuid NULL REFERENCES sources (id) ON DELETE SET NULL,
    confidence   double precision NOT NULL DEFAULT 1.0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT entity_aliases_confidence_range CHECK (confidence >= 0 AND confidence <= 1)
);

CREATE UNIQUE INDEX entity_aliases_entity_normalized_key ON entity_aliases (entity_id, normalized);
CREATE INDEX entity_aliases_lookup_idx ON entity_aliases (workspace_id, normalized);

-- Derivations explain claims that were reasoned rather than observed. A derived
-- assertion without one would be an unexplainable fact (AGENTS.md section 6.11).
CREATE TABLE derivations (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    graph_space_id uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE RESTRICT,
    method         text NOT NULL,
    rule_name      text NOT NULL DEFAULT '',
    rule_version   text NOT NULL DEFAULT '',
    model_run_id   uuid NULL,
    parameters     jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX derivations_graph_space_idx ON derivations (workspace_id, graph_space_id);

-- Conflict sets group claims that cannot all hold. Nothing is deleted; the
-- disagreement is recorded and stays visible (AGENTS.md section 14.2).
CREATE TABLE conflict_sets (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    graph_space_id uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE RESTRICT,
    subject_id     uuid NOT NULL REFERENCES entities (id) ON DELETE CASCADE,
    predicate      text NOT NULL,
    scope_key      text NOT NULL DEFAULT '',
    reason         text NOT NULL DEFAULT '',
    resolution     text NOT NULL DEFAULT 'open',
    resolved_at    timestamptz NULL,
    resolved_by    text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX conflict_sets_open_idx ON conflict_sets (workspace_id, graph_space_id)
    WHERE resolution = 'open';

CREATE TABLE assertions (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    graph_space_id uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE RESTRICT,

    subject_id        uuid NOT NULL REFERENCES entities (id) ON DELETE RESTRICT,
    predicate_id      uuid NOT NULL REFERENCES predicates (id) ON DELETE RESTRICT,
    predicate_name    text NOT NULL,
    -- The registry version this claim was validated under. A later change to predicate
    -- semantics must not silently reinterpret claims made under the old definition.
    predicate_version int NOT NULL,

    -- Typed object columns. Values keep their types rather than being stringified,
    -- which is what makes numeric, temporal, and geo reasoning possible later
    -- (AGENTS.md section 6.9).
    object_kind        text NOT NULL,
    object_entity_id   uuid NULL REFERENCES entities (id) ON DELETE RESTRICT,
    object_text        text NULL,
    object_integer     bigint NULL,
    object_decimal     numeric NULL,
    object_boolean     boolean NULL,
    object_timestamp   timestamptz NULL,
    object_date        date NULL,
    object_duration_ns bigint NULL,
    object_geo_lat     double precision NULL,
    object_geo_lon     double precision NULL,
    object_json        jsonb NULL,
    -- object_key is the canonical comparable form, used for equality, deduplication,
    -- and conflict detection without having to branch on kind in SQL.
    object_key         text NOT NULL,

    memory_kind text NOT NULL,
    scope_key   text NOT NULL DEFAULT '',

    -- World time.
    event_time     timestamptz NULL,
    valid_from     timestamptz NULL,
    valid_to       timestamptz NULL,
    effective_from timestamptz NULL,
    effective_to   timestamptz NULL,

    -- Knowledge time.
    observed_at   timestamptz NOT NULL,
    recorded_at   timestamptz NOT NULL,
    superseded_at timestamptz NULL,

    -- Source time.
    source_time        timestamptz NULL,
    source_commit_time timestamptz NULL,
    source_sequence    text NOT NULL DEFAULT '',
    source_version     text NOT NULL DEFAULT '',

    -- Context lifecycle time.
    active_from     timestamptz NULL,
    active_until    timestamptz NULL,
    decay_starts_at timestamptz NULL,
    expires_at      timestamptz NULL,

    confidence           double precision NOT NULL DEFAULT 1.0,
    confidence_breakdown jsonb NULL,
    status               text NOT NULL,
    supersedes_id        uuid NULL REFERENCES assertions (id) ON DELETE SET NULL,
    conflict_set_id      uuid NULL REFERENCES conflict_sets (id) ON DELETE SET NULL,

    provenance_mode text NOT NULL,
    derivation_id   uuid NULL REFERENCES derivations (id) ON DELETE SET NULL,
    source_event_id uuid NOT NULL REFERENCES source_events (id) ON DELETE RESTRICT,

    -- Replay guard: reprocessing an event produces the same fingerprint and collides
    -- instead of duplicating knowledge.
    fingerprint text NOT NULL,

    retracted_at      timestamptz NULL,
    retraction_reason text NOT NULL DEFAULT '',

    classification  text NOT NULL,
    created_by_id   text NOT NULL DEFAULT '',
    created_by_kind text NOT NULL DEFAULT '',
    created_by_name text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT assertions_confidence_range CHECK (confidence >= 0 AND confidence <= 1),
    CONSTRAINT assertions_validity_ordered CHECK (valid_to IS NULL OR valid_from IS NULL OR valid_to >= valid_from),
    CONSTRAINT assertions_active_ordered CHECK (active_until IS NULL OR active_from IS NULL OR active_until >= active_from),
    -- Inference must always name what produced it.
    CONSTRAINT assertions_derivation_required CHECK (
        provenance_mode NOT IN ('inferred', 'derived') OR derivation_id IS NOT NULL
    )
);

CREATE UNIQUE INDEX assertions_fingerprint_key ON assertions (workspace_id, fingerprint);

-- The primary read path: current belief about a subject.
CREATE INDEX assertions_subject_idx
    ON assertions (workspace_id, graph_space_id, subject_id, predicate_name);
-- Knowledge-time reconstruction: what was believed as of an instant.
CREATE INDEX assertions_knowledge_time_idx
    ON assertions (workspace_id, graph_space_id, recorded_at, superseded_at);
-- World-time filtering.
CREATE INDEX assertions_validity_idx
    ON assertions (workspace_id, graph_space_id, valid_from, valid_to);
CREATE INDEX assertions_object_entity_idx ON assertions (workspace_id, object_entity_id)
    WHERE object_entity_id IS NOT NULL;
CREATE INDEX assertions_source_event_idx ON assertions (workspace_id, source_event_id);
CREATE INDEX assertions_supersedes_idx ON assertions (supersedes_id) WHERE supersedes_id IS NOT NULL;
CREATE INDEX assertions_conflict_idx ON assertions (conflict_set_id) WHERE conflict_set_id IS NOT NULL;
-- Current belief is the hot query, so keep it a partial index.
CREATE INDEX assertions_believable_idx
    ON assertions (workspace_id, graph_space_id, subject_id)
    WHERE status IN ('active', 'disputed');

-- Which assertions a derivation reasoned from. Separate from derivations so the inputs
-- are joinable and a changed input can be traced to what it supported.
CREATE TABLE derivation_inputs (
    derivation_id uuid NOT NULL REFERENCES derivations (id) ON DELETE CASCADE,
    assertion_id  uuid NOT NULL REFERENCES assertions (id) ON DELETE CASCADE,
    PRIMARY KEY (derivation_id, assertion_id)
);

CREATE INDEX derivation_inputs_assertion_idx ON derivation_inputs (assertion_id);

-- Evidence ties a claim to the exact source material behind it, so every fact can be
-- walked back to archived bytes (AGENTS.md section 6.10).
CREATE TABLE evidence (
    id              uuid PRIMARY KEY,
    workspace_id    uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    assertion_id    uuid NOT NULL REFERENCES assertions (id) ON DELETE CASCADE,
    episode_id      uuid NOT NULL REFERENCES episodes (id) ON DELETE RESTRICT,
    chunk_id        uuid NULL REFERENCES chunks (id) ON DELETE SET NULL,
    artifact_id     uuid NULL REFERENCES artifacts (id) ON DELETE SET NULL,
    source_event_id uuid NOT NULL REFERENCES source_events (id) ON DELETE RESTRICT,
    quote_start     int NULL,
    quote_end       int NULL,
    extracted_text  text NOT NULL DEFAULT '',
    model_run_id    uuid NULL,
    confidence      double precision NOT NULL DEFAULT 1.0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT evidence_confidence_range CHECK (confidence >= 0 AND confidence <= 1),
    CONSTRAINT evidence_quote_ordered CHECK (quote_end IS NULL OR quote_start IS NULL OR quote_end >= quote_start)
);

CREATE INDEX evidence_assertion_idx ON evidence (assertion_id);
CREATE INDEX evidence_episode_idx ON evidence (workspace_id, episode_id);
CREATE INDEX evidence_source_event_idx ON evidence (workspace_id, source_event_id);
