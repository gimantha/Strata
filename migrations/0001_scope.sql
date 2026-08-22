-- Scope hierarchy: Workspace -> GraphSpace -> Collection (AGENTS.md section 6.1).
--
-- Workspace is the hard tenant and security boundary. Every knowledge table added
-- by later migrations carries workspace_id, and most also carry graph_space_id.

CREATE TABLE workspaces (
    id         uuid PRIMARY KEY,
    slug       text NOT NULL,
    name       text NOT NULL,
    metadata   jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX workspaces_slug_key ON workspaces (slug);

CREATE TABLE graph_spaces (
    id           uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    slug         text NOT NULL,
    name         text NOT NULL,
    metadata     jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- Slugs are unique per workspace, never globally: two tenants may both have "main".
CREATE UNIQUE INDEX graph_spaces_workspace_slug_key ON graph_spaces (workspace_id, slug);

CREATE TABLE collections (
    id             uuid PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspaces (id) ON DELETE RESTRICT,
    graph_space_id uuid NOT NULL REFERENCES graph_spaces (id) ON DELETE RESTRICT,
    slug           text NOT NULL,
    name           text NOT NULL,
    metadata       jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX collections_graph_space_slug_key ON collections (graph_space_id, slug);
CREATE INDEX collections_workspace_idx ON collections (workspace_id);
