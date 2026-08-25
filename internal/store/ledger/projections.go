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
			                            source_event_id, content_hash, entity_type,
			                            source_id, predicate,
			                            active_from, active_until, decay_starts_at, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
			ON CONFLICT (workspace_id, surface, record_id, embedding_model, embedding_version)
			DO UPDATE SET embedding = EXCLUDED.embedding,
			              valid_from = EXCLUDED.valid_from,
			              valid_to = EXCLUDED.valid_to,
			              status = EXCLUDED.status,
			              classification = EXCLUDED.classification,
			              memory_kind = EXCLUDED.memory_kind,
			              content_hash = EXCLUDED.content_hash,
			              entity_type = EXCLUDED.entity_type,
			              source_id = EXCLUDED.source_id,
			              predicate = EXCLUDED.predicate,
			              active_from = EXCLUDED.active_from,
			              active_until = EXCLUDED.active_until,
			              decay_starts_at = EXCLUDED.decay_starts_at,
			              expires_at = EXCLUDED.expires_at`,
			domain.NewUUIDString(), record.Scope.WorkspaceID, record.Scope.GraphSpaceID,
			record.Surface, record.RecordID, record.Model, record.Version,
			formatVector(record.Embedding),
			record.ValidFrom, record.ValidTo, record.Status, record.Classification,
			record.MemoryKind, nullableString(record.SourceEventID), record.ContentHash,
			record.EntityType, nullableString(record.SourceID), record.Predicate,
			record.Lifecycle.ActiveFrom, record.Lifecycle.ActiveUntil,
			record.Lifecycle.DecayStartsAt, record.Lifecycle.ExpiresAt)
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

	sql, args, err := buildVectorSearch(op, q)
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError(err, op, "cannot search vectors")
	}
	defer rows.Close()

	now := time.Now().UTC()
	if q.ActiveAt != nil {
		now = q.ActiveAt.UTC()
	}

	var out []domain.Hit
	for rows.Next() {
		var (
			hit           domain.Hit
			decayStartsAt *time.Time
		)
		if err := rows.Scan(&hit.Surface, &hit.RecordID, &hit.Score, &decayStartsAt); err != nil {
			return nil, mapError(err, op, "cannot scan vector hit")
		}
		hit.Decay = domain.Lifecycle{DecayStartsAt: decayStartsAt}.
			DecayWeight(now, domain.DecayHalfLife)
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

// buildVectorSearch renders the nearest-neighbour query.
//
// Separated from execution so the plan can be inspected without running the search
// (see ExplainVectorSearch). AGENTS.md section 39 requires that ordinary semantic search
// never degrades into a full scan, and the only way to hold a system to that is to look at
// the plan the database actually chose rather than at the indexes someone meant to create.
func buildVectorSearch(op string, q domain.VectorQuery) (string, []any, error) {
	if len(q.Embedding) == 0 {
		return "", nil, domain.Errorf(domain.CodeInvalidArgument, op, "a query embedding is required")
	}
	if domain.IsZero(q.Scope.WorkspaceID) {
		return "", nil, domain.Errorf(domain.CodeInvalidArgument, op, "workspace scope is required")
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
	applyPolicyFilters(add, "v", q.Policy)
	applyLifecycle(add, "v", q.ActiveAt)

	args = append(args, formatVector(q.Embedding))
	probe := len(args)
	args = append(args, limit)

	// Two sorts, and the nesting is load-bearing.
	//
	// The inner ORDER BY is the distance expression alone, because an HNSW index scan can
	// only satisfy an ordering that matches the indexed expression exactly. Adding a
	// second sort key there forces PostgreSQL to sort the whole candidate set instead,
	// which means a sequential scan of every vector in the workspace — measured at 200,000
	// rows, still a sequential scan, so this is not a small-table effect.
	//
	// The outer ORDER BY makes the returned order deterministic. Without it two records at
	// the same distance come back in whatever order the scan produced, the same query
	// against unchanged data answers differently on different machines, and a caller
	// paging through results can see an item twice or never.
	//
	// What remains undetermined is which rows sit exactly on the limit boundary when
	// distances tie there. That is inherent to approximate nearest-neighbour search and is
	// not worth a full scan to remove.
	sql := `
		SELECT ranked.surface, ranked.record_id, ranked.score, ranked.decay_starts_at
		FROM (
			SELECT v.surface, v.record_id,
			       1 - (v.embedding <=> $` + strconv.Itoa(probe) + `::vector) AS score,
			       v.decay_starts_at
			FROM vector_records v
			WHERE ` + strings.Join(where, " AND ") + `
			ORDER BY v.embedding <=> $` + strconv.Itoa(probe) + `::vector
			LIMIT $` + strconv.Itoa(len(args)) + `
		) ranked
		ORDER BY ranked.score DESC, ranked.record_id`

	return sql, args, nil
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
			                             memory_kind, source_event_id, entity_type,
			                             source_id, predicate,
			                             active_from, active_until, decay_starts_at, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
			ON CONFLICT (workspace_id, surface, record_id)
			DO UPDATE SET content = EXCLUDED.content,
			              valid_from = EXCLUDED.valid_from,
			              valid_to = EXCLUDED.valid_to,
			              status = EXCLUDED.status,
			              classification = EXCLUDED.classification,
			              memory_kind = EXCLUDED.memory_kind,
			              entity_type = EXCLUDED.entity_type,
			              source_id = EXCLUDED.source_id,
			              predicate = EXCLUDED.predicate,
			              active_from = EXCLUDED.active_from,
			              active_until = EXCLUDED.active_until,
			              decay_starts_at = EXCLUDED.decay_starts_at,
			              expires_at = EXCLUDED.expires_at`,
			domain.NewUUIDString(), record.Scope.WorkspaceID, record.Scope.GraphSpaceID,
			record.Surface, record.RecordID, record.Content,
			record.ValidFrom, record.ValidTo, record.Status, record.Classification,
			record.MemoryKind, nullableString(record.SourceEventID), record.EntityType,
			nullableString(record.SourceID), record.Predicate,
			record.Lifecycle.ActiveFrom, record.Lifecycle.ActiveUntil,
			record.Lifecycle.DecayStartsAt, record.Lifecycle.ExpiresAt)
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

	sql, args, err := buildLexicalSearch(op, q)
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError(err, op, "cannot search lexically")
	}
	defer rows.Close()

	return s.scanLexicalHits(rows, q, op)
}

// buildLexicalSearch renders the full-text or substring query.
//
// Split from execution for the same reason as the vector search: section 39's "no full
// graph scan for ordinary semantic search" is a claim about query plans, and a claim about
// plans can only be tested by reading one.
func buildLexicalSearch(op string, q domain.LexicalQuery) (string, []any, error) {
	if strings.TrimSpace(q.Text) == "" {
		return "", nil, domain.Errorf(domain.CodeInvalidArgument, op, "query text is required")
	}
	if domain.IsZero(q.Scope.WorkspaceID) {
		return "", nil, domain.Errorf(domain.CodeInvalidArgument, op, "workspace scope is required")
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
	applyPolicyFilters(add, "l", q.Policy)
	applyLifecycle(add, "l", q.ActiveAt)

	args = append(args, q.Text)
	term := len(args)
	args = append(args, limit)

	var sql string
	if q.Exact {
		where = append(where, fmt.Sprintf("l.content ILIKE '%%' || $%d || '%%'", term))
		sql = `SELECT l.surface, l.record_id, l.content,
		              similarity(l.content, $` + strconv.Itoa(term) + `) AS score,
		              l.decay_starts_at
		       FROM lexical_records l
		       WHERE ` + strings.Join(where, " AND ") + `
		       ORDER BY score DESC, length(l.content), l.record_id
		       LIMIT $` + strconv.Itoa(len(args))
	} else {
		where = append(where, fmt.Sprintf("l.search_vector @@ websearch_to_tsquery('english', $%d)", term))
		// ts_rank_cd weighs term proximity, which ranks a passage discussing the terms
		// together above one that merely mentions them.
		sql = `SELECT l.surface, l.record_id, l.content,
		              ts_rank_cd(l.search_vector, websearch_to_tsquery('english', $` + strconv.Itoa(term) + `)) AS score,
		              l.decay_starts_at
		       FROM lexical_records l
		       WHERE ` + strings.Join(where, " AND ") + `
		       ORDER BY score DESC, l.record_id
		       LIMIT $` + strconv.Itoa(len(args))
	}

	return sql, args, nil
}

// scanLexicalHits reads the result set.
func (s *Store) scanLexicalHits(rows pgx.Rows, q domain.LexicalQuery, op string) ([]domain.Hit, error) {
	mode := "lexical"
	if q.Exact {
		mode = "lexical_exact"
	}

	now := time.Now().UTC()
	if q.ActiveAt != nil {
		now = q.ActiveAt.UTC()
	}

	var out []domain.Hit
	for rows.Next() {
		var (
			hit           domain.Hit
			decayStartsAt *time.Time
		)
		if err := rows.Scan(&hit.Surface, &hit.RecordID, &hit.Content, &hit.Score,
			&decayStartsAt); err != nil {
			return nil, mapError(err, op, "cannot scan lexical hit")
		}
		hit.Decay = domain.Lifecycle{DecayStartsAt: decayStartsAt}.
			DecayWeight(now, domain.DecayHalfLife)
		hit.Detail = map[string]any{"retriever": mode, "rank": hit.Score, "decay": hit.Decay}
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
			                         status, confidence, classification, source_id,
			                         active_until, expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			ON CONFLICT (assertion_id)
			DO UPDATE SET status = EXCLUDED.status,
			              valid_from = EXCLUDED.valid_from,
			              valid_to = EXCLUDED.valid_to,
			              confidence = EXCLUDED.confidence,
			              classification = EXCLUDED.classification,
			              source_id = EXCLUDED.source_id,
			              active_until = EXCLUDED.active_until,
			              expires_at = EXCLUDED.expires_at`,
			domain.NewUUIDString(), edge.WorkspaceID, edge.GraphSpaceID, edge.SubjectID,
			edge.Predicate, edge.ObjectEntityID, edge.AssertionID, edge.ValidFrom, edge.ValidTo,
			edge.Status, edge.Confidence, edge.Classification, nullableString(edge.SourceID),
			edge.ActiveUntil, edge.ExpiresAt)
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
			-- Seeded from the ids the caller supplied rather than from the entities
			-- table. Traversal walks edges; it does not own identities, and a graph
			-- backend that had to read canonical entities to start would not be a graph
			-- backend. The caller resolved these roots through a scoped canonical lookup
			-- already, and hydrating the names afterwards drops anything that is not in
			-- the workspace, so nothing reaches a caller that a join here would have
			-- filtered.
			SELECT root.id AS entity_id,
			       0 AS depth,
			       NULL::uuid AS via_assertion,
			       ''::text AS via_predicate,
			       NULL::uuid AS from_entity,
			       ARRAY[root.id] AS path
			FROM unnest($2::uuid[]) AS root(id)

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
				  `+edgePolicyClause+`
				  `+edgeLifecycleClause+`
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
				  `+edgePolicyClause+`
				  `+edgeLifecycleClause+`
			) next ON true
			WHERE w.depth < $7
			  AND NOT next.entity_id = ANY(w.path)
		)
		SELECT reached.entity_id, reached.depth, reached.via_assertion,
		       reached.via_predicate, reached.from_entity
		FROM (
			SELECT DISTINCT ON (walk.entity_id)
			       walk.entity_id, walk.depth,
			       coalesce(walk.via_assertion::text, '') AS via_assertion,
			       walk.via_predicate,
			       coalesce(walk.from_entity::text, '') AS from_entity
			FROM walk
			ORDER BY walk.entity_id, walk.depth
			LIMIT $8
		) reached
		-- Roots are dropped rather than reported. They are already candidates from
		-- whichever retriever found them, so the only caller discarded them anyway, and
		-- dropping them here means a walk seeded with an id from another workspace
		-- returns nothing at all: its edges are filtered by workspace, so it can reach
		-- nothing, and it is no longer echoed back as a hit. Tenancy stays enforced by
		-- the traversal rather than by whoever reads its results.
		--
		-- Filtered after the limit, not before, because the limit has always counted
		-- roots. Moving it would quietly widen every traversal.
		WHERE reached.depth > 0`,
		q.Scope.WorkspaceID, idStrings(q.Roots), nullableString(q.Scope.GraphSpaceID),
		statuses, predicates, validAt, q.Depth, q.Limit,
		policyClassifications(q.Policy), orNilIDs(q.Policy.AllowedSources),
		orNilIDs(q.Policy.DeniedSources), orNilStrings(q.Policy.AllowedPredicates),
		orNilStrings(q.Policy.DeniedPredicates), activeAtOrNil(q.ActiveAt))
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
		if err := rows.Scan(&hit.EntityID, &hit.Depth, &viaAssertion,
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
		// Only the checkpoints a rebuild owns. projection_checkpoints is shared: the
		// consolidation job keeps its cursor in the same table, and clearing the table by
		// workspace would reset a component that has nothing to do with retrieval. It
		// would then rescan the whole ledger — idempotent, so not corrupting, but silent
		// work nobody asked for and nobody could see the cause of.
		if _, err := tx.Exec(ctx,
			`DELETE FROM projection_checkpoints
			 WHERE workspace_id = $1 AND projection = ANY($2::text[])`,
			ws, domain.RetrievalProjections()); err != nil {
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

	// A checkpoint is a high-water mark, and several workers advance it concurrently
	// (phase 14). Two of them finishing out of order must not let the older position win,
	// so the position only moves forward — while the counter accumulates and the error
	// field always reflects the latest attempt, because a stale error is worse than none.
	//
	// The alternative, a lock around read-modify-write, would serialize every projecting
	// worker on one row for a value that is a hint rather than a source of truth.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO projection_checkpoints (workspace_id, projection, last_recorded_at,
		                                    last_record_id, records_projected, last_error,
		                                    rebuilt_at, advanced_by, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,now())
		ON CONFLICT (workspace_id, projection)
		DO UPDATE SET
		    last_recorded_at = CASE
		        WHEN EXCLUDED.last_recorded_at IS NULL THEN projection_checkpoints.last_recorded_at
		        WHEN projection_checkpoints.last_recorded_at IS NULL THEN EXCLUDED.last_recorded_at
		        WHEN EXCLUDED.last_recorded_at > projection_checkpoints.last_recorded_at
		            THEN EXCLUDED.last_recorded_at
		        ELSE projection_checkpoints.last_recorded_at
		    END,
		    last_record_id = CASE
		        WHEN EXCLUDED.last_recorded_at IS NULL THEN projection_checkpoints.last_record_id
		        WHEN projection_checkpoints.last_recorded_at IS NULL THEN EXCLUDED.last_record_id
		        WHEN EXCLUDED.last_recorded_at > projection_checkpoints.last_recorded_at
		            THEN EXCLUDED.last_record_id
		        ELSE projection_checkpoints.last_record_id
		    END,
		    -- Accumulated, not replaced: each worker reports what it did, and the total is
		    -- the fleet's throughput rather than whichever member wrote last. A rebuild
		    -- resets it instead, because after a rebuild the count describes the rebuild.
		    records_projected = CASE
		        WHEN EXCLUDED.rebuilt_at IS NOT NULL THEN EXCLUDED.records_projected
		        ELSE projection_checkpoints.records_projected + EXCLUDED.records_projected
		    END,
		    last_error = EXCLUDED.last_error,
		    rebuilt_at = coalesce(EXCLUDED.rebuilt_at, projection_checkpoints.rebuilt_at),
		    advanced_by = EXCLUDED.advanced_by,
		    updated_at = now()`,
		checkpoint.WorkspaceID, checkpoint.Projection, checkpoint.LastRecordedAt,
		nullableString(domain.EntityID(checkpoint.LastRecordID)), checkpoint.RecordsProjected,
		checkpoint.LastError, checkpoint.RebuiltAt, checkpoint.AdvancedBy)
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
		       last_error, rebuilt_at, advanced_by, updated_at
		FROM projection_checkpoints WHERE workspace_id = $1 AND projection = $2`, ws, projection,
	).Scan(&checkpoint.WorkspaceID, &checkpoint.Projection, &checkpoint.LastRecordedAt,
		&lastRecordID, &checkpoint.RecordsProjected, &checkpoint.LastError,
		&checkpoint.RebuiltAt, &checkpoint.AdvancedBy, &checkpoint.UpdatedAt)
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
		       last_error, rebuilt_at, advanced_by, updated_at
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
			&checkpoint.LastError, &checkpoint.RebuiltAt, &checkpoint.AdvancedBy,
			&checkpoint.UpdatedAt); err != nil {
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

// applyPolicyFilters narrows a projection query to what a principal may see
// (AGENTS.md section 22.4).
//
// In the WHERE clause rather than after the scan, on purpose. Filtering results would mean
// unauthorized rows had already been read into memory, ranked against authorized ones, and
// counted — and would make a restricted query return fewer results than asked for while
// looking like there was nothing to find.
func applyPolicyFilters(add func(clause string, value any), alias string, filters domain.PolicyFilters) {
	if !filters.Restrictive() {
		return
	}

	// The ceiling and any carve-outs collapse into one permitted set, so the query has a
	// single membership test rather than a ceiling comparison the database cannot index.
	add(alias+".classification = ANY($%d::text[])",
		enumStrings(filters.PermittedClassifications()))

	if len(filters.AllowedSources) > 0 {
		add(alias+".source_id = ANY($%d::uuid[])", idStrings(filters.AllowedSources))
	}
	if len(filters.DeniedSources) > 0 {
		add("("+alias+".source_id IS NULL OR NOT ("+alias+".source_id = ANY($%d::uuid[])))",
			idStrings(filters.DeniedSources))
	}
	if len(filters.AllowedMemoryKinds) > 0 {
		add(alias+".memory_kind = ANY($%d::text[])", enumStrings(filters.AllowedMemoryKinds))
	}
	if len(filters.DeniedMemoryKinds) > 0 {
		add("NOT ("+alias+".memory_kind = ANY($%d::text[]))", enumStrings(filters.DeniedMemoryKinds))
	}
	if len(filters.AllowedEntityTypes) > 0 {
		// Chunks carry no entity type and are not entity-scoped material, so a rule about
		// entity types must not silently hide every passage.
		add("("+alias+".entity_type = '' OR "+alias+".entity_type = ANY($%d::text[]))",
			filters.AllowedEntityTypes)
	}
	if len(filters.DeniedEntityTypes) > 0 {
		add("NOT ("+alias+".entity_type = ANY($%d::text[]))", filters.DeniedEntityTypes)
	}
	if len(filters.AllowedPredicates) > 0 {
		add("("+alias+".predicate = '' OR "+alias+".predicate = ANY($%d::text[]))",
			filters.AllowedPredicates)
	}
	if len(filters.DeniedPredicates) > 0 {
		add("NOT ("+alias+".predicate = ANY($%d::text[]))", filters.DeniedPredicates)
	}
}

// edgePolicyClause narrows graph traversal to edges a principal may follow.
//
// Applied inside the walk rather than to its results, because traversal leaks by reaching:
// an entity found only through a restricted edge is disclosed by its presence, even if the
// edge's own claim is filtered out of the answer. Filtering afterwards would also change
// depth, since a hidden edge would still have counted as a hop.
//
// Written as static SQL with nullable array parameters rather than assembled per query. A
// recursive CTE with two lateral branches has to apply exactly the same condition in both,
// and a clause built by string concatenation is one edit away from applying it in only one.
const edgePolicyClause = `AND ($9::text[] IS NULL OR ge.classification = ANY($9::text[]))
				  AND ($10::uuid[] IS NULL OR ge.source_id = ANY($10::uuid[]))
				  AND ($11::uuid[] IS NULL OR ge.source_id IS NULL OR NOT (ge.source_id = ANY($11::uuid[])))
				  AND ($12::text[] IS NULL OR ge.predicate = ANY($12::text[]))
				  AND ($13::text[] IS NULL OR NOT (ge.predicate = ANY($13::text[])))`

// policyClassifications renders the permitted set, or nil when policy does not narrow.
func policyClassifications(filters domain.PolicyFilters) []string {
	if !filters.Restrictive() {
		return nil
	}
	return enumStrings(filters.PermittedClassifications())
}

func orNilIDs[T ~string](values []T) []string {
	if len(values) == 0 {
		return nil
	}
	return idStrings(values)
}

func orNilStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return values
}

// applyLifecycle restricts a projection query to knowledge in scope at an instant
// (AGENTS.md sections 21.2, 21.3).
//
// Expiry and the active window filter; decay does not. Decay affects ranking, not truth, and
// a decayed memory that vanished from results would be deletion wearing a ranking function's
// clothes. An expired one is different: the deployment said stop using it.
//
// Records with no lifecycle set — which is nearly all of them — pass untouched, so the clause
// costs nothing for knowledge that never expires.
func applyLifecycle(add func(clause string, value any), alias string, activeAt *time.Time) {
	if activeAt == nil {
		return
	}
	at := activeAt.UTC()

	add("("+alias+".active_from IS NULL OR "+alias+".active_from <= $%d)", at)
	// Half-open, like every other interval here: active until noon is not active at noon.
	add("("+alias+".active_until IS NULL OR "+alias+".active_until > $%d)", at)
	add("("+alias+".expires_at IS NULL OR "+alias+".expires_at > $%d)", at)
}

// edgeLifecycleClause keeps expired relationships out of traversal.
//
// Applied inside the walk for the same reason the policy clause is: an edge that should no
// longer be in scope still discloses the entity it reaches and still counts as a hop, so
// filtering the results would give the right names at the wrong depths.
const edgeLifecycleClause = `AND ($14::timestamptz IS NULL OR ge.active_until IS NULL OR ge.active_until > $14)
				  AND ($14::timestamptz IS NULL OR ge.expires_at IS NULL OR ge.expires_at > $14)`

func activeAtOrNil(at *time.Time) any {
	if at == nil {
		return nil
	}
	return at.UTC()
}

// ExplainVectorSearch returns the query plan PostgreSQL chose for a nearest-neighbour
// search, without running it.
//
// This exists because AGENTS.md section 39 requires that ordinary semantic search never
// degrades into a full scan, and that is a property of the plan rather than of the schema.
// Indexes can exist and go unused — a filter the planner cannot push down, a cast that
// defeats an operator class, a table small enough today that a scan wins and large enough
// tomorrow that it does not. Reading the plan is the only way to know.
func (s *Store) ExplainVectorSearch(ctx context.Context, q domain.VectorQuery,
	preference PlanPreference) (string, error) {
	const op = "ledger.ExplainVectorSearch"

	sql, args, err := buildVectorSearch(op, q)
	if err != nil {
		return "", err
	}
	return s.explain(ctx, op, sql, args, preference)
}

// PlanPreference selects which question an EXPLAIN is answering.
type PlanPreference int

const (
	// PlanAsChosen reports the plan for the data as it stands. On a small table a
	// sequential scan is often genuinely cheapest, so this answers "what happens today".
	PlanAsChosen PlanPreference = iota
	// PlanPreferIndexes reports the plan with sequential scans disabled, answering the
	// question that does not depend on how much data there happens to be: can this query
	// use the index built for it at all?
	//
	// The distinction matters. An index that is merely unused today because the table is
	// small is fine; an index the planner cannot apply to this query is a defect that
	// stays hidden until the table is large enough for it to matter, which is the worst
	// possible time to find it.
	PlanPreferIndexes
)

// ExplainLexicalSearch returns the query plan for a full-text or substring search.
func (s *Store) ExplainLexicalSearch(ctx context.Context, q domain.LexicalQuery,
	preference PlanPreference) (string, error) {
	const op = "ledger.ExplainLexicalSearch"

	sql, args, err := buildLexicalSearch(op, q)
	if err != nil {
		return "", err
	}
	return s.explain(ctx, op, sql, args, preference)
}

// explain runs EXPLAIN and joins the plan into one string.
//
// Plan only, never ANALYZE: this is asked about a live database, and executing the query a
// second time to describe it would double the cost of the diagnostic.
func (s *Store) explain(ctx context.Context, op, sql string, args []any,
	preference PlanPreference) (string, error) {
	// A transaction, because PlanPreferIndexes sets a planner flag and SET LOCAL confines
	// it to this statement. Leaking enable_seqscan onto a pooled connection would change
	// how every later query on it is planned.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", mapError(err, op, "cannot begin a transaction to explain the query")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if preference == PlanPreferIndexes {
		if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
			return "", mapError(err, op, "cannot ask the planner to prefer indexes")
		}
	}

	rows, err := tx.Query(ctx, "EXPLAIN "+sql, args...)
	if err != nil {
		return "", mapError(err, op, "cannot explain the query")
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", mapError(err, op, "cannot read the query plan")
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	return plan.String(), mapError(rows.Err(), op, "cannot read the query plan")
}

// RefreshVectorMetadata updates everything about a vector row except the vector itself.
//
// It exists because embedding is expensive and lifecycle is not. The projector skips
// re-embedding text whose content is unchanged, and a claim that has been deactivated,
// expired, superseded, or reclassified has exactly that shape: identical text, different
// standing. Without a metadata-only write, skipping the embedding also skipped the
// lifecycle, and a forgotten claim stayed active in the vector index while the lexical
// index dropped it (AGENTS.md section 21).
//
// Scoped to one model and version, matching how ExistingVectorHashes decides what is
// already embedded: rows from another model are a different projection with their own
// lifecycle to maintain.
func (s *Store) RefreshVectorMetadata(ctx context.Context, model string, version int,
	records []domain.ProjectedRecord) error {
	const op = "ledger.RefreshVectorMetadata"

	if len(records) == 0 {
		return nil
	}

	return s.InTx(ctx, func(tx pgx.Tx) error {
		batch := &pgx.Batch{}
		for _, record := range records {
			batch.Queue(`
				UPDATE vector_records
				SET valid_from = $6, valid_to = $7, status = $8, classification = $9,
				    memory_kind = $10, entity_type = $11, source_id = $12, predicate = $13,
				    active_from = $14, active_until = $15, decay_starts_at = $16, expires_at = $17
				WHERE workspace_id = $1 AND surface = $2 AND record_id = $3
				  AND embedding_model = $4 AND embedding_version = $5`,
				record.Scope.WorkspaceID, record.Surface, record.RecordID, model, version,
				record.ValidFrom, record.ValidTo, record.Status, record.Classification,
				record.MemoryKind, record.EntityType,
				nullableString(record.SourceID), record.Predicate,
				record.Lifecycle.ActiveFrom, record.Lifecycle.ActiveUntil,
				record.Lifecycle.DecayStartsAt, record.Lifecycle.ExpiresAt)
		}

		results := tx.SendBatch(ctx, batch)
		defer func() { _ = results.Close() }()
		for range records {
			if _, err := results.Exec(); err != nil {
				return mapError(err, op, "cannot refresh vector metadata")
			}
		}
		return nil
	})
}
