package ledger

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/gimantha/strata/internal/domain"
)

// maxRedirectDepth bounds how far a merge chain is followed. A cycle should be impossible
// given the checks on merge, but following one forever would hang every read that touches
// the entity, so the walk is bounded regardless.
const maxRedirectDepth = 32

// UpsertIdentifier binds a stable external key to an identity.
//
// If the key is already bound to a different identity, that is reported as a conflict
// rather than silently rebound: two entities claiming one primary key is a real problem
// upstream, and quietly picking a winner would hide it.
func (s *Store) UpsertIdentifier(ctx context.Context, id domain.EntityIdentifier) (domain.EntityIdentifier, error) {
	const op = "ledger.UpsertIdentifier"

	id.Value = domain.NormalizeIdentifierValue(id.Value)
	id.Namespace = domain.NormalizeIdentifierValue(id.Namespace)
	if err := id.Validate(); err != nil {
		return domain.EntityIdentifier{}, err
	}
	if id.ID == "" {
		id.ID = domain.NewUUIDString()
	}

	var existing domain.EntityID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO entity_identifiers (id, workspace_id, graph_space_id, entity_id, kind,
		                                namespace, value, source_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (workspace_id, graph_space_id, kind, namespace, value) DO NOTHING
		RETURNING entity_id`,
		id.ID, id.WorkspaceID, id.GraphSpaceID, id.EntityID, id.Kind,
		id.Namespace, id.Value, nullableString(sourceIDValue(id.SourceID))).Scan(&existing)

	switch {
	case err == nil:
		return id, nil
	case isNoRows(err):
		bound, lookupErr := s.FindByIdentifier(ctx,
			domain.Scope{WorkspaceID: id.WorkspaceID, GraphSpaceID: id.GraphSpaceID},
			id.Kind, id.Namespace, id.Value)
		if lookupErr != nil {
			return domain.EntityIdentifier{}, lookupErr
		}
		if bound.EntityID == id.EntityID {
			return bound, nil
		}
		return domain.EntityIdentifier{}, domain.Errorf(domain.CodeConflict, op,
			"identifier %s/%s is already bound to a different entity", id.Namespace, id.Value)
	default:
		return domain.EntityIdentifier{}, mapError(err, op, "cannot bind identifier")
	}
}

// FindByIdentifier looks up an identity by a stable key.
func (s *Store) FindByIdentifier(ctx context.Context, scope domain.Scope, kind domain.IdentifierKind, namespace, value string) (domain.EntityIdentifier, error) {
	const op = "ledger.FindByIdentifier"

	var out domain.EntityIdentifier
	var sourceID *string
	err := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, graph_space_id, entity_id, kind, namespace, value, source_id, created_at
		FROM entity_identifiers
		WHERE workspace_id = $1 AND graph_space_id = $2 AND kind = $3 AND namespace = $4 AND value = $5`,
		scope.WorkspaceID, scope.GraphSpaceID, kind,
		domain.NormalizeIdentifierValue(namespace), domain.NormalizeIdentifierValue(value),
	).Scan(&out.ID, &out.WorkspaceID, &out.GraphSpaceID, &out.EntityID, &out.Kind,
		&out.Namespace, &out.Value, &sourceID, &out.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.EntityIdentifier{}, domain.Errorf(domain.CodeNotFound, op, "identifier not found")
		}
		return domain.EntityIdentifier{}, mapError(err, op, "cannot look up identifier")
	}
	if sourceID != nil {
		id := domain.SourceID(*sourceID)
		out.SourceID = &id
	}
	return out, nil
}

// ListIdentifiers returns the stable keys bound to an identity.
func (s *Store) ListIdentifiers(ctx context.Context, ws domain.WorkspaceID, entityID domain.EntityID) ([]domain.EntityIdentifier, error) {
	const op = "ledger.ListIdentifiers"

	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, graph_space_id, entity_id, kind, namespace, value, source_id, created_at
		FROM entity_identifiers WHERE workspace_id = $1 AND entity_id = $2 ORDER BY created_at`,
		ws, entityID)
	if err != nil {
		return nil, mapError(err, op, "cannot list identifiers")
	}
	defer rows.Close()

	var out []domain.EntityIdentifier
	for rows.Next() {
		var (
			item     domain.EntityIdentifier
			sourceID *string
		)
		if err := rows.Scan(&item.ID, &item.WorkspaceID, &item.GraphSpaceID, &item.EntityID,
			&item.Kind, &item.Namespace, &item.Value, &sourceID, &item.CreatedAt); err != nil {
			return nil, mapError(err, op, "cannot scan identifier")
		}
		if sourceID != nil {
			id := domain.SourceID(*sourceID)
			item.SourceID = &id
		}
		out = append(out, item)
	}
	return out, mapError(rows.Err(), op, "cannot list identifiers")
}

// FindByExactAlias returns identities whose known names match exactly after normalization.
//
// Merged identities are excluded: a redirected name should resolve to whatever it was
// merged into, which the caller does by canonicalizing the result.
func (s *Store) FindByExactAlias(ctx context.Context, scope domain.Scope, name string) ([]domain.AliasMatch, error) {
	const op = "ledger.FindByExactAlias"

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (e.id) `+prefixColumns("e", entityColumns)+`, a.alias
		FROM entities e
		JOIN entity_aliases a ON a.entity_id = e.id
		WHERE e.workspace_id = $1 AND e.graph_space_id = $2
		  AND a.normalized = $3
		  AND e.retired_at IS NULL
		ORDER BY e.id, e.created_at`,
		scope.WorkspaceID, scope.GraphSpaceID, domain.NormalizeAlias(name))
	if err != nil {
		return nil, mapError(err, op, "cannot match aliases")
	}
	defer rows.Close()

	return scanAliasMatches(rows, op, 1.0)
}

// FindBySimilarAlias generates fuzzy candidates using trigram similarity.
//
// This is candidate *generation*, not a decision. Two different people can score highly
// against each other - "Alice Chen" and "Alice Chan" sit around 0.57 - so the caller
// decides what is close enough, and by what margin (AGENTS.md section 12.1, rung 5).
func (s *Store) FindBySimilarAlias(ctx context.Context, scope domain.Scope, name string, threshold float64, limit int) ([]domain.AliasMatch, error) {
	const op = "ledger.FindBySimilarAlias"

	if limit <= 0 || limit > 50 {
		limit = 10
	}
	normalized := domain.NormalizeAlias(name)
	if normalized == "" {
		return nil, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (e.id) `+prefixColumns("e", entityColumns)+`, a.alias,
		       similarity(a.normalized, $3) AS score
		FROM entities e
		JOIN entity_aliases a ON a.entity_id = e.id
		WHERE e.workspace_id = $1 AND e.graph_space_id = $2
		  AND e.retired_at IS NULL
		  AND similarity(a.normalized, $3) >= $4
		ORDER BY e.id, score DESC
		LIMIT $5`,
		scope.WorkspaceID, scope.GraphSpaceID, normalized, threshold, limit)
	if err != nil {
		return nil, mapError(err, op, "cannot match similar aliases")
	}
	defer rows.Close()

	return scanAliasMatches(rows, op, -1)
}

func scanAliasMatches(rows pgx.Rows, op string, fixedScore float64) ([]domain.AliasMatch, error) {
	var out []domain.AliasMatch
	for rows.Next() {
		var (
			match domain.Entity
			alias string
			score float64
		)
		dest := []any{
			&match.ID, &match.WorkspaceID, &match.GraphSpaceID, &match.CanonicalName,
			&match.EntityType, &match.Metadata, &match.CreatedAt, &match.RetiredAt, &alias,
		}
		if fixedScore < 0 {
			dest = append(dest, &score)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, mapError(err, op, "cannot scan alias match")
		}
		if fixedScore >= 0 {
			score = fixedScore
		}
		out = append(out, domain.AliasMatch{Entity: match, Alias: alias, Similarity: score})
	}
	return out, mapError(rows.Err(), op, "cannot match aliases")
}

// CanonicalEntityID follows the merge chain to the identity that currently represents this
// one. An unmerged identity is its own canonical form.
func (s *Store) CanonicalEntityID(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) (domain.EntityID, error) {
	const op = "ledger.CanonicalEntityID"

	current := id
	for depth := 0; depth < maxRedirectDepth; depth++ {
		var next *string
		err := s.pool.QueryRow(ctx,
			`SELECT merged_into_id FROM entities WHERE workspace_id = $1 AND id = $2`,
			ws, current).Scan(&next)
		if err != nil {
			if isNoRows(err) {
				return "", domain.Errorf(domain.CodeNotFound, op, "entity not found")
			}
			return "", mapError(err, op, "cannot follow merge chain")
		}
		if next == nil {
			return current, nil
		}
		current = domain.EntityID(*next)
	}
	return "", domain.Errorf(domain.CodeInternal, op, "merge chain is too deep to follow")
}

// IdentityCluster returns every identity that resolves to the same thing: the canonical
// entity plus everything merged into it, transitively.
//
// Queries expand through this so a merge makes both identities' facts reachable without
// having rewritten any of them.
func (s *Store) IdentityCluster(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) ([]domain.EntityID, error) {
	const op = "ledger.IdentityCluster"

	canonical, err := s.CanonicalEntityID(ctx, ws, id)
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE cluster AS (
			SELECT id FROM entities WHERE workspace_id = $1 AND id = $2
			UNION
			SELECT e.id FROM entities e
			JOIN cluster c ON e.merged_into_id = c.id
			WHERE e.workspace_id = $1
		)
		SELECT id FROM cluster`, ws, canonical)
	if err != nil {
		return nil, mapError(err, op, "cannot expand identity cluster")
	}
	defer rows.Close()

	var out []domain.EntityID
	for rows.Next() {
		var member domain.EntityID
		if err := rows.Scan(&member); err != nil {
			return nil, mapError(err, op, "cannot scan cluster member")
		}
		out = append(out, member)
	}
	return out, mapError(rows.Err(), op, "cannot expand identity cluster")
}

// MergeEntities redirects one identity into another.
//
// Nothing is collapsed: the merged entity keeps its row, its aliases, and every assertion
// that referenced it. Only a pointer changes, which is exactly why the operation can be
// undone (AGENTS.md section 12.3).
func (s *Store) MergeEntities(ctx context.Context, ws domain.WorkspaceID, from, into domain.EntityID, decision domain.ResolutionDecision) (domain.ResolutionDecision, error) {
	const op = "ledger.MergeEntities"

	if from == into {
		return domain.ResolutionDecision{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"an entity cannot be merged into itself")
	}

	// Merging into something that is itself merged would build a chain for no reason;
	// point straight at the canonical identity.
	canonicalTarget, err := s.CanonicalEntityID(ctx, ws, into)
	if err != nil {
		return domain.ResolutionDecision{}, err
	}
	if canonicalTarget == from {
		return domain.ResolutionDecision{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"that merge would create a cycle")
	}

	var stored domain.ResolutionDecision
	err = s.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE entities SET merged_into_id = $3, merged_at = now()
			WHERE workspace_id = $1 AND id = $2 AND merged_into_id IS NULL`,
			ws, from, canonicalTarget)
		if err != nil {
			return mapError(err, op, "cannot merge entities")
		}
		if tag.RowsAffected() == 0 {
			return domain.Errorf(domain.CodeConflict, op,
				"entity not found or already merged")
		}

		decision.Method = domain.MethodHumanMerge
		decision.ChosenEntityID = canonicalTarget
		decision.PreviousEntityID = from
		decision.HumanOverride = true
		stored, err = s.recordDecisionTx(ctx, tx, decision)
		if err != nil {
			return err
		}
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID:  ws,
			GraphSpaceID: decision.GraphSpaceID,
			PrincipalID:  decision.ActorID,
			Action:       "entity.merge",
			TargetKind:   "entity",
			TargetID:     string(from),
			Outcome:      "allowed",
			Detail:       map[string]any{"merged_into": string(canonicalTarget), "reason": decision.Reason},
		})
	})
	if err != nil {
		return domain.ResolutionDecision{}, err
	}
	return stored, nil
}

// SplitEntity undoes a merge, restoring an identity to standing on its own.
//
// This is possible because the merge never destroyed anything: the assertions that were
// made about this identity still name it, so clearing the redirect is enough to separate
// them again (AGENTS.md section 12.3).
func (s *Store) SplitEntity(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID, decision domain.ResolutionDecision) (domain.ResolutionDecision, error) {
	const op = "ledger.SplitEntity"

	var stored domain.ResolutionDecision
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var previous *string
		err := tx.QueryRow(ctx, `
			UPDATE entities SET merged_into_id = NULL, merged_at = NULL
			WHERE workspace_id = $1 AND id = $2 AND merged_into_id IS NOT NULL
			RETURNING (SELECT merged_into_id FROM entities WHERE id = $2)`, ws, id).Scan(&previous)
		if err != nil {
			if isNoRows(err) {
				return domain.Errorf(domain.CodeNotFound, op, "entity not found or not merged")
			}
			return mapError(err, op, "cannot split entity")
		}

		// Mark the merge decisions this reverses, so the ledger shows both that the merge
		// happened and that it was undone.
		if _, err := tx.Exec(ctx, `
			UPDATE resolution_decisions SET reverted_at = now()
			WHERE workspace_id = $1 AND previous_entity_id = $2
			  AND method = 'human_merge' AND reverted_at IS NULL`, ws, id); err != nil {
			return mapError(err, op, "cannot mark the merge as reverted")
		}

		decision.Method = domain.MethodHumanSplit
		decision.ChosenEntityID = id
		decision.HumanOverride = true
		stored, err = s.recordDecisionTx(ctx, tx, decision)
		if err != nil {
			return err
		}
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID:  ws,
			GraphSpaceID: decision.GraphSpaceID,
			PrincipalID:  decision.ActorID,
			Action:       "entity.split",
			TargetKind:   "entity",
			TargetID:     string(id),
			Outcome:      "allowed",
			Detail:       map[string]any{"reason": decision.Reason},
		})
	})
	if err != nil {
		return domain.ResolutionDecision{}, err
	}
	return stored, nil
}

// RecordResolutionDecision stores a decision outside any wider transaction.
func (s *Store) RecordResolutionDecision(ctx context.Context, decision domain.ResolutionDecision) (domain.ResolutionDecision, error) {
	var stored domain.ResolutionDecision
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		stored, err = s.recordDecisionTx(ctx, tx, decision)
		return err
	})
	if err != nil {
		return domain.ResolutionDecision{}, err
	}
	return stored, nil
}

func (s *Store) recordDecisionTx(ctx context.Context, tx pgx.Tx, decision domain.ResolutionDecision) (domain.ResolutionDecision, error) {
	const op = "ledger.recordDecision"

	if domain.IsZero(decision.ID) {
		decision.ID = domain.ResolutionDecisionID(domain.NewUUIDString())
	}
	if decision.ResolverVersion < 1 {
		decision.ResolverVersion = 1
	}
	if err := decision.Validate(); err != nil {
		return domain.ResolutionDecision{}, err
	}

	if err := tx.QueryRow(ctx, `
		INSERT INTO resolution_decisions (id, workspace_id, graph_space_id, mention_text, mention_type,
		                                  method, chosen_entity_id, previous_entity_id, confidence,
		                                  resolver_version, features, human_override, actor_id, reason,
		                                  source_event_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		RETURNING created_at`,
		decision.ID, decision.WorkspaceID, decision.GraphSpaceID, decision.MentionText,
		decision.MentionType, decision.Method, nullableString(decision.ChosenEntityID),
		nullableString(decision.PreviousEntityID), decision.Confidence, decision.ResolverVersion,
		jsonMap(decision.Features), decision.HumanOverride, string(decision.ActorID),
		decision.Reason, nullableString(decision.SourceEventID),
	).Scan(&decision.CreatedAt); err != nil {
		return domain.ResolutionDecision{}, mapError(err, op, "cannot record resolution decision")
	}

	for _, candidate := range decision.Candidates {
		features, err := jsonValue(candidate.Features)
		if err != nil {
			return domain.ResolutionDecision{}, domain.Wrap(err, domain.CodeInternal, op,
				"cannot encode candidate features")
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO resolution_candidates (decision_id, entity_id, score, features)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (decision_id, entity_id) DO NOTHING`,
			decision.ID, candidate.EntityID, candidate.Score, features); err != nil {
			return domain.ResolutionDecision{}, mapError(err, op, "cannot record candidate")
		}
	}
	return decision, nil
}

const decisionColumns = `id, workspace_id, graph_space_id, mention_text, mention_type, method,
	chosen_entity_id, previous_entity_id, confidence, resolver_version, features, human_override,
	actor_id, reason, source_event_id, reverted_at, created_at`

// GetResolutionDecision loads one decision with the identities it considered.
func (s *Store) GetResolutionDecision(ctx context.Context, ws domain.WorkspaceID, id domain.ResolutionDecisionID) (domain.ResolutionDecision, error) {
	const op = "ledger.GetResolutionDecision"

	row := s.pool.QueryRow(ctx, `SELECT `+decisionColumns+` FROM resolution_decisions
		WHERE workspace_id = $1 AND id = $2`, ws, id)
	decision, err := scanDecision(row, op)
	if err != nil {
		return domain.ResolutionDecision{}, err
	}

	candidates, err := s.listCandidates(ctx, decision.ID)
	if err != nil {
		return domain.ResolutionDecision{}, err
	}
	decision.Candidates = candidates
	return decision, nil
}

// ListResolutionDecisions returns recent decisions, newest first.
func (s *Store) ListResolutionDecisions(ctx context.Context, scope domain.Scope, reviewOnly bool, limit int) ([]domain.ResolutionDecision, error) {
	const op = "ledger.ListResolutionDecisions"

	if limit <= 0 || limit > domain.MaxAssertionLimit {
		limit = domain.DefaultAssertionLimit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+decisionColumns+` FROM resolution_decisions
		WHERE workspace_id = $1
		  AND ($2 = '' OR graph_space_id = $2::uuid)
		  AND (NOT $3 OR method = 'ambiguous_kept_separate' OR human_override)
		ORDER BY created_at DESC LIMIT $4`,
		scope.WorkspaceID, string(scope.GraphSpaceID), reviewOnly, limit)
	if err != nil {
		return nil, mapError(err, op, "cannot list resolution decisions")
	}
	defer rows.Close()

	var out []domain.ResolutionDecision
	for rows.Next() {
		decision, err := scanDecision(rowAdapter{rows}, op)
		if err != nil {
			return nil, err
		}
		out = append(out, decision)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err, op, "cannot list resolution decisions")
	}

	for i := range out {
		candidates, err := s.listCandidates(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Candidates = candidates
	}
	return out, nil
}

func (s *Store) listCandidates(ctx context.Context, id domain.ResolutionDecisionID) ([]domain.ScoredCandidate, error) {
	const op = "ledger.listCandidates"

	rows, err := s.pool.Query(ctx, `
		SELECT c.entity_id, c.score, c.features, coalesce(e.canonical_name, '')
		FROM resolution_candidates c
		LEFT JOIN entities e ON e.id = c.entity_id
		WHERE c.decision_id = $1 ORDER BY c.score DESC`, id)
	if err != nil {
		return nil, mapError(err, op, "cannot list candidates")
	}
	defer rows.Close()

	var out []domain.ScoredCandidate
	for rows.Next() {
		var candidate domain.ScoredCandidate
		if err := rows.Scan(&candidate.EntityID, &candidate.Score, &candidate.Features,
			&candidate.Name); err != nil {
			return nil, mapError(err, op, "cannot scan candidate")
		}
		out = append(out, candidate)
	}
	return out, mapError(rows.Err(), op, "cannot list candidates")
}

func scanDecision(row pgx.Row, op string) (domain.ResolutionDecision, error) {
	var (
		d           domain.ResolutionDecision
		chosen      *string
		previous    *string
		sourceEvent *string
	)
	err := row.Scan(&d.ID, &d.WorkspaceID, &d.GraphSpaceID, &d.MentionText, &d.MentionType,
		&d.Method, &chosen, &previous, &d.Confidence, &d.ResolverVersion, &d.Features,
		&d.HumanOverride, &d.ActorID, &d.Reason, &sourceEvent, &d.RevertedAt, &d.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.ResolutionDecision{}, domain.Errorf(domain.CodeNotFound, op,
				"resolution decision not found")
		}
		return domain.ResolutionDecision{}, mapError(err, op, "cannot scan resolution decision")
	}
	if chosen != nil {
		d.ChosenEntityID = domain.EntityID(*chosen)
	}
	if previous != nil {
		d.PreviousEntityID = domain.EntityID(*previous)
	}
	if sourceEvent != nil {
		d.SourceEventID = domain.SourceEventID(*sourceEvent)
	}
	return d, nil
}

func sourceIDValue(id *domain.SourceID) domain.SourceID {
	if id == nil {
		return ""
	}
	return *id
}

// ListIdentifiersInNamespace returns every identity bound to a key in one namespace.
//
// The resolver uses this to find identities the source has already distinguished from the
// mention at hand: if a record carries key A and another identity carries key B in the same
// namespace, the source has said they are two records, whatever their names suggest.
func (s *Store) ListIdentifiersInNamespace(ctx context.Context, scope domain.Scope, kind domain.IdentifierKind, namespace string) ([]domain.EntityIdentifier, error) {
	const op = "ledger.ListIdentifiersInNamespace"

	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, graph_space_id, entity_id, kind, namespace, value, source_id, created_at
		FROM entity_identifiers
		WHERE workspace_id = $1 AND graph_space_id = $2 AND kind = $3 AND namespace = $4`,
		scope.WorkspaceID, scope.GraphSpaceID, kind, domain.NormalizeIdentifierValue(namespace))
	if err != nil {
		return nil, mapError(err, op, "cannot list identifiers in namespace")
	}
	defer rows.Close()

	var out []domain.EntityIdentifier
	for rows.Next() {
		var (
			item     domain.EntityIdentifier
			sourceID *string
		)
		if err := rows.Scan(&item.ID, &item.WorkspaceID, &item.GraphSpaceID, &item.EntityID,
			&item.Kind, &item.Namespace, &item.Value, &sourceID, &item.CreatedAt); err != nil {
			return nil, mapError(err, op, "cannot scan identifier")
		}
		if sourceID != nil {
			id := domain.SourceID(*sourceID)
			item.SourceID = &id
		}
		out = append(out, item)
	}
	return out, mapError(rows.Err(), op, "cannot list identifiers in namespace")
}
