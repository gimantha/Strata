-- Attribute-based policy, and the traces that record what it decided
-- (AGENTS.md sections 22 and 6.12).

CREATE TABLE policy_sets (
    id           uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    version      integer NOT NULL,
    name         text NOT NULL,
    notes        text NOT NULL DEFAULT '',
    active       boolean NOT NULL DEFAULT false,

    -- The ceiling for a principal no rule mentions. Stated once and visibly, because it is
    -- the setting that decides whether this workspace is closed or open by default.
    default_clearance text NOT NULL DEFAULT 'internal',

    -- Rules as JSON: read and written whole, never queried by their interior, and required
    -- to round-trip exactly so an audit record naming a version can be checked against what
    -- that version actually said.
    rules jsonb NOT NULL DEFAULT '[]'::jsonb,

    created_at timestamptz NOT NULL DEFAULT now(),
    created_by text NOT NULL DEFAULT '',

    UNIQUE (workspace_id, version)
);

CREATE INDEX policy_sets_workspace_idx ON policy_sets (workspace_id, version DESC);

-- At most one active policy set per workspace. Two would make "the policy" ambiguous at
-- exactly the moment somebody needs to know what it was.
CREATE UNIQUE INDEX policy_sets_active_key ON policy_sets (workspace_id) WHERE active;

-- Query-time explainability (AGENTS.md section 6.12).
--
-- Deferred from phase 8 on purpose: section 6.12 marks query text "subject to
-- policy/redaction", and there was no policy to redact against until now. A trace records
-- what was asked, what the policy decided, what was considered, and what was returned.
CREATE TABLE retrieval_traces (
    id             uuid PRIMARY KEY,
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
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX retrieval_traces_workspace_idx
    ON retrieval_traces (workspace_id, graph_space_id, query_time DESC);
CREATE INDEX retrieval_traces_principal_idx
    ON retrieval_traces (workspace_id, principal_id, query_time DESC);

-- A principal's clearance inside one workspace, when it differs from the policy default.
--
-- On the grant because clearance is per workspace: the same person can be cleared for
-- restricted material in one tenant and internal-only in another, and a single column on the
-- principal could not express that.
ALTER TABLE principal_workspaces
    ADD COLUMN max_classification text NOT NULL DEFAULT '';

-- Policy filter columns on the projections (AGENTS.md section 22.4).
--
-- Source and predicate are copied onto projected records for the same reason validity and
-- classification already are: a policy that restricts a principal to certain sources has to
-- narrow the query, not the result set. Joining back to the ledger per candidate would make
-- every filtered query slower than an unfiltered one, which is how "filter afterwards"
-- becomes the tempting shortcut.
ALTER TABLE vector_records  ADD COLUMN source_id uuid NULL REFERENCES sources (id) ON DELETE CASCADE;
ALTER TABLE lexical_records ADD COLUMN source_id uuid NULL REFERENCES sources (id) ON DELETE CASCADE;
ALTER TABLE vector_records  ADD COLUMN predicate text NOT NULL DEFAULT '';
ALTER TABLE lexical_records ADD COLUMN predicate text NOT NULL DEFAULT '';

CREATE INDEX vector_records_source_idx
    ON vector_records (workspace_id, graph_space_id, source_id) WHERE source_id IS NOT NULL;
CREATE INDEX lexical_records_source_idx
    ON lexical_records (workspace_id, graph_space_id, source_id) WHERE source_id IS NOT NULL;

-- Graph edges already carry predicate and classification; they need the source for the same
-- reason.
ALTER TABLE graph_edges ADD COLUMN source_id uuid NULL REFERENCES sources (id) ON DELETE CASCADE;
