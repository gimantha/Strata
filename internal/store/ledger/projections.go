package ledger

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gimantha/strata/internal/domain"
)

// UpsertVectors writes embeddings to the vector projection.
//
// Upsert rather than insert, because a projection is rebuilt by re-running it: replaying an
// event that was already projected must converge on the same state rather than duplicate it
// (AGENTS.md section 15.2).
func (s *Store) UpsertVectors(ctx context.Context, records []domain.VectorRecord) error {
	const op = "ledger.UpsertVectors"

	if len(records) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, record := range records {
		batch.Queue(`
			INSERT INTO vector_records (id, workspace_id, graph_space_id, surface, record_id,
			                            embedding_model, embedding_version, embedding,
			                            valid_from, valid_to, status, classification, memory_kind,
			                            source_event_id, content_hash, entity_type)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
			ON CONFLICT (workspace_id, surface, record_id, embedding_model, embedding_version)
			DO UPDATE SET embedding = EXCLUDED.embedding,
			              valid_from = EXCLUDED.valid_from,
			              valid_to = EXCLUDED.valid_to,
			              status = EXCLUDED.status,
			              classification = EXCLUDED.classification,
			              memory_kind = EXCLUDED.memory_kind,
			              content_hash = EXCLUDED.content_hash,
			              entity_type = EXCLUDED.entity_type`,
			domain.NewUUIDString(), record.Scope.WorkspaceID, record.Scope.GraphSpaceID,
			record.Surface, record.RecordID, record.Model, record.Version,
			formatVector(record.Embedding),
			record.ValidFrom, record.ValidTo, record.Status, record.Classification,
			record.MemoryKind, nullableString(record.SourceEventID), record.ContentHash,
			record.EntityType)
	}

	return s.InTx(ctx, func(tx pgx.Tx) error {
		results := tx.SendBatch(ctx, batch)
		defer results.Close()
		for range records {
			if _, err := results.Exec(); err != nil {
				return mapError(err, op, "cannot upsert vector record")
			}
		}
		return results.Close()
	})
}

// SearchVectors finds the nearest neighbours, filtered before ranking.
//
// Filtering first is what keeps tenancy safe and results relevant: ranking the whole table
// and discarding afterwards would return fewer results than asked for, and would let another
// tenant's vectors influence which of ours came back.
func (s *Store) SearchVectors(ctx context.Context, q domain.VectorQuery) ([]domain.Hit, error) {
	const op = "ledger.SearchVectors"

	if len(q.Embedding) == 0 {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op, "a query embedding is required")
	}
	if domain.IsZero(q.Scope.WorkspaceID) {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op, "workspace scope is required")
	}
	limit := q.Limit
	if limit <= 0 || limit > domain.MaxAssertionLimit {
		limit = 20
	}

	where := []string{"v.workspace_id = $1"}
	args := []any{q.Scope.WorkspaceID}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	if !domain.IsZero(q.Scope.GraphSpaceID) {
		add("v.graph_space_id = $%d", q.Scope.GraphSpaceID)
	}
	if len(q.Surfaces) > 0 {
		add("v.surface = ANY($%d::text[])", surfaceStrings(q.Surfaces))
	}
	if q.Model != "" {
		// Vectors from different models are not comparable, so mixing them in one search
		// would rank noise against signal.
		add("v.embedding_model = $%d", q.Model)
		add("v.embedding_version = $%d", q.Version)
	}
	if q.ValidAt != nil {
		at := q.ValidAt.UTC()
		add("(v.valid_from IS NULL OR v.valid_from <= $%d)", at)
		add("(v.valid_to IS NULL OR v.valid_to > $%d)", at)
	}
	if len(q.Statuses) > 0 {
		add("v.status = ANY($%d::text[])", q.Statuses)
	}
	if len(q.Classification) > 0 {
		add("v.classification = ANY($%d::text[])", enumStrings(q.Classification))
	}
	if len(q.MemoryKinds) > 0 {
		add("v.memory_kind = ANY($%d::text[])", enumStrings(q.MemoryKinds))
	}
	if len(q.EntityTypes) > 0 {
		add("v.entity_type = ANY($%d::text[])", q.EntityTypes)
	}

	args = append(args, formatVector(q.Embedding))
	probe := len(args)
	args = append(args, limit)

	sql := `
		SELECT v.surface, v.record_id, 1 - (v.embedding <=> $` + strconv.Itoa(probe) + `::vector) AS score
		FROM vector_records v
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY v.embedding <=> $` + strconv.Itoa(probe) + `::vector
		LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError(err, op, "cannot search vectors")
	}
	defer rows.Close()

	var out []domain.Hit
	for rows.Next() {
		var hit domain.Hit
		if err := rows.Scan(&hit.Surface, &hit.RecordID, &hit.Score); err != nil {
			return nil, mapError(err, op, "cannot scan vector hit")
		}
		// Nearest-neighbour search always returns its k nearest, however far away. Without
		// a floor, an unrelated question still gets confident-looking answers.
		if hit.Score < q.MinScore {
			continue
		}
		hit.Detail = map[string]any{"retriever": "vector", "cosine_similarity": hit.Score}
		out = append(out, hit)
	}
	return out, mapError(rows.Err(), op, "cannot search vectors")
}

// UpsertLexical writes text to the lexical projection.
func (s *Store) UpsertLexical(ctx context.Context, records []domain.ProjectedRecord) error {
	const op = "ledger.UpsertLexical"

	if len(records) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, record := range records {
		batch.Queue(`
			INSERT INTO lexical_records (id, workspace_id, graph_space_id, surface, record_id,
			                             content, valid_from, valid_to, status, classification,
			                             memory_kind, source_event_id, entity_type)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			ON CONFLICT (workspace_id, surface, record_id)
			DO UPDATE SET content = EXCLUDED.content,
			              valid_from = EXCLUDED.valid_from,
			              valid_to = EXCLUDED.valid_to,
			              status = EXCLUDED.status,
			              classification = EXCLUDED.classification,
			              memory_kind = EXCLUDED.memory_kind,
			              entity_type = EXCLUDED.entity_type`,
			domain.NewUUIDString(), record.Scope.WorkspaceID, record.Scope.GraphSpaceID,
			record.Surface, record.RecordID, record.Content,
			record.ValidFrom, record.ValidTo, record.Status, record.Classification,
			record.MemoryKind, nullableString(record.SourceEventID), record.EntityType)
	}

	return s.InTx(ctx, func(tx pgx.Tx) error {
		results := tx.SendBatch(ctx, batch)
		defer results.Close()
		for range records {
			if _, err := results.Exec(); err != nil {
				return mapError(err, op, "cannot upsert lexical record")
			}
		}
		return results.Close()
	})
}

// SearchLexical runs full-text or substring search.
//
// Both modes exist because they fail in opposite places. Stemmed full text handles prose and
// mangles an error code; substring matching finds the error code and cannot rank a sentence.
func (s *Store) SearchLexical(ctx context.Context, q domain.LexicalQuery) ([]domain.Hit, error) {
	const op = "ledger.SearchLexical"

	if strings.TrimSpace(q.Text) == "" {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op, "query text is required")
	}
	if domain.IsZero(q.Scope.WorkspaceID) {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op, "workspace scope is required")
	}
	limit := q.Limit
	if limit <= 0 || limit > domain.MaxAssertionLimit {
		limit = 20
	}

	where := []string{"l.workspace_id = $1"}
	args := []any{q.Scope.WorkspaceID}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	if !domain.IsZero(q.Scope.GraphSpaceID) {
		add("l.graph_space_id = $%d", q.Scope.GraphSpaceID)
	}
	if len(q.Surfaces) > 0 {
		add("l.surface = ANY($%d::text[])", surfaceStrings(q.Surfaces))
	}
	if q.ValidAt != nil {
		at := q.ValidAt.UTC()
		add("(l.valid_from IS NULL OR l.valid_from <= $%d)", at)
		add("(l.valid_to IS NULL OR l.valid_to > $%d)", at)
	}
	if len(q.Statuses) > 0 {
		add("l.status = ANY($%d::text[])", q.Statuses)
	}
	if len(q.Classification) > 0 {
		add("l.classification = ANY($%d::text[])", enumStrings(q.Classification))
	}
	if len(q.MemoryKinds) > 0 {
		add("l.memory_kind = ANY($%d::text[])", enumStrings(q.MemoryKinds))
	}
	if len(q.EntityTypes) > 0 {
		add("l.entity_type = ANY($%d::text[])", q.EntityTypes)
	}

	args = append(args, q.Text)
	term := len(args)
	args = append(args, limit)

	var sql string
	if q.Exact {
		where = append(where, fmt.Sprintf("l.content ILIKE '%%' || $%d || '%%'", term))
		sql = `SELECT l.surface, l.record_id, l.content,
		              similarity(l.content, $` + strconv.Itoa(term) + `) AS score
		       FROM lexical_records l
		       WHERE ` + strings.Join(where, " AND ") + `
		       ORDER BY score DESC, length(l.content)
		       LIMIT $` + strconv.Itoa(len(args))
	} else {
		where = append(where, fmt.Sprintf("l.search_vector @@ websearch_to_tsquery('english', $%d)", term))
		// ts_rank_cd weighs term proximity, which ranks a passage discussing the terms
		// together above one that merely mentions them.
		sql = `SELECT l.surface, l.record_id, l.content,
		              ts_rank_cd(l.search_vector, websearch_to_tsquery('english', $` + strconv.Itoa(term) + `)) AS score
		       FROM lexical_records l
		       WHERE ` + strings.Join(where, " AND ") + `
		       ORDER BY score DESC
		       LIMIT $` + strconv.Itoa(len(args))
	}

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError(err, op, "cannot search lexically")
	}
	defer rows.Close()

	mode := "lexical"
	if q.Exact {
		mode = "lexical_exact"
	}

	var out []domain.Hit
	for rows.Next() {
		var hit domain.Hit
		if err := rows.Scan(&hit.Surface, &hit.RecordID, &hit.Content, &hit.Score); err != nil {
			return nil, mapError(err, op, "cannot scan lexical hit")
		}
		hit.Detail = map[string]any{"retriever": mode, "rank": hit.Score}
		out = append(out, hit)
	}
	return out, mapError(rows.Err(), op, "cannot search lexically")
}

// UpsertGraphEdges writes entity-to-entity edges to the graph projection.
func (s *Store) UpsertGraphEdges(ctx context.Context, edges []domain.GraphEdge) error {
	const op = "ledger.UpsertGraphEdges"

	if len(edges) == 0 {
		return nil
	}

	batch := &pgx.Batch{}
	for _, edge := range edges {
		batch.Queue(`
			INSERT INTO graph_edges (id, workspace_id, graph_space_id, subject_id, predicate,
			                         object_entity_id, assertion_id, valid_from, valid_to,
			                         status, confidence, classification)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (assertion_id)
			DO UPDATE SET status = EXCLUDED.status,
			              valid_from = EXCLUDED.valid_from,
			              valid_to = EXCLUDED.valid_to,
			              confidence = EXCLUDED.confidence,
			              classification = EXCLUDED.classification`,
			domain.NewUUIDString(), edge.WorkspaceID, edge.GraphSpaceID, edge.SubjectID,
			edge.Predicate, edge.ObjectEntityID, edge.AssertionID, edge.ValidFrom, edge.ValidTo,
			edge.Status, edge.Confidence, edge.Classification)
	}

	return s.InTx(ctx, func(tx pgx.Tx) error {
		results := tx.SendBatch(ctx, batch)
		defer results.Close()
		for range edges {
			if _, err := results.Exec(); err != nil {
				return mapError(err, op, "cannot upsert graph edge")
			}
		}
		return results.Close()
	})
}

// ExpandGraph walks outwards from a set of entities, bounded by depth and result count.
//
// Both bounds are mandatory. A well-connected graph reaches everything within a few hops, so
// an unbounded traversal is not a slow answer but a useless one (AGENTS.md sections 16, 39).
func (s *Store) ExpandGraph(ctx context.Context, q domain.GraphExpandQuery) ([]domain.GraphHit, error) {
	const op = "ledger.ExpandGraph"

	q = q.Normalize()
	if domain.IsZero(q.Scope.WorkspaceID) {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op, "workspace scope is required")
	}
	if len(q.Roots) == 0 {
		return nil, nil
	}

	statuses := []string{string(domain.AssertionActive), string(domain.AssertionDisputed)}
	if q.IncludeSuperseded {
		statuses = append(statuses, string(domain.AssertionSuperseded))
	}

	var validAt any
	if q.ValidAt != nil {
		validAt = q.ValidAt.UTC()
	}
	var predicates []string
	if len(q.Predicates) > 0 {
		predicates = q.Predicates
	}

	// The walk is breadth-first with a visited set, so a cycle terminates rather than
	// looping, and each entity is reported at the shallowest depth it was reached.
	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE walk AS (
			SELECT e.id AS entity_id,
			       0 AS depth,
			       NULL::uuid AS via_assertion,
			       ''::text AS via_predicate,
			       NULL::uuid AS from_entity,
			       ARRAY[e.id] AS path
			FROM entities e
			WHERE e.workspace_id = $1 AND e.id = ANY($2::uuid[])

			UNION ALL

			SELECT next.entity_id, w.depth + 1, next.assertion_id, next.predicate, w.entity_id,
			       w.path || next.entity_id
			FROM walk w
			JOIN LATERAL (
				SELECT ge.object_entity_id AS entity_id, ge.assertion_id, ge.predicate
				FROM graph_edges ge
				WHERE ge.workspace_id = $1
				  AND ($3::uuid IS NULL OR ge.graph_space_id = $3)
				  AND ge.subject_id = w.entity_id
				  AND ge.status = ANY($4::text[])
				  AND ($5::text[] IS NULL OR ge.predicate = ANY($5::text[]))
				  AND ($6::timestamptz IS NULL OR
				       ((ge.valid_from IS NULL OR ge.valid_from <= $6) AND
				        (ge.valid_to IS NULL OR ge.valid_to > $6)))
				UNION ALL
				-- Traversal follows edges in both directions: "who supplies Acme" and "what
				-- does Acme supply" are the same graph seen from opposite ends.
				SELECT ge.subject_id AS entity_id, ge.assertion_id, ge.predicate
				FROM graph_edges ge
				WHERE ge.workspace_id = $1
				  AND ($3::uuid IS NULL OR ge.graph_space_id = $3)
				  AND ge.object_entity_id = w.entity_id
				  AND ge.status = ANY($4::text[])
				  AND ($5::text[] IS NULL OR ge.predicate = ANY($5::text[]))
				  AND ($6::timestamptz IS NULL OR
				       ((ge.valid_from IS NULL OR ge.valid_from <= $6) AND
				        (ge.valid_to IS NULL OR ge.valid_to > $6)))
			) next ON true
			WHERE w.depth < $7
			  AND NOT next.entity_id = ANY(w.path)
		)
		SELECT DISTINCT ON (walk.entity_id)
		       walk.entity_id, e.canonical_name, walk.depth,
		       coalesce(walk.via_assertion::text, ''), walk.via_predicate,
		       coalesce(walk.from_entity::text, '')
		FROM walk
		JOIN entities e ON e.id = walk.entity_id
		ORDER BY walk.entity_id, walk.depth
		LIMIT $8`,
		q.Scope.WorkspaceID, idStrings(q.Roots), nullableString(q.Scope.GraphSpaceID),
		statuses, predicates, validAt, q.Depth, q.Limit)
	if err != nil {
		return nil, mapError(err, op, "cannot expand graph")
	}
	defer rows.Close()

	var out []domain.GraphHit
	for rows.Next() {
		var (
			hit          domain.GraphHit
			viaAssertion string
			fromEntity   string
		)
		if err := rows.Scan(&hit.EntityID, &hit.Name, &hit.Depth, &viaAssertion,
			&hit.ViaPredicate, &fromEntity); err != nil {
			return nil, mapError(err, op, "cannot scan graph hit")
		}
		hit.ViaAssertion = domain.AssertionID(viaAssertion)
		hit.FromEntityID = domain.EntityID(fromEntity)
		out = append(out, hit)
	}
	return out, mapError(rows.Err(), op, "cannot expand graph")
}

// DeleteProjections removes every projected record for a workspace.
//
// This exists so the rebuildability claim can be exercised rather than merely asserted:
// drop everything derived, replay, and compare (AGENTS.md scenario I).
func (s *Store) DeleteProjections(ctx context.Context, ws domain.WorkspaceID) error {
	const op = "ledger.DeleteProjections"

	return s.InTx(ctx, func(tx pgx.Tx) error {
		for _, table := range []string{"vector_records", "lexical_records", "graph_edges"} {
			if _, err := tx.Exec(ctx,
				`DELETE FROM `+table+` WHERE workspace_id = $1`, ws); err != nil {
				return mapError(err, op, "cannot delete projection records")
			}
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM projection_checkpoints WHERE workspace_id = $1`, ws); err != nil {
			return mapError(err, op, "cannot delete projection checkpoints")
		}
		return nil
	})
}

// DeleteProjectionsForEvent removes what one source event projected, so reprocessing does
// not leave records behind that the ledger no longer supports.
func (s *Store) DeleteProjectionsForEvent(ctx context.Context, ws domain.WorkspaceID, eventID domain.SourceEventID) error {
	const op = "ledger.DeleteProjectionsForEvent"

	return s.InTx(ctx, func(tx pgx.Tx) error {
		for _, table := range []string{"vector_records", "lexical_records"} {
			if _, err := tx.Exec(ctx,
				`DELETE FROM `+table+` WHERE workspace_id = $1 AND source_event_id = $2`,
				ws, eventID); err != nil {
				return mapError(err, op, "cannot delete projection records")
			}
		}
		return nil
	})
}

// SaveCheckpoint records how far a projection has consumed the ledger.
func (s *Store) SaveCheckpoint(ctx context.Context, checkpoint domain.ProjectionCheckpoint) error {
	const op = "ledger.SaveCheckpoint"

	_, err := s.pool.Exec(ctx, `
		INSERT INTO projection_checkpoints (workspace_id, projection, last_recorded_at,
		                                    last_record_id, records_projected, last_error,
		                                    rebuilt_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,now())
		ON CONFLICT (workspace_id, projection)
		DO UPDATE SET last_recorded_at = EXCLUDED.last_recorded_at,
		              last_record_id = EXCLUDED.last_record_id,
		              records_projected = EXCLUDED.records_projected,
		              last_error = EXCLUDED.last_error,
		              rebuilt_at = coalesce(EXCLUDED.rebuilt_at, projection_checkpoints.rebuilt_at),
		              updated_at = now()`,
		checkpoint.WorkspaceID, checkpoint.Projection, checkpoint.LastRecordedAt,
		nullableString(domain.EntityID(checkpoint.LastRecordID)), checkpoint.RecordsProjected,
		checkpoint.LastError, checkpoint.RebuiltAt)
	return mapError(err, op, "cannot save projection checkpoint")
}

// GetCheckpoint reads a projection's position, returning a zero checkpoint when it has never
// run.
func (s *Store) GetCheckpoint(ctx context.Context, ws domain.WorkspaceID, projection string) (domain.ProjectionCheckpoint, error) {
	const op = "ledger.GetCheckpoint"

	var (
		checkpoint   domain.ProjectionCheckpoint
		lastRecordID *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT workspace_id, projection, last_recorded_at, last_record_id, records_projected,
		       last_error, rebuilt_at, updated_at
		FROM projection_checkpoints WHERE workspace_id = $1 AND projection = $2`, ws, projection,
	).Scan(&checkpoint.WorkspaceID, &checkpoint.Projection, &checkpoint.LastRecordedAt,
		&lastRecordID, &checkpoint.RecordsProjected, &checkpoint.LastError,
		&checkpoint.RebuiltAt, &checkpoint.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.ProjectionCheckpoint{WorkspaceID: ws, Projection: projection}, nil
		}
		return domain.ProjectionCheckpoint{}, mapError(err, op, "cannot read projection checkpoint")
	}
	if lastRecordID != nil {
		checkpoint.LastRecordID = *lastRecordID
	}
	return checkpoint, nil
}

// ListCheckpoints returns every projection's position for a workspace.
func (s *Store) ListCheckpoints(ctx context.Context, ws domain.WorkspaceID) ([]domain.ProjectionCheckpoint, error) {
	const op = "ledger.ListCheckpoints"

	rows, err := s.pool.Query(ctx, `
		SELECT workspace_id, projection, last_recorded_at, last_record_id, records_projected,
		       last_error, rebuilt_at, updated_at
		FROM projection_checkpoints WHERE workspace_id = $1 ORDER BY projection`, ws)
	if err != nil {
		return nil, mapError(err, op, "cannot list projection checkpoints")
	}
	defer rows.Close()

	var out []domain.ProjectionCheckpoint
	for rows.Next() {
		var (
			checkpoint   domain.ProjectionCheckpoint
			lastRecordID *string
		)
		if err := rows.Scan(&checkpoint.WorkspaceID, &checkpoint.Projection,
			&checkpoint.LastRecordedAt, &lastRecordID, &checkpoint.RecordsProjected,
			&checkpoint.LastError, &checkpoint.RebuiltAt, &checkpoint.UpdatedAt); err != nil {
			return nil, mapError(err, op, "cannot scan projection checkpoint")
		}
		if lastRecordID != nil {
			checkpoint.LastRecordID = *lastRecordID
		}
		out = append(out, checkpoint)
	}
	return out, mapError(rows.Err(), op, "cannot list projection checkpoints")
}

// CountProjected reports how many records a projection holds, for verifying a rebuild.
func (s *Store) CountProjected(ctx context.Context, ws domain.WorkspaceID) (map[string]int, error) {
	const op = "ledger.CountProjected"

	out := map[string]int{}
	for name, table := range map[string]string{
		"vector":  "vector_records",
		"lexical": "lexical_records",
		"graph":   "graph_edges",
	} {
		var count int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, ws).Scan(&count); err != nil {
			return nil, mapError(err, op, "cannot count projected records")
		}
		out[name] = count
	}
	return out, nil
}

// formatVector renders an embedding in pgvector's text form.
//
// pgx encodes a Go slice as a PostgreSQL array, which pgvector rejects, so the text form is
// built explicitly and passed with a cast (see ADR 0007).
func formatVector(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.WriteByte('[')
	for i, component := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(component), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// surfaceStrings renders surfaces for a SQL array parameter.
func surfaceStrings(surfaces []domain.Surface) []string {
	out := make([]string, 0, len(surfaces))
	for _, surface := range surfaces {
		out = append(out, string(surface))
	}
	return out
}

// ExistingVectorHashes returns the content hash of each already-embedded record.
//
// Re-projection uses this to skip text that has not changed. Embedding is the expensive part
// of a rebuild, and re-embedding identical text costs money and time for an identical
// result.
func (s *Store) ExistingVectorHashes(ctx context.Context, ws domain.WorkspaceID, model string, version int, surface domain.Surface, recordIDs []string) (map[string]string, error) {
	const op = "ledger.ExistingVectorHashes"

	if len(recordIDs) == 0 {
		return map[string]string{}, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT record_id, content_hash FROM vector_records
		WHERE workspace_id = $1 AND embedding_model = $2 AND embedding_version = $3
		  AND surface = $4 AND record_id = ANY($5::uuid[])`,
		ws, model, version, surface, recordIDs)
	if err != nil {
		return nil, mapError(err, op, "cannot read existing embeddings")
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, mapError(err, op, "cannot scan embedding hash")
		}
		out[id] = hash
	}
	return out, mapError(rows.Err(), op, "cannot read existing embeddings")
}

// ListSourceEventIDsAfter pages through a workspace's events in ledger order.
//
// Recorded time is the cursor because it is monotonic and stable, and the identifier breaks
// ties so a page boundary landing inside a group of simultaneous events cannot skip one.
func (s *Store) ListSourceEventIDsAfter(ctx context.Context, ws domain.WorkspaceID, after *time.Time, afterID domain.SourceEventID, limit int) ([]domain.SourceEvent, error) {
	const op = "ledger.ListSourceEventIDsAfter"

	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `SELECT `+sourceEventColumns+` FROM source_events
		WHERE workspace_id = $1
		  AND ($2::timestamptz IS NULL OR (recorded_at, id) > ($2, $3::uuid))
		ORDER BY recorded_at, id
		LIMIT $4`, ws, after, nullableString(afterID), limit)
	if err != nil {
		return nil, mapError(err, op, "cannot list source events")
	}
	defer rows.Close()

	var out []domain.SourceEvent
	for rows.Next() {
		event, err := scanSourceEvent(rowAdapter{rows}, op)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, mapError(rows.Err(), op, "cannot list source events")
}
