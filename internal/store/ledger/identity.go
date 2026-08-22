package ledger

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/gimantha/strata/internal/domain"
)

// UpsertPrincipal records a principal so grants and audit rows can reference it.
// Authentication secrets are never stored here (see migration 0002).
func (s *Store) UpsertPrincipal(ctx context.Context, p domain.Principal) error {
	const op = "ledger.UpsertPrincipal"

	if p.ID == "" {
		return domain.Errorf(domain.CodeInvalidArgument, op, "principal id is required")
	}
	if _, err := domain.ParsePrincipalKind(string(p.Kind)); err != nil {
		return err
	}
	if _, err := domain.ParseRole(string(p.SystemRole)); err != nil {
		return err
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO principals (id, kind, display_name, system_role)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE
		SET kind = EXCLUDED.kind,
		    display_name = EXCLUDED.display_name,
		    system_role = EXCLUDED.system_role,
		    updated_at = now()`,
		string(p.ID), p.Kind, p.DisplayName, p.SystemRole)
	return mapError(err, op, "cannot upsert principal")
}

// Grant gives a principal a role in a workspace.
func (s *Store) Grant(ctx context.Context, principal domain.PrincipalID, ws domain.WorkspaceID, role domain.Role, actor domain.PrincipalID) error {
	const op = "ledger.Grant"

	if _, err := domain.ParseRole(string(role)); err != nil {
		return err
	}

	return s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO principal_workspaces (principal_id, workspace_id, role)
			VALUES ($1, $2, $3)
			ON CONFLICT (principal_id, workspace_id) DO UPDATE SET role = EXCLUDED.role`,
			string(principal), ws, role); err != nil {
			return mapError(err, op, "cannot write grant")
		}
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID: ws,
			PrincipalID: actor,
			Action:      "grant.create",
			TargetKind:  "principal",
			TargetID:    string(principal),
			Outcome:     "allowed",
			Detail:      map[string]any{"role": string(role)},
		})
	})
}

// Revoke removes a principal's access to a workspace.
func (s *Store) Revoke(ctx context.Context, principal domain.PrincipalID, ws domain.WorkspaceID, actor domain.PrincipalID) error {
	const op = "ledger.Revoke"

	return s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM principal_workspaces WHERE principal_id = $1 AND workspace_id = $2`,
			string(principal), ws); err != nil {
			return mapError(err, op, "cannot delete grant")
		}
		return s.appendAudit(ctx, tx, AuditEntry{
			WorkspaceID: ws,
			PrincipalID: actor,
			Action:      "grant.revoke",
			TargetKind:  "principal",
			TargetID:    string(principal),
			Outcome:     "allowed",
		})
	})
}

// GrantFor returns a principal's role in a workspace. This is the authoritative
// answer to "may this caller touch this tenant" (AGENTS.md section 22.1).
func (s *Store) GrantFor(ctx context.Context, principal domain.PrincipalID, ws domain.WorkspaceID) (domain.Role, bool, error) {
	const op = "ledger.GrantFor"

	var role domain.Role
	err := s.pool.QueryRow(ctx,
		`SELECT role FROM principal_workspaces WHERE principal_id = $1 AND workspace_id = $2`,
		string(principal), ws).Scan(&role)
	if err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, mapError(err, op, "cannot read grant")
	}
	return role, true, nil
}

// GrantsFor returns every workspace grant a principal holds.
func (s *Store) GrantsFor(ctx context.Context, principal domain.PrincipalID) ([]domain.Grant, error) {
	const op = "ledger.GrantsFor"

	rows, err := s.pool.Query(ctx, `
		SELECT principal_id, workspace_id, role, created_at
		FROM principal_workspaces WHERE principal_id = $1 ORDER BY created_at`, string(principal))
	if err != nil {
		return nil, mapError(err, op, "cannot list grants")
	}
	defer rows.Close()

	var out []domain.Grant
	for rows.Next() {
		var g domain.Grant
		if err := rows.Scan(&g.PrincipalID, &g.WorkspaceID, &g.Role, &g.CreatedAt); err != nil {
			return nil, mapError(err, op, "cannot scan grant")
		}
		out = append(out, g)
	}
	return out, mapError(rows.Err(), op, "cannot list grants")
}
