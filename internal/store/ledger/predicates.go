package ledger

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/gimantha/strata/internal/domain"
)

const predicateColumns = `id, workspace_id, name, description, subject_types, object_types,
	object_kinds, functional, inverse_functional, is_symmetric, transitive, temporal_policy,
	conflict_policy, default_memory_kind, sensitivity, status, version, created_at, updated_at`

// EnsurePredicate returns the registry entry for a name, registering it as a candidate
// if it is new.
//
// This is open mode from AGENTS.md section 8: extraction may invent predicate names, and
// they are normalized and recorded rather than rejected. Ontology-guided mode in phase 9
// constrains this. Candidates get conservative semantics - non-functional, coexisting -
// because wrongly assuming a predicate is functional would make the reconciler invent
// contradictions that do not exist.
func (s *Store) EnsurePredicate(ctx context.Context, ws domain.WorkspaceID, name string) (domain.PredicateDefinition, error) {
	const op = "ledger.EnsurePredicate"

	normalized := domain.NormalizePredicateName(name)
	if normalized == "" {
		return domain.PredicateDefinition{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"predicate name %q normalizes to nothing usable", name)
	}

	if existing, err := s.GetPredicateByName(ctx, ws, normalized); err == nil {
		return existing, nil
	} else if !domain.IsCode(err, domain.CodeNotFound) {
		return domain.PredicateDefinition{}, err
	}

	candidate := domain.PredicateDefinition{
		ID:                domain.PredicateID(domain.NewUUIDString()),
		WorkspaceID:       ws,
		Name:              normalized,
		TemporalPolicy:    domain.TemporalPolicyStateful,
		ConflictPolicy:    domain.ConflictPolicyCoexist,
		DefaultMemoryKind: domain.MemorySemantic,
		Sensitivity:       domain.ClassificationInternal,
		Status:            domain.PredicateCandidate,
		Version:           1,
	}
	if err := candidate.Validate(); err != nil {
		return domain.PredicateDefinition{}, err
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO predicates (id, workspace_id, name, temporal_policy, conflict_policy,
		                        default_memory_kind, sensitivity, status, version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (workspace_id, name) DO UPDATE SET updated_at = now()
		RETURNING `+predicateColumns,
		candidate.ID, candidate.WorkspaceID, candidate.Name, candidate.TemporalPolicy,
		candidate.ConflictPolicy, candidate.DefaultMemoryKind, candidate.Sensitivity,
		candidate.Status, candidate.Version)

	return scanPredicate(row, op)
}

// GetPredicateByName loads a registry entry.
func (s *Store) GetPredicateByName(ctx context.Context, ws domain.WorkspaceID, name string) (domain.PredicateDefinition, error) {
	const op = "ledger.GetPredicateByName"

	row := s.pool.QueryRow(ctx, `SELECT `+predicateColumns+` FROM predicates
		WHERE workspace_id = $1 AND name = $2`, ws, domain.NormalizePredicateName(name))
	return scanPredicate(row, op)
}

// GetPredicate loads a registry entry by identifier.
func (s *Store) GetPredicate(ctx context.Context, ws domain.WorkspaceID, id domain.PredicateID) (domain.PredicateDefinition, error) {
	const op = "ledger.GetPredicate"

	row := s.pool.QueryRow(ctx, `SELECT `+predicateColumns+` FROM predicates
		WHERE workspace_id = $1 AND id = $2`, ws, id)
	return scanPredicate(row, op)
}

// DefinePredicate creates or updates a predicate's semantics.
//
// Changing semantics bumps the version. Assertions record the version they were
// validated under, so tightening a predicate later cannot silently reinterpret claims
// that were legal when they were made (AGENTS.md section 8).
func (s *Store) DefinePredicate(ctx context.Context, def domain.PredicateDefinition, actor domain.PrincipalID) (domain.PredicateDefinition, error) {
	const op = "ledger.DefinePredicate"

	def.Name = domain.NormalizePredicateName(def.Name)
	if def.Version < 1 {
		def.Version = 1
	}
	if def.Status == "" {
		def.Status = domain.PredicateApproved
	}
	if def.TemporalPolicy == "" {
		def.TemporalPolicy = domain.TemporalPolicyStateful
	}
	if def.ConflictPolicy == "" {
		def.ConflictPolicy = domain.ConflictPolicyCoexist
	}
	if def.DefaultMemoryKind == "" {
		def.DefaultMemoryKind = domain.MemorySemantic
	}
	if def.Sensitivity == "" {
		def.Sensitivity = domain.ClassificationInternal
	}
	if err := def.Validate(); err != nil {
		return domain.PredicateDefinition{}, err
	}
	if domain.IsZero(def.ID) {
		def.ID = domain.PredicateID(domain.NewUUIDString())
	}

	var stored domain.PredicateDefinition
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO predicates (id, workspace_id, name, description, subject_types, object_types,
			                        object_kinds, functional, inverse_functional, is_symmetric, transitive,
			                        temporal_policy, conflict_policy, default_memory_kind, sensitivity,
			                        status, version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
			ON CONFLICT (workspace_id, name) DO UPDATE
			SET description = EXCLUDED.description,
			    subject_types = EXCLUDED.subject_types,
			    object_types = EXCLUDED.object_types,
			    object_kinds = EXCLUDED.object_kinds,
			    functional = EXCLUDED.functional,
			    inverse_functional = EXCLUDED.inverse_functional,
			    is_symmetric = EXCLUDED.is_symmetric,
			    transitive = EXCLUDED.transitive,
			    temporal_policy = EXCLUDED.temporal_policy,
			    conflict_policy = EXCLUDED.conflict_policy,
			    default_memory_kind = EXCLUDED.default_memory_kind,
			    sensitivity = EXCLUDED.sensitivity,
			    status = EXCLUDED.status,
			    -- A semantic change is a new version of the definition.
			    version = CASE
			        WHEN predicates.functional IS DISTINCT FROM EXCLUDED.functional
			          OR predicates.conflict_policy IS DISTINCT FROM EXCLUDED.conflict_policy
			          OR predicates.temporal_policy IS DISTINCT FROM EXCLUDED.temporal_policy
			          OR predicates.object_kinds IS DISTINCT FROM EXCLUDED.object_kinds
			        THEN predicates.version + 1
			        ELSE predicates.version
			    END,
			    updated_at = now()
			RETURNING `+predicateColumns,
			def.ID, def.WorkspaceID, def.Name, def.Description,
			orEmptyStrings(def.SubjectTypes), orEmptyStrings(def.ObjectTypes),
			objectKindStrings(def.ObjectKinds), def.Functional, def.InverseFunctional, def.Symmetric,
			def.Transitive, def.TemporalPolicy, def.ConflictPolicy, def.DefaultMemoryKind,
			def.Sensitivity, def.Status, def.Version)

		var err error
		stored, err = scanPredicate(row, op)
		if err != nil {
			return err
		}
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID: def.WorkspaceID,
			PrincipalID: actor,
			Action:      "predicate.define",
			TargetKind:  "predicate",
			TargetID:    stored.Name,
			Outcome:     "allowed",
			Detail: map[string]any{
				"functional":      stored.Functional,
				"conflict_policy": string(stored.ConflictPolicy),
				"version":         stored.Version,
			},
		})
	})
	if err != nil {
		return domain.PredicateDefinition{}, err
	}
	return stored, nil
}

// ListPredicates returns the registry for a workspace.
func (s *Store) ListPredicates(ctx context.Context, ws domain.WorkspaceID) ([]domain.PredicateDefinition, error) {
	const op = "ledger.ListPredicates"

	rows, err := s.pool.Query(ctx, `SELECT `+predicateColumns+` FROM predicates
		WHERE workspace_id = $1 ORDER BY name`, ws)
	if err != nil {
		return nil, mapError(err, op, "cannot list predicates")
	}
	defer rows.Close()

	var out []domain.PredicateDefinition
	for rows.Next() {
		def, err := scanPredicateRows(rows, op)
		if err != nil {
			return nil, err
		}
		out = append(out, def)
	}
	return out, mapError(rows.Err(), op, "cannot list predicates")
}

func scanPredicate(row pgx.Row, op string) (domain.PredicateDefinition, error) {
	var (
		def         domain.PredicateDefinition
		objectKinds []string
	)
	err := row.Scan(&def.ID, &def.WorkspaceID, &def.Name, &def.Description, &def.SubjectTypes,
		&def.ObjectTypes, &objectKinds, &def.Functional, &def.InverseFunctional, &def.Symmetric,
		&def.Transitive, &def.TemporalPolicy, &def.ConflictPolicy, &def.DefaultMemoryKind,
		&def.Sensitivity, &def.Status, &def.Version, &def.CreatedAt, &def.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.PredicateDefinition{}, domain.Errorf(domain.CodeNotFound, op, "predicate not found")
		}
		return domain.PredicateDefinition{}, mapError(err, op, "cannot scan predicate")
	}
	def.ObjectKinds = parseObjectKinds(objectKinds)
	return def, nil
}

func scanPredicateRows(rows pgx.Rows, op string) (domain.PredicateDefinition, error) {
	return scanPredicate(rowAdapter{rows}, op)
}

// rowAdapter lets one scan function serve both QueryRow and Query results.
type rowAdapter struct{ rows pgx.Rows }

func (r rowAdapter) Scan(dest ...any) error { return r.rows.Scan(dest...) }

// orEmptyStrings converts a nil slice to an empty one. A nil Go slice encodes as SQL
// NULL, which a NOT NULL array column rejects.
func orEmptyStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func objectKindStrings(kinds []domain.ObjectKind) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}

func parseObjectKinds(raw []string) []domain.ObjectKind {
	if len(raw) == 0 {
		return nil
	}
	out := make([]domain.ObjectKind, 0, len(raw))
	for _, r := range raw {
		out = append(out, domain.ObjectKind(r))
	}
	return out
}
