package http

import (
	"net/http"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/memory"
)

type consolidateRequest struct {
	MinObservations    int      `json:"min_observations,omitempty"`
	MinDistinctSources int      `json:"min_distinct_sources,omitempty"`
	MinConfidence      float64  `json:"min_confidence,omitempty"`
	MemoryKinds        []string `json:"memory_kinds,omitempty"`
	Limit              int      `json:"limit,omitempty"`
	DryRun             bool     `json:"dry_run,omitempty"`
}

// handleConsolidate turns repeated observation into stable facts (AGENTS.md section 21.1).
func (s *Server) handleConsolidate(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleWriter)
	if !ok {
		return
	}
	if s.memory == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleConsolidate",
			"memory consolidation is not configured on this server"))
		return
	}

	var req consolidateRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	rule := domain.ConsolidationRule{
		MinObservations:    req.MinObservations,
		MinDistinctSources: req.MinDistinctSources,
		MinConfidence:      req.MinConfidence,
	}
	for _, kind := range req.MemoryKinds {
		parsed, err := domain.ParseMemoryKind(kind)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		rule.Kinds = append(rule.Kinds, parsed)
	}

	result, err := s.memory.Consolidate(r.Context(), memory.ConsolidateRequest{
		Scope:     scope,
		Principal: principalFrom(r.Context()).Ref(),
		Rule:      rule,
		Limit:     req.Limit,
		DryRun:    req.DryRun,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	qualified := make([]map[string]any, 0, len(result.Qualified))
	for _, group := range result.Qualified {
		qualified = append(qualified, map[string]any{
			"subject_id":   string(group.SubjectID),
			"predicate":    group.Predicate,
			"observations": len(group.Members),
			"sources":      len(group.Sources),
			"confidence":   group.Confidence(),
			"summary":      group.Summary(),
		})
	}

	derived := make([]string, 0, len(result.Derived))
	for _, assertion := range result.Derived {
		derived = append(derived, string(assertion.ID))
	}

	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"examined":        result.Examined,
		"groups":          result.Groups,
		"qualified":       qualified,
		"derived":         derived,
		"already_derived": result.Existing,
		"dry_run":         req.DryRun,
	})
}

type forgetRequest struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// handleForget takes a claim out of use in the way the caller names
// (AGENTS.md section 21.4).
//
// The kind is required. The four ways of forgetting differ in what survives, and making the
// caller say which one they mean is the entire reason this is not a `delete` flag.
func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	principal := principalFrom(r.Context())
	ws, assertionID, ok := s.resolveAssertion(w, r)
	if !ok {
		return
	}
	scope := domain.Scope{WorkspaceID: ws}
	if s.memory == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleForget",
			"memory lifecycle is not configured on this server"))
		return
	}

	var req forgetRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.Kind == "" {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleForget",
			"kind is required: deactivate, retract, retention, or erasure"))
		return
	}

	kind, err := domain.ParseForgetKind(req.Kind)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	assertion, err := s.memory.Forget(r.Context(), memory.ForgetRequest{
		Scope: scope, Actor: principal.Ref(), AssertionID: assertionID,
		Kind: kind, Reason: req.Reason,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"assertion_id": string(assertion.ID),
		"kind":         string(kind),
		// Status is unchanged on purpose: a deactivated claim is still asserted, still
		// true, and still answerable as of any instant.
		"status":         string(assertion.Status),
		"deactivated_at": assertion.DeactivatedAt,
		"reason":         assertion.DeactivationReason,
		"still_asserted": assertion.Status == domain.AssertionActive,
	})
}

// handleReactivate puts deactivated knowledge back in scope.
func (s *Server) handleReactivate(w http.ResponseWriter, r *http.Request) {
	principal := principalFrom(r.Context())
	ws, assertionID, ok := s.resolveAssertion(w, r)
	if !ok {
		return
	}
	scope := domain.Scope{WorkspaceID: ws}
	if s.memory == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleReactivate",
			"memory lifecycle is not configured on this server"))
		return
	}

	assertion, err := s.memory.Reactivate(r.Context(), scope, principal.Ref(), assertionID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"assertion_id": string(assertion.ID),
		"active":       assertion.DeactivatedAt == nil,
	})
}
