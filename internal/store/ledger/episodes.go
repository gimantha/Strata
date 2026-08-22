package ledger

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/gimantha/strata/internal/domain"
)

// InsertEpisodes stores segmentation output idempotently.
//
// Episodes are keyed by (workspace, source event, sequence). Because a source event
// is immutable, re-running segmentation at the same stage version produces the same
// episodes, so a replay collides and is skipped rather than duplicating knowledge
// (AGENTS.md section 2.12). The stored rows are read back so callers always see
// canonical identifiers, including on a replay.
func (s *Store) InsertEpisodes(ctx context.Context, episodes []domain.Episode) ([]domain.Episode, error) {
	const op = "ledger.InsertEpisodes"

	if len(episodes) == 0 {
		return nil, nil
	}
	ws := episodes[0].WorkspaceID
	eventID := episodes[0].SourceEventID

	batch := &pgx.Batch{}
	for _, e := range episodes {
		if err := e.Validate(); err != nil {
			return nil, err
		}
		if e.WorkspaceID != ws || e.SourceEventID != eventID {
			return nil, domain.Errorf(domain.CodeInvalidArgument, op,
				"all episodes in a batch must belong to one workspace and source event")
		}
		if domain.IsZero(e.ID) {
			e.ID = domain.NewEpisodeID()
		}
		locator, err := jsonValue(e.Locator)
		if err != nil {
			return nil, domain.Wrap(err, domain.CodeInternal, op, "cannot encode locator")
		}
		batch.Queue(`
			INSERT INTO episodes (id, workspace_id, graph_space_id, source_event_id, artifact_id,
			                      sequence, content, content_type, event_time, observed_at, recorded_at,
			                      locator, classification, metadata)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			ON CONFLICT (workspace_id, source_event_id, sequence) DO NOTHING`,
			e.ID, e.WorkspaceID, e.GraphSpaceID, e.SourceEventID, nullableString(e.ArtifactID),
			e.Sequence, e.Content, e.ContentType, e.EventTime, e.ObservedAt, e.RecordedAt,
			locator, e.Classification, jsonMap(e.Metadata))
	}

	if err := s.InTx(ctx, func(tx pgx.Tx) error {
		results := tx.SendBatch(ctx, batch)
		defer results.Close()
		for range episodes {
			if _, err := results.Exec(); err != nil {
				return mapError(err, op, "cannot insert episode")
			}
		}
		return results.Close()
	}); err != nil {
		return nil, err
	}

	return s.ListEpisodes(ctx, ws, eventID)
}

const episodeColumns = `id, workspace_id, graph_space_id, source_event_id, artifact_id, sequence,
	content, content_type, event_time, observed_at, recorded_at, locator, classification, metadata`

// ListEpisodes returns an event's episodes in source order.
func (s *Store) ListEpisodes(ctx context.Context, ws domain.WorkspaceID, eventID domain.SourceEventID) ([]domain.Episode, error) {
	const op = "ledger.ListEpisodes"

	rows, err := s.pool.Query(ctx, `SELECT `+episodeColumns+` FROM episodes
		WHERE workspace_id = $1 AND source_event_id = $2 ORDER BY sequence`, ws, eventID)
	if err != nil {
		return nil, mapError(err, op, "cannot list episodes")
	}
	defer rows.Close()

	var out []domain.Episode
	for rows.Next() {
		var (
			e          domain.Episode
			artifactID *string
		)
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.GraphSpaceID, &e.SourceEventID, &artifactID,
			&e.Sequence, &e.Content, &e.ContentType, &e.EventTime, &e.ObservedAt, &e.RecordedAt,
			&e.Locator, &e.Classification, &e.Metadata); err != nil {
			return nil, mapError(err, op, "cannot scan episode")
		}
		if artifactID != nil {
			e.ArtifactID = domain.ArtifactID(*artifactID)
		}
		out = append(out, e)
	}
	return out, mapError(rows.Err(), op, "cannot list episodes")
}

// InsertChunks stores chunking output idempotently, keyed by (workspace, episode,
// sequence) for the same reason episodes are keyed by sequence.
func (s *Store) InsertChunks(ctx context.Context, chunks []domain.Chunk) ([]domain.Chunk, error) {
	const op = "ledger.InsertChunks"

	if len(chunks) == 0 {
		return nil, nil
	}
	ws := chunks[0].WorkspaceID
	eventID := chunks[0].SourceEventID

	batch := &pgx.Batch{}
	for _, c := range chunks {
		if err := c.Validate(); err != nil {
			return nil, err
		}
		if c.WorkspaceID != ws || c.SourceEventID != eventID {
			return nil, domain.Errorf(domain.CodeInvalidArgument, op,
				"all chunks in a batch must belong to one workspace and source event")
		}
		if domain.IsZero(c.ID) {
			c.ID = domain.NewChunkID()
		}
		locator, err := jsonValue(c.Locator)
		if err != nil {
			return nil, domain.Wrap(err, domain.CodeInternal, op, "cannot encode locator")
		}
		batch.Queue(`
			INSERT INTO chunks (id, workspace_id, graph_space_id, source_event_id, episode_id, artifact_id,
			                    sequence, content, content_type, token_count, char_start, char_end,
			                    byte_start, byte_end, locator, classification, metadata)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
			ON CONFLICT (workspace_id, episode_id, sequence) DO NOTHING`,
			c.ID, c.WorkspaceID, c.GraphSpaceID, c.SourceEventID, c.EpisodeID, nullableString(c.ArtifactID),
			c.Sequence, c.Content, c.ContentType, c.TokenCount, c.CharStart, c.CharEnd,
			c.ByteStart, c.ByteEnd, locator, c.Classification, jsonMap(c.Metadata))
	}

	if err := s.InTx(ctx, func(tx pgx.Tx) error {
		results := tx.SendBatch(ctx, batch)
		defer results.Close()
		for range chunks {
			if _, err := results.Exec(); err != nil {
				return mapError(err, op, "cannot insert chunk")
			}
		}
		return results.Close()
	}); err != nil {
		return nil, err
	}

	return s.ListChunks(ctx, ws, eventID)
}

const chunkColumns = `id, workspace_id, graph_space_id, source_event_id, episode_id, artifact_id,
	sequence, content, content_type, token_count, char_start, char_end, byte_start, byte_end,
	locator, classification, metadata`

// ListChunks returns an event's chunks ordered by episode then sequence.
func (s *Store) ListChunks(ctx context.Context, ws domain.WorkspaceID, eventID domain.SourceEventID) ([]domain.Chunk, error) {
	const op = "ledger.ListChunks"

	rows, err := s.pool.Query(ctx, `SELECT `+chunkColumns+` FROM chunks
		WHERE workspace_id = $1 AND source_event_id = $2 ORDER BY episode_id, sequence`, ws, eventID)
	if err != nil {
		return nil, mapError(err, op, "cannot list chunks")
	}
	defer rows.Close()

	var out []domain.Chunk
	for rows.Next() {
		var (
			c          domain.Chunk
			artifactID *string
		)
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.GraphSpaceID, &c.SourceEventID, &c.EpisodeID,
			&artifactID, &c.Sequence, &c.Content, &c.ContentType, &c.TokenCount,
			&c.CharStart, &c.CharEnd, &c.ByteStart, &c.ByteEnd,
			&c.Locator, &c.Classification, &c.Metadata); err != nil {
			return nil, mapError(err, op, "cannot scan chunk")
		}
		if artifactID != nil {
			c.ArtifactID = domain.ArtifactID(*artifactID)
		}
		out = append(out, c)
	}
	return out, mapError(rows.Err(), op, "cannot list chunks")
}

// DeleteDerivedUnits removes the episodes and chunks derived from an event, for
// projection-style replay tests and forced reprocessing. It never touches the source
// event or its archived artifact, which remain the authoritative record.
func (s *Store) DeleteDerivedUnits(ctx context.Context, ws domain.WorkspaceID, eventID domain.SourceEventID) error {
	const op = "ledger.DeleteDerivedUnits"

	return s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM chunks WHERE workspace_id = $1 AND source_event_id = $2`, ws, eventID); err != nil {
			return mapError(err, op, "cannot delete chunks")
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM episodes WHERE workspace_id = $1 AND source_event_id = $2`, ws, eventID); err != nil {
			return mapError(err, op, "cannot delete episodes")
		}
		return nil
	})
}
