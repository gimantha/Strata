package ledger

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/observability"
)

const outboxColumns = `id, workspace_id, graph_space_id, source_event_id, topic, event_type,
	schema_version, payload, dedupe_key, status, attempts, max_attempts, visible_at, claimed_by,
	claim_expires_at, last_error, error_class, trace_parent, created_at, updated_at, completed_at`

// insertOutboxTx writes a work item inside the caller's transaction. This is the only
// way work enters the system, which is what makes "commit then publish" impossible.
func (s *Store) insertOutboxTx(ctx context.Context, tx pgx.Tx, o domain.OutboxEvent) error {
	const op = "ledger.insertOutbox"

	if domain.IsZero(o.ID) {
		o.ID = domain.NewOutboxEventID()
	}
	if o.Status == "" {
		o.Status = domain.OutboxPending
	}
	if len(o.Payload) == 0 {
		o.Payload = json.RawMessage("{}")
	}
	if o.TraceParent == "" {
		// Capture trace context at publish time so the worker's spans join this trace.
		o.TraceParent = observability.TraceParentFromContext(ctx)
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO outbox_events (id, workspace_id, graph_space_id, source_event_id, topic, event_type,
		                           schema_version, payload, dedupe_key, status, max_attempts,
		                           visible_at, trace_parent)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,coalesce($12, now()),$13)
		ON CONFLICT (workspace_id, dedupe_key) DO NOTHING`,
		o.ID, o.WorkspaceID, nullableString(o.GraphSpaceID), nullableString(o.SourceEventID),
		o.Topic, o.EventType, o.SchemaVersion, []byte(o.Payload), o.DedupeKey,
		o.Status, o.MaxAttempts, visibleAtOrNil(o.VisibleAt), o.TraceParent)
	return mapError(err, op, "cannot insert outbox event")
}

// PublishOutbox writes work items in their own transaction, for producers that have
// no wider canonical mutation to attach to (a forced replay, for example).
func (s *Store) PublishOutbox(ctx context.Context, events ...domain.OutboxEvent) error {
	if len(events) == 0 {
		return nil
	}
	for _, e := range events {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	return s.InTx(ctx, func(tx pgx.Tx) error {
		for _, e := range events {
			if err := s.insertOutboxTx(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})
}

// ClaimOutbox leases up to limit pending work items for one worker.
//
// FOR UPDATE SKIP LOCKED is what lets many workers share one queue without either
// locking each other out or handing the same item to two of them. The lease is
// expressed in database time, not the worker's clock, so clock skew between workers
// cannot expire a healthy claim (AGENTS.md section 28).
func (s *Store) ClaimOutbox(ctx context.Context, topics []string, workerID string, lease time.Duration, limit int) ([]domain.OutboxEvent, error) {
	const op = "ledger.ClaimOutbox"

	// Normalize a nil filter to an empty array: nil encodes as SQL NULL, and
	// cardinality(NULL) is NULL, which would silently match nothing.
	if topics == nil {
		topics = []string{}
	}

	rows, err := s.pool.Query(ctx, `
		UPDATE outbox_events
		SET status = 'claimed',
		    claimed_by = $1,
		    claim_expires_at = now() + make_interval(secs => $2),
		    attempts = outbox_events.attempts + 1,
		    updated_at = now()
		WHERE id IN (
			SELECT id FROM outbox_events
			WHERE status = 'pending'
			  AND visible_at <= now()
			  AND ($3::text[] IS NULL OR cardinality($3::text[]) = 0 OR topic = ANY($3::text[]))
			ORDER BY visible_at, created_at
			FOR UPDATE SKIP LOCKED
			LIMIT $4
		)
		RETURNING `+outboxColumns,
		workerID, lease.Seconds(), topics, limit)
	if err != nil {
		return nil, mapError(err, op, "cannot claim outbox events")
	}
	defer rows.Close()

	var out []domain.OutboxEvent
	for rows.Next() {
		o, err := scanOutbox(rows, op)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, mapError(rows.Err(), op, "cannot claim outbox events")
}

// RenewClaim extends a lease for work still in progress. A worker that stops
// renewing (because it died) has its item reclaimed by ReapExpiredClaims.
func (s *Store) RenewClaim(ctx context.Context, id domain.OutboxEventID, workerID string, lease time.Duration) (bool, error) {
	const op = "ledger.RenewClaim"

	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox_events
		SET claim_expires_at = now() + make_interval(secs => $3), updated_at = now()
		WHERE id = $1 AND claimed_by = $2 AND status = 'claimed'`, id, workerID, lease.Seconds())
	if err != nil {
		return false, mapError(err, op, "cannot renew claim")
	}
	return tag.RowsAffected() == 1, nil
}

// CompleteOutbox marks work as done. Completed rows are retained rather than deleted:
// they are the audit trail of what was processed and when.
func (s *Store) CompleteOutbox(ctx context.Context, id domain.OutboxEventID) error {
	const op = "ledger.CompleteOutbox"

	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox_events
		SET status = 'succeeded', completed_at = now(), claimed_by = '', claim_expires_at = NULL,
		    last_error = '', error_class = '', updated_at = now()
		WHERE id = $1`, id)
	if err != nil {
		return mapError(err, op, "cannot complete outbox event")
	}
	if tag.RowsAffected() == 0 {
		return domain.Errorf(domain.CodeNotFound, op, "outbox event not found")
	}
	return nil
}

// RetryOutbox returns an item to pending, invisible until retryAfter has elapsed.
func (s *Store) RetryOutbox(ctx context.Context, id domain.OutboxEventID, retryAfter time.Duration, cause error) error {
	const op = "ledger.RetryOutbox"

	lastError, class := describeError(cause)
	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox_events
		SET status = 'pending',
		    visible_at = now() + make_interval(secs => $2),
		    claimed_by = '', claim_expires_at = NULL,
		    last_error = $3, error_class = $4, updated_at = now()
		WHERE id = $1`, id, retryAfter.Seconds(), lastError, class)
	if err != nil {
		return mapError(err, op, "cannot reschedule outbox event")
	}
	if tag.RowsAffected() == 0 {
		return domain.Errorf(domain.CodeNotFound, op, "outbox event not found")
	}
	return nil
}

// DeadLetterOutbox parks an item for human review. Persistent failures stop consuming
// worker capacity but are never discarded (AGENTS.md section 28.4).
func (s *Store) DeadLetterOutbox(ctx context.Context, id domain.OutboxEventID, cause error) error {
	const op = "ledger.DeadLetterOutbox"

	lastError, class := describeError(cause)
	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox_events
		SET status = 'dead', completed_at = now(), claimed_by = '', claim_expires_at = NULL,
		    last_error = $2, error_class = $3, updated_at = now()
		WHERE id = $1`, id, lastError, class)
	if err != nil {
		return mapError(err, op, "cannot dead-letter outbox event")
	}
	if tag.RowsAffected() == 0 {
		return domain.Errorf(domain.CodeNotFound, op, "outbox event not found")
	}
	return nil
}

// ReviveDeadLetters returns dead items to pending with a fresh attempt budget, for an
// operator who has fixed the underlying cause.
func (s *Store) ReviveDeadLetters(ctx context.Context, ws domain.WorkspaceID, ids []domain.OutboxEventID) (int, error) {
	const op = "ledger.ReviveDeadLetters"

	var (
		tag pgx.Rows
		err error
	)
	if len(ids) == 0 {
		tag, err = s.pool.Query(ctx, `
			UPDATE outbox_events
			SET status = 'pending', attempts = 0, visible_at = now(), last_error = '', error_class = '',
			    completed_at = NULL, updated_at = now()
			WHERE workspace_id = $1 AND status = 'dead' RETURNING id`, ws)
	} else {
		raw := make([]string, len(ids))
		for i, id := range ids {
			raw[i] = string(id)
		}
		tag, err = s.pool.Query(ctx, `
			UPDATE outbox_events
			SET status = 'pending', attempts = 0, visible_at = now(), last_error = '', error_class = '',
			    completed_at = NULL, updated_at = now()
			WHERE workspace_id = $1 AND status = 'dead' AND id = ANY($2::uuid[]) RETURNING id`, ws, raw)
	}
	if err != nil {
		return 0, mapError(err, op, "cannot revive dead letters")
	}
	defer tag.Close()

	revived := 0
	for tag.Next() {
		revived++
	}
	return revived, mapError(tag.Err(), op, "cannot revive dead letters")
}

// ReapExpiredClaims returns leases from dead workers to the pending pool.
//
// This is the mechanism that makes accepted work impossible to lose: a worker that is
// killed mid-item never acknowledges it, its lease expires, and the item is retried.
func (s *Store) ReapExpiredClaims(ctx context.Context) (int, error) {
	const op = "ledger.ReapExpiredClaims"

	rows, err := s.pool.Query(ctx, `
		UPDATE outbox_events
		SET status = 'pending', claimed_by = '', claim_expires_at = NULL, updated_at = now(),
		    last_error = 'claim expired before completion', error_class = 'transient'
		WHERE status = 'claimed' AND claim_expires_at IS NOT NULL AND claim_expires_at < now()
		RETURNING id`)
	if err != nil {
		return 0, mapError(err, op, "cannot reap expired claims")
	}
	defer rows.Close()

	reaped := 0
	for rows.Next() {
		reaped++
	}
	return reaped, mapError(rows.Err(), op, "cannot reap expired claims")
}

// GetOutbox loads one work item.
func (s *Store) GetOutbox(ctx context.Context, id domain.OutboxEventID) (domain.OutboxEvent, error) {
	const op = "ledger.GetOutbox"

	rows, err := s.pool.Query(ctx, `SELECT `+outboxColumns+` FROM outbox_events WHERE id = $1`, id)
	if err != nil {
		return domain.OutboxEvent{}, mapError(err, op, "cannot load outbox event")
	}
	defer rows.Close()

	if !rows.Next() {
		return domain.OutboxEvent{}, domain.Errorf(domain.CodeNotFound, op, "outbox event not found")
	}
	return scanOutbox(rows, op)
}

// ListOutbox returns work items filtered by status, newest first.
func (s *Store) ListOutbox(ctx context.Context, ws domain.WorkspaceID, status domain.OutboxStatus, limit int) ([]domain.OutboxEvent, error) {
	const op = "ledger.ListOutbox"

	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT `+outboxColumns+` FROM outbox_events
		WHERE workspace_id = $1 AND ($2 = '' OR status = $2)
		ORDER BY created_at DESC LIMIT $3`, ws, string(status), limit)
	if err != nil {
		return nil, mapError(err, op, "cannot list outbox events")
	}
	defer rows.Close()

	var out []domain.OutboxEvent
	for rows.Next() {
		o, err := scanOutbox(rows, op)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, mapError(rows.Err(), op, "cannot list outbox events")
}

// OutboxDepth reports queue depth by status and the age of the oldest claimable item.
// Queue lag is the earliest signal that processing is falling behind
// (AGENTS.md section 30.2).
func (s *Store) OutboxDepth(ctx context.Context) (observability.OutboxDepthSnapshot, error) {
	const op = "ledger.OutboxDepth"

	snap := observability.OutboxDepthSnapshot{ByStatus: map[string]int64{}}

	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM outbox_events GROUP BY status`)
	if err != nil {
		return snap, mapError(err, op, "cannot read outbox depth")
	}
	defer rows.Close()
	for rows.Next() {
		var (
			status string
			count  int64
		)
		if err := rows.Scan(&status, &count); err != nil {
			return snap, mapError(err, op, "cannot scan outbox depth")
		}
		snap.ByStatus[status] = count
	}
	if err := rows.Err(); err != nil {
		return snap, mapError(err, op, "cannot read outbox depth")
	}

	if err := s.pool.QueryRow(ctx, `
		SELECT coalesce(extract(epoch FROM (now() - min(created_at))), 0)
		FROM outbox_events WHERE status = 'pending' AND visible_at <= now()`,
	).Scan(&snap.OldestPendingAge); err != nil {
		return snap, mapError(err, op, "cannot read outbox lag")
	}
	return snap, nil
}

// visibleAtOrNil leaves visibility to the database when the caller did not schedule the
// work for later.
//
// Claim eligibility is evaluated against the database clock, so publishing with the
// application clock would make freshly queued work briefly unclaimable whenever the two
// disagree - and permanently late if the application clock runs ahead. Every timestamp
// governing the claim lifecycle comes from one clock: the database's.
func visibleAtOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}

func scanOutbox(rows pgx.Rows, op string) (domain.OutboxEvent, error) {
	var (
		o             domain.OutboxEvent
		graphSpaceID  *string
		sourceEventID *string
		payload       []byte
	)
	err := rows.Scan(&o.ID, &o.WorkspaceID, &graphSpaceID, &sourceEventID, &o.Topic, &o.EventType,
		&o.SchemaVersion, &payload, &o.DedupeKey, &o.Status, &o.Attempts, &o.MaxAttempts,
		&o.VisibleAt, &o.ClaimedBy, &o.ClaimExpiresAt, &o.LastError, &o.ErrorClass,
		&o.TraceParent, &o.CreatedAt, &o.UpdatedAt, &o.CompletedAt)
	if err != nil {
		return domain.OutboxEvent{}, mapError(err, op, "cannot scan outbox event")
	}
	if graphSpaceID != nil {
		o.GraphSpaceID = domain.GraphSpaceID(*graphSpaceID)
	}
	if sourceEventID != nil {
		o.SourceEventID = domain.SourceEventID(*sourceEventID)
	}
	o.Payload = json.RawMessage(payload)
	return o, nil
}
