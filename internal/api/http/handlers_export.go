package http

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/gimantha/strata/internal/domain"
)

// handleExport streams a graph space's knowledge as JSONL (AGENTS.md sections 22.6, 25.4).
//
// Export is a separate action from read, not a convenience wrapper over it. Being able to
// look something up and being able to walk out with everything are different powers, and a
// system that conflates them has no way to grant the first without the second.
//
// Streamed rather than assembled: an export of a real workspace does not fit in memory, and
// the shape that works for a thousand claims should be the shape that works for a million.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	scope, filters, ok := s.policyFor(w, r, domain.RoleAdmin, domain.ActionExport)
	if !ok {
		return
	}

	limit := intParam(r, "limit", domain.MaxAssertionLimit)
	assertions, err := s.ledger.QueryAssertions(r.Context(), domain.AssertionQuery{
		Scope: scope,
		// Policy narrows the query itself. Exporting everything and dropping rows on the
		// way out would put restricted material in this process's memory, which is what
		// section 22.4 forbids.
		Classifications:   filters.PermittedClassifications(),
		IncludeSuperseded: r.URL.Query().Get("include_superseded") == "true",
		Limit:             limit,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	encoder := json.NewEncoder(w)
	written := 0
	for _, assertion := range assertions {
		// The second gate, on the record rather than the query. Classification is filtered
		// in SQL above; source, predicate, and memory kind are checked here because
		// assertions carry them and an export is the one path where a missed filter hands
		// over a complete copy.
		if !filters.Allows(assertion.Classification, "", assertion.Predicate.Name,
			assertion.MemoryKind, "") {
			continue
		}
		if err := encoder.Encode(exportRow(assertion)); err != nil {
			// The status line is already sent, so this cannot become an error response.
			// The truncated stream and the audit record are what the caller has.
			return
		}
		written++
	}

	s.logger.InfoContext(r.Context(), "knowledge exported",
		slog.String("workspace_id", string(scope.WorkspaceID)),
		slog.String("graph_space_id", string(scope.GraphSpaceID)),
		slog.Int("assertions", written))
}

func exportRow(assertion domain.Assertion) map[string]any {
	row := map[string]any{
		"assertion_id":   string(assertion.ID),
		"graph_space_id": string(assertion.GraphSpaceID),
		"subject_id":     string(assertion.SubjectID),
		"predicate":      assertion.Predicate.Name,
		"object":         assertion.Object,
		"status":         string(assertion.Status),
		"confidence":     assertion.Confidence,
		"classification": string(assertion.Classification),
		"memory_kind":    string(assertion.MemoryKind),
		"recorded_at":    assertion.Temporal.RecordedAt,
	}
	if assertion.ScopeKey != "" {
		row["scope_key"] = assertion.ScopeKey
	}
	if assertion.Temporal.ValidFrom != nil {
		row["valid_from"] = assertion.Temporal.ValidFrom
	}
	if assertion.Temporal.ValidTo != nil {
		row["valid_to"] = assertion.Temporal.ValidTo
	}
	return row
}

// handleGetTrace returns one retrieval trace (AGENTS.md sections 6.12, 25.2).
func (s *Server) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	principal := principalFrom(r.Context())

	// A trace names records in a workspace, so it is resolved through the workspaces this
	// principal actually holds rather than by id alone. Serving one by id across tenants
	// would be a cross-workspace leak with extra steps.
	traceID := domain.TraceID(r.PathValue("trace_id"))
	for _, grant := range principal.Grants {
		trace, err := s.ledger.GetTrace(r.Context(), grant.WorkspaceID, traceID)
		if err != nil {
			if domain.IsCode(err, domain.CodeNotFound) {
				continue
			}
			s.writeError(w, r, err)
			return
		}
		s.writeJSON(w, r, http.StatusOK, traceJSON(trace))
		return
	}

	// Absence rather than denial: confirming a trace exists in a workspace the caller
	// cannot see is itself information.
	s.writeError(w, r, domain.Errorf(domain.CodeNotFound, "http.handleGetTrace",
		"trace not found"))
}

// handleListTraces returns recent traces for a graph space.
func (s *Server) handleListTraces(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleAdmin)
	if !ok {
		return
	}

	traces, err := s.ledger.ListTraces(r.Context(), scope, intParam(r, "limit", 50))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	out := make([]map[string]any, 0, len(traces))
	for _, trace := range traces {
		out = append(out, traceJSON(trace))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"traces": out, "count": len(out)})
}

func traceJSON(trace domain.RetrievalTrace) map[string]any {
	out := map[string]any{
		"id":             string(trace.ID),
		"graph_space_id": string(trace.GraphSpaceID),
		"query_hash":     trace.QueryHash,
		"redacted":       trace.Redacted,
		"principal_id":   string(trace.Principal.ID),
		"action":         string(trace.Action),
		"policy_version": trace.PolicyVersion,
		"policy_rule":    trace.PolicyRule,
		"candidates":     len(trace.CandidateRefs),
		"selected":       len(trace.SelectedRefs),
		"latency_ms":     trace.Latency.Milliseconds(),
		"query_time":     trace.QueryTime,
	}
	if !trace.Redacted && trace.QueryText != "" {
		out["query_text"] = trace.QueryText
	}
	if trace.Purpose != "" {
		out["purpose"] = trace.Purpose
	}
	if trace.PolicyFilters.MaxClassification != "" {
		out["max_classification"] = string(trace.PolicyFilters.MaxClassification)
	}
	if len(trace.SelectedRefs) > 0 {
		refs := make([]map[string]any, 0, len(trace.SelectedRefs))
		for _, ref := range trace.SelectedRefs {
			refs = append(refs, map[string]any{
				"surface": string(ref.Surface), "record_id": ref.RecordID, "score": ref.Score,
			})
		}
		out["selected_refs"] = refs
	}
	return out
}
