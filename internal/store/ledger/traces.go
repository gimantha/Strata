package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

// RecordTrace persists one retrieval trace (AGENTS.md section 6.12).
//
// Deferred from phase 8 for a stated reason: section 6.12 marks query text "subject to
// policy/redaction", and there was no policy to redact against until this phase. The hash is
// always stored and the text only when the caller says policy permits it, so "which queries
// run often" stays answerable in deployments where the words themselves may not be kept.
func (s *Store) RecordTrace(ctx context.Context, trace domain.RetrievalTrace) (domain.RetrievalTrace, error) {
	const op = "ledger.RecordTrace"

	if domain.IsZero(trace.WorkspaceID) {
		return domain.RetrievalTrace{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"workspace scope is required")
	}
	if domain.IsZero(trace.ID) {
		trace.ID = domain.TraceID(domain.NewUUIDString())
	}
	if trace.QueryHash == "" {
		trace.QueryHash = HashQuery(trace.QueryText)
	}
	if trace.QueryTime.IsZero() {
		trace.QueryTime = time.Now().UTC()
	}

	text := trace.QueryText
	if trace.Redacted {
		text = ""
	}

	policyFilters, err := json.Marshal(trace.PolicyFilters)
	if err != nil {
		return domain.RetrievalTrace{}, domain.Wrap(err, domain.CodeInternal, op,
			"cannot encode policy filters")
	}
	filters, err := json.Marshal(trace.Filters)
	if err != nil {
		return domain.RetrievalTrace{}, domain.Wrap(err, domain.CodeInternal, op,
			"cannot encode query filters")
	}
	candidates, err := json.Marshal(orEmptyRefs(trace.CandidateRefs))
	if err != nil {
		return domain.RetrievalTrace{}, domain.Wrap(err, domain.CodeInternal, op,
			"cannot encode candidates")
	}
	selected, err := json.Marshal(orEmptyRefs(trace.SelectedRefs))
	if err != nil {
		return domain.RetrievalTrace{}, domain.Wrap(err, domain.CodeInternal, op,
			"cannot encode selections")
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO retrieval_traces
		    (id, workspace_id, graph_space_id, query_hash, query_text, redacted,
		     principal_id, principal_kind, purpose, action, policy_version, policy_rule,
		     policy_filters, filters, candidate_refs, selected_refs, latency_ms, query_time)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		trace.ID, trace.WorkspaceID, trace.GraphSpaceID, trace.QueryHash, text, trace.Redacted,
		string(trace.Principal.ID), string(trace.Principal.Kind), trace.Purpose,
		string(trace.Action), trace.PolicyVersion, trace.PolicyRule,
		policyFilters, filters, candidates, selected,
		trace.Latency.Milliseconds(), trace.QueryTime.UTC())
	if err != nil {
		return domain.RetrievalTrace{}, mapError(err, op, "cannot record the trace")
	}
	return trace, nil
}

// GetTrace reads one trace, scoped to a workspace.
//
// Scoped, not global. A trace names records in a workspace, so serving one across tenants
// would be a leak with extra steps.
func (s *Store) GetTrace(ctx context.Context, ws domain.WorkspaceID, id domain.TraceID) (domain.RetrievalTrace, error) {
	const op = "ledger.GetTrace"

	row := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, graph_space_id, query_hash, query_text, redacted,
		       principal_id, principal_kind, purpose, action, policy_version, policy_rule,
		       policy_filters, filters, candidate_refs, selected_refs, latency_ms, query_time
		FROM retrieval_traces WHERE workspace_id = $1 AND id = $2`, ws, id)

	trace, err := scanTrace(row)
	if err != nil {
		if isNoRows(err) {
			return domain.RetrievalTrace{}, domain.Errorf(domain.CodeNotFound, op, "trace not found")
		}
		return domain.RetrievalTrace{}, mapError(err, op, "cannot load the trace")
	}
	return trace, nil
}

// ListTraces returns recent traces for a graph space.
func (s *Store) ListTraces(ctx context.Context, scope domain.Scope, limit int) ([]domain.RetrievalTrace, error) {
	const op = "ledger.ListTraces"

	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, workspace_id, graph_space_id, query_hash, query_text, redacted,
		       principal_id, principal_kind, purpose, action, policy_version, policy_rule,
		       policy_filters, filters, candidate_refs, selected_refs, latency_ms, query_time
		FROM retrieval_traces
		WHERE workspace_id = $1 AND ($2::uuid IS NULL OR graph_space_id = $2)
		ORDER BY query_time DESC LIMIT $3`,
		scope.WorkspaceID, nullableString(scope.GraphSpaceID), limit)
	if err != nil {
		return nil, mapError(err, op, "cannot list traces")
	}
	defer rows.Close()

	var out []domain.RetrievalTrace
	for rows.Next() {
		trace, err := scanTrace(rows)
		if err != nil {
			return nil, mapError(err, op, "cannot scan trace")
		}
		out = append(out, trace)
	}
	return out, mapError(rows.Err(), op, "cannot list traces")
}

func scanTrace(row scanner) (domain.RetrievalTrace, error) {
	var (
		trace                                        domain.RetrievalTrace
		policyFilters, filters, candidates, selected []byte
		latencyMillis                                int64
	)
	if err := row.Scan(&trace.ID, &trace.WorkspaceID, &trace.GraphSpaceID, &trace.QueryHash,
		&trace.QueryText, &trace.Redacted, &trace.Principal.ID, &trace.Principal.Kind,
		&trace.Purpose, &trace.Action, &trace.PolicyVersion, &trace.PolicyRule,
		&policyFilters, &filters, &candidates, &selected, &latencyMillis,
		&trace.QueryTime); err != nil {
		return domain.RetrievalTrace{}, err
	}
	trace.Latency = time.Duration(latencyMillis) * time.Millisecond

	if err := json.Unmarshal(policyFilters, &trace.PolicyFilters); err != nil {
		return domain.RetrievalTrace{}, err
	}
	if err := json.Unmarshal(filters, &trace.Filters); err != nil {
		return domain.RetrievalTrace{}, err
	}
	if err := json.Unmarshal(candidates, &trace.CandidateRefs); err != nil {
		return domain.RetrievalTrace{}, err
	}
	if err := json.Unmarshal(selected, &trace.SelectedRefs); err != nil {
		return domain.RetrievalTrace{}, err
	}
	return trace, nil
}

// HashQuery is the stable identifier for a query's text.
//
// Stored even when the text is not, so a deployment that may not retain what people asked can
// still answer how often the same thing was asked.
func HashQuery(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func orEmptyRefs(refs []domain.ScoredRef) []domain.ScoredRef {
	if refs == nil {
		return []domain.ScoredRef{}
	}
	return refs
}
