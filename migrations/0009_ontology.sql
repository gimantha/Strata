-- Ontology versions and the graph spaces bound to them (AGENTS.md section 8).
--
-- Two modes coexist by design. Open mode lets extraction invent entity types and predicate
-- names, normalized through the registry, which is how a corpus is explored before anyone
-- knows its shape. Guided mode validates every claim against a bound version, so what the
-- schema does not describe does not silently become knowledge.
--
-- Versions are immutable. Assertions record the version they were validated under, and
-- editing a version in place would silently change what those claims were checked against —
-- the same class of mistake as editing a committed claim.

CREATE TABLE ontology_versions (
    id           uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- Monotonic per workspace. A schema's history is a sequence, not a set.
    version      integer NOT NULL,
    name         text NOT NULL,
    notes        text NOT NULL DEFAULT '',
    status       text NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'active', 'superseded')),

    -- The schema itself. JSON rather than relational tables because a version is read and
    -- written whole, is never queried by its interior, and must round-trip byte-for-byte to
    -- stay auditable. Normalizing it would buy queries nobody makes and cost immutability.
    entity_types jsonb NOT NULL DEFAULT '[]'::jsonb,
    predicates   jsonb NOT NULL DEFAULT '[]'::jsonb,

    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   text NOT NULL DEFAULT '',

    UNIQUE (workspace_id, version)
);

CREATE INDEX ontology_versions_workspace_idx
    ON ontology_versions (workspace_id, version DESC);

-- A graph space is either open or guided by exactly one version.
--
-- On the graph space rather than the workspace because the acceptance criterion is that the
-- same source can be processed both ways: ingest into two spaces, one open and one guided,
-- and compare. A workspace-level setting would make that a migration instead of a choice.
ALTER TABLE graph_spaces
    ADD COLUMN ontology_mode text NOT NULL DEFAULT 'open'
        CHECK (ontology_mode IN ('open', 'guided')),
    ADD COLUMN ontology_version_id uuid NULL REFERENCES ontology_versions (id);

-- Guided mode without a version would reject everything while looking like a broken
-- pipeline. The database refuses the combination rather than letting it be discovered at
-- the first ingest.
ALTER TABLE graph_spaces
    ADD CONSTRAINT graph_spaces_guided_needs_version
        CHECK (ontology_mode = 'open' OR ontology_version_id IS NOT NULL);

-- Which schema a claim was checked against, where relevant (AGENTS.md section 8).
--
-- Nullable: a claim committed in open mode was validated against nothing, and recording a
-- version it never saw would be a lie that survives longer than the mistake.
ALTER TABLE assertions
    ADD COLUMN ontology_version_id uuid NULL REFERENCES ontology_versions (id);

CREATE INDEX assertions_ontology_version_idx
    ON assertions (workspace_id, ontology_version_id)
    WHERE ontology_version_id IS NOT NULL;

-- Why a quarantined claim is being held.
--
-- Quarantine already existed for injection defense; a schema violation is a second reason,
-- and a status that cannot say which is a status nobody can triage.
ALTER TABLE assertions
    ADD COLUMN quarantine_reason text NOT NULL DEFAULT '';

-- Type-constrained retrieval (AGENTS.md sections 19.2 and 16).
--
-- The projections copy the subject's entity type so a query can ask for organizations
-- without joining back to the ledger for every candidate. Empty for chunks, which are
-- passages rather than things.
ALTER TABLE vector_records  ADD COLUMN entity_type text NOT NULL DEFAULT '';
ALTER TABLE lexical_records ADD COLUMN entity_type text NOT NULL DEFAULT '';

CREATE INDEX vector_records_entity_type_idx
    ON vector_records (workspace_id, graph_space_id, entity_type)
    WHERE entity_type <> '';
CREATE INDEX lexical_records_entity_type_idx
    ON lexical_records (workspace_id, graph_space_id, entity_type)
    WHERE entity_type <> '';
