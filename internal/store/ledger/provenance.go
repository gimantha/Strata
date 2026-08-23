package ledger

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gimantha/strata/internal/domain"
)

// insertEvidenceTx records one link from a claim to its source material.
func (s *Store) insertEvidenceTx(ctx context.Context, tx pgx.Tx, e domain.Evidence) error {
	const op = "ledger.insertEvidence"

	if domain.IsZero(e.ID) {
		e.ID = domain.NewEvidenceID()
	}
	if e.Confidence == 0 {
		e.Confidence = 1
	}
	if err := e.Validate(); err != nil {
		return err
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO evidence (id, workspace_id, assertion_id, episode_id, chunk_id, artifact_id,
		                      source_event_id, quote_start, quote_end, extracted_text, model_run_id, confidence)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		e.ID, e.WorkspaceID, e.AssertionID, e.EpisodeID, nullableString(chunkID(e.ChunkID)),
		nullableString(artifactID(e.ArtifactID)), e.SourceEventID, e.QuoteStart, e.QuoteEnd,
		e.ExtractedText, modelRunID(e.ModelRunID), e.Confidence)
	return mapError(err, op, "cannot insert evidence")
}

// insertDerivationTx records what produced a reasoned claim, along with the claims it
// reasoned from.
func (s *Store) insertDerivationTx(ctx context.Context, tx pgx.Tx, d domain.Derivation) error {
	const op = "ledger.insertDerivation"

	if domain.IsZero(d.ID) {
		d.ID = domain.NewDerivationID()
	}
	if err := d.Validate(); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO derivations (id, workspace_id, graph_space_id, method, rule_name, rule_version,
		                         model_run_id, parameters)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (id) DO NOTHING`,
		d.ID, d.WorkspaceID, d.GraphSpaceID, d.Method, d.RuleName, d.RuleVersion,
		modelRunID(d.ModelRunID), jsonMap(d.Parameters)); err != nil {
		return mapError(err, op, "cannot insert derivation")
	}

	for _, input := range d.InputAssertionIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO derivation_inputs (derivation_id, assertion_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, d.ID, input); err != nil {
			return mapError(err, op, "cannot link derivation input")
		}
	}
	return nil
}

// GetDerivation loads a derivation with the assertions it reasoned from.
func (s *Store) GetDerivation(ctx context.Context, ws domain.WorkspaceID, id domain.DerivationID) (domain.Derivation, error) {
	const op = "ledger.GetDerivation"

	var (
		d        domain.Derivation
		modelRun *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, graph_space_id, method, rule_name, rule_version, model_run_id,
		       parameters, created_at
		FROM derivations WHERE workspace_id = $1 AND id = $2`, ws, id,
	).Scan(&d.ID, &d.WorkspaceID, &d.GraphSpaceID, &d.Method, &d.RuleName, &d.RuleVersion,
		&modelRun, &d.Parameters, &d.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.Derivation{}, domain.Errorf(domain.CodeNotFound, op, "derivation not found")
		}
		return domain.Derivation{}, mapError(err, op, "cannot load derivation")
	}
	if modelRun != nil {
		runID := domain.ModelRunID(*modelRun)
		d.ModelRunID = &runID
	}

	rows, err := s.pool.Query(ctx,
		`SELECT assertion_id FROM derivation_inputs WHERE derivation_id = $1`, id)
	if err != nil {
		return domain.Derivation{}, mapError(err, op, "cannot list derivation inputs")
	}
	defer rows.Close()
	for rows.Next() {
		var input domain.AssertionID
		if err := rows.Scan(&input); err != nil {
			return domain.Derivation{}, mapError(err, op, "cannot scan derivation input")
		}
		d.InputAssertionIDs = append(d.InputAssertionIDs, input)
	}
	return d, mapError(rows.Err(), op, "cannot list derivation inputs")
}

// ProvenanceChain walks a claim back to the source material behind it.
//
// This is scenario G from AGENTS.md section 37: assertion, evidence, chunk or episode,
// artifact, source event, source. A fact that cannot be walked back this way is a fact
// the system should not be asserting.
func (s *Store) ProvenanceChain(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) (domain.ProvenanceChain, error) {
	const op = "ledger.ProvenanceChain"

	assertion, err := s.GetAssertion(ctx, ws, id)
	if err != nil {
		return domain.ProvenanceChain{}, err
	}
	subject, err := s.GetEntity(ctx, ws, assertion.SubjectID)
	if err != nil {
		return domain.ProvenanceChain{}, err
	}
	chain := domain.ProvenanceChain{Assertion: assertion, Subject: subject}

	rows, err := s.pool.Query(ctx, `
		SELECT
			ev.id, ev.workspace_id, ev.assertion_id, ev.episode_id, ev.chunk_id, ev.artifact_id,
			ev.source_event_id, ev.quote_start, ev.quote_end, ev.extracted_text, ev.model_run_id,
			ev.confidence, ev.created_at,

			ep.id, ep.sequence, ep.content, ep.content_type, ep.event_time, ep.observed_at,
			ep.recorded_at, ep.locator, ep.classification,

			c.id, c.sequence, c.content, c.token_count, c.char_start, c.char_end, c.locator,

			ar.id, ar.content_hash, ar.media_type, ar.size_bytes, ar.blob_key, ar.storage,

			se.id, se.external_id, se.event_type, se.operation, se.content_hash, se.idempotency_key,
			se.event_time, se.observed_at, se.recorded_at, se.status, se.classification,

			sr.id, sr.kind, sr.name, sr.uri, sr.trust_level, sr.classification
		FROM evidence ev
		JOIN episodes ep      ON ep.id = ev.episode_id
		LEFT JOIN chunks c    ON c.id = ev.chunk_id
		JOIN source_events se ON se.id = ev.source_event_id
		JOIN artifacts ar     ON ar.id = se.raw_artifact_id
		JOIN sources sr       ON sr.id = se.source_id
		WHERE ev.workspace_id = $1 AND ev.assertion_id = $2
		ORDER BY ev.created_at`, ws, id)
	if err != nil {
		return domain.ProvenanceChain{}, mapError(err, op, "cannot walk provenance")
	}
	defer rows.Close()

	for rows.Next() {
		var (
			link          domain.ProvenanceLink
			chunkIDRaw    *string
			artifactIDRaw *string
			modelRun      *string

			chunkRowID     *string
			chunkSequence  *int64
			chunkContent   *string
			chunkTokens    *int
			chunkCharStart *int
			chunkCharEnd   *int
			chunkLocator   *domain.Locator
		)

		if err := rows.Scan(
			&link.Evidence.ID, &link.Evidence.WorkspaceID, &link.Evidence.AssertionID,
			&link.Evidence.EpisodeID, &chunkIDRaw, &artifactIDRaw, &link.Evidence.SourceEventID,
			&link.Evidence.QuoteStart, &link.Evidence.QuoteEnd, &link.Evidence.ExtractedText,
			&modelRun, &link.Evidence.Confidence, &link.Evidence.CreatedAt,

			&link.Episode.ID, &link.Episode.Sequence, &link.Episode.Content, &link.Episode.ContentType,
			&link.Episode.EventTime, &link.Episode.ObservedAt, &link.Episode.RecordedAt,
			&link.Episode.Locator, &link.Episode.Classification,

			&chunkRowID, &chunkSequence, &chunkContent, &chunkTokens, &chunkCharStart, &chunkCharEnd,
			&chunkLocator,

			&link.Artifact.ID, &link.Artifact.ContentHash, &link.Artifact.MediaType,
			&link.Artifact.SizeBytes, &link.Artifact.BlobKey, &link.Artifact.Storage,

			&link.SourceEvent.ID, &link.SourceEvent.ExternalID, &link.SourceEvent.EventType,
			&link.SourceEvent.Operation, &link.SourceEvent.ContentHash, &link.SourceEvent.IdempotencyKey,
			&link.SourceEvent.EventTime, &link.SourceEvent.ObservedAt, &link.SourceEvent.RecordedAt,
			&link.SourceEvent.Status, &link.SourceEvent.Classification,

			&link.Source.ID, &link.Source.Kind, &link.Source.Name, &link.Source.URI,
			&link.Source.TrustLevel, &link.Source.Classification,
		); err != nil {
			return domain.ProvenanceChain{}, mapError(err, op, "cannot scan provenance link")
		}

		if chunkIDRaw != nil {
			cid := domain.ChunkID(*chunkIDRaw)
			link.Evidence.ChunkID = &cid
		}
		if artifactIDRaw != nil {
			aid := domain.ArtifactID(*artifactIDRaw)
			link.Evidence.ArtifactID = &aid
		}
		if modelRun != nil {
			runID := domain.ModelRunID(*modelRun)
			link.Evidence.ModelRunID = &runID
		}
		if chunkRowID != nil {
			chunk := domain.Chunk{
				ID:          domain.ChunkID(*chunkRowID),
				WorkspaceID: ws,
				EpisodeID:   link.Episode.ID,
			}
			if chunkSequence != nil {
				chunk.Sequence = *chunkSequence
			}
			if chunkContent != nil {
				chunk.Content = *chunkContent
			}
			if chunkTokens != nil {
				chunk.TokenCount = *chunkTokens
			}
			if chunkCharStart != nil {
				chunk.CharStart = *chunkCharStart
			}
			if chunkCharEnd != nil {
				chunk.CharEnd = *chunkCharEnd
			}
			if chunkLocator != nil {
				chunk.Locator = *chunkLocator
			}
			link.Chunk = &chunk
		}

		link.Episode.WorkspaceID = ws
		link.Artifact.WorkspaceID = ws
		link.SourceEvent.WorkspaceID = ws
		link.Source.WorkspaceID = ws
		chain.Links = append(chain.Links, link)
	}
	if err := rows.Err(); err != nil {
		return domain.ProvenanceChain{}, mapError(err, op, "cannot walk provenance")
	}

	// A reasoned claim also explains itself through its derivation and the claims it
	// was reasoned from.
	if assertion.DerivationID != nil {
		derivation, err := s.GetDerivation(ctx, ws, *assertion.DerivationID)
		if err != nil {
			return domain.ProvenanceChain{}, err
		}
		chain.Derivation = &derivation

		for _, input := range derivation.InputAssertionIDs {
			support, err := s.GetAssertion(ctx, ws, input)
			if err != nil {
				if domain.IsCode(err, domain.CodeNotFound) {
					continue
				}
				return domain.ProvenanceChain{}, err
			}
			chain.Supports = append(chain.Supports, support)
		}
	}

	return chain, nil
}

// ListEvidence returns the evidence supporting a claim.
func (s *Store) ListEvidence(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) ([]domain.Evidence, error) {
	const op = "ledger.ListEvidence"

	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, assertion_id, episode_id, chunk_id, artifact_id, source_event_id,
		       quote_start, quote_end, extracted_text, model_run_id, confidence, created_at
		FROM evidence WHERE workspace_id = $1 AND assertion_id = $2 ORDER BY created_at`, ws, id)
	if err != nil {
		return nil, mapError(err, op, "cannot list evidence")
	}
	defer rows.Close()

	var out []domain.Evidence
	for rows.Next() {
		var (
			e             domain.Evidence
			chunkIDRaw    *string
			artifactIDRaw *string
			modelRun      *string
		)
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.AssertionID, &e.EpisodeID, &chunkIDRaw,
			&artifactIDRaw, &e.SourceEventID, &e.QuoteStart, &e.QuoteEnd, &e.ExtractedText,
			&modelRun, &e.Confidence, &e.CreatedAt); err != nil {
			return nil, mapError(err, op, "cannot scan evidence")
		}
		if chunkIDRaw != nil {
			cid := domain.ChunkID(*chunkIDRaw)
			e.ChunkID = &cid
		}
		if artifactIDRaw != nil {
			aid := domain.ArtifactID(*artifactIDRaw)
			e.ArtifactID = &aid
		}
		if modelRun != nil {
			runID := domain.ModelRunID(*modelRun)
			e.ModelRunID = &runID
		}
		out = append(out, e)
	}
	return out, mapError(rows.Err(), op, "cannot list evidence")
}

// CreateConflictSet records an unresolved disagreement and attaches claims to it.
//
// Both claims stay believable and are marked disputed. Deleting one would destroy
// information and hide the contradiction (AGENTS.md section 14.2).
func (s *Store) CreateConflictSet(ctx context.Context, set domain.ConflictSet, members []domain.AssertionID) (domain.ConflictSet, error) {
	const op = "ledger.CreateConflictSet"

	if set.Resolution == "" {
		set.Resolution = domain.ConflictOpen
	}
	if err := set.Validate(); err != nil {
		return domain.ConflictSet{}, err
	}
	if domain.IsZero(set.ID) {
		set.ID = domain.NewConflictSetID()
	}

	err := s.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO conflict_sets (id, workspace_id, graph_space_id, subject_id, predicate,
			                           scope_key, reason, resolution)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			RETURNING created_at`,
			set.ID, set.WorkspaceID, set.GraphSpaceID, set.SubjectID, set.Predicate,
			set.ScopeKey, set.Reason, set.Resolution,
		).Scan(&set.CreatedAt); err != nil {
			return mapError(err, op, "cannot create conflict set")
		}

		if _, err := tx.Exec(ctx, `
			UPDATE assertions
			SET conflict_set_id = $3, status = 'disputed'
			WHERE workspace_id = $1 AND id = ANY($2::uuid[]) AND status = 'active'`,
			set.WorkspaceID, idStrings(members), set.ID); err != nil {
			return mapError(err, op, "cannot attach assertions to conflict set")
		}
		return nil
	})
	if err != nil {
		return domain.ConflictSet{}, err
	}
	return set, nil
}

// ResolveConflictSet closes a disagreement, returning the surviving claims to active.
func (s *Store) ResolveConflictSet(ctx context.Context, ws domain.WorkspaceID, id domain.ConflictSetID, resolution domain.ConflictResolution, at time.Time, actor domain.PrincipalID) error {
	const op = "ledger.ResolveConflictSet"

	if _, err := domain.ParseConflictResolution(string(resolution)); err != nil {
		return err
	}

	return s.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE conflict_sets SET resolution = $3, resolved_at = $4, resolved_by = $5
			WHERE workspace_id = $1 AND id = $2 AND resolution = 'open'`,
			ws, id, resolution, at.UTC(), string(actor))
		if err != nil {
			return mapError(err, op, "cannot resolve conflict set")
		}
		if tag.RowsAffected() == 0 {
			return domain.Errorf(domain.CodeNotFound, op, "conflict set not found or already resolved")
		}

		// Claims still standing return to active; ones superseded while disputed keep
		// their superseded status.
		if _, err := tx.Exec(ctx, `
			UPDATE assertions SET status = 'active'
			WHERE workspace_id = $1 AND conflict_set_id = $2 AND status = 'disputed'
			  AND superseded_at IS NULL AND retracted_at IS NULL`, ws, id); err != nil {
			return mapError(err, op, "cannot reactivate assertions")
		}

		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID: ws,
			PrincipalID: actor,
			Action:      "conflict.resolve",
			TargetKind:  "conflict_set",
			TargetID:    string(id),
			Outcome:     "allowed",
			Detail:      map[string]any{"resolution": string(resolution)},
		})
	})
}

// ListConflictSets returns disagreements, optionally only the open ones.
func (s *Store) ListConflictSets(ctx context.Context, scope domain.Scope, openOnly bool, limit int) ([]domain.ConflictSet, error) {
	const op = "ledger.ListConflictSets"

	if limit <= 0 || limit > domain.MaxAssertionLimit {
		limit = domain.DefaultAssertionLimit
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, graph_space_id, subject_id, predicate, scope_key, reason,
		       resolution, resolved_at, resolved_by, created_at
		FROM conflict_sets
		WHERE workspace_id = $1
		  AND ($2 = '' OR graph_space_id = $2::uuid)
		  AND (NOT $3 OR resolution = 'open')
		ORDER BY created_at DESC LIMIT $4`,
		scope.WorkspaceID, string(scope.GraphSpaceID), openOnly, limit)
	if err != nil {
		return nil, mapError(err, op, "cannot list conflict sets")
	}
	defer rows.Close()

	var out []domain.ConflictSet
	for rows.Next() {
		var c domain.ConflictSet
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.GraphSpaceID, &c.SubjectID, &c.Predicate,
			&c.ScopeKey, &c.Reason, &c.Resolution, &c.ResolvedAt, &c.ResolvedBy, &c.CreatedAt); err != nil {
			return nil, mapError(err, op, "cannot scan conflict set")
		}
		out = append(out, c)
	}
	return out, mapError(rows.Err(), op, "cannot list conflict sets")
}

func chunkID(id *domain.ChunkID) domain.ChunkID {
	if id == nil {
		return ""
	}
	return *id
}

func artifactID(id *domain.ArtifactID) domain.ArtifactID {
	if id == nil {
		return ""
	}
	return *id
}

func modelRunID(id *domain.ModelRunID) *string {
	if id == nil {
		return nil
	}
	v := string(*id)
	return &v
}
