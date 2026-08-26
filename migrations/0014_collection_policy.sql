-- Collection-scoped policy reaches the projections (AGENTS.md section 22.4).
--
-- PolicyFilters has carried AllowedCollections and DeniedCollections since phase 11, and a
-- deny rule naming a collection has been setting them. Nothing applied them: the projections
-- had no collection to filter on, and applyPolicyFilters had no branch for one. A policy that
-- denied a collection therefore excluded nothing from vector, lexical, or graph retrieval,
-- which is precisely what section 22.4 forbids — retrieving unauthorized data and relying on
-- something downstream to hide it.
--
-- Nullable, because not every projected record belongs to a collection. An entity is not
-- ingested into one, and an event may be ingested without naming one. A record with no
-- collection is not collection-scoped material, so an allow rule about collections must not
-- hide it — the same reasoning already applied to entity_type and predicate.

ALTER TABLE vector_records ADD COLUMN collection_id uuid NULL
    REFERENCES collections (id) ON DELETE SET NULL;
ALTER TABLE lexical_records ADD COLUMN collection_id uuid NULL
    REFERENCES collections (id) ON DELETE SET NULL;
ALTER TABLE graph_edges ADD COLUMN collection_id uuid NULL
    REFERENCES collections (id) ON DELETE SET NULL;

-- Backfilled rather than left to a rebuild. The projections are rebuildable, so a rebuild
-- would also populate this — but until someone ran one the hole would stay open, and a
-- security fix that waits for an operator to notice is not a fix.
UPDATE vector_records v
SET collection_id = se.collection_id
FROM source_events se
WHERE se.id = v.source_event_id AND se.collection_id IS NOT NULL;

UPDATE lexical_records l
SET collection_id = se.collection_id
FROM source_events se
WHERE se.id = l.source_event_id AND se.collection_id IS NOT NULL;

UPDATE graph_edges ge
SET collection_id = se.collection_id
FROM assertions a
JOIN source_events se ON se.id = a.source_event_id
WHERE a.id = ge.assertion_id AND se.collection_id IS NOT NULL;

-- Partial, because most deployments use no collections at all and an index over a column
-- that is NULL everywhere costs writes and buys nothing.
CREATE INDEX vector_records_collection_idx ON vector_records (workspace_id, collection_id)
    WHERE collection_id IS NOT NULL;
CREATE INDEX lexical_records_collection_idx ON lexical_records (workspace_id, collection_id)
    WHERE collection_id IS NOT NULL;
CREATE INDEX graph_edges_collection_idx ON graph_edges (workspace_id, collection_id)
    WHERE collection_id IS NOT NULL;
