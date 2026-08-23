-- Entity resolution: stable identifiers, reversible merges, and a decision ledger.
--
-- Resolution is the highest-risk component in the system (AGENTS.md section 12). Merging
-- two identities that are not the same thing corrupts every fact about both, and the
-- corruption is hard to notice. The schema is built so that no merge is ever destructive:
-- identities are redirected, never collapsed, and every decision is recorded well enough
-- to be reversed.

-- Trigram matching backs fuzzy candidate generation (rung 5 of the ladder).
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Rungs 1 and 2: an upstream primary key or a configured business key. These settle
-- identity outright, which is why they are unique within a graph space.
CREATE TABLE entity_identifiers (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    graph_space_id uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE RESTRICT,
    entity_id      uuid NOT NULL REFERENCES entities (id) ON DELETE CASCADE,
    kind           text NOT NULL,
    -- The source id for an upstream identifier, or the key type such as "email" for a
    -- domain key.
    namespace      text NOT NULL,
    value          text NOT NULL,
    source_id      uuid NULL REFERENCES sources (id) ON DELETE SET NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- One identifier names one identity. A second entity claiming the same key is a conflict
-- to surface, not a duplicate to accept.
CREATE UNIQUE INDEX entity_identifiers_key
    ON entity_identifiers (workspace_id, graph_space_id, kind, namespace, value);
CREATE INDEX entity_identifiers_entity_idx ON entity_identifiers (entity_id);

-- Reversible merges (AGENTS.md section 12.3).
--
-- A merged entity is redirected, not deleted, and the assertions that referenced it keep
-- referencing it. The original source-local identity survives, which is what makes a split
-- possible later: undoing a merge is clearing a pointer, not reconstructing history.
ALTER TABLE entities ADD COLUMN merged_into_id uuid NULL REFERENCES entities (id) ON DELETE SET NULL;
ALTER TABLE entities ADD COLUMN merged_at timestamptz NULL;

CREATE INDEX entities_merged_into_idx ON entities (merged_into_id) WHERE merged_into_id IS NOT NULL;
-- An entity cannot be merged into itself.
ALTER TABLE entities ADD CONSTRAINT entities_no_self_merge CHECK (merged_into_id IS NULL OR merged_into_id <> id);

-- The decision ledger (AGENTS.md section 12.2).
CREATE TABLE resolution_decisions (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    graph_space_id uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE RESTRICT,

    mention_text text NOT NULL DEFAULT '',
    mention_type text NOT NULL DEFAULT '',

    method             text NOT NULL,
    chosen_entity_id   uuid NULL REFERENCES entities (id) ON DELETE SET NULL,
    -- For a merge, the identity that was redirected.
    previous_entity_id uuid NULL REFERENCES entities (id) ON DELETE SET NULL,
    confidence         double precision NOT NULL DEFAULT 1.0,
    -- Which version of the resolver decided, so a change in matching behavior can be
    -- correlated with a change in outcomes.
    resolver_version   int NOT NULL,

    features jsonb NOT NULL DEFAULT '{}'::jsonb,

    human_override  boolean NOT NULL DEFAULT false,
    actor_id        text NOT NULL DEFAULT '',
    reason          text NOT NULL DEFAULT '',
    source_event_id uuid NULL REFERENCES source_events (id) ON DELETE SET NULL,

    -- A reverted decision stays in the ledger: reversing a merge must not erase the record
    -- that it happened.
    reverted_at timestamptz NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT resolution_decisions_confidence_range CHECK (confidence >= 0 AND confidence <= 1)
);

CREATE INDEX resolution_decisions_entity_idx ON resolution_decisions (chosen_entity_id);
CREATE INDEX resolution_decisions_created_idx ON resolution_decisions (workspace_id, created_at DESC);
-- Ambiguous outcomes and human overrides are the review queue.
CREATE INDEX resolution_decisions_review_idx ON resolution_decisions (workspace_id, created_at DESC)
    WHERE method = 'ambiguous_kept_separate' OR human_override;

-- Every identity that was considered, with its score and the features behind it. A score
-- with no explanation cannot be reviewed.
CREATE TABLE resolution_candidates (
    decision_id uuid NOT NULL REFERENCES resolution_decisions (id) ON DELETE CASCADE,
    entity_id   uuid NOT NULL REFERENCES entities (id) ON DELETE CASCADE,
    score       double precision NOT NULL,
    features    jsonb NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (decision_id, entity_id)
);

CREATE INDEX resolution_candidates_entity_idx ON resolution_candidates (entity_id);

-- Fuzzy candidate generation over known names.
CREATE INDEX entity_aliases_trgm_idx ON entity_aliases USING gin (normalized gin_trgm_ops);
