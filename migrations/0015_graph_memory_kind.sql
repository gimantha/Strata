-- Memory-kind policy reaches graph traversal (AGENTS.md section 22.4).
--
-- A rule denying a memory kind narrowed the vector and lexical projections and did nothing
-- to the graph. Traversal from a permitted entity still disclosed relationships the rule
-- excluded, which is the same failure collection-scoped policy had in migration 0014 and
-- has the same cause: the filter had no column to apply to.
--
-- Unlike entity type — which lives on the entity rather than the edge, and so has to be
-- checked when a hit is hydrated into a name — a memory kind belongs to the assertion the
-- edge was derived from. It can travel with the edge, so the narrowing happens inside the
-- walk rather than at its results, which is what section 22.4 asks for.

ALTER TABLE graph_edges ADD COLUMN memory_kind text NOT NULL DEFAULT '';

UPDATE graph_edges ge
SET memory_kind = a.memory_kind
FROM assertions a
WHERE a.id = ge.assertion_id AND a.memory_kind <> '';

-- Partial: a deployment that never restricts by memory kind should not pay for the index,
-- and an edge with no kind is excluded by an allow-list anyway.
CREATE INDEX graph_edges_memory_kind_idx ON graph_edges (workspace_id, memory_kind)
    WHERE memory_kind <> '';
