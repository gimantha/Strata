-- Memory lifecycle on the projections (AGENTS.md sections 21.2, 21.3).
--
-- The context clock was already on assertions; it was never on the projections, so retrieval
-- could not tell an expired memory from a current one. A working note that "stops being
-- active" while still being returned by every search has not stopped being active in any
-- sense the user would recognize.
--
-- Copied rather than joined, for the same reason validity and classification already are:
-- these decide what a query returns and must narrow it, not filter its results.
ALTER TABLE vector_records  ADD COLUMN active_from timestamptz NULL;
ALTER TABLE vector_records  ADD COLUMN active_until timestamptz NULL;
ALTER TABLE vector_records  ADD COLUMN decay_starts_at timestamptz NULL;
ALTER TABLE vector_records  ADD COLUMN expires_at timestamptz NULL;

ALTER TABLE lexical_records ADD COLUMN active_from timestamptz NULL;
ALTER TABLE lexical_records ADD COLUMN active_until timestamptz NULL;
ALTER TABLE lexical_records ADD COLUMN decay_starts_at timestamptz NULL;
ALTER TABLE lexical_records ADD COLUMN expires_at timestamptz NULL;

ALTER TABLE graph_edges ADD COLUMN active_until timestamptz NULL;
ALTER TABLE graph_edges ADD COLUMN expires_at timestamptz NULL;

-- Most records never expire, so the indexes only cover the ones that do.
CREATE INDEX vector_records_lifecycle_idx
    ON vector_records (workspace_id, graph_space_id, active_until)
    WHERE active_until IS NOT NULL OR expires_at IS NOT NULL;
CREATE INDEX lexical_records_lifecycle_idx
    ON lexical_records (workspace_id, graph_space_id, active_until)
    WHERE active_until IS NOT NULL OR expires_at IS NOT NULL;

-- Why a claim was taken out of active context, when it was.
--
-- Deactivation is not retraction and not deletion (AGENTS.md section 21.4): the claim stays
-- true, cited, and answerable as of an earlier instant. Recording the reason is what lets a
-- reader tell "we stopped using this" from "this turned out to be wrong", which a single
-- delete flag cannot express.
ALTER TABLE assertions ADD COLUMN deactivated_at timestamptz NULL;
ALTER TABLE assertions ADD COLUMN deactivation_reason text NOT NULL DEFAULT '';

CREATE INDEX assertions_deactivated_idx
    ON assertions (workspace_id, deactivated_at)
    WHERE deactivated_at IS NOT NULL;
