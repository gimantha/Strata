-- Workspace-scoped predicate vocabulary. Extraction may use natural variants while the
-- assertion ledger commits one stable predicate identity and one set of temporal rules.
CREATE TABLE predicate_aliases (
    id           uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    predicate_id uuid NOT NULL REFERENCES predicates (id) ON DELETE CASCADE,
    alias        text NOT NULL,
    normalized   text NOT NULL,
    confidence   real NOT NULL DEFAULT 1 CHECK (confidence >= 0 AND confidence <= 1),
    created_by   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),

    UNIQUE (workspace_id, normalized)
);

CREATE INDEX predicate_aliases_predicate_idx
    ON predicate_aliases (workspace_id, predicate_id);
