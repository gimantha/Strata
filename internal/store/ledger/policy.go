package ledger

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/gimantha/strata/internal/domain"
)

const policySetColumns = `id, workspace_id, version, name, notes, active, default_clearance,
	rules, created_at, created_by`

// CreatePolicySet appends an immutable policy version.
//
// Versions are sequenced in the insert itself, so two concurrent policy changes cannot both
// believe they are version 4 — the same argument as ontology versions, and for the same
// reason: an audit record naming a version has to mean one thing.
func (s *Store) CreatePolicySet(ctx context.Context, set domain.PolicySet) (domain.PolicySet, error) {
	const op = "ledger.CreatePolicySet"

	if err := set.Validate(); err != nil {
		return domain.PolicySet{}, err
	}
	if domain.IsZero(set.ID) {
		set.ID = domain.NewPolicySetID()
	}
	if set.DefaultClearance == "" {
		set.DefaultClearance = domain.ClassificationInternal
	}

	rules, err := json.Marshal(orEmptyRules(set.Rules))
	if err != nil {
		return domain.PolicySet{}, domain.Wrap(err, domain.CodeInternal, op, "cannot encode rules")
	}

	err = s.InTx(ctx, func(tx pgx.Tx) error {
		if set.Active {
			// One active set per workspace. Deactivating first keeps the partial unique
			// index satisfied at every point rather than only at commit.
			if _, err := tx.Exec(ctx,
				`UPDATE policy_sets SET active = false WHERE workspace_id = $1 AND active`,
				set.WorkspaceID); err != nil {
				return mapError(err, op, "cannot deactivate the previous policy set")
			}
		}

		row := tx.QueryRow(ctx, `
			INSERT INTO policy_sets
			    (id, workspace_id, version, name, notes, active, default_clearance, rules, created_by)
			VALUES ($1, $2,
			        (SELECT coalesce(max(version), 0) + 1 FROM policy_sets WHERE workspace_id = $2),
			        $3, $4, $5, $6, $7, $8)
			RETURNING `+policySetColumns,
			set.ID, set.WorkspaceID, set.Name, set.Notes, set.Active, set.DefaultClearance,
			rules, string(set.CreatedBy.ID))

		created, err := scanPolicySet(row)
		if err != nil {
			return mapError(err, op, "cannot create the policy set")
		}
		set = created
		return nil
	})
	if err != nil {
		return domain.PolicySet{}, err
	}
	return set, nil
}

// ActivePolicySet returns the workspace's current policy.
//
// Absence is not an error: a workspace with no policy runs on role-based access with an
// internal ceiling, which is a describable state rather than a missing one.
func (s *Store) ActivePolicySet(ctx context.Context, ws domain.WorkspaceID) (domain.PolicySet, error) {
	const op = "ledger.ActivePolicySet"

	row := s.pool.QueryRow(ctx, `SELECT `+policySetColumns+` FROM policy_sets
		WHERE workspace_id = $1 AND active`, ws)

	set, err := scanPolicySet(row)
	if err != nil {
		if isNoRows(err) {
			return domain.DefaultPolicySet(ws), nil
		}
		return domain.PolicySet{}, mapError(err, op, "cannot load the active policy set")
	}
	return set, nil
}

// GetPolicySet reads one version by id.
func (s *Store) GetPolicySet(ctx context.Context, ws domain.WorkspaceID, id domain.PolicySetID) (domain.PolicySet, error) {
	const op = "ledger.GetPolicySet"

	row := s.pool.QueryRow(ctx, `SELECT `+policySetColumns+` FROM policy_sets
		WHERE workspace_id = $1 AND id = $2`, ws, id)

	set, err := scanPolicySet(row)
	if err != nil {
		if isNoRows(err) {
			return domain.PolicySet{}, domain.Errorf(domain.CodeNotFound, op, "policy set not found")
		}
		return domain.PolicySet{}, mapError(err, op, "cannot load the policy set")
	}
	return set, nil
}

// ListPolicySets returns a workspace's policy history, newest first.
func (s *Store) ListPolicySets(ctx context.Context, ws domain.WorkspaceID, limit int) ([]domain.PolicySet, error) {
	const op = "ledger.ListPolicySets"

	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+policySetColumns+` FROM policy_sets
		WHERE workspace_id = $1 ORDER BY version DESC LIMIT $2`, ws, limit)
	if err != nil {
		return nil, mapError(err, op, "cannot list policy sets")
	}
	defer rows.Close()

	var out []domain.PolicySet
	for rows.Next() {
		set, err := scanPolicySet(rows)
		if err != nil {
			return nil, mapError(err, op, "cannot scan policy set")
		}
		out = append(out, set)
	}
	return out, mapError(rows.Err(), op, "cannot list policy sets")
}

// SetGrantClearance records a principal's clearance ceiling inside one workspace.
func (s *Store) SetGrantClearance(ctx context.Context, ws domain.WorkspaceID, principal domain.PrincipalID, clearance domain.Classification) error {
	const op = "ledger.SetGrantClearance"

	if clearance != "" {
		if _, err := domain.ParseClassification(string(clearance)); err != nil {
			return err
		}
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE principal_workspaces SET max_classification = $3
		WHERE workspace_id = $1 AND principal_id = $2`, ws, principal, clearance)
	if err != nil {
		return mapError(err, op, "cannot set the clearance")
	}
	if tag.RowsAffected() == 0 {
		return domain.Errorf(domain.CodeNotFound, op,
			"principal %s has no grant in this workspace", principal)
	}
	return nil
}

// GrantClearance reads a principal's clearance ceiling, empty when none is set.
func (s *Store) GrantClearance(ctx context.Context, ws domain.WorkspaceID, principal domain.PrincipalID) (domain.Classification, error) {
	const op = "ledger.GrantClearance"

	var clearance domain.Classification
	err := s.pool.QueryRow(ctx, `
		SELECT max_classification FROM principal_workspaces
		WHERE workspace_id = $1 AND principal_id = $2`, ws, principal).Scan(&clearance)
	if err != nil {
		if isNoRows(err) {
			return "", nil
		}
		return "", mapError(err, op, "cannot read the clearance")
	}
	return clearance, nil
}

func scanPolicySet(row scanner) (domain.PolicySet, error) {
	var (
		set   domain.PolicySet
		rules []byte
	)
	if err := row.Scan(&set.ID, &set.WorkspaceID, &set.Version, &set.Name, &set.Notes,
		&set.Active, &set.DefaultClearance, &rules, &set.CreatedAt, &set.CreatedBy.ID); err != nil {
		return domain.PolicySet{}, err
	}
	if err := json.Unmarshal(rules, &set.Rules); err != nil {
		return domain.PolicySet{}, err
	}
	return set, nil
}

func orEmptyRules(rules []domain.PolicyRule) []domain.PolicyRule {
	if rules == nil {
		return []domain.PolicyRule{}
	}
	return rules
}
