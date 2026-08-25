-- Ordered partitioning for the work fabric (AGENTS.md sections 28.3, phase 14).
--
-- Two workers claiming two events for the same record process them concurrently, and each
-- sees the state before the other. Phase 10 hit this with CDC and fixed it with an advisory
-- lock inside one stage; a partition key fixes it once, for every kind of work, by making the
-- claim itself refuse to hand out two events from the same partition at a time.
--
-- Empty means unpartitioned: work with no ordering requirement stays fully parallel, which is
-- most of it. Serializing everything would be correct and would also make the fleet
-- single-threaded.

ALTER TABLE outbox_events ADD COLUMN partition_key text NOT NULL DEFAULT '';

-- The claim path needs to find, per partition, whether anything is already in flight. A
-- partial index over just the live claims keeps that a lookup rather than a scan.
CREATE INDEX outbox_events_partition_inflight_idx
    ON outbox_events (workspace_id, partition_key)
    WHERE partition_key <> '' AND status = 'claimed';

-- And ordering within a partition is by publication, so a later event never overtakes an
-- earlier one from the same record.
CREATE INDEX outbox_events_partition_order_idx
    ON outbox_events (workspace_id, partition_key, created_at)
    WHERE partition_key <> '' AND status = 'pending';

-- Projection checkpoints under concurrent writers (phase 14).
--
-- The checkpoint is a high-water mark, so two workers finishing out of order must not let the
-- older one move it backwards. Recording which worker last advanced it makes a stuck
-- projection attributable instead of merely stale.
ALTER TABLE projection_checkpoints ADD COLUMN advanced_by text NOT NULL DEFAULT '';
