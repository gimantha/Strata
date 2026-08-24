package ledger

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

// UpsertCDCStream registers or updates a stream's mapping.
//
// The checkpoint is deliberately not touched here: re-registering a mapping must not rewind
// a running connector, and changing how rows are interpreted is a different decision from
// deciding where to read from.
func (s *Store) UpsertCDCStream(ctx context.Context, stream domain.CDCStream, actor domain.PrincipalID) (domain.CDCStream, error) {
	const op = "ledger.UpsertCDCStream"

	if err := stream.Mapping.Validate(); err != nil {
		return domain.CDCStream{}, err
	}
	if domain.IsZero(stream.ID) {
		stream.ID = domain.NewCDCStreamID()
	}

	mapping, err := json.Marshal(stream.Mapping)
	if err != nil {
		return domain.CDCStream{}, domain.Wrap(err, domain.CodeInternal, op, "cannot encode mapping")
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO cdc_streams (id, workspace_id, source_id, stream, mapping)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (workspace_id, source_id, stream)
		DO UPDATE SET mapping = EXCLUDED.mapping, updated_at = now()
		RETURNING `+cdcStreamColumns,
		stream.ID, stream.WorkspaceID, stream.SourceID, stream.Stream, mapping)

	stored, err := scanCDCStream(row)
	if err != nil {
		return domain.CDCStream{}, mapError(err, op, "cannot register the stream")
	}

	// The mapping's columns become registry entries. Without this the predicates would be
	// discovered as candidates on first use, with the conservative non-functional
	// semantics discovery has to assume — and every column update would then accumulate a
	// second value beside the first instead of superseding it.
	for _, definition := range stored.Mapping.PredicateDefinitions(stored.WorkspaceID) {
		if _, err := s.DefinePredicate(ctx, definition, actor); err != nil {
			return domain.CDCStream{}, err
		}
	}
	return stored, nil
}

const cdcStreamColumns = `id, workspace_id, source_id, stream, mapping, last_offset,
	last_sequence, last_commit_time, events_consumed, created_at, updated_at`

// GetCDCStream reads one stream by name.
func (s *Store) GetCDCStream(ctx context.Context, ws domain.WorkspaceID, source domain.SourceID, stream string) (domain.CDCStream, error) {
	const op = "ledger.GetCDCStream"

	row := s.pool.QueryRow(ctx, `SELECT `+cdcStreamColumns+` FROM cdc_streams
		WHERE workspace_id = $1 AND source_id = $2 AND stream = $3`, ws, source, stream)

	stored, err := scanCDCStream(row)
	if err != nil {
		if isNoRows(err) {
			return domain.CDCStream{}, domain.Errorf(domain.CodeNotFound, op,
				"no stream %q is registered for this source", stream)
		}
		return domain.CDCStream{}, mapError(err, op, "cannot load the stream")
	}
	return stored, nil
}

// ListCDCStreams returns the streams registered in a workspace.
func (s *Store) ListCDCStreams(ctx context.Context, ws domain.WorkspaceID) ([]domain.CDCStream, error) {
	const op = "ledger.ListCDCStreams"

	rows, err := s.pool.Query(ctx, `SELECT `+cdcStreamColumns+` FROM cdc_streams
		WHERE workspace_id = $1 ORDER BY stream`, ws)
	if err != nil {
		return nil, mapError(err, op, "cannot list streams")
	}
	defer rows.Close()

	var out []domain.CDCStream
	for rows.Next() {
		stream, err := scanCDCStream(rows)
		if err != nil {
			return nil, mapError(err, op, "cannot scan stream")
		}
		out = append(out, stream)
	}
	return out, mapError(rows.Err(), op, "cannot list streams")
}

// SaveCheckpoint advances a stream's position.
//
// Monotonic by construction: the update refuses to move backwards, because a connector that
// restarts mid-batch and reports an older offset must not undo progress another worker
// already made. Replaying an event is free; skipping one is not.
func (s *Store) SaveStreamCheckpoint(ctx context.Context, checkpoint domain.StreamCheckpoint) error {
	const op = "ledger.SaveStreamCheckpoint"

	tag, err := s.pool.Exec(ctx, `
		UPDATE cdc_streams
		SET last_offset = $4,
		    last_sequence = $5,
		    last_commit_time = $6,
		    events_consumed = events_consumed + $7,
		    updated_at = now()
		WHERE workspace_id = $1 AND source_id = $2 AND stream = $3
		  AND ($4 = '' OR last_offset = '' OR $4 > last_offset)`,
		checkpoint.WorkspaceID, checkpoint.SourceID, checkpoint.Stream,
		checkpoint.LastOffset, checkpoint.LastSequence, checkpoint.LastCommitTime,
		checkpoint.EventsConsumed)
	if err != nil {
		return mapError(err, op, "cannot save the checkpoint")
	}
	if tag.RowsAffected() == 0 {
		// Either the stream is unregistered or the offset moved backwards. Both are worth
		// distinguishing from success, and neither is worth failing a batch over.
		if _, lookupErr := s.GetCDCStream(ctx, checkpoint.WorkspaceID, checkpoint.SourceID, checkpoint.Stream); lookupErr != nil {
			return lookupErr
		}
	}
	return nil
}

// RetractSourceRecord withdraws every current claim that came from one upstream record.
//
// This is a tombstone, not an erasure (AGENTS.md section 11.3). The claims keep their
// evidence and stay queryable as of any earlier knowledge time; what changes is that the
// system stops believing them now, because the source stopped saying them.
func (s *Store) RetractSourceRecord(ctx context.Context, ws domain.WorkspaceID, source domain.SourceID, externalID string, at time.Time, reason string, actor domain.PrincipalID) ([]domain.AssertionID, error) {
	const op = "ledger.RetractSourceRecord"

	if externalID == "" {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op,
			"an external record id is required")
	}

	rows, err := s.pool.Query(ctx, `
		UPDATE assertions a
		SET status = 'retracted', retracted_at = $4, retraction_reason = $5
		FROM source_events se
		WHERE se.id = a.source_event_id
		  AND a.workspace_id = $1
		  AND se.source_id = $2
		  AND se.external_id = $3
		  AND a.status IN ('active', 'disputed')
		RETURNING a.id`,
		ws, source, externalID, at.UTC(), reason)
	if err != nil {
		return nil, mapError(err, op, "cannot retract the record's claims")
	}
	defer rows.Close()

	var retracted []domain.AssertionID
	for rows.Next() {
		var id domain.AssertionID
		if err := rows.Scan(&id); err != nil {
			return nil, mapError(err, op, "cannot scan retracted claim")
		}
		retracted = append(retracted, id)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, op, "cannot retract the record's claims")
	}

	if len(retracted) > 0 {
		_ = s.AppendAudit(ctx, AuditEntry{
			WorkspaceID: ws,
			PrincipalID: actor,
			Action:      "cdc.tombstone",
			TargetKind:  "source_record",
			TargetID:    externalID,
			Outcome:     "retracted",
			Detail: map[string]any{
				"source_id": string(source),
				"retracted": len(retracted),
				"reason":    reason,
			},
		})
	}
	return retracted, nil
}

func scanCDCStream(row scanner) (domain.CDCStream, error) {
	var (
		stream  domain.CDCStream
		mapping []byte
	)
	if err := row.Scan(&stream.ID, &stream.WorkspaceID, &stream.SourceID, &stream.Stream,
		&mapping, &stream.Checkpoint.LastOffset, &stream.Checkpoint.LastSequence,
		&stream.Checkpoint.LastCommitTime, &stream.Checkpoint.EventsConsumed,
		&stream.CreatedAt, &stream.UpdatedAt); err != nil {
		return domain.CDCStream{}, err
	}
	if err := json.Unmarshal(mapping, &stream.Mapping); err != nil {
		return domain.CDCStream{}, err
	}

	stream.Checkpoint.WorkspaceID = stream.WorkspaceID
	stream.Checkpoint.SourceID = stream.SourceID
	stream.Checkpoint.Stream = stream.Stream
	stream.Checkpoint.UpdatedAt = stream.UpdatedAt
	return stream, nil
}

// CurrentClaimsForRecord returns the believed claims derived from one upstream record.
//
// A CDC stream re-sends every column of a row on every update, and most of them did not
// change. Without this the same unchanged value would be asserted again under a new source
// event id — a different fingerprint, a second active claim, and unbounded growth for a table
// that is written hourly. Comparing against what is already believed is what
// AGENTS.md section 11.2 means by determining which assertions are "unchanged".
func (s *Store) CurrentClaimsForRecord(ctx context.Context, ws domain.WorkspaceID, source domain.SourceID, externalID string) ([]domain.Assertion, error) {
	const op = "ledger.CurrentClaimsForRecord"

	if externalID == "" {
		return nil, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+prefixColumns("a", assertionColumns)+`
		FROM assertions a
		JOIN source_events se ON se.id = a.source_event_id
		WHERE a.workspace_id = $1
		  AND se.source_id = $2
		  AND se.external_id = $3
		  AND a.status IN ('active', 'disputed')`,
		ws, source, externalID)
	if err != nil {
		return nil, mapError(err, op, "cannot load the record's current claims")
	}
	defer rows.Close()

	var out []domain.Assertion
	for rows.Next() {
		claim, err := scanAssertion(rowAdapter{rows}, op)
		if err != nil {
			return nil, err
		}
		out = append(out, claim)
	}
	return out, mapError(rows.Err(), op, "cannot load the record's current claims")
}

// WithRecordLock runs fn holding an advisory lock on one upstream record.
//
// Changes to a row are inherently sequential: what the second change should do depends on
// what the first one produced. Workers claim outbox items independently, so without this two
// changes to the same row are processed at once, both read "nothing is believed yet", and
// both assert the same unchanged columns — the duplicate that this whole comparison exists to
// prevent.
//
// The lock is per record, not per stream: rows are independent, and serializing a whole table
// would make a busy stream single-threaded for no reason. It is advisory and session-scoped,
// so a crashed worker releases it when its connection drops.
func (s *Store) WithRecordLock(ctx context.Context, ws domain.WorkspaceID, source domain.SourceID, externalID string, fn func(context.Context) error) error {
	const op = "ledger.WithRecordLock"

	if externalID == "" {
		return fn(ctx)
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return mapError(err, op, "cannot acquire a connection for the record lock")
	}
	defer conn.Release()

	key := recordLockKey(ws, source, externalID)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		return mapError(err, op, "cannot lock the record")
	}
	defer func() {
		// Best effort: a failed unlock is released when the connection closes, and
		// failing the stage over it would turn a slow path into a lost one.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, key)
	}()

	return fn(ctx)
}

// recordLockKey hashes a record identity into the advisory lock space.
//
// A collision costs two unrelated records some serialization, never correctness, which is why
// a 64-bit hash is enough and a lock table is not needed.
func recordLockKey(ws domain.WorkspaceID, source domain.SourceID, externalID string) int64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(ws))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(source))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(externalID))
	return int64(hash.Sum64())
}
