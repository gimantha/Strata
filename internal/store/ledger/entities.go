package ledger

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gimantha/strata/internal/domain"
)

const entityColumns = `id, workspace_id, graph_space_id, canonical_name, entity_type,
	metadata, created_at, retired_at`

// CreateEntity records a new identity.
func (s *Store) CreateEntity(ctx context.Context, e domain.Entity) (domain.Entity, error) {
	const op = "ledger.CreateEntity"

	if err := e.Validate(); err != nil {
		return domain.Entity{}, err
	}
	if domain.IsZero(e.ID) {
		e.ID = domain.NewEntityID()
	}

	var stored domain.Entity
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		stored, err = s.insertEntityTx(ctx, tx, e)
		return err
	})
	if err != nil {
		return domain.Entity{}, err
	}
	return stored, nil
}

// insertEntityTx inserts an entity and its canonical name as an alias, so lookups by
// any known name go through one path.
func (s *Store) insertEntityTx(ctx context.Context, tx pgx.Tx, e domain.Entity) (domain.Entity, error) {
	const op = "ledger.insertEntity"

	row := tx.QueryRow(ctx, `
		INSERT INTO entities (id, workspace_id, graph_space_id, canonical_name, entity_type, metadata, retired_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET metadata = entities.metadata
		RETURNING `+entityColumns,
		e.ID, e.WorkspaceID, e.GraphSpaceID, e.CanonicalName, e.EntityType, jsonMap(e.Metadata), e.RetiredAt)

	stored, err := scanEntity(row, op)
	if err != nil {
		return domain.Entity{}, err
	}

	if err := s.insertAliasTx(ctx, tx, domain.EntityAlias{
		EntityID:    stored.ID,
		WorkspaceID: stored.WorkspaceID,
		Alias:       stored.CanonicalName,
		Normalized:  domain.NormalizeAlias(stored.CanonicalName),
		Confidence:  1,
	}); err != nil {
		return domain.Entity{}, err
	}
	return stored, nil
}

// GetEntity loads an identity within a workspace.
func (s *Store) GetEntity(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) (domain.Entity, error) {
	const op = "ledger.GetEntity"

	row := s.pool.QueryRow(ctx, `SELECT `+entityColumns+` FROM entities
		WHERE workspace_id = $1 AND id = $2`, ws, id)
	return scanEntity(row, op)
}

// FindEntitiesByName resolves a name through the alias table.
//
// It returns every match rather than picking one: deciding that two identities with the
// same name are the same thing is entity resolution's job in phase 4, and guessing here
// would silently merge distinct people.
func (s *Store) FindEntitiesByName(ctx context.Context, scope domain.Scope, name string) ([]domain.Entity, error) {
	const op = "ledger.FindEntitiesByName"

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT `+prefixColumns("e", entityColumns)+`
		FROM entities e
		JOIN entity_aliases a ON a.entity_id = e.id
		WHERE e.workspace_id = $1
		  AND ($2 = '' OR e.graph_space_id = $2::uuid)
		  AND a.normalized = $3
		  AND e.retired_at IS NULL
		ORDER BY e.created_at`,
		scope.WorkspaceID, string(scope.GraphSpaceID), domain.NormalizeAlias(name))
	if err != nil {
		return nil, mapError(err, op, "cannot find entities by name")
	}
	defer rows.Close()

	var out []domain.Entity
	for rows.Next() {
		e, err := scanEntity(rowAdapter{rows}, op)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, mapError(rows.Err(), op, "cannot find entities by name")
}

// ListEntities returns identities in a graph space, optionally filtered by type.
func (s *Store) ListEntities(ctx context.Context, scope domain.Scope, entityType string, limit int) ([]domain.Entity, error) {
	const op = "ledger.ListEntities"

	if limit <= 0 || limit > domain.MaxAssertionLimit {
		limit = domain.DefaultAssertionLimit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+entityColumns+` FROM entities
		WHERE workspace_id = $1
		  AND ($2 = '' OR graph_space_id = $2::uuid)
		  AND ($3 = '' OR entity_type = $3)
		ORDER BY created_at DESC LIMIT $4`,
		scope.WorkspaceID, string(scope.GraphSpaceID), entityType, limit)
	if err != nil {
		return nil, mapError(err, op, "cannot list entities")
	}
	defer rows.Close()

	var out []domain.Entity
	for rows.Next() {
		e, err := scanEntity(rowAdapter{rows}, op)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, mapError(rows.Err(), op, "cannot list entities")
}

// RetireEntity marks an identity as no longer in use.
//
// It is not deleted: assertions that referenced it stay valid and walkable, because
// history that mentions an identity does not stop having happened.
func (s *Store) RetireEntity(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID, at time.Time, actor domain.PrincipalID) error {
	const op = "ledger.RetireEntity"

	return s.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE entities SET retired_at = $3
			WHERE workspace_id = $1 AND id = $2 AND retired_at IS NULL`, ws, id, at.UTC())
		if err != nil {
			return mapError(err, op, "cannot retire entity")
		}
		if tag.RowsAffected() == 0 {
			return domain.Errorf(domain.CodeNotFound, op, "entity not found or already retired")
		}
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID: ws,
			PrincipalID: actor,
			Action:      "entity.retire",
			TargetKind:  "entity",
			TargetID:    string(id),
			Outcome:     "allowed",
		})
	})
}

// AddAlias records another name for an identity.
func (s *Store) AddAlias(ctx context.Context, alias domain.EntityAlias) error {
	const op = "ledger.AddAlias"

	if alias.Normalized == "" {
		alias.Normalized = domain.NormalizeAlias(alias.Alias)
	}
	if err := alias.Validate(); err != nil {
		return err
	}
	_ = op
	return s.InTx(ctx, func(tx pgx.Tx) error {
		return s.insertAliasTx(ctx, tx, alias)
	})
}

func (s *Store) insertAliasTx(ctx context.Context, tx pgx.Tx, alias domain.EntityAlias) error {
	const op = "ledger.insertAlias"

	if alias.Normalized == "" {
		alias.Normalized = domain.NormalizeAlias(alias.Alias)
	}
	if alias.Confidence == 0 {
		alias.Confidence = 1
	}
	if alias.ID == "" {
		alias.ID = domain.NewUUIDString()
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO entity_aliases (id, workspace_id, entity_id, alias, normalized, source_id, confidence)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (entity_id, normalized) DO NOTHING`,
		alias.ID, alias.WorkspaceID, alias.EntityID, alias.Alias, alias.Normalized,
		alias.SourceID, alias.Confidence)
	return mapError(err, op, "cannot insert alias")
}

// ListAliases returns the known names for an identity.
func (s *Store) ListAliases(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) ([]domain.EntityAlias, error) {
	const op = "ledger.ListAliases"

	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, entity_id, alias, normalized, source_id, confidence, created_at
		FROM entity_aliases WHERE workspace_id = $1 AND entity_id = $2 ORDER BY created_at`, ws, id)
	if err != nil {
		return nil, mapError(err, op, "cannot list aliases")
	}
	defer rows.Close()

	var out []domain.EntityAlias
	for rows.Next() {
		var a domain.EntityAlias
		if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.EntityID, &a.Alias, &a.Normalized,
			&a.SourceID, &a.Confidence, &a.CreatedAt); err != nil {
			return nil, mapError(err, op, "cannot scan alias")
		}
		out = append(out, a)
	}
	return out, mapError(rows.Err(), op, "cannot list aliases")
}

func scanEntity(row pgx.Row, op string) (domain.Entity, error) {
	var e domain.Entity
	err := row.Scan(&e.ID, &e.WorkspaceID, &e.GraphSpaceID, &e.CanonicalName, &e.EntityType,
		&e.Metadata, &e.CreatedAt, &e.RetiredAt)
	if err != nil {
		if isNoRows(err) {
			return domain.Entity{}, domain.Errorf(domain.CodeNotFound, op, "entity not found")
		}
		return domain.Entity{}, mapError(err, op, "cannot scan entity")
	}
	return e, nil
}

// prefixColumns qualifies a column list with a table alias for joined queries.
func prefixColumns(alias, columns string) string {
	var out []byte
	field := make([]byte, 0, 32)
	flush := func() {
		if len(field) == 0 {
			return
		}
		out = append(out, alias...)
		out = append(out, '.')
		out = append(out, field...)
		field = field[:0]
	}
	for i := 0; i < len(columns); i++ {
		c := columns[i]
		switch c {
		case ',':
			flush()
			out = append(out, ',', ' ')
		case ' ', '\n', '\t':
			// separators between column names are normalized away
		default:
			field = append(field, c)
		}
	}
	flush()
	return string(out)
}

// GetEntities resolves several identities at once.
//
// Batched because the alternative is a lookup per graph hit, and a traversal returning fifty
// entities would then cost fifty round trips to name them. Scoped to a workspace, which is
// also what makes it safe for the graph port to seed a walk from caller-supplied ids: an id
// belonging to another tenant resolves to nothing here and is dropped before it reaches a
// caller (ADR 0021).
func (s *Store) GetEntities(ctx context.Context, ws domain.WorkspaceID,
	ids []domain.EntityID) (map[domain.EntityID]domain.Entity, error) {
	const op = "ledger.GetEntities"

	if len(ids) == 0 {
		return map[domain.EntityID]domain.Entity{}, nil
	}

	rows, err := s.pool.Query(ctx,
		`SELECT `+entityColumns+` FROM entities
		 WHERE workspace_id = $1 AND id = ANY($2::uuid[])`, ws, idStrings(ids))
	if err != nil {
		return nil, mapError(err, op, "cannot load entities")
	}
	defer rows.Close()

	out := make(map[domain.EntityID]domain.Entity, len(ids))
	for rows.Next() {
		entity, err := scanEntity(rowAdapter{rows}, op)
		if err != nil {
			return nil, err
		}
		out[entity.ID] = entity
	}
	return out, mapError(rows.Err(), op, "cannot load entities")
}
