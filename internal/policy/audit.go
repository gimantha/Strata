package policy

import (
	"context"

	"github.com/gimantha/strata/internal/store/ledger"
)

// LedgerAuditor writes decisions to the audit table (AGENTS.md section 22.6).
//
// Audit rows never contain source content — only who asked, what for, and what was decided.
// A log that quotes the material it was guarding is a second copy of the thing being
// protected, in a table people grant broad read access to precisely because it is "just
// metadata".
type LedgerAuditor struct {
	store *ledger.Store
}

func NewLedgerAuditor(store *ledger.Store) *LedgerAuditor {
	return &LedgerAuditor{store: store}
}

func (a *LedgerAuditor) Record(ctx context.Context, entry AuditEntry) error {
	if a == nil || a.store == nil {
		return nil
	}

	outcome := "denied"
	if entry.Allowed {
		outcome = "allowed"
	}

	detail := map[string]any{}
	for key, value := range entry.Detail {
		detail[key] = value
	}
	if entry.Rule != "" {
		detail["rule"] = entry.Rule
	}
	if entry.Reason != "" {
		detail["reason"] = entry.Reason
	}
	if entry.Purpose != "" {
		detail["purpose"] = entry.Purpose
	}
	if entry.Version > 0 {
		detail["policy_version"] = entry.Version
	}

	target := entry.Target
	if target == "" {
		target = string(entry.Scope.GraphSpaceID)
	}

	return a.store.AppendAudit(ctx, ledger.AuditEntry{
		WorkspaceID:  entry.Scope.WorkspaceID,
		GraphSpaceID: entry.Scope.GraphSpaceID,
		PrincipalID:  entry.Principal.ID,
		Action:       "policy." + string(entry.Action),
		TargetKind:   "graph_space",
		TargetID:     target,
		Outcome:      outcome,
		Detail:       detail,
	})
}

// Ensure the ledger auditor satisfies the interface the service depends on.
var _ Auditor = (*LedgerAuditor)(nil)

// DiscardAuditor drops decisions. For tests and for tooling that evaluates hypotheticals.
type DiscardAuditor struct{}

func (DiscardAuditor) Record(context.Context, AuditEntry) error { return nil }

var _ Auditor = DiscardAuditor{}
