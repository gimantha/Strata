package ledger

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gimantha/strata/internal/domain"
)

// CreateWorkspace inserts a workspace and grants its creator ownership in the same
// transaction, so no workspace can exist that nobody may administer.
func (s *Store) CreateWorkspace(ctx context.Context, ws domain.Workspace, creator domain.PrincipalID) (domain.Workspace, error) {
	const op = "ledger.CreateWorkspace"

	if err := ws.Validate(); err != nil {
		return domain.Workspace{}, err
	}
	if domain.IsZero(ws.ID) {
		ws.ID = domain.NewWorkspaceID()
	}

	err := s.InTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO workspaces (id, slug, name, metadata)
			VALUES ($1, $2, $3, $4)
			RETURNING created_at, updated_at`,
			ws.ID, ws.Slug, ws.Name, jsonMap(ws.Metadata))
		if err := row.Scan(&ws.CreatedAt, &ws.UpdatedAt); err != nil {
			if isUniqueViolation(err, "workspaces_slug_key") {
				return domain.Errorf(domain.CodeConflict, op, "workspace slug %q already exists", ws.Slug)
			}
			return mapError(err, op, "cannot insert workspace")
		}

		if creator != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO principal_workspaces (principal_id, workspace_id, role)
				VALUES ($1, $2, $3)
				ON CONFLICT (principal_id, workspace_id) DO UPDATE SET role = EXCLUDED.role`,
				creator, ws.ID, domain.RoleOwner); err != nil {
				return mapError(err, op, "cannot grant workspace ownership")
			}
		}

		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID: ws.ID,
			PrincipalID: creator,
			Action:      "workspace.create",
			TargetKind:  "workspace",
			TargetID:    string(ws.ID),
			Outcome:     "allowed",
			Detail:      map[string]any{"slug": ws.Slug},
		})
	})
	if err != nil {
		return domain.Workspace{}, err
	}
	return ws, nil
}

// GetWorkspace loads a workspace by identifier.
func (s *Store) GetWorkspace(ctx context.Context, id domain.WorkspaceID) (domain.Workspace, error) {
	const op = "ledger.GetWorkspace"

	var ws domain.Workspace
	err := s.pool.QueryRow(ctx, `
		SELECT id, slug, name, metadata, created_at, updated_at FROM workspaces WHERE id = $1`, id,
	).Scan(&ws.ID, &ws.Slug, &ws.Name, &ws.Metadata, &ws.CreatedAt, &ws.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.Workspace{}, domain.Errorf(domain.CodeWorkspaceNotFound, op, "workspace not found")
		}
		return domain.Workspace{}, mapError(err, op, "cannot load workspace")
	}
	return ws, nil
}

// GetWorkspaceBySlug loads a workspace by its human-facing slug.
func (s *Store) GetWorkspaceBySlug(ctx context.Context, slug string) (domain.Workspace, error) {
	const op = "ledger.GetWorkspaceBySlug"

	var ws domain.Workspace
	err := s.pool.QueryRow(ctx, `
		SELECT id, slug, name, metadata, created_at, updated_at FROM workspaces WHERE slug = $1`, slug,
	).Scan(&ws.ID, &ws.Slug, &ws.Name, &ws.Metadata, &ws.CreatedAt, &ws.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.Workspace{}, domain.Errorf(domain.CodeWorkspaceNotFound, op, "workspace not found")
		}
		return domain.Workspace{}, mapError(err, op, "cannot load workspace")
	}
	return ws, nil
}

// ListWorkspaces returns the workspaces a principal may see. There is no
// list-everything path: enumeration is scoped by grants like every other read
// (AGENTS.md section 22.1).
func (s *Store) ListWorkspaces(ctx context.Context, principal domain.PrincipalID) ([]domain.Workspace, error) {
	const op = "ledger.ListWorkspaces"

	rows, err := s.pool.Query(ctx, `
		SELECT w.id, w.slug, w.name, w.metadata, w.created_at, w.updated_at
		FROM workspaces w
		JOIN principal_workspaces pw ON pw.workspace_id = w.id
		WHERE pw.principal_id = $1
		ORDER BY w.created_at`, principal)
	if err != nil {
		return nil, mapError(err, op, "cannot list workspaces")
	}
	defer rows.Close()

	var out []domain.Workspace
	for rows.Next() {
		var ws domain.Workspace
		if err := rows.Scan(&ws.ID, &ws.Slug, &ws.Name, &ws.Metadata, &ws.CreatedAt, &ws.UpdatedAt); err != nil {
			return nil, mapError(err, op, "cannot scan workspace")
		}
		out = append(out, ws)
	}
	return out, mapError(rows.Err(), op, "cannot list workspaces")
}

// CreateGraphSpace inserts a graph space inside an already-authorized workspace.
func (s *Store) CreateGraphSpace(ctx context.Context, gs domain.GraphSpace, actor domain.PrincipalID) (domain.GraphSpace, error) {
	const op = "ledger.CreateGraphSpace"

	if err := gs.Validate(); err != nil {
		return domain.GraphSpace{}, err
	}
	if domain.IsZero(gs.ID) {
		gs.ID = domain.NewGraphSpaceID()
	}

	err := s.InTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO graph_spaces (id, workspace_id, slug, name, metadata)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING created_at, updated_at`,
			gs.ID, gs.WorkspaceID, gs.Slug, gs.Name, jsonMap(gs.Metadata))
		if err := row.Scan(&gs.CreatedAt, &gs.UpdatedAt); err != nil {
			if isUniqueViolation(err, "graph_spaces_workspace_slug_key") {
				return domain.Errorf(domain.CodeConflict, op,
					"graph space slug %q already exists in this workspace", gs.Slug)
			}
			return mapError(err, op, "cannot insert graph space")
		}
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID:  gs.WorkspaceID,
			GraphSpaceID: gs.ID,
			PrincipalID:  actor,
			Action:       "graph_space.create",
			TargetKind:   "graph_space",
			TargetID:     string(gs.ID),
			Outcome:      "allowed",
			Detail:       map[string]any{"slug": gs.Slug},
		})
	})
	if err != nil {
		return domain.GraphSpace{}, err
	}
	return gs, nil
}

// GetGraphSpace resolves a graph space to its owning workspace. Authorization uses
// this to derive scope from the path rather than trusting a request body
// (AGENTS.md section 22.1).
func (s *Store) GetGraphSpace(ctx context.Context, id domain.GraphSpaceID) (domain.GraphSpace, error) {
	const op = "ledger.GetGraphSpace"

	var (
		gs              domain.GraphSpace
		ontologyVersion *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, slug, name, metadata, ontology_mode, ontology_version_id,
		       created_at, updated_at
		FROM graph_spaces WHERE id = $1`, id,
	).Scan(&gs.ID, &gs.WorkspaceID, &gs.Slug, &gs.Name, &gs.Metadata, &gs.OntologyMode,
		&ontologyVersion, &gs.CreatedAt, &gs.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.GraphSpace{}, domain.Errorf(domain.CodeGraphSpaceNotFound, op, "graph space not found")
		}
		return domain.GraphSpace{}, mapError(err, op, "cannot load graph space")
	}
	gs.OntologyVersionID = ontologyVersionOrNil(ontologyVersion)
	return gs, nil
}

func ontologyVersionOrNil(raw *string) *domain.OntologyVersionID {
	if raw == nil {
		return nil
	}
	id := domain.OntologyVersionID(*raw)
	return &id
}

// ListGraphSpaces returns the graph spaces in a workspace.
func (s *Store) ListGraphSpaces(ctx context.Context, ws domain.WorkspaceID) ([]domain.GraphSpace, error) {
	const op = "ledger.ListGraphSpaces"

	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, slug, name, metadata, ontology_mode, ontology_version_id,
		       created_at, updated_at
		FROM graph_spaces WHERE workspace_id = $1 ORDER BY created_at`, ws)
	if err != nil {
		return nil, mapError(err, op, "cannot list graph spaces")
	}
	defer rows.Close()

	var out []domain.GraphSpace
	for rows.Next() {
		var (
			gs              domain.GraphSpace
			ontologyVersion *string
		)
		if err := rows.Scan(&gs.ID, &gs.WorkspaceID, &gs.Slug, &gs.Name, &gs.Metadata,
			&gs.OntologyMode, &ontologyVersion, &gs.CreatedAt, &gs.UpdatedAt); err != nil {
			return nil, mapError(err, op, "cannot scan graph space")
		}
		gs.OntologyVersionID = ontologyVersionOrNil(ontologyVersion)
		out = append(out, gs)
	}
	return out, mapError(rows.Err(), op, "cannot list graph spaces")
}

// CreateCollection inserts a collection inside a graph space.
func (s *Store) CreateCollection(ctx context.Context, c domain.Collection, actor domain.PrincipalID) (domain.Collection, error) {
	const op = "ledger.CreateCollection"

	if err := c.Validate(); err != nil {
		return domain.Collection{}, err
	}
	if domain.IsZero(c.ID) {
		c.ID = domain.NewCollectionID()
	}

	err := s.InTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO collections (id, workspace_id, graph_space_id, slug, name, metadata)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING created_at`,
			c.ID, c.WorkspaceID, c.GraphSpaceID, c.Slug, c.Name, jsonMap(c.Metadata))
		if err := row.Scan(&c.CreatedAt); err != nil {
			if isUniqueViolation(err, "collections_graph_space_slug_key") {
				return domain.Errorf(domain.CodeConflict, op,
					"collection slug %q already exists in this graph space", c.Slug)
			}
			return mapError(err, op, "cannot insert collection")
		}
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID:  c.WorkspaceID,
			GraphSpaceID: c.GraphSpaceID,
			PrincipalID:  actor,
			Action:       "collection.create",
			TargetKind:   "collection",
			TargetID:     string(c.ID),
			Outcome:      "allowed",
		})
	})
	if err != nil {
		return domain.Collection{}, err
	}
	return c, nil
}

// GetCollection loads a collection, scoped by workspace so a valid identifier from
// another tenant cannot be dereferenced.
func (s *Store) GetCollection(ctx context.Context, ws domain.WorkspaceID, id domain.CollectionID) (domain.Collection, error) {
	const op = "ledger.GetCollection"

	var c domain.Collection
	err := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, graph_space_id, slug, name, metadata, created_at
		FROM collections WHERE workspace_id = $1 AND id = $2`, ws, id,
	).Scan(&c.ID, &c.WorkspaceID, &c.GraphSpaceID, &c.Slug, &c.Name, &c.Metadata, &c.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.Collection{}, domain.Errorf(domain.CodeNotFound, op, "collection not found")
		}
		return domain.Collection{}, mapError(err, op, "cannot load collection")
	}
	return c, nil
}

// CreateSource registers an upstream origin.
func (s *Store) CreateSource(ctx context.Context, src domain.Source, actor domain.PrincipalID) (domain.Source, error) {
	const op = "ledger.CreateSource"

	if src.Classification == "" {
		src.Classification = domain.ClassificationInternal
	}
	if src.TrustLevel == "" {
		// Unspecified trust is not high trust: content arrives untrusted by default
		// (AGENTS.md section 24).
		src.TrustLevel = domain.TrustStandard
	}
	if err := src.Validate(); err != nil {
		return domain.Source{}, err
	}
	if domain.IsZero(src.ID) {
		src.ID = domain.NewSourceID()
	}

	err := s.InTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO sources (id, workspace_id, kind, name, uri, trust_level, classification, metadata)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING created_at`,
			src.ID, src.WorkspaceID, src.Kind, src.Name, src.URI,
			src.TrustLevel, src.Classification, jsonMap(src.Metadata))
		if err := row.Scan(&src.CreatedAt); err != nil {
			if isUniqueViolation(err, "sources_workspace_name_key") {
				return domain.Errorf(domain.CodeConflict, op,
					"source %q already exists in this workspace", src.Name)
			}
			return mapError(err, op, "cannot insert source")
		}
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID: src.WorkspaceID,
			PrincipalID: actor,
			Action:      "source.create",
			TargetKind:  "source",
			TargetID:    string(src.ID),
			Outcome:     "allowed",
			Detail:      map[string]any{"kind": string(src.Kind), "trust_level": string(src.TrustLevel)},
		})
	})
	if err != nil {
		return domain.Source{}, err
	}
	return src, nil
}

// GetSource loads a source within a workspace.
func (s *Store) GetSource(ctx context.Context, ws domain.WorkspaceID, id domain.SourceID) (domain.Source, error) {
	const op = "ledger.GetSource"

	var src domain.Source
	err := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, kind, name, uri, trust_level, classification, metadata, created_at
		FROM sources WHERE workspace_id = $1 AND id = $2`, ws, id,
	).Scan(&src.ID, &src.WorkspaceID, &src.Kind, &src.Name, &src.URI,
		&src.TrustLevel, &src.Classification, &src.Metadata, &src.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.Source{}, domain.Errorf(domain.CodeNotFound, op, "source not found")
		}
		return domain.Source{}, mapError(err, op, "cannot load source")
	}
	return src, nil
}

// GetSourceByName loads a source by its workspace-unique name, which the CLI uses so
// operators do not have to paste identifiers.
func (s *Store) GetSourceByName(ctx context.Context, ws domain.WorkspaceID, name string) (domain.Source, error) {
	const op = "ledger.GetSourceByName"

	var src domain.Source
	err := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, kind, name, uri, trust_level, classification, metadata, created_at
		FROM sources WHERE workspace_id = $1 AND name = $2`, ws, name,
	).Scan(&src.ID, &src.WorkspaceID, &src.Kind, &src.Name, &src.URI,
		&src.TrustLevel, &src.Classification, &src.Metadata, &src.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.Source{}, domain.Errorf(domain.CodeNotFound, op, "source not found")
		}
		return domain.Source{}, mapError(err, op, "cannot load source")
	}
	return src, nil
}

// ListSources returns the sources registered in a workspace.
func (s *Store) ListSources(ctx context.Context, ws domain.WorkspaceID) ([]domain.Source, error) {
	const op = "ledger.ListSources"

	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, kind, name, uri, trust_level, classification, metadata, created_at
		FROM sources WHERE workspace_id = $1 ORDER BY created_at`, ws)
	if err != nil {
		return nil, mapError(err, op, "cannot list sources")
	}
	defer rows.Close()

	var out []domain.Source
	for rows.Next() {
		var src domain.Source
		if err := rows.Scan(&src.ID, &src.WorkspaceID, &src.Kind, &src.Name, &src.URI,
			&src.TrustLevel, &src.Classification, &src.Metadata, &src.CreatedAt); err != nil {
			return nil, mapError(err, op, "cannot scan source")
		}
		out = append(out, src)
	}
	return out, mapError(rows.Err(), op, "cannot list sources")
}

// AuditEntry is a security-relevant operation to record (AGENTS.md section 22.6).
// Detail carries identifiers and decisions only, never source content.
type AuditEntry struct {
	WorkspaceID  domain.WorkspaceID
	GraphSpaceID domain.GraphSpaceID
	PrincipalID  domain.PrincipalID
	Action       string
	TargetKind   string
	TargetID     string
	Outcome      string
	Detail       map[string]any
	CreatedAt    time.Time
}

// appendAudit writes an audit row inside the caller's transaction, so an audited
// operation and its audit record either both happen or neither does.
func (s *Store) appendAudit(ctx context.Context, tx pgx.Tx, e AuditEntry) error {
	const op = "ledger.appendAudit"

	_, err := tx.Exec(ctx, `
		INSERT INTO audit_events (id, workspace_id, graph_space_id, principal_id, action,
		                          target_kind, target_id, outcome, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		domain.NewUUIDString(), nullableString(e.WorkspaceID), nullableString(e.GraphSpaceID),
		string(e.PrincipalID), e.Action, e.TargetKind, e.TargetID, e.Outcome, jsonMap(e.Detail))
	return mapError(err, op, "cannot write audit event")
}

// AppendAudit records an audit entry outside any wider transaction, for denials and
// other events with no accompanying mutation.
func (s *Store) AppendAudit(ctx context.Context, e AuditEntry) error {
	return s.InTx(ctx, func(tx pgx.Tx) error { return s.appendAudit(ctx, tx, e) })
}

// CountAuditEvents reports how many audit rows match an action, which tests use to
// assert that security-relevant operations are recorded.
func (s *Store) CountAuditEvents(ctx context.Context, ws domain.WorkspaceID, action string) (int, error) {
	const op = "ledger.CountAuditEvents"

	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE workspace_id = $1 AND action = $2`, ws, action).Scan(&n)
	return n, mapError(err, op, "cannot count audit events")
}

// GetPrincipal loads a registered principal.
//
// Grants are not included: they are read separately and per request, so a revocation takes
// effect immediately rather than whenever a cached principal happens to be refreshed.
func (s *Store) GetPrincipal(ctx context.Context, id domain.PrincipalID) (domain.Principal, error) {
	const op = "ledger.GetPrincipal"

	var p domain.Principal
	err := s.pool.QueryRow(ctx, `
		SELECT id, kind, display_name, system_role FROM principals WHERE id = $1`, id).
		Scan(&p.ID, &p.Kind, &p.DisplayName, &p.SystemRole)
	if err != nil {
		if isNoRows(err) {
			return domain.Principal{}, domain.Errorf(domain.CodeNotFound, op,
				"principal %s is not registered", id)
		}
		return domain.Principal{}, mapError(err, op, "cannot load principal")
	}
	return p, nil
}
