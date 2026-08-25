package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gimantha/strata/internal/domain"
)

const assertionColumns = `id, workspace_id, graph_space_id, subject_id, predicate_id, predicate_name,
	predicate_version, object_kind, object_entity_id, object_text, object_integer, object_decimal,
	object_boolean, object_timestamp, object_date, object_duration_ns, object_geo_lat, object_geo_lon,
	object_json, object_key, memory_kind, scope_key, event_time, valid_from, valid_to, effective_from,
	effective_to, observed_at, recorded_at, superseded_at, source_time, source_commit_time,
	source_sequence, source_version, active_from, active_until, decay_starts_at, expires_at,
	confidence, confidence_breakdown, status, supersedes_id, conflict_set_id, provenance_mode,
	derivation_id, source_event_id, fingerprint, retracted_at, retraction_reason, classification,
	created_by_id, created_by_kind, created_by_name, created_at, ontology_version_id,
	quarantine_reason, deactivated_at, deactivation_reason`

// CommitKnowledge durably records entities, assertions, evidence, and derivations in one
// transaction (AGENTS.md section 27.1).
//
// Atomicity is the point: a claim, the evidence that supports it, and the supersession
// of whatever it corrects either all land or none do. A correction visible without its
// cause, or a claim visible without its evidence, would be a lie about provenance.
func (s *Store) CommitKnowledge(ctx context.Context, commit domain.KnowledgeCommit) (domain.KnowledgeCommitResult, error) {
	const op = "ledger.CommitKnowledge"

	if domain.IsZero(commit.Scope.WorkspaceID) || domain.IsZero(commit.Scope.GraphSpaceID) {
		return domain.KnowledgeCommitResult{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"scope was not resolved before committing knowledge")
	}
	if commit.SupersededAt.IsZero() {
		commit.SupersededAt = nowUTC()
	}

	var result domain.KnowledgeCommitResult
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		for _, entity := range commit.Entities {
			stored, err := s.insertEntityTx(ctx, tx, entity)
			if err != nil {
				return err
			}
			result.Entities = append(result.Entities, stored)
		}
		for _, alias := range commit.Aliases {
			if err := s.insertAliasTx(ctx, tx, alias); err != nil {
				return err
			}
		}
		for _, derivation := range commit.Derivations {
			if err := s.insertDerivationTx(ctx, tx, derivation); err != nil {
				return err
			}
		}

		for _, item := range commit.Assertions {
			stored, isNew, err := s.insertAssertionTx(ctx, tx, item.Assertion)
			if err != nil {
				return err
			}
			if !isNew {
				// A replay of the same claim from the same event. Its evidence and
				// supersessions were committed the first time, so there is nothing to do.
				result.Duplicates++
				result.Assertions = append(result.Assertions, stored)
				continue
			}

			for _, evidence := range item.Evidence {
				evidence.AssertionID = stored.ID
				evidence.WorkspaceID = stored.WorkspaceID
				if domain.IsZero(evidence.SourceEventID) {
					evidence.SourceEventID = stored.SourceEventID
				}
				if err := s.insertEvidenceTx(ctx, tx, evidence); err != nil {
					return err
				}
			}

			if len(item.SupersedesIDs) > 0 {
				superseded, err := s.supersedeTx(ctx, tx, stored.WorkspaceID, item.SupersedesIDs,
					commit.SupersededAt)
				if err != nil {
					return err
				}
				result.Superseded = append(result.Superseded, superseded...)
			}
			result.Assertions = append(result.Assertions, stored)
		}

		for _, work := range commit.Outbox {
			if err := s.insertOutboxTx(ctx, tx, work); err != nil {
				return err
			}
		}

		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID:  commit.Scope.WorkspaceID,
			GraphSpaceID: commit.Scope.GraphSpaceID,
			PrincipalID:  commit.Actor.ID,
			Action:       "knowledge.commit",
			TargetKind:   "source_event",
			TargetID:     string(commit.SourceEventID),
			Outcome:      "allowed",
			Detail: map[string]any{
				"assertions": len(commit.Assertions),
				"entities":   len(commit.Entities),
				"duplicates": result.Duplicates,
				"superseded": len(result.Superseded),
			},
		})
	})
	if err != nil {
		return domain.KnowledgeCommitResult{}, err
	}
	return result, nil
}

// insertAssertionTx writes one claim, returning whether it was new.
func (s *Store) insertAssertionTx(ctx context.Context, tx pgx.Tx, a domain.Assertion) (domain.Assertion, bool, error) {
	const op = "ledger.insertAssertion"

	if domain.IsZero(a.ID) {
		a.ID = domain.NewAssertionID()
	}
	if a.Fingerprint == "" {
		a.Fingerprint = a.ComputeFingerprint()
	}
	if err := a.Validate(); err != nil {
		return domain.Assertion{}, false, err
	}

	breakdown, err := marshalBreakdown(a.ConfidenceBreakdown)
	if err != nil {
		return domain.Assertion{}, false, domain.Wrap(err, domain.CodeInternal, op,
			"cannot encode confidence breakdown")
	}
	obj := a.Object

	row := tx.QueryRow(ctx, `
		INSERT INTO assertions (
			id, workspace_id, graph_space_id, subject_id, predicate_id, predicate_name, predicate_version,
			object_kind, object_entity_id, object_text, object_integer, object_decimal, object_boolean,
			object_timestamp, object_date, object_duration_ns, object_geo_lat, object_geo_lon, object_json,
			object_key, memory_kind, scope_key, event_time, valid_from, valid_to, effective_from,
			effective_to, observed_at, recorded_at, superseded_at, source_time, source_commit_time,
			source_sequence, source_version, active_from, active_until, decay_starts_at, expires_at,
			confidence, confidence_breakdown, status, supersedes_id, conflict_set_id, provenance_mode,
			derivation_id, source_event_id, fingerprint, retracted_at, retraction_reason, classification,
			created_by_id, created_by_kind, created_by_name, ontology_version_id, quarantine_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,
		        $25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41,$42,$43,$44,$45,$46,
		        $47,$48,$49,$50,$51,$52,$53,$54,$55)
		ON CONFLICT (workspace_id, fingerprint) DO NOTHING
		RETURNING `+assertionColumns,
		a.ID, a.WorkspaceID, a.GraphSpaceID, a.SubjectID, a.Predicate.ID, a.Predicate.Name,
		a.Predicate.Version,
		obj.Kind, nullableString(obj.EntityID), textOrNil(obj, domain.ObjectString, domain.ObjectURI, domain.ObjectSymbol),
		integerOrNil(obj), decimalOrNil(obj), booleanOrNil(obj), timestampOrNil(obj), dateOrNil(obj),
		durationOrNil(obj), latOrNil(obj), lonOrNil(obj), jsonOrNil(obj), obj.Key(),
		a.MemoryKind, a.ScopeKey,
		a.Temporal.EventTime, a.Temporal.ValidFrom, a.Temporal.ValidTo, a.Temporal.EffectiveFrom,
		a.Temporal.EffectiveTo, a.Temporal.ObservedAt, a.Temporal.RecordedAt, a.Temporal.SupersededAt,
		a.Temporal.SourceTime, a.Temporal.SourceCommitTime, a.Temporal.SourceSequence,
		a.Temporal.SourceVersion, a.Temporal.ActiveFrom, a.Temporal.ActiveUntil,
		a.Temporal.DecayStartsAt, a.Temporal.ExpiresAt,
		a.Confidence, breakdown, a.Status, assertionIDOrNil(a.SupersedesID),
		conflictIDOrNil(a.ConflictSetID), a.ProvenanceMode, derivationIDOrNil(a.DerivationID),
		a.SourceEventID, a.Fingerprint, a.RetractedAt, a.RetractionReason, a.Classification,
		string(a.CreatedBy.ID), string(a.CreatedBy.Kind), a.CreatedBy.DisplayName,
		ontologyVersionIDOrNil(a.OntologyVersionID), a.QuarantineReason)

	stored, err := scanAssertion(row, op)
	switch {
	case err == nil:
		return stored, true, nil
	case domain.IsCode(err, domain.CodeNotFound):
		// The fingerprint already exists: this claim was committed by an earlier run.
		existing, lookupErr := s.assertionByFingerprint(ctx, tx, a.WorkspaceID, a.Fingerprint)
		if lookupErr != nil {
			return domain.Assertion{}, false, lookupErr
		}
		return existing, false, nil
	default:
		return domain.Assertion{}, false, err
	}
}

func (s *Store) assertionByFingerprint(ctx context.Context, q rowScanner, ws domain.WorkspaceID, fingerprint string) (domain.Assertion, error) {
	const op = "ledger.assertionByFingerprint"

	row := q.QueryRow(ctx, `SELECT `+assertionColumns+` FROM assertions
		WHERE workspace_id = $1 AND fingerprint = $2`, ws, fingerprint)
	return scanAssertion(row, op)
}

// GetAssertion loads one claim, scoped to a workspace.
func (s *Store) GetAssertion(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) (domain.Assertion, error) {
	const op = "ledger.GetAssertion"

	row := s.pool.QueryRow(ctx, `SELECT `+assertionColumns+` FROM assertions
		WHERE workspace_id = $1 AND id = $2`, ws, id)
	return scanAssertion(row, op)
}

// supersedeTx marks claims as replaced by later knowledge.
//
// This changes knowledge time, not world validity: the old claim still describes what
// the system believed, and for how long it believed it (AGENTS.md section 14.3). The
// guard on superseded_at keeps the first supersession authoritative, so a re-run cannot
// move the instant at which belief changed.
func (s *Store) supersedeTx(ctx context.Context, tx pgx.Tx, ws domain.WorkspaceID, ids []domain.AssertionID, at time.Time) ([]domain.AssertionID, error) {
	const op = "ledger.supersede"

	raw := make([]string, len(ids))
	for i, id := range ids {
		raw[i] = string(id)
	}

	rows, err := tx.Query(ctx, `
		UPDATE assertions
		SET status = 'superseded', superseded_at = $3
		WHERE workspace_id = $1
		  AND id = ANY($2::uuid[])
		  AND superseded_at IS NULL
		  AND retracted_at IS NULL
		RETURNING id`, ws, raw, at.UTC())
	if err != nil {
		return nil, mapError(err, op, "cannot supersede assertions")
	}
	defer rows.Close()

	var superseded []domain.AssertionID
	for rows.Next() {
		var id domain.AssertionID
		if err := rows.Scan(&id); err != nil {
			return nil, mapError(err, op, "cannot scan superseded assertion")
		}
		superseded = append(superseded, id)
	}
	return superseded, mapError(rows.Err(), op, "cannot supersede assertions")
}

// RetractAssertion withdraws a claim without replacing it.
//
// Like supersession this is a knowledge-time event, so a query as of an earlier instant
// still sees the claim. Retraction says "we no longer assert this", not "this never
// happened" (AGENTS.md section 2.1).
func (s *Store) RetractAssertion(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID, at time.Time, reason string, actor domain.PrincipalID) (domain.Assertion, error) {
	const op = "ledger.RetractAssertion"

	var retracted domain.Assertion
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			UPDATE assertions
			SET status = 'retracted', retracted_at = $3, retraction_reason = $4
			WHERE workspace_id = $1 AND id = $2 AND retracted_at IS NULL
			RETURNING `+assertionColumns, ws, id, at.UTC(), reason)

		var err error
		retracted, err = scanAssertion(row, op)
		if err != nil {
			if domain.IsCode(err, domain.CodeNotFound) {
				return domain.Errorf(domain.CodeNotFound, op, "assertion not found or already retracted")
			}
			return err
		}
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID:  ws,
			GraphSpaceID: retracted.GraphSpaceID,
			PrincipalID:  actor,
			Action:       "assertion.retract",
			TargetKind:   "assertion",
			TargetID:     string(id),
			Outcome:      "allowed",
			Detail:       map[string]any{"reason": reason},
		})
	})
	if err != nil {
		return domain.Assertion{}, err
	}
	return retracted, nil
}

// SupersedeWithLink marks claims as replaced and records which claim replaced them.
//
// The link is written onto the superseding claim as part of the same transaction that
// closes the superseded ones' knowledge-time window. It is filled exactly once, when
// supersession happens, and never changed afterwards: like superseded_at, it is a
// knowledge-time fact about the claim rather than an edit to what the claim says.
func (s *Store) SupersedeWithLink(ctx context.Context, ws domain.WorkspaceID, superseding domain.AssertionID, superseded []domain.AssertionID, at time.Time) ([]domain.AssertionID, error) {
	const op = "ledger.SupersedeWithLink"

	if len(superseded) == 0 {
		return nil, nil
	}

	var closed []domain.AssertionID
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		closed, err = s.supersedeTx(ctx, tx, ws, superseded, at)
		if err != nil {
			return err
		}
		if len(closed) == 0 {
			return nil
		}
		// Only a single-predecessor supersession can be expressed as one link. When a
		// claim replaces several at once the relationship lives in their closed
		// knowledge-time windows, which is where a reader looks anyway.
		if len(closed) == 1 {
			if _, err := tx.Exec(ctx, `
				UPDATE assertions SET supersedes_id = $3
				WHERE workspace_id = $1 AND id = $2 AND supersedes_id IS NULL`,
				ws, superseding, closed[0]); err != nil {
				return mapError(err, op, "cannot link supersession")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return closed, nil
}

// SupersedeAssertions marks claims as replaced by later knowledge.
//
// The reconciler uses this for a late-arriving claim that the source has already moved past:
// it was superseded before anyone learned it, so its knowledge-time window opens and closes
// at the same instant.
func (s *Store) SupersedeAssertions(ctx context.Context, ws domain.WorkspaceID, ids []domain.AssertionID, at time.Time) ([]domain.AssertionID, error) {
	var superseded []domain.AssertionID
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		superseded, err = s.supersedeTx(ctx, tx, ws, ids, at)
		return err
	})
	if err != nil {
		return nil, err
	}
	return superseded, nil
}

// QueryAssertions answers the temporal questions from AGENTS.md section 7.3.
//
// The two filters that matter most are independent. valid_at asks what held in the world
// at an instant; known_at asks what the system believed at an instant, including claims
// it has since replaced. Combining them answers "what did we believe on April 10 about
// March 25", which a single-timestamp model cannot express at all.
func (s *Store) QueryAssertions(ctx context.Context, q domain.AssertionQuery) ([]domain.Assertion, error) {
	const op = "ledger.QueryAssertions"

	q = q.Normalize()
	if err := q.Validate(); err != nil {
		return nil, err
	}

	var (
		where []string
		args  []any
	)
	add := func(clause string, values ...any) {
		placeholders := make([]any, len(values))
		for i, v := range values {
			args = append(args, v)
			placeholders[i] = len(args)
		}
		switch len(placeholders) {
		case 0:
			where = append(where, clause)
		case 1:
			where = append(where, fmt.Sprintf(clause, placeholders[0]))
		default:
			where = append(where, fmt.Sprintf(clause, placeholders...))
		}
	}

	add("workspace_id = $%d", q.Scope.WorkspaceID)
	if !domain.IsZero(q.Scope.GraphSpaceID) {
		add("graph_space_id = $%d", q.Scope.GraphSpaceID)
	}
	if len(q.SubjectIDs) > 0 {
		add("subject_id = ANY($%d::uuid[])", idStrings(q.SubjectIDs))
	}
	if len(q.Predicates) > 0 {
		add("predicate_name = ANY($%d::text[])", q.Predicates)
	}
	if len(q.ObjectEntityIDs) > 0 {
		add("object_entity_id = ANY($%d::uuid[])", idStrings(q.ObjectEntityIDs))
	}
	if q.ObjectKey != "" {
		add("object_key = $%d", q.ObjectKey)
	}
	if q.ScopeKey != "" {
		add("scope_key = $%d", q.ScopeKey)
	}
	if len(q.MemoryKinds) > 0 {
		add("memory_kind = ANY($%d::text[])", enumStrings(q.MemoryKinds))
	}
	if !domain.IsZero(q.SourceEventID) {
		add("source_event_id = $%d", q.SourceEventID)
	}
	if len(q.Classifications) > 0 {
		add("classification = ANY($%d::text[])", enumStrings(q.Classifications))
	}
	if q.MinConfidence > 0 {
		add("confidence >= $%d", q.MinConfidence)
	}
	if len(q.ProvenanceModes) > 0 {
		add("provenance_mode = ANY($%d::text[])", enumStrings(q.ProvenanceModes))
	}

	// Source identity and trust live on the event that produced the claim, so both narrow
	// through it. EXISTS rather than a join, because a claim has exactly one source event
	// and a join would invite duplicate rows the moment that stops being obvious.
	if len(q.SourceIDs) > 0 {
		add(`EXISTS (SELECT 1 FROM source_events se
			WHERE se.id = assertions.source_event_id AND se.source_id = ANY($%d::uuid[]))`,
			idStrings(q.SourceIDs))
	}
	if levels := domain.TrustLevelsAtLeast(q.MinTrustLevel); len(levels) > 0 {
		add(`EXISTS (SELECT 1 FROM source_events se
			JOIN sources src ON src.id = se.source_id
			WHERE se.id = assertions.source_event_id
			  AND src.trust_level = ANY($%d::text[]))`, enumStrings(levels))
	}
	if q.ChangedSince != nil {
		clause, value := changedSinceClause(*q.ChangedSince)
		add(clause, value)
	}

	switch {
	case q.KnownAt != nil:
		// Knowledge-time reconstruction. Status is deliberately not filtered here: an
		// assertion that is superseded today was current belief then, and excluding it
		// would answer the wrong question.
		known := q.KnownAt.UTC()
		add("recorded_at <= $%d", known)
		add("(superseded_at IS NULL OR superseded_at > $%d)", known)
		add("(retracted_at IS NULL OR retracted_at > $%d)", known)
	case len(q.Statuses) > 0:
		add("status = ANY($%d::text[])", enumStrings(q.Statuses))
	case q.IncludeSuperseded:
		add("status <> 'retracted'")
	default:
		// Current belief. Disputed claims are included: a contested fact is still
		// believed, and hiding it would present uncertainty as settled.
		add("status IN ('active', 'disputed')")
	}

	if q.ValidAt != nil {
		at := q.ValidAt.UTC()
		add("(valid_from IS NULL OR valid_from <= $%d)", at)
		add("(valid_to IS NULL OR valid_to > $%d)", at)
	}
	if q.ValidBetween != nil {
		// Overlap, not containment: a claim spanning the whole range is relevant to it.
		add("(valid_to IS NULL OR valid_to > $%d)", q.ValidBetween.Start.UTC())
		add("(valid_from IS NULL OR valid_from < $%d)", q.ValidBetween.End.UTC())
	}
	if q.EventBetween != nil {
		add("event_time >= $%d", q.EventBetween.Start.UTC())
		add("event_time < $%d", q.EventBetween.End.UTC())
	}
	if q.ActiveAt != nil {
		at := q.ActiveAt.UTC()
		add("(active_from IS NULL OR active_from <= $%d)", at)
		add("(active_until IS NULL OR active_until > $%d)", at)
		add("(expires_at IS NULL OR expires_at > $%d)", at)
	}

	args = append(args, q.Limit, q.Offset)
	sql := `SELECT ` + assertionColumns + ` FROM assertions WHERE ` +
		strings.Join(where, " AND ") +
		fmt.Sprintf(` ORDER BY recorded_at DESC, id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args))

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError(err, op, "cannot query assertions")
	}
	defer rows.Close()

	var out []domain.Assertion
	for rows.Next() {
		a, err := scanAssertion(rowAdapter{rows}, op)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, mapError(rows.Err(), op, "cannot query assertions")
}

// FindOverlappingClaims returns believable claims about the same subject, predicate, and
// scope whose world validity overlaps a candidate's.
//
// This is the input to conflict detection: two claims can only contradict each other if
// they claim to hold at the same time (AGENTS.md section 14.1).
func (s *Store) FindOverlappingClaims(ctx context.Context, a domain.Assertion) ([]domain.Assertion, error) {
	const op = "ledger.FindOverlappingClaims"

	rows, err := s.pool.Query(ctx, `SELECT `+prefixColumns("a", assertionColumns)+`, se.source_id
		FROM assertions a
		JOIN source_events se ON se.id = a.source_event_id
		WHERE a.workspace_id = $1
		  AND a.graph_space_id = $2
		  AND a.subject_id = $3
		  AND a.predicate_name = $4
		  AND a.scope_key = $5
		  AND a.status IN ('active', 'disputed')
		  AND a.id <> $6
		  -- Half-open overlap: a claim that ends exactly when another begins is a clean
		  -- handover, not a contradiction.
		  AND (a.valid_to IS NULL OR $7::timestamptz IS NULL OR a.valid_to > $7)
		  AND (a.valid_from IS NULL OR $8::timestamptz IS NULL OR a.valid_from < $8)
		ORDER BY a.recorded_at`,
		a.WorkspaceID, a.GraphSpaceID, a.SubjectID, a.Predicate.Name, a.ScopeKey, a.ID,
		a.Temporal.ValidFrom, a.Temporal.ValidTo)
	if err != nil {
		return nil, mapError(err, op, "cannot find overlapping claims")
	}
	defer rows.Close()

	var out []domain.Assertion
	for rows.Next() {
		claim, sourceID, err := scanAssertionWithSource(rowAdapter{rows}, op)
		if err != nil {
			return nil, err
		}
		out = append(out, claim.WithSourceID(sourceID))
	}
	return out, mapError(rows.Err(), op, "cannot find overlapping claims")
}

func idStrings[T ~string](ids []T) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

func enumStrings[T ~string](values []T) []string { return idStrings(values) }

// scanAssertionWithSource scans an assertion followed by its source id.
func scanAssertionWithSource(row pgx.Row, op string) (domain.Assertion, domain.SourceID, error) {
	var sourceID domain.SourceID
	assertion, err := scanAssertionInto(row, op, &sourceID)
	return assertion, sourceID, err
}

func scanAssertion(row pgx.Row, op string) (domain.Assertion, error) {
	return scanAssertionInto(row, op)
}

func scanAssertionInto(row pgx.Row, op string, extra ...any) (domain.Assertion, error) {
	var (
		a              domain.Assertion
		objectEntityID *string
		objectText     *string
		objectInteger  *int64
		objectDecimal  *string
		objectBoolean  *bool
		objectTime     *time.Time
		objectDate     *time.Time
		objectDuration *int64
		objectLat      *float64
		objectLon      *float64
		objectJSON     []byte
		breakdown      []byte
		supersedesID   *string
		conflictSetID  *string
		derivationID   *string
		ontologyID     *string
	)

	dest := []any{&a.ID, &a.WorkspaceID, &a.GraphSpaceID, &a.SubjectID, &a.Predicate.ID,
		&a.Predicate.Name, &a.Predicate.Version,
		&a.Object.Kind, &objectEntityID, &objectText, &objectInteger, &objectDecimal, &objectBoolean,
		&objectTime, &objectDate, &objectDuration, &objectLat, &objectLon, &objectJSON, new(string),
		&a.MemoryKind, &a.ScopeKey,
		&a.Temporal.EventTime, &a.Temporal.ValidFrom, &a.Temporal.ValidTo, &a.Temporal.EffectiveFrom,
		&a.Temporal.EffectiveTo, &a.Temporal.ObservedAt, &a.Temporal.RecordedAt, &a.Temporal.SupersededAt,
		&a.Temporal.SourceTime, &a.Temporal.SourceCommitTime, &a.Temporal.SourceSequence,
		&a.Temporal.SourceVersion, &a.Temporal.ActiveFrom, &a.Temporal.ActiveUntil,
		&a.Temporal.DecayStartsAt, &a.Temporal.ExpiresAt,
		&a.Confidence, &breakdown, &a.Status, &supersedesID, &conflictSetID, &a.ProvenanceMode,
		&derivationID, &a.SourceEventID, &a.Fingerprint, &a.RetractedAt, &a.RetractionReason,
		&a.Classification, &a.CreatedBy.ID, &a.CreatedBy.Kind, &a.CreatedBy.DisplayName, &a.CreatedAt,
		&ontologyID, &a.QuarantineReason, &a.DeactivatedAt, &a.DeactivationReason}
	dest = append(dest, extra...)

	err := row.Scan(dest...)
	if err != nil {
		if isNoRows(err) {
			return domain.Assertion{}, domain.Errorf(domain.CodeNotFound, op, "assertion not found")
		}
		return domain.Assertion{}, mapError(err, op, "cannot scan assertion")
	}

	if ontologyID != nil {
		version := domain.OntologyVersionID(*ontologyID)
		a.OntologyVersionID = &version
	}

	// Rebuild the typed object from whichever column its kind uses.
	switch a.Object.Kind {
	case domain.ObjectEntity:
		if objectEntityID != nil {
			a.Object.EntityID = domain.EntityID(*objectEntityID)
		}
	case domain.ObjectString, domain.ObjectURI, domain.ObjectSymbol:
		if objectText != nil {
			a.Object.Text = *objectText
		}
	case domain.ObjectInteger:
		if objectInteger != nil {
			a.Object.Integer = *objectInteger
		}
	case domain.ObjectDecimal:
		if objectDecimal != nil {
			a.Object.Decimal = *objectDecimal
		}
	case domain.ObjectBoolean:
		if objectBoolean != nil {
			a.Object.Boolean = *objectBoolean
		}
	case domain.ObjectTimestamp:
		if objectTime != nil {
			a.Object.Timestamp = objectTime.UTC()
		}
	case domain.ObjectDate:
		if objectDate != nil {
			a.Object.Date = objectDate.UTC()
		}
	case domain.ObjectDuration:
		if objectDuration != nil {
			a.Object.Duration = time.Duration(*objectDuration)
		}
	case domain.ObjectGeo:
		if objectLat != nil {
			a.Object.Geo.Latitude = *objectLat
		}
		if objectLon != nil {
			a.Object.Geo.Longitude = *objectLon
		}
	case domain.ObjectJSON:
		if len(objectJSON) > 0 {
			a.Object.JSON = json.RawMessage(objectJSON)
		}
	}

	if len(breakdown) > 0 {
		var parsed domain.ConfidenceBreakdown
		if err := json.Unmarshal(breakdown, &parsed); err == nil {
			a.ConfidenceBreakdown = &parsed
		}
	}
	if supersedesID != nil {
		id := domain.AssertionID(*supersedesID)
		a.SupersedesID = &id
	}
	if conflictSetID != nil {
		id := domain.ConflictSetID(*conflictSetID)
		a.ConflictSetID = &id
	}
	if derivationID != nil {
		id := domain.DerivationID(*derivationID)
		a.DerivationID = &id
	}
	return a, nil
}

// Object column helpers. Each returns nil unless the object's kind uses that column, so
// a row only ever populates the one that matches.

func textOrNil(o domain.AssertionObject, kinds ...domain.ObjectKind) *string {
	for _, k := range kinds {
		if o.Kind == k {
			text := o.Text
			return &text
		}
	}
	return nil
}

func integerOrNil(o domain.AssertionObject) *int64 {
	if o.Kind != domain.ObjectInteger {
		return nil
	}
	v := o.Integer
	return &v
}

func decimalOrNil(o domain.AssertionObject) *string {
	if o.Kind != domain.ObjectDecimal {
		return nil
	}
	v := o.Decimal
	return &v
}

func booleanOrNil(o domain.AssertionObject) *bool {
	if o.Kind != domain.ObjectBoolean {
		return nil
	}
	v := o.Boolean
	return &v
}

func timestampOrNil(o domain.AssertionObject) *time.Time {
	if o.Kind != domain.ObjectTimestamp {
		return nil
	}
	v := o.Timestamp.UTC()
	return &v
}

func dateOrNil(o domain.AssertionObject) *time.Time {
	if o.Kind != domain.ObjectDate {
		return nil
	}
	v := o.Date.UTC()
	return &v
}

func durationOrNil(o domain.AssertionObject) *int64 {
	if o.Kind != domain.ObjectDuration {
		return nil
	}
	v := int64(o.Duration)
	return &v
}

func latOrNil(o domain.AssertionObject) *float64 {
	if o.Kind != domain.ObjectGeo {
		return nil
	}
	v := o.Geo.Latitude
	return &v
}

func lonOrNil(o domain.AssertionObject) *float64 {
	if o.Kind != domain.ObjectGeo {
		return nil
	}
	v := o.Geo.Longitude
	return &v
}

func jsonOrNil(o domain.AssertionObject) []byte {
	if o.Kind != domain.ObjectJSON {
		return nil
	}
	return o.JSON
}

func assertionIDOrNil(id *domain.AssertionID) *string {
	if id == nil {
		return nil
	}
	v := string(*id)
	return &v
}

func conflictIDOrNil(id *domain.ConflictSetID) *string {
	if id == nil {
		return nil
	}
	v := string(*id)
	return &v
}

func ontologyVersionIDOrNil(id *domain.OntologyVersionID) *string {
	if id == nil {
		return nil
	}
	v := string(*id)
	return &v
}

func derivationIDOrNil(id *domain.DerivationID) *string {
	if id == nil {
		return nil
	}
	v := string(*id)
	return &v
}

func marshalBreakdown(b *domain.ConfidenceBreakdown) ([]byte, error) {
	if b == nil {
		return nil, nil
	}
	return json.Marshal(b)
}

// ResolveAssertionWorkspace finds which of a principal's workspaces owns an assertion.
//
// It lets an endpoint accept a bare identifier while keeping the tenancy check intact:
// a claim in a workspace the caller has no grant for is reported as absent, so
// identifiers cannot be probed across tenants (AGENTS.md section 22.1).
func (s *Store) ResolveAssertionWorkspace(ctx context.Context, id domain.AssertionID, allowed []domain.WorkspaceID) (domain.WorkspaceID, error) {
	return s.resolveOwningWorkspace(ctx, "assertions", string(id), allowed, "assertion")
}

// ResolveEntityWorkspace does the same for an entity identifier.
func (s *Store) ResolveEntityWorkspace(ctx context.Context, id domain.EntityID, allowed []domain.WorkspaceID) (domain.WorkspaceID, error) {
	return s.resolveOwningWorkspace(ctx, "entities", string(id), allowed, "entity")
}

// ResolveConflictSetWorkspace does the same for a conflict set identifier.
func (s *Store) ResolveConflictSetWorkspace(ctx context.Context, id domain.ConflictSetID, allowed []domain.WorkspaceID) (domain.WorkspaceID, error) {
	return s.resolveOwningWorkspace(ctx, "conflict_sets", string(id), allowed, "conflict set")
}

// resolveOwningWorkspace looks up a row's workspace, restricted to the ones the caller
// may see. The table name is supplied only by this package, never by a request.
func (s *Store) resolveOwningWorkspace(ctx context.Context, table, id string, allowed []domain.WorkspaceID, label string) (domain.WorkspaceID, error) {
	const op = "ledger.resolveOwningWorkspace"

	if len(allowed) == 0 {
		return "", domain.Errorf(domain.CodeNotFound, op, "%s not found", label)
	}
	var found domain.WorkspaceID
	err := s.pool.QueryRow(ctx,
		`SELECT workspace_id FROM `+table+` WHERE id = $1 AND workspace_id = ANY($2::uuid[])`,
		id, idStrings(allowed)).Scan(&found)
	if err != nil {
		if isNoRows(err) {
			return "", domain.Errorf(domain.CodeNotFound, op, "%s not found", label)
		}
		return "", mapError(err, op, "cannot resolve owning workspace")
	}
	return found, nil
}

// SourceAuthority reports how far the source behind an event is trusted.
//
// Authority-weighted conflict resolution needs this: when two claims disagree, which source
// said it is often the only thing that can settle the matter without guessing
// (AGENTS.md sections 14.1, 24).
func (s *Store) SourceAuthority(ctx context.Context, ws domain.WorkspaceID, eventID domain.SourceEventID) (domain.TrustLevel, error) {
	const op = "ledger.SourceAuthority"

	var trust domain.TrustLevel
	err := s.pool.QueryRow(ctx, `
		SELECT sr.trust_level
		FROM source_events se
		JOIN sources sr ON sr.id = se.source_id
		WHERE se.workspace_id = $1 AND se.id = $2`, ws, eventID).Scan(&trust)
	if err != nil {
		if isNoRows(err) {
			return "", domain.Errorf(domain.CodeNotFound, op, "source event not found")
		}
		return "", mapError(err, op, "cannot read source authority")
	}
	return trust, nil
}

// DeactivateAssertion takes a claim out of active context without changing what it says
// (AGENTS.md sections 21.3, 21.4).
//
// This is soft forgetting, and it is deliberately not retraction. The claim's status,
// validity, evidence, and knowledge time are untouched: it remains true, remains cited, and
// remains answerable as of any instant. What changes is the context clock — active_until is
// closed at this moment — so retrieval stops surfacing it as current.
//
// Reversible on purpose. An operation people are afraid to use because it might be permanent
// is an operation they will avoid in favour of something worse.
func (s *Store) DeactivateAssertion(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID, at time.Time, reason string, actor domain.PrincipalID) (domain.Assertion, error) {
	const op = "ledger.DeactivateAssertion"

	if reason == "" {
		return domain.Assertion{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"a reason is required")
	}

	row := s.pool.QueryRow(ctx, `
		UPDATE assertions
		SET active_until = $3,
		    expires_at = coalesce(expires_at, $3),
		    deactivated_at = $3,
		    deactivation_reason = $4
		WHERE workspace_id = $1 AND id = $2 AND deactivated_at IS NULL
		RETURNING `+assertionColumns,
		ws, id, at.UTC(), reason)

	assertion, err := scanAssertion(row, op)
	if err != nil {
		if domain.IsCode(err, domain.CodeNotFound) {
			// Either it does not exist here or it is already deactivated. Distinguish the
			// two, because "already done" is a success from the caller's point of view.
			existing, lookupErr := s.GetAssertion(ctx, ws, id)
			if lookupErr != nil {
				return domain.Assertion{}, lookupErr
			}
			return existing, nil
		}
		return domain.Assertion{}, err
	}

	_ = s.AppendAudit(ctx, AuditEntry{
		WorkspaceID:  ws,
		GraphSpaceID: assertion.GraphSpaceID,
		PrincipalID:  actor,
		Action:       "memory.deactivate",
		TargetKind:   "assertion",
		TargetID:     string(id),
		Outcome:      "deactivated",
		Detail:       map[string]any{"reason": reason},
	})
	return assertion, nil
}

// ReactivateAssertion puts deactivated knowledge back in scope.
//
// Clears the context clock this system set, and only that. A claim whose expiry came from the
// source rather than from a deactivation keeps it: reactivating must not extend a lifetime
// somebody else decided.
func (s *Store) ReactivateAssertion(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID, actor domain.PrincipalID) (domain.Assertion, error) {
	const op = "ledger.ReactivateAssertion"

	row := s.pool.QueryRow(ctx, `
		UPDATE assertions
		SET active_until = NULL,
		    expires_at = CASE WHEN expires_at = deactivated_at THEN NULL ELSE expires_at END,
		    deactivated_at = NULL,
		    deactivation_reason = ''
		WHERE workspace_id = $1 AND id = $2 AND deactivated_at IS NOT NULL
		RETURNING `+assertionColumns,
		ws, id)

	assertion, err := scanAssertion(row, op)
	if err != nil {
		if domain.IsCode(err, domain.CodeNotFound) {
			existing, lookupErr := s.GetAssertion(ctx, ws, id)
			if lookupErr != nil {
				return domain.Assertion{}, lookupErr
			}
			return existing, nil
		}
		return domain.Assertion{}, err
	}

	_ = s.AppendAudit(ctx, AuditEntry{
		WorkspaceID:  ws,
		GraphSpaceID: assertion.GraphSpaceID,
		PrincipalID:  actor,
		Action:       "memory.reactivate",
		TargetKind:   "assertion",
		TargetID:     string(id),
		Outcome:      "reactivated",
	})
	return assertion, nil
}

// changedSinceClause renders a source-order cursor as SQL.
//
// The precedence mirrors domain.CompareSourcePosition exactly — sequence, then version,
// then commit time, then source time — because a cursor that ordered claims differently
// from the reconciler would hand a consumer a "change" the system does not consider newer.
//
// Numeric comparison when both sides are integers, lexicographic otherwise: "9" precedes
// "10" in every source that emits counters and follows it in string order, while LSNs and
// ULIDs are designed to sort as text. This is the SQL counterpart of
// domain.compareSequenceStrings, and the two are tested against the same expectations.
func changedSinceClause(pos domain.SourcePosition) (string, any) {
	const numeric = `^[0-9]+$`

	// The placeholder is written as an indexed verb so one bound value can be referenced
	// three times: once to test whether it is numeric, then in each branch.
	marker := func(column, value string) (string, any) {
		return fmt.Sprintf(`(assertions.%[1]s <> ''
			AND CASE
				WHEN assertions.%[1]s ~ '%[2]s' AND $%%[1]d ~ '%[2]s'
					THEN assertions.%[1]s::numeric > $%%[1]d::numeric
				ELSE assertions.%[1]s > $%%[1]d
			END)`, column, numeric), value
	}

	switch {
	case pos.Sequence != "":
		return marker("source_sequence", pos.Sequence)
	case pos.Version != "":
		return marker("source_version", pos.Version)
	case pos.CommitTime != nil:
		return `assertions.source_commit_time > $%d`, pos.CommitTime.UTC()
	default:
		return `assertions.source_time > $%d`, pos.SourceTime.UTC()
	}
}
