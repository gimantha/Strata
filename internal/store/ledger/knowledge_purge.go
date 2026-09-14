package ledger

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gimantha/strata/internal/domain"
)

// knowledgePurgeEligibility is deliberately conservative. It selects whole source-event
// provenance units and then removes candidates still needed by a retained correction,
// derivation, or evidence record. A stable assertion with no end remains protected however
// old its source is.
const knowledgePurgeEligibility = `
WITH candidate_events AS (
    SELECT se.id
    FROM source_events se
    WHERE se.workspace_id = $1 AND se.graph_space_id = $2
      AND se.recorded_at < $3
      AND se.status IN ('processed', 'failed', 'quarantined')
      AND NOT EXISTS (
          SELECT 1 FROM knowledge_holds h
          WHERE h.workspace_id = se.workspace_id
            AND h.graph_space_id = se.graph_space_id
            AND (h.source_event_id IS NULL OR h.source_event_id = se.id)
            AND (h.expires_at IS NULL OR h.expires_at > CURRENT_TIMESTAMP)
      )
      AND NOT EXISTS (
          SELECT 1 FROM assertions a
          WHERE a.workspace_id = se.workspace_id AND a.source_event_id = se.id
            AND NOT (
                a.status = 'quarantined'
                OR (a.status = 'superseded' AND a.superseded_at IS NOT NULL AND a.superseded_at < $3)
                OR (a.status = 'retracted' AND a.retracted_at IS NOT NULL AND a.retracted_at < $3)
                OR (a.deactivated_at IS NOT NULL AND a.deactivated_at < $3)
                OR (a.status IN ('active', 'disputed')
                    AND GREATEST(a.valid_to, a.effective_to, a.active_until, a.expires_at) IS NOT NULL
                    AND GREATEST(a.valid_to, a.effective_to, a.active_until, a.expires_at) < $3)
            )
      )
), eligible_events AS (
    SELECT candidate.id
    FROM candidate_events candidate
    WHERE NOT EXISTS (
        SELECT 1
        FROM assertions old_assertion
        JOIN assertions retained ON retained.supersedes_id = old_assertion.id
        WHERE old_assertion.source_event_id = candidate.id
          AND NOT EXISTS (
              SELECT 1 FROM candidate_events retained_event
              WHERE retained_event.id = retained.source_event_id
          )
    )
      AND NOT EXISTS (
        SELECT 1
        FROM assertions input_assertion
        JOIN derivation_inputs input ON input.assertion_id = input_assertion.id
        JOIN assertions retained ON retained.derivation_id = input.derivation_id
        WHERE input_assertion.source_event_id = candidate.id
          AND NOT EXISTS (
              SELECT 1 FROM candidate_events retained_event
              WHERE retained_event.id = retained.source_event_id
          )
    )
      AND NOT EXISTS (
        SELECT 1
        FROM evidence e
        JOIN assertions retained ON retained.id = e.assertion_id
        WHERE e.source_event_id = candidate.id
          AND NOT EXISTS (
              SELECT 1 FROM candidate_events retained_event
              WHERE retained_event.id = retained.source_event_id
          )
    )
)
`

const purgeCountsQuery = knowledgePurgeEligibility + `
SELECT
    (SELECT count(*) FROM eligible_events),
    (SELECT count(*) FROM assertions a WHERE a.source_event_id IN (SELECT id FROM eligible_events)),
    (SELECT count(*) FROM evidence e WHERE e.source_event_id IN (SELECT id FROM eligible_events)),
    (SELECT count(*) FROM episodes e WHERE e.source_event_id IN (SELECT id FROM eligible_events)),
    (SELECT count(*) FROM chunks c WHERE c.source_event_id IN (SELECT id FROM eligible_events)),
    (SELECT count(DISTINCT entity_id) FROM (
        SELECT a.subject_id AS entity_id FROM assertions a WHERE a.source_event_id IN (SELECT id FROM eligible_events)
        UNION
        SELECT a.object_entity_id FROM assertions a
          WHERE a.source_event_id IN (SELECT id FROM eligible_events) AND a.object_entity_id IS NOT NULL
    ) touched
	WHERE NOT EXISTS (
		SELECT 1 FROM assertions retained
		WHERE (retained.source_event_id IS NULL OR retained.source_event_id NOT IN (SELECT id FROM eligible_events))
		  AND (retained.subject_id = touched.entity_id OR retained.object_entity_id = touched.entity_id)
	) AND NOT EXISTS (
		SELECT 1 FROM resolution_decisions retained
		WHERE (retained.source_event_id IS NULL OR retained.source_event_id NOT IN (SELECT id FROM eligible_events))
		  AND (retained.chosen_entity_id = touched.entity_id OR retained.previous_entity_id = touched.entity_id)
	) AND NOT EXISTS (
		SELECT 1 FROM resolution_candidates candidate
		JOIN resolution_decisions retained ON retained.id = candidate.decision_id
		WHERE retained.source_event_id NOT IN (SELECT id FROM eligible_events)
		  AND candidate.entity_id = touched.entity_id
	) AND NOT EXISTS (
		SELECT 1 FROM entities retained WHERE retained.merged_into_id = touched.entity_id
	)),
    (SELECT count(*) FROM artifacts artifact
      WHERE artifact.id IN (SELECT raw_artifact_id FROM source_events WHERE id IN (SELECT id FROM eligible_events))
        AND NOT EXISTS (
          SELECT 1 FROM source_events retained
          WHERE retained.raw_artifact_id = artifact.id AND retained.id NOT IN (SELECT id FROM eligible_events)
        )),
    (SELECT count(*) FROM model_runs run WHERE run.source_event_id IN (SELECT id FROM eligible_events)),
    (SELECT count(*) FROM resolution_decisions decision WHERE decision.source_event_id IN (SELECT id FROM eligible_events))`

func queryPurgeCounts(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, scope domain.Scope, before time.Time) (domain.KnowledgePurgeCounts, error) {
	var counts domain.KnowledgePurgeCounts
	err := q.QueryRow(ctx, purgeCountsQuery, scope.WorkspaceID, scope.GraphSpaceID, before.UTC()).Scan(
		&counts.Events, &counts.Assertions, &counts.Evidence, &counts.Episodes, &counts.Chunks,
		&counts.Entities, &counts.Artifacts, &counts.ModelRuns, &counts.Resolutions)
	if err != nil {
		return counts, mapError(err, "ledger.queryPurgeCounts", "cannot plan knowledge purge")
	}
	counts.Blobs = counts.Artifacts
	return counts, nil
}

// PlanKnowledgePurge records a dry-run or executable plan and returns aggregate counts.
func (s *Store) PlanKnowledgePurge(ctx context.Context, req domain.KnowledgePurgeRequest) (domain.KnowledgePurgeRun, error) {
	const op = "ledger.PlanKnowledgePurge"
	if err := req.Validate(); err != nil {
		return domain.KnowledgePurgeRun{}, err
	}
	if domain.IsZero(req.ID) {
		req.ID = domain.NewKnowledgePurgeID()
	}
	var run domain.KnowledgePurgeRun
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		counts, err := queryPurgeCounts(ctx, tx, req.Scope, req.Before)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(counts)
		if err != nil {
			return domain.Wrap(err, domain.CodeInternal, op, "cannot encode purge counts")
		}
		status := domain.PurgePlanned
		var completedAt *time.Time
		if req.DryRun {
			status = domain.PurgeCompleted
			at := req.RequestedAt.UTC()
			completedAt = &at
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO knowledge_purge_runs
			    (id, workspace_id, graph_space_id, cutoff, dry_run, status, reason,
			     requested_by_id, requested_by_kind, eligible_counts, completed_at, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			RETURNING created_at`,
			req.ID, req.Scope.WorkspaceID, req.Scope.GraphSpaceID, req.Before.UTC(), req.DryRun,
			status, req.Reason, req.Principal.ID, req.Principal.Kind, encoded, completedAt,
			req.RequestedAt.UTC()).Scan(&run.CreatedAt); err != nil {
			return mapError(err, op, "cannot record purge plan")
		}
		run = domain.KnowledgePurgeRun{
			ID: req.ID, Scope: req.Scope, Before: req.Before.UTC(), DryRun: req.DryRun,
			Status: status, Reason: req.Reason, RequestedBy: req.Principal, Eligible: counts,
			CreatedAt: run.CreatedAt, CompletedAt: completedAt,
		}
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID: req.Scope.WorkspaceID, GraphSpaceID: req.Scope.GraphSpaceID,
			PrincipalID: req.Principal.ID, Action: "knowledge.purge.plan",
			TargetKind: "knowledge_purge", TargetID: string(req.ID), Outcome: string(status),
			Detail: map[string]any{"before": req.Before.UTC(), "dry_run": req.DryRun,
				"eligible_events": counts.Events, "reason": req.Reason},
		})
	})
	return run, err
}

// ExecuteKnowledgePurge deletes the selected canonical units in one transaction. Blob keys
// are returned for the service to remove only after their artifact rows are committed gone.
func (s *Store) ExecuteKnowledgePurge(ctx context.Context, ws domain.WorkspaceID, id domain.KnowledgePurgeID) (domain.KnowledgePurgeRun, []string, error) {
	const op = "ledger.ExecuteKnowledgePurge"
	var run domain.KnowledgePurgeRun
	var blobKeys []string
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var eligibleJSON, deletedJSON, blobJSON []byte
		var principalID, principalKind string
		if err := tx.QueryRow(ctx, `
			SELECT graph_space_id, cutoff, dry_run, status, reason, requested_by_id,
			       requested_by_kind, eligible_counts, deleted_counts, blob_keys, created_at
			FROM knowledge_purge_runs WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, ws, id).Scan(
			&run.Scope.GraphSpaceID, &run.Before, &run.DryRun, &run.Status, &run.Reason,
			&principalID, &principalKind, &eligibleJSON, &deletedJSON, &blobJSON, &run.CreatedAt); err != nil {
			if isNoRows(err) {
				return domain.Errorf(domain.CodeNotFound, op, "knowledge purge not found")
			}
			return mapError(err, op, "cannot load purge plan")
		}
		run.ID, run.Scope.WorkspaceID = id, ws
		run.RequestedBy = domain.PrincipalRef{ID: domain.PrincipalID(principalID), Kind: domain.PrincipalKind(principalKind)}
		if run.DryRun {
			return domain.Errorf(domain.CodeConflict, op, "a dry-run plan cannot be executed")
		}
		if run.Status == domain.PurgeCompleted {
			return nil
		}
		if err := json.Unmarshal(eligibleJSON, &run.Eligible); err != nil {
			return domain.Wrap(err, domain.CodeInternal, op, "cannot decode purge counts")
		}
		if run.Status == domain.PurgeCanonicalDeleted || run.Status == domain.PurgeFailed {
			if err := json.Unmarshal(deletedJSON, &run.Deleted); err != nil {
				return domain.Wrap(err, domain.CodeInternal, op, "cannot decode deleted counts")
			}
			if err := json.Unmarshal(blobJSON, &blobKeys); err != nil {
				return domain.Wrap(err, domain.CodeInternal, op, "cannot decode purge blobs")
			}
			return nil
		}

		// Serialize destructive plans per workspace. Normal ingestion may continue; its new
		// recorded_at cannot enter this old cutoff and a subsequent rebuild includes it.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 719))`, string(ws)); err != nil {
			return mapError(err, op, "cannot lock workspace purge")
		}
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE purge_event_ids ON COMMIT DROP AS `+
			knowledgePurgeEligibility+` SELECT id FROM eligible_events`, ws, run.Scope.GraphSpaceID, run.Before); err != nil {
			return mapError(err, op, "cannot materialize purge selection")
		}
		actual, err := queryPurgeCountsFromTemp(ctx, tx)
		if err != nil {
			return err
		}
		run.Eligible = actual

		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE purge_entity_ids ON COMMIT DROP AS
			SELECT subject_id AS id FROM assertions WHERE source_event_id IN (SELECT id FROM purge_event_ids)
			UNION SELECT object_entity_id FROM assertions
			WHERE source_event_id IN (SELECT id FROM purge_event_ids) AND object_entity_id IS NOT NULL`); err != nil {
			return mapError(err, op, "cannot retain purge entity set")
		}
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE purge_model_run_ids ON COMMIT DROP AS
			SELECT id FROM model_runs WHERE source_event_id IN (SELECT id FROM purge_event_ids)`); err != nil {
			return mapError(err, op, "cannot retain purge model-run set")
		}
		rows, err := tx.Query(ctx, `SELECT artifact.blob_key FROM artifacts artifact
			WHERE artifact.id IN (SELECT raw_artifact_id FROM source_events WHERE id IN (SELECT id FROM purge_event_ids))
			AND NOT EXISTS (SELECT 1 FROM source_events retained
			  WHERE retained.raw_artifact_id=artifact.id AND retained.id NOT IN (SELECT id FROM purge_event_ids))`)
		if err != nil {
			return mapError(err, op, "cannot list unshared purge blobs")
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return mapError(err, op, "cannot scan purge blob")
			}
			blobKeys = append(blobKeys, key)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return mapError(err, op, "cannot list purge blobs")
		}

		// Evidence and graph edges cascade with assertions; work rows, episodes, chunks,
		// vector and lexical records cascade with source events.
		if _, err := tx.Exec(ctx, `DELETE FROM resolution_decisions
			WHERE source_event_id IN (SELECT id FROM purge_event_ids)`); err != nil {
			return mapError(err, op, "cannot delete resolution provenance")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM assertions
			WHERE source_event_id IN (SELECT id FROM purge_event_ids)`); err != nil {
			return mapError(err, op, "cannot delete assertions")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM derivations d WHERE workspace_id=$1 AND graph_space_id=$2
			AND d.created_at < $3 AND NOT EXISTS (SELECT 1 FROM assertions a WHERE a.derivation_id=d.id)
			AND NOT EXISTS (SELECT 1 FROM derivation_inputs i WHERE i.derivation_id=d.id)`,
			ws, run.Scope.GraphSpaceID, run.Before); err != nil {
			return mapError(err, op, "cannot delete orphan derivations")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM conflict_sets c WHERE workspace_id=$1 AND graph_space_id=$2
			AND c.created_at < $3 AND NOT EXISTS (SELECT 1 FROM assertions a WHERE a.conflict_set_id=c.id)`,
			ws, run.Scope.GraphSpaceID, run.Before); err != nil {
			return mapError(err, op, "cannot delete orphan conflicts")
		}
		if tag, err := tx.Exec(ctx, `DELETE FROM source_events WHERE id IN (SELECT id FROM purge_event_ids)`); err != nil {
			return mapError(err, op, "cannot delete source events")
		} else {
			run.Deleted.Events = tag.RowsAffected()
		}
		if tag, err := tx.Exec(ctx, `DELETE FROM model_runs run
			WHERE run.id IN (SELECT id FROM purge_model_run_ids)
			AND NOT EXISTS (SELECT 1 FROM evidence e WHERE e.model_run_id=run.id)
			AND NOT EXISTS (SELECT 1 FROM derivations d WHERE d.model_run_id=run.id)`); err != nil {
			return mapError(err, op, "cannot delete model runs")
		} else {
			run.Deleted.ModelRuns = tag.RowsAffected()
		}
		if tag, err := tx.Exec(ctx, `DELETE FROM entities entity
			WHERE entity.id IN (SELECT id FROM purge_entity_ids)
			AND entity.workspace_id=$1 AND entity.graph_space_id=$2
			AND NOT EXISTS (SELECT 1 FROM assertions a
			  WHERE a.subject_id=entity.id OR a.object_entity_id=entity.id)
			AND NOT EXISTS (SELECT 1 FROM resolution_decisions decision
			  WHERE decision.chosen_entity_id=entity.id OR decision.previous_entity_id=entity.id)
			AND NOT EXISTS (SELECT 1 FROM resolution_candidates candidate WHERE candidate.entity_id=entity.id)
			AND NOT EXISTS (SELECT 1 FROM entities retained WHERE retained.merged_into_id=entity.id)`,
			ws, run.Scope.GraphSpaceID); err != nil {
			return mapError(err, op, "cannot delete orphan entities")
		} else {
			run.Deleted.Entities = tag.RowsAffected()
		}
		if tag, err := tx.Exec(ctx, `DELETE FROM artifacts artifact
			WHERE artifact.workspace_id=$1 AND artifact.blob_key=ANY($2::text[])
			AND NOT EXISTS (SELECT 1 FROM source_events se WHERE se.raw_artifact_id=artifact.id)`,
			ws, blobKeys); err != nil {
			return mapError(err, op, "cannot delete artifact metadata")
		} else {
			run.Deleted.Artifacts = tag.RowsAffected()
		}
		run.Deleted.Assertions, run.Deleted.Evidence = actual.Assertions, actual.Evidence
		run.Deleted.Episodes, run.Deleted.Chunks = actual.Episodes, actual.Chunks
		run.Deleted.Resolutions = actual.Resolutions
		deletedJSON, _ = json.Marshal(run.Deleted)
		eligibleJSON, _ = json.Marshal(run.Eligible)
		blobJSON, _ = json.Marshal(blobKeys)
		if _, err := tx.Exec(ctx, `UPDATE knowledge_purge_runs
			SET status=$3, eligible_counts=$4, deleted_counts=$5, blob_keys=$6, last_error=''
			WHERE workspace_id=$1 AND id=$2`, ws, id, domain.PurgeCanonicalDeleted,
			eligibleJSON, deletedJSON, blobJSON); err != nil {
			return mapError(err, op, "cannot record canonical purge")
		}
		run.Status = domain.PurgeCanonicalDeleted
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID: ws, GraphSpaceID: run.Scope.GraphSpaceID, PrincipalID: run.RequestedBy.ID,
			Action: "knowledge.purge.execute", TargetKind: "knowledge_purge", TargetID: string(id),
			Outcome: string(run.Status), Detail: map[string]any{"deleted_events": run.Deleted.Events},
		})
	})
	return run, blobKeys, err
}

func queryPurgeCountsFromTemp(ctx context.Context, tx pgx.Tx) (domain.KnowledgePurgeCounts, error) {
	var c domain.KnowledgePurgeCounts
	err := tx.QueryRow(ctx, `WITH touched_entities AS (
		SELECT subject_id AS id FROM assertions WHERE source_event_id IN (SELECT id FROM purge_event_ids)
		UNION SELECT object_entity_id FROM assertions
		WHERE source_event_id IN (SELECT id FROM purge_event_ids) AND object_entity_id IS NOT NULL
	) SELECT
		(SELECT count(*) FROM purge_event_ids),
		(SELECT count(*) FROM assertions WHERE source_event_id IN (SELECT id FROM purge_event_ids)),
		(SELECT count(*) FROM evidence WHERE source_event_id IN (SELECT id FROM purge_event_ids)),
		(SELECT count(*) FROM episodes WHERE source_event_id IN (SELECT id FROM purge_event_ids)),
		(SELECT count(*) FROM chunks WHERE source_event_id IN (SELECT id FROM purge_event_ids)),
		(SELECT count(*) FROM resolution_decisions WHERE source_event_id IN (SELECT id FROM purge_event_ids)),
		(SELECT count(*) FROM model_runs WHERE source_event_id IN (SELECT id FROM purge_event_ids)),
		(SELECT count(*) FROM touched_entities touched
		 WHERE NOT EXISTS (SELECT 1 FROM assertions retained
		 WHERE (retained.source_event_id IS NULL OR retained.source_event_id NOT IN (SELECT id FROM purge_event_ids))
		   AND (retained.subject_id=touched.id OR retained.object_entity_id=touched.id))
		 AND NOT EXISTS (SELECT 1 FROM resolution_decisions retained
		   WHERE (retained.source_event_id IS NULL OR retained.source_event_id NOT IN (SELECT id FROM purge_event_ids))
		   AND (retained.chosen_entity_id=touched.id OR retained.previous_entity_id=touched.id))
		 AND NOT EXISTS (SELECT 1 FROM resolution_candidates candidate
		   JOIN resolution_decisions retained ON retained.id=candidate.decision_id
		   WHERE retained.source_event_id NOT IN (SELECT id FROM purge_event_ids)
		   AND candidate.entity_id=touched.id)
		 AND NOT EXISTS (SELECT 1 FROM entities retained WHERE retained.merged_into_id=touched.id)),
		(SELECT count(*) FROM artifacts artifact
		 WHERE artifact.id IN (SELECT raw_artifact_id FROM source_events WHERE id IN (SELECT id FROM purge_event_ids))
		 AND NOT EXISTS (SELECT 1 FROM source_events retained
		   WHERE retained.raw_artifact_id=artifact.id AND retained.id NOT IN (SELECT id FROM purge_event_ids)))`).Scan(
		&c.Events, &c.Assertions, &c.Evidence, &c.Episodes, &c.Chunks, &c.Resolutions,
		&c.ModelRuns, &c.Entities, &c.Artifacts)
	if err != nil {
		return c, mapError(err, "ledger.queryPurgeCountsFromTemp", "cannot count purge selection")
	}
	c.Blobs = c.Artifacts
	return c, nil
}

// FinishKnowledgePurge records external blob/projection cleanup success or failure.
func (s *Store) FinishKnowledgePurge(ctx context.Context, ws domain.WorkspaceID, id domain.KnowledgePurgeID, status domain.KnowledgePurgeStatus, message string, blobsDeleted int64, at time.Time) error {
	const op = "ledger.FinishKnowledgePurge"
	if status != domain.PurgeCompleted && status != domain.PurgeFailed {
		return domain.Errorf(domain.CodeInvalidArgument, op, "finish status must be completed or failed")
	}
	return s.InTx(ctx, func(tx pgx.Tx) error {
		var graphSpace domain.GraphSpaceID
		var actor domain.PrincipalID
		err := tx.QueryRow(ctx, `UPDATE knowledge_purge_runs SET status=$3, last_error=$4,
			completed_at=$5, deleted_counts=jsonb_set(deleted_counts,'{blobs}',to_jsonb($6::bigint),true)
			WHERE workspace_id=$1 AND id=$2 RETURNING graph_space_id, requested_by_id`,
			ws, id, status, message, at.UTC(), blobsDeleted).
			Scan(&graphSpace, &actor)
		if err != nil {
			if isNoRows(err) {
				return domain.Errorf(domain.CodeNotFound, op, "knowledge purge not found")
			}
			return mapError(err, op, "cannot finish purge run")
		}
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID: ws, GraphSpaceID: graphSpace, PrincipalID: actor,
			Action: "knowledge.purge.finish", TargetKind: "knowledge_purge", TargetID: string(id),
			Outcome: string(status), Detail: map[string]any{"cleanup_error": message != ""},
		})
	})
}

func (s *Store) CreateKnowledgeHold(ctx context.Context, hold domain.KnowledgeHold) (domain.KnowledgeHold, error) {
	const op = "ledger.CreateKnowledgeHold"
	if err := hold.Validate(); err != nil {
		return hold, err
	}
	if domain.IsZero(hold.ID) {
		hold.ID = domain.NewKnowledgeHoldID()
	}
	if !domain.IsZero(hold.SourceEventID) {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM source_events
			WHERE workspace_id=$1 AND graph_space_id=$2 AND id=$3)`, hold.Scope.WorkspaceID,
			hold.Scope.GraphSpaceID, hold.SourceEventID).Scan(&exists); err != nil {
			return hold, mapError(err, op, "cannot validate held source event")
		}
		if !exists {
			return hold, domain.Errorf(domain.CodeNotFound, op, "source event not found")
		}
	}
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO knowledge_holds
			(id,workspace_id,graph_space_id,source_event_id,reason,created_by_id,created_by_kind,expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING created_at`, hold.ID,
			hold.Scope.WorkspaceID, hold.Scope.GraphSpaceID, nullableString(hold.SourceEventID), hold.Reason,
			hold.CreatedBy.ID, hold.CreatedBy.Kind, hold.ExpiresAt).Scan(&hold.CreatedAt); err != nil {
			return mapError(err, op, "cannot create knowledge hold")
		}
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID: hold.Scope.WorkspaceID, GraphSpaceID: hold.Scope.GraphSpaceID,
			PrincipalID: hold.CreatedBy.ID, Action: "knowledge.hold.create",
			TargetKind: "knowledge_hold", TargetID: string(hold.ID), Outcome: "created",
			Detail: map[string]any{"source_event_id": hold.SourceEventID, "expires_at": hold.ExpiresAt},
		})
	})
	return hold, err
}

func (s *Store) ReleaseKnowledgeHold(ctx context.Context, scope domain.Scope, id domain.KnowledgeHoldID, actor domain.PrincipalRef) error {
	const op = "ledger.ReleaseKnowledgeHold"
	return s.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM knowledge_holds
			WHERE workspace_id=$1 AND graph_space_id=$2 AND id=$3`, scope.WorkspaceID, scope.GraphSpaceID, id)
		if err != nil {
			return mapError(err, op, "cannot release knowledge hold")
		}
		if tag.RowsAffected() == 0 {
			return domain.Errorf(domain.CodeNotFound, op, "knowledge hold not found")
		}
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID: scope.WorkspaceID, GraphSpaceID: scope.GraphSpaceID,
			PrincipalID: actor.ID, Action: "knowledge.hold.release", TargetKind: "knowledge_hold",
			TargetID: string(id), Outcome: "released",
		})
	})
}
