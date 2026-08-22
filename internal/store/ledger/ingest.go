package ledger

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gimantha/strata/internal/domain"
)

// AppendSourceEvent durably records a source event and everything that must happen
// atomically with it.
//
// One transaction covers the artifact row, the source event, its pipeline run, its
// outbox work items, and the audit record. There is no window in which an accepted
// event exists without its queued work, and none in which work is queued for an
// event that was never committed (AGENTS.md sections 10.2, 28.1).
//
// Replay is idempotent: the same idempotency key returns the original event instead
// of creating a second one. The same key with different content is a conflict, not a
// silent overwrite, because one of the two payloads would otherwise be lost.
func (s *Store) AppendSourceEvent(ctx context.Context, req domain.SourceEventAppend) (domain.SourceEventAppendResult, error) {
	const op = "ledger.AppendSourceEvent"

	if err := req.Artifact.Validate(); err != nil {
		return domain.SourceEventAppendResult{}, err
	}
	if err := req.Event.Validate(); err != nil {
		return domain.SourceEventAppendResult{}, err
	}
	for _, o := range req.Outbox {
		if err := o.Validate(); err != nil {
			return domain.SourceEventAppendResult{}, err
		}
	}

	var result domain.SourceEventAppendResult
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		artifact, err := s.upsertArtifact(ctx, tx, req.Artifact)
		if err != nil {
			return err
		}
		result.Artifact = artifact

		event := req.Event
		event.RawArtifactID = artifact.ID
		if domain.IsZero(event.ID) {
			event.ID = domain.NewSourceEventID()
		}

		stored, duplicate, err := s.insertSourceEvent(ctx, tx, event)
		if err != nil {
			return err
		}
		result.Event = stored
		result.Duplicate = duplicate

		// Queue work and seed the run even on a duplicate: an event whose first
		// attempt crashed before the worker picked it up must still be processable,
		// and both inserts are idempotent.
		if req.PipelineVersion > 0 {
			if err := s.ensurePipelineRunTx(ctx, tx, stored, req.PipelineVersion); err != nil {
				return err
			}
		}
		for _, o := range req.Outbox {
			o.SourceEventID = stored.ID
			if err := s.insertOutboxTx(ctx, tx, o); err != nil {
				return err
			}
		}

		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID:  stored.WorkspaceID,
			GraphSpaceID: stored.GraphSpaceID,
			PrincipalID:  req.Actor,
			Action:       "ingest.accept",
			TargetKind:   "source_event",
			TargetID:     string(stored.ID),
			Outcome:      "allowed",
			Detail: map[string]any{
				"duplicate":    duplicate,
				"operation":    string(stored.Operation),
				"source_id":    string(stored.SourceID),
				"size_bytes":   artifact.SizeBytes,
				"content_hash": stored.ContentHash,
			},
		})
	})
	if err != nil {
		return domain.SourceEventAppendResult{}, err
	}
	return result, nil
}

// upsertArtifact stores artifact metadata, reusing an existing row when identical
// bytes were already archived in this workspace.
func (s *Store) upsertArtifact(ctx context.Context, tx pgx.Tx, a domain.Artifact) (domain.Artifact, error) {
	const op = "ledger.upsertArtifact"

	if domain.IsZero(a.ID) {
		a.ID = domain.NewArtifactID()
	}
	// The no-op SET exists so the statement returns the existing row's identifier on
	// conflict; DO NOTHING would return no rows at all.
	err := tx.QueryRow(ctx, `
		INSERT INTO artifacts (id, workspace_id, content_hash, media_type, size_bytes, blob_key, storage, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (workspace_id, content_hash)
		DO UPDATE SET updated_at = now()
		RETURNING id, created_at`,
		a.ID, a.WorkspaceID, a.ContentHash, a.MediaType, a.SizeBytes, a.BlobKey, a.Storage, jsonMap(a.Metadata),
	).Scan(&a.ID, &a.CreatedAt)
	if err != nil {
		return domain.Artifact{}, mapError(err, op, "cannot upsert artifact")
	}
	return a, nil
}

const sourceEventColumns = `id, workspace_id, graph_space_id, collection_id, source_id, external_id,
	event_type, operation, content_hash, idempotency_key, event_time, source_time, source_commit_time,
	source_sequence, source_version, observed_at, recorded_at, raw_artifact_id, media_type, status,
	classification, created_by_id, created_by_kind, created_by_name, metadata`

// insertSourceEvent inserts an event, returning the stored row and whether this was
// a replay of an existing one.
//
// The insert runs inside a savepoint. A unique-index violation aborts a PostgreSQL
// transaction, and this function has to keep querying afterwards to tell a harmless
// replay from a genuine conflict; the savepoint is what makes that possible without
// discarding the artifact row already written by the caller's transaction.
func (s *Store) insertSourceEvent(ctx context.Context, tx pgx.Tx, e domain.SourceEvent) (domain.SourceEvent, bool, error) {
	const op = "ledger.insertSourceEvent"

	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return domain.SourceEvent{}, false, mapError(err, op, "cannot open savepoint")
	}

	insertErr := savepoint.QueryRow(ctx, `
		INSERT INTO source_events (`+sourceEventColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
		ON CONFLICT (workspace_id, source_id, idempotency_key) DO NOTHING
		RETURNING id`,
		e.ID, e.WorkspaceID, e.GraphSpaceID, nullableString(e.CollectionID), e.SourceID, e.ExternalID,
		e.EventType, e.Operation, e.ContentHash, e.IdempotencyKey, e.EventTime, e.SourceTime,
		e.SourceCommitTime, e.SourceSequence, e.SourceVersion, e.ObservedAt, e.RecordedAt,
		e.RawArtifactID, e.MediaType, e.Status, e.Classification,
		string(e.CreatedBy.ID), string(e.CreatedBy.Kind), e.CreatedBy.DisplayName, jsonMap(e.Metadata),
	).Scan(&e.ID)

	switch {
	case insertErr == nil:
		if err := savepoint.Commit(ctx); err != nil {
			return domain.SourceEvent{}, false, mapError(err, op, "cannot release savepoint")
		}
		return e, false, nil

	case isNoRows(insertErr):
		// ON CONFLICT DO NOTHING suppressed the insert, so the transaction is still
		// healthy: the idempotency key already exists and this is a replay.
		if err := savepoint.Commit(ctx); err != nil {
			return domain.SourceEvent{}, false, mapError(err, op, "cannot release savepoint")
		}
		existing, lookupErr := s.sourceEventByIdempotencyKey(ctx, tx, e.WorkspaceID, e.SourceID, e.IdempotencyKey)
		if lookupErr != nil {
			return domain.SourceEvent{}, false, lookupErr
		}
		if existing.ContentHash != e.ContentHash {
			return domain.SourceEvent{}, false, domain.Errorf(domain.CodeSourceEventConflict, op,
				"idempotency key already used for different content (stored %s, submitted %s)",
				existing.ContentHash, e.ContentHash)
		}
		return existing, true, nil

	case isUniqueViolation(insertErr, "source_events_external_version_key"):
		// The upstream (external_id, source_version) pair was already ingested under a
		// different idempotency key. Roll back to the savepoint to clear the aborted
		// state, then decide whether this is a replay or a source that reused a version
		// number for changed data.
		if err := savepoint.Rollback(ctx); err != nil {
			return domain.SourceEvent{}, false, mapError(err, op, "cannot roll back to savepoint")
		}
		existing, lookupErr := s.sourceEventByExternalVersion(ctx, tx, e.WorkspaceID, e.SourceID, e.ExternalID, e.SourceVersion)
		if lookupErr != nil {
			return domain.SourceEvent{}, false, lookupErr
		}
		if existing.ContentHash != e.ContentHash {
			return domain.SourceEvent{}, false, domain.Errorf(domain.CodeSourceEventConflict, op,
				"source version %q for external id %q was already ingested with different content",
				e.SourceVersion, e.ExternalID)
		}
		return existing, true, nil

	default:
		_ = savepoint.Rollback(ctx)
		return domain.SourceEvent{}, false, mapError(insertErr, op, "cannot insert source event")
	}
}

type rowScanner interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) sourceEventByIdempotencyKey(ctx context.Context, q rowScanner, ws domain.WorkspaceID, src domain.SourceID, key string) (domain.SourceEvent, error) {
	const op = "ledger.sourceEventByIdempotencyKey"

	row := q.QueryRow(ctx, `SELECT `+sourceEventColumns+` FROM source_events
		WHERE workspace_id = $1 AND source_id = $2 AND idempotency_key = $3`, ws, src, key)
	return scanSourceEvent(row, op)
}

func (s *Store) sourceEventByExternalVersion(ctx context.Context, q rowScanner, ws domain.WorkspaceID, src domain.SourceID, externalID, version string) (domain.SourceEvent, error) {
	const op = "ledger.sourceEventByExternalVersion"

	row := q.QueryRow(ctx, `SELECT `+sourceEventColumns+` FROM source_events
		WHERE workspace_id = $1 AND source_id = $2 AND external_id = $3 AND source_version = $4`,
		ws, src, externalID, version)
	return scanSourceEvent(row, op)
}

// GetSourceEvent loads an event within a workspace. The workspace argument is not
// optional: a valid identifier from another tenant must not resolve.
func (s *Store) GetSourceEvent(ctx context.Context, ws domain.WorkspaceID, id domain.SourceEventID) (domain.SourceEvent, error) {
	const op = "ledger.GetSourceEvent"

	row := s.pool.QueryRow(ctx, `SELECT `+sourceEventColumns+` FROM source_events
		WHERE workspace_id = $1 AND id = $2`, ws, id)
	event, err := scanSourceEvent(row, op)
	if err != nil {
		return domain.SourceEvent{}, err
	}
	return event, nil
}

// GetSourceEventByIdempotencyKey resolves a replay without needing the assigned id.
func (s *Store) GetSourceEventByIdempotencyKey(ctx context.Context, ws domain.WorkspaceID, src domain.SourceID, key string) (domain.SourceEvent, error) {
	return s.sourceEventByIdempotencyKey(ctx, s.pool, ws, src, key)
}

func scanSourceEvent(row pgx.Row, op string) (domain.SourceEvent, error) {
	var (
		e            domain.SourceEvent
		collectionID *string
	)
	err := row.Scan(&e.ID, &e.WorkspaceID, &e.GraphSpaceID, &collectionID, &e.SourceID, &e.ExternalID,
		&e.EventType, &e.Operation, &e.ContentHash, &e.IdempotencyKey, &e.EventTime, &e.SourceTime,
		&e.SourceCommitTime, &e.SourceSequence, &e.SourceVersion, &e.ObservedAt, &e.RecordedAt,
		&e.RawArtifactID, &e.MediaType, &e.Status, &e.Classification,
		&e.CreatedBy.ID, &e.CreatedBy.Kind, &e.CreatedBy.DisplayName, &e.Metadata)
	if err != nil {
		if isNoRows(err) {
			return domain.SourceEvent{}, domain.Errorf(domain.CodeNotFound, op, "source event not found")
		}
		return domain.SourceEvent{}, mapError(err, op, "cannot scan source event")
	}
	if collectionID != nil {
		e.CollectionID = domain.CollectionID(*collectionID)
	}
	return e, nil
}

// ResolveSourceEventWorkspace finds which of a principal's workspaces owns an event.
//
// It lets an endpoint accept a bare event identifier without the caller naming a
// workspace, while keeping the tenancy check intact: an event in a workspace the
// principal has no grant for is reported as absent, so identifiers cannot be probed
// across tenants (AGENTS.md section 22.1).
func (s *Store) ResolveSourceEventWorkspace(ctx context.Context, id domain.SourceEventID, allowed []domain.WorkspaceID) (domain.WorkspaceID, error) {
	const op = "ledger.ResolveSourceEventWorkspace"

	if len(allowed) == 0 {
		return "", domain.Errorf(domain.CodeNotFound, op, "source event not found")
	}
	raw := make([]string, len(allowed))
	for i, ws := range allowed {
		raw[i] = string(ws)
	}

	var found domain.WorkspaceID
	err := s.pool.QueryRow(ctx,
		`SELECT workspace_id FROM source_events WHERE id = $1 AND workspace_id = ANY($2::uuid[])`,
		id, raw).Scan(&found)
	if err != nil {
		if isNoRows(err) {
			return "", domain.Errorf(domain.CodeNotFound, op, "source event not found")
		}
		return "", mapError(err, op, "cannot resolve source event workspace")
	}
	return found, nil
}

// SetSourceEventStatus records processing progress. Status is the only mutable field
// on a source event; the event's content and clocks are immutable
// (AGENTS.md section 2.1).
func (s *Store) SetSourceEventStatus(ctx context.Context, ws domain.WorkspaceID, id domain.SourceEventID, status domain.SourceEventStatus) error {
	const op = "ledger.SetSourceEventStatus"

	if _, err := domain.ParseSourceEventStatus(string(status)); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE source_events SET status = $3, updated_at = now()
		WHERE workspace_id = $1 AND id = $2`, ws, id, status)
	if err != nil {
		return mapError(err, op, "cannot update source event status")
	}
	if tag.RowsAffected() == 0 {
		return domain.Errorf(domain.CodeNotFound, op, "source event not found")
	}
	return nil
}

// GetArtifact loads artifact metadata within a workspace.
func (s *Store) GetArtifact(ctx context.Context, ws domain.WorkspaceID, id domain.ArtifactID) (domain.Artifact, error) {
	const op = "ledger.GetArtifact"

	var a domain.Artifact
	err := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, content_hash, media_type, size_bytes, blob_key, storage, metadata, created_at
		FROM artifacts WHERE workspace_id = $1 AND id = $2`, ws, id,
	).Scan(&a.ID, &a.WorkspaceID, &a.ContentHash, &a.MediaType, &a.SizeBytes,
		&a.BlobKey, &a.Storage, &a.Metadata, &a.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.Artifact{}, domain.Errorf(domain.CodeNotFound, op, "artifact not found")
		}
		return domain.Artifact{}, mapError(err, op, "cannot load artifact")
	}
	return a, nil
}

// EventStatus is the processing view returned by the status endpoint. It exposes
// where an event is in the pipeline, never whether its content is true.
type EventStatus struct {
	Event    domain.SourceEvent
	Run      *domain.PipelineRun
	Stages   []domain.StageRun
	Episodes int
	Chunks   int
	Work     []WorkItemStatus
}

// WorkItemStatus summarizes one outbox row for operators.
type WorkItemStatus struct {
	ID         domain.OutboxEventID
	EventType  string
	Status     domain.OutboxStatus
	Attempts   int
	VisibleAt  time.Time
	LastError  string
	ErrorClass domain.ErrorClass
}

// SourceEventStatus assembles the processing status of one event.
func (s *Store) SourceEventStatus(ctx context.Context, ws domain.WorkspaceID, id domain.SourceEventID) (EventStatus, error) {
	const op = "ledger.SourceEventStatus"

	event, err := s.GetSourceEvent(ctx, ws, id)
	if err != nil {
		return EventStatus{}, err
	}
	status := EventStatus{Event: event}

	run, ok, err := s.LatestPipelineRun(ctx, ws, id)
	if err != nil {
		return EventStatus{}, err
	}
	if ok {
		status.Run = &run
		stages, err := s.ListStageRuns(ctx, run.ID)
		if err != nil {
			return EventStatus{}, err
		}
		status.Stages = stages
	}

	if err := s.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM episodes WHERE workspace_id = $1 AND source_event_id = $2),
		        (SELECT count(*) FROM chunks   WHERE workspace_id = $1 AND source_event_id = $2)`,
		ws, id).Scan(&status.Episodes, &status.Chunks); err != nil {
		return EventStatus{}, mapError(err, op, "cannot count derived units")
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, event_type, status, attempts, visible_at, last_error, error_class
		FROM outbox_events WHERE workspace_id = $1 AND source_event_id = $2 ORDER BY created_at`, ws, id)
	if err != nil {
		return EventStatus{}, mapError(err, op, "cannot list work items")
	}
	defer rows.Close()
	for rows.Next() {
		var w WorkItemStatus
		if err := rows.Scan(&w.ID, &w.EventType, &w.Status, &w.Attempts,
			&w.VisibleAt, &w.LastError, &w.ErrorClass); err != nil {
			return EventStatus{}, mapError(err, op, "cannot scan work item")
		}
		status.Work = append(status.Work, w)
	}
	return status, mapError(rows.Err(), op, "cannot list work items")
}
