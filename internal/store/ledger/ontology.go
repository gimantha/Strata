package ledger

import (
	"context"
	"encoding/json"

	"github.com/gimantha/strata/internal/domain"
)

// CreateOntologyVersion appends an immutable schema version.
//
// The version number is assigned here rather than by the caller, in the same statement that
// inserts the row, so two concurrent definitions cannot both believe they are version 4.
func (s *Store) CreateOntologyVersion(ctx context.Context, version domain.OntologyVersion) (domain.OntologyVersion, error) {
	const op = "ledger.CreateOntologyVersion"

	if err := version.Validate(); err != nil {
		return domain.OntologyVersion{}, err
	}
	if domain.IsZero(version.ID) {
		version.ID = domain.NewOntologyVersionID()
	}
	if version.Status == "" {
		version.Status = domain.OntologyActive
	}

	entityTypes, err := json.Marshal(orEmptyEntityTypes(version.EntityTypes))
	if err != nil {
		return domain.OntologyVersion{}, domain.Wrap(err, domain.CodeInternal, op,
			"cannot encode entity types")
	}
	predicates, err := json.Marshal(orEmptyConstraints(version.Predicates))
	if err != nil {
		return domain.OntologyVersion{}, domain.Wrap(err, domain.CodeInternal, op,
			"cannot encode predicate constraints")
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO ontology_versions
		    (id, workspace_id, version, name, notes, status, entity_types, predicates, created_by)
		VALUES ($1, $2,
		        (SELECT coalesce(max(version), 0) + 1 FROM ontology_versions WHERE workspace_id = $2),
		        $3, $4, $5, $6, $7, $8)
		RETURNING id, workspace_id, version, name, notes, status, entity_types, predicates,
		          created_at, created_by`,
		version.ID, version.WorkspaceID, version.Name, version.Notes, version.Status,
		entityTypes, predicates, string(version.CreatedBy.ID))

	created, err := scanOntologyVersion(row)
	if err != nil {
		return domain.OntologyVersion{}, mapError(err, op, "cannot create ontology version")
	}
	return created, nil
}

// GetOntologyVersion reads one version by id.
func (s *Store) GetOntologyVersion(ctx context.Context, ws domain.WorkspaceID, id domain.OntologyVersionID) (domain.OntologyVersion, error) {
	const op = "ledger.GetOntologyVersion"

	row := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, version, name, notes, status, entity_types, predicates,
		       created_at, created_by
		FROM ontology_versions WHERE workspace_id = $1 AND id = $2`, ws, id)

	version, err := scanOntologyVersion(row)
	if err != nil {
		return domain.OntologyVersion{}, mapError(err, op, "cannot load ontology version")
	}
	return version, nil
}

// LatestOntologyVersion returns the highest-numbered version for a workspace.
func (s *Store) LatestOntologyVersion(ctx context.Context, ws domain.WorkspaceID) (domain.OntologyVersion, error) {
	const op = "ledger.LatestOntologyVersion"

	row := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, version, name, notes, status, entity_types, predicates,
		       created_at, created_by
		FROM ontology_versions WHERE workspace_id = $1
		ORDER BY version DESC LIMIT 1`, ws)

	version, err := scanOntologyVersion(row)
	if err != nil {
		return domain.OntologyVersion{}, mapError(err, op, "cannot load the latest ontology version")
	}
	return version, nil
}

// ListOntologyVersions returns a workspace's schema history, newest first.
func (s *Store) ListOntologyVersions(ctx context.Context, ws domain.WorkspaceID, limit int) ([]domain.OntologyVersion, error) {
	const op = "ledger.ListOntologyVersions"

	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, version, name, notes, status, entity_types, predicates,
		       created_at, created_by
		FROM ontology_versions WHERE workspace_id = $1
		ORDER BY version DESC LIMIT $2`, ws, limit)
	if err != nil {
		return nil, mapError(err, op, "cannot list ontology versions")
	}
	defer rows.Close()

	var out []domain.OntologyVersion
	for rows.Next() {
		version, err := scanOntologyVersion(rows)
		if err != nil {
			return nil, mapError(err, op, "cannot scan ontology version")
		}
		out = append(out, version)
	}
	return out, mapError(rows.Err(), op, "cannot list ontology versions")
}

// SupersedeOntologyVersions marks every active version but one as superseded.
//
// Superseded rather than deleted: assertions name the version that validated them, and a
// claim whose schema cannot be looked up is a claim nobody can re-check.
func (s *Store) SupersedeOntologyVersions(ctx context.Context, ws domain.WorkspaceID, keep domain.OntologyVersionID) error {
	const op = "ledger.SupersedeOntologyVersions"

	_, err := s.pool.Exec(ctx, `
		UPDATE ontology_versions SET status = 'superseded'
		WHERE workspace_id = $1 AND id <> $2 AND status = 'active'`, ws, keep)
	return mapError(err, op, "cannot supersede ontology versions")
}

// BindGraphSpace sets a graph space's ontology mode and version.
func (s *Store) BindGraphSpace(ctx context.Context, ws domain.WorkspaceID, id domain.GraphSpaceID, mode domain.OntologyMode, version *domain.OntologyVersionID) error {
	const op = "ledger.BindGraphSpace"

	if mode == domain.OntologyGuided && version == nil {
		return domain.Errorf(domain.CodeInvalidArgument, op,
			"guided mode requires an ontology version")
	}
	if mode == domain.OntologyOpen {
		// Open mode keeps no version: leaving a stale one behind would make a later
		// switch back to guided silently adopt whatever was bound months ago.
		version = nil
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE graph_spaces SET ontology_mode = $3, ontology_version_id = $4, updated_at = now()
		WHERE workspace_id = $1 AND id = $2`, ws, id, mode, version)
	if err != nil {
		return mapError(err, op, "cannot bind the graph space")
	}
	if tag.RowsAffected() == 0 {
		return domain.Errorf(domain.CodeGraphSpaceNotFound, op, "graph space %s not found", id)
	}
	return nil
}

// GraphSpaceBinding resolves a graph space's mode and, when guided, its version.
//
// One query rather than two: this runs on the commit path for every claim, and an extra
// round trip per assertion is a cost paid on the busiest path in the system.
func (s *Store) GraphSpaceBinding(ctx context.Context, ws domain.WorkspaceID, id domain.GraphSpaceID) (domain.OntologyBinding, error) {
	const op = "ledger.GraphSpaceBinding"

	var (
		mode       domain.OntologyMode
		version    domain.OntologyVersion
		hasVersion bool
	)
	row := s.pool.QueryRow(ctx, `
		SELECT gs.ontology_mode,
		       ov.id IS NOT NULL,
		       coalesce(ov.id::text, ''), coalesce(ov.workspace_id::text, ''),
		       coalesce(ov.version, 0), coalesce(ov.name, ''), coalesce(ov.notes, ''),
		       coalesce(ov.status, ''), coalesce(ov.entity_types, '[]'::jsonb),
		       coalesce(ov.predicates, '[]'::jsonb), coalesce(ov.created_by, '')
		FROM graph_spaces gs
		LEFT JOIN ontology_versions ov ON ov.id = gs.ontology_version_id
		WHERE gs.workspace_id = $1 AND gs.id = $2`, ws, id)

	var entityTypes, predicates []byte
	var versionID, versionWorkspace, status string
	if err := row.Scan(&mode, &hasVersion, &versionID, &versionWorkspace, &version.Version,
		&version.Name, &version.Notes, &status, &entityTypes, &predicates,
		&version.CreatedBy.ID); err != nil {
		return domain.OntologyBinding{}, mapError(err, op, "cannot resolve the graph space binding")
	}

	binding := domain.OntologyBinding{Mode: mode}
	if !hasVersion {
		return binding, nil
	}

	version.ID = domain.OntologyVersionID(versionID)
	version.WorkspaceID = domain.WorkspaceID(versionWorkspace)
	version.Status = domain.OntologyStatus(status)
	if err := json.Unmarshal(entityTypes, &version.EntityTypes); err != nil {
		return domain.OntologyBinding{}, domain.Wrap(err, domain.CodeInternal, op,
			"cannot decode entity types")
	}
	if err := json.Unmarshal(predicates, &version.Predicates); err != nil {
		return domain.OntologyBinding{}, domain.Wrap(err, domain.CodeInternal, op,
			"cannot decode predicate constraints")
	}
	binding.Version = &version
	return binding, nil
}

// scanner is anything a row or a row set can be read through.
type scanner interface {
	Scan(dest ...any) error
}

func scanOntologyVersion(row scanner) (domain.OntologyVersion, error) {
	var (
		version                 domain.OntologyVersion
		entityTypes, predicates []byte
	)
	if err := row.Scan(&version.ID, &version.WorkspaceID, &version.Version, &version.Name,
		&version.Notes, &version.Status, &entityTypes, &predicates,
		&version.CreatedAt, &version.CreatedBy.ID); err != nil {
		return domain.OntologyVersion{}, err
	}
	if err := json.Unmarshal(entityTypes, &version.EntityTypes); err != nil {
		return domain.OntologyVersion{}, err
	}
	if err := json.Unmarshal(predicates, &version.Predicates); err != nil {
		return domain.OntologyVersion{}, err
	}
	return version, nil
}

// orEmptyEntityTypes keeps a nil slice out of a NOT NULL jsonb column.
func orEmptyEntityTypes(types []domain.EntityTypeDef) []domain.EntityTypeDef {
	if types == nil {
		return []domain.EntityTypeDef{}
	}
	return types
}

func orEmptyConstraints(constraints []domain.PredicateConstraint) []domain.PredicateConstraint {
	if constraints == nil {
		return []domain.PredicateConstraint{}
	}
	return constraints
}
