-- Identity, workspace grants, and the security audit trail.
--
-- Authentication material (API key secrets) is deliberately NOT stored here. Keys
-- live in a secret-managed file so the database never holds a credential; this
-- table exists so grants and audit rows have a principal to reference
-- (AGENTS.md sections 22.1, 22.5, 22.6). Phase 11 replaces the file with real auth.

CREATE TABLE principals (
    id           text PRIMARY KEY,
    kind         text NOT NULL,
    display_name text NOT NULL,
    system_role  text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- A principal reaches a workspace only through a grant. Nothing about a request
-- body can substitute for a row here (AGENTS.md section 22.1).
CREATE TABLE principal_workspaces (
    principal_id text NOT NULL REFERENCES principals (id) ON DELETE CASCADE,
    workspace_id uuid NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    role         text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (principal_id, workspace_id)
);

CREATE INDEX principal_workspaces_workspace_idx ON principal_workspaces (workspace_id);

-- Security-sensitive operations are recorded regardless of outcome
-- (AGENTS.md section 22.6). Audit rows never contain source content.
CREATE TABLE audit_events (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NULL REFERENCES workspaces (id) ON DELETE SET NULL,
    graph_space_id uuid NULL,
    principal_id   text NOT NULL DEFAULT '',
    action         text NOT NULL,
    target_kind    text NOT NULL DEFAULT '',
    target_id      text NOT NULL DEFAULT '',
    outcome        text NOT NULL,
    detail         jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_workspace_created_idx ON audit_events (workspace_id, created_at DESC);
CREATE INDEX audit_events_principal_created_idx ON audit_events (principal_id, created_at DESC);
