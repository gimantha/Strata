package ledger

import (
	"context"
	"strings"

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

	// A name that is new only by an auxiliary differs from a registered predicate by
	// grammar, not meaning: HAS_APPROVED_BUDGET and APPROVED_BUDGET, DELIVERED_PROTOTYPE and
	// DELIVERED_PROTOTYPE_FOR. Open-mode extraction produces both spellings for the same
	// relation across a corpus, and every fact then splits across two vocabularies that no
	// query asks for together. The spelling is recorded as an alias of the existing
	// predicate, so the resolution is visible and reversible in the registry.
	for _, variant := range predicateSpellingVariants(normalized) {
		existing, err := s.GetPredicateByName(ctx, ws, variant)
		if domain.IsCode(err, domain.CodeNotFound) {
			continue
		}
		if err != nil {
			return domain.PredicateDefinition{}, err
		}
		if err := s.recordPredicateAlias(ctx, ws, existing.ID, name, normalized); err != nil {
			return domain.PredicateDefinition{}, err
		}
		existing.Aliases = append(existing.Aliases, name)
		return existing, nil
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

	normalized := domain.NormalizePredicateName(name)
	row := s.pool.QueryRow(ctx, `SELECT `+predicateColumns+` FROM predicates
		WHERE workspace_id = $1 AND name = $2`, ws, normalized)
	def, err := scanPredicate(row, op)
	if err != nil && domain.IsCode(err, domain.CodeNotFound) {
		row = s.pool.QueryRow(ctx, `SELECT `+prefixedPredicateColumns("p")+`
			FROM predicate_aliases pa JOIN predicates p ON p.id = pa.predicate_id
			WHERE pa.workspace_id = $1 AND pa.normalized = $2`, ws, normalized)
		def, err = scanPredicate(row, op)
	}
	if err != nil {
		return domain.PredicateDefinition{}, err
	}
	def.Aliases, err = s.listPredicateAliases(ctx, ws, def.ID)
	return def, err
}

// GetPredicate loads a registry entry by identifier.
func (s *Store) GetPredicate(ctx context.Context, ws domain.WorkspaceID, id domain.PredicateID) (domain.PredicateDefinition, error) {
	const op = "ledger.GetPredicate"

	row := s.pool.QueryRow(ctx, `SELECT `+predicateColumns+` FROM predicates
		WHERE workspace_id = $1 AND id = $2`, ws, id)
	def, err := scanPredicate(row, op)
	if err != nil {
		return domain.PredicateDefinition{}, err
	}
	def.Aliases, err = s.listPredicateAliases(ctx, ws, def.ID)
	return def, err
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
		for _, alias := range def.Aliases {
			normalized := domain.NormalizePredicateName(alias)
			if normalized == "" || normalized == stored.Name {
				continue
			}
			var owner domain.PredicateID
			err := tx.QueryRow(ctx, `
					INSERT INTO predicate_aliases
					    (id, workspace_id, predicate_id, alias, normalized, confidence, created_by)
					VALUES ($1,$2,$3,$4,$5,1,$6)
					ON CONFLICT (workspace_id, normalized) DO UPDATE SET alias = EXCLUDED.alias
					WHERE predicate_aliases.predicate_id = EXCLUDED.predicate_id
					RETURNING predicate_id`, domain.NewUUIDString(), def.WorkspaceID, stored.ID,
				alias, normalized, actor).Scan(&owner)
			if err != nil {
				if isNoRows(err) {
					return domain.Errorf(domain.CodeConflict, op,
						"predicate alias %q already belongs to another predicate", alias)
				}
				return mapError(err, op, "cannot define predicate alias")
			}
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
	stored.Aliases, err = s.listPredicateAliases(ctx, def.WorkspaceID, stored.ID)
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

	var out []domain.PredicateDefinition
	for rows.Next() {
		def, err := scanPredicateRows(rows, op)
		if err != nil {
			return nil, err
		}
		out = append(out, def)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, mapError(err, op, "cannot list predicates")
	}
	rows.Close()
	aliases, err := s.allPredicateAliases(ctx, ws)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Aliases = aliases[out[i].ID]
	}
	return out, nil
}

func prefixedPredicateColumns(alias string) string {
	parts := []string{"id", "workspace_id", "name", "description", "subject_types", "object_types",
		"object_kinds", "functional", "inverse_functional", "is_symmetric", "transitive", "temporal_policy",
		"conflict_policy", "default_memory_kind", "sensitivity", "status", "version", "created_at", "updated_at"}
	for i := range parts {
		parts[i] = alias + "." + parts[i]
	}
	return strings.Join(parts, ", ")
}

func (s *Store) listPredicateAliases(ctx context.Context, ws domain.WorkspaceID, id domain.PredicateID) ([]string, error) {
	const op = "ledger.listPredicateAliases"
	rows, err := s.pool.Query(ctx, `SELECT alias FROM predicate_aliases
		WHERE workspace_id = $1 AND predicate_id = $2 ORDER BY normalized`, ws, id)
	if err != nil {
		return nil, mapError(err, op, "cannot list predicate aliases")
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return nil, mapError(err, op, "cannot scan predicate alias")
		}
		out = append(out, alias)
	}
	return out, mapError(rows.Err(), op, "cannot list predicate aliases")
}

func (s *Store) allPredicateAliases(ctx context.Context, ws domain.WorkspaceID) (map[domain.PredicateID][]string, error) {
	const op = "ledger.allPredicateAliases"
	rows, err := s.pool.Query(ctx, `SELECT predicate_id, alias FROM predicate_aliases
		WHERE workspace_id = $1 ORDER BY predicate_id, normalized`, ws)
	if err != nil {
		return nil, mapError(err, op, "cannot list predicate aliases")
	}
	defer rows.Close()
	out := map[domain.PredicateID][]string{}
	for rows.Next() {
		var id domain.PredicateID
		var alias string
		if err := rows.Scan(&id, &alias); err != nil {
			return nil, mapError(err, op, "cannot scan predicate alias")
		}
		out[id] = append(out[id], alias)
	}
	return out, mapError(rows.Err(), op, "cannot list predicate aliases")
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

// predicateSpellingVariants lists the names a normalized predicate may already be registered
// under: with an auxiliary prefix or a trailing preposition removed, or added.
func predicateSpellingVariants(name string) []string {
	var out []string
	add := func(value string) {
		if value == "" || value == name {
			return
		}
		for _, existing := range out {
			if existing == value {
				return
			}
		}
		out = append(out, value)
	}
	bare := name
	for _, prefix := range []string{"HAS_", "IS_", "WAS_"} {
		bare = strings.TrimPrefix(bare, prefix)
	}
	bare = strings.TrimSuffix(bare, "_FOR")
	add(bare)
	for _, prefix := range []string{"HAS_", "IS_"} {
		add(prefix + bare)
		add(prefix + bare + "_FOR")
	}
	add(bare + "_FOR")
	return out
}

// recordPredicateAlias maps an extraction spelling to a registered predicate.
func (s *Store) recordPredicateAlias(ctx context.Context, ws domain.WorkspaceID, id domain.PredicateID, alias, normalized string) error {
	const op = "ledger.recordPredicateAlias"
	_, err := s.pool.Exec(ctx, `
		INSERT INTO predicate_aliases
		    (id, workspace_id, predicate_id, alias, normalized, confidence, created_by)
		VALUES ($1, $2, $3, $4, $5, 1, $6)
		ON CONFLICT (workspace_id, normalized) DO NOTHING`,
		domain.NewUUIDString(), ws, id, alias, normalized, "system:predicate-spelling")
	return mapError(err, op, "cannot record predicate alias")
}
