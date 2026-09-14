package http

import (
	"net/http"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

type knowledgePurgeRequest struct {
	Before  time.Time `json:"before"`
	Reason  string    `json:"reason"`
	Execute bool      `json:"execute,omitempty"`
}

func (s *Server) handleKnowledgePurge(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := s.policyFor(w, r, domain.RoleAdmin, domain.ActionAdmin)
	if !ok {
		return
	}
	if s.purge == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleKnowledgePurge",
			"knowledge purge is not configured on this server"))
		return
	}
	var req knowledgePurgeRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	run, err := s.purge.Run(r.Context(), domain.KnowledgePurgeRequest{
		Scope: scope, Before: req.Before, Reason: req.Reason,
		Principal: principalFrom(r.Context()).Ref(), DryRun: !req.Execute,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, run)
}

func (s *Server) handleRetryKnowledgePurge(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := s.policyFor(w, r, domain.RoleAdmin, domain.ActionAdmin)
	if !ok {
		return
	}
	id := domain.KnowledgePurgeID(r.PathValue("purge_id"))
	if !domain.ValidUUID(id) {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleRetryKnowledgePurge",
			"malformed purge id"))
		return
	}
	if s.purge == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleRetryKnowledgePurge",
			"knowledge purge is not configured on this server"))
		return
	}
	run, err := s.purge.Retry(r.Context(), scope, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, run)
}

type knowledgeHoldRequest struct {
	SourceEventID string     `json:"source_event_id,omitempty"`
	Reason        string     `json:"reason"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
}

func (s *Server) handleCreateKnowledgeHold(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := s.policyFor(w, r, domain.RoleAdmin, domain.ActionAdmin)
	if !ok {
		return
	}
	if s.purge == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleCreateKnowledgeHold",
			"knowledge purge is not configured on this server"))
		return
	}
	var req knowledgeHoldRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.SourceEventID != "" && !domain.ValidUUID(domain.SourceEventID(req.SourceEventID)) {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleCreateKnowledgeHold",
			"malformed source_event_id"))
		return
	}
	hold, err := s.purge.Hold(r.Context(), domain.KnowledgeHold{
		Scope: scope, SourceEventID: domain.SourceEventID(req.SourceEventID), Reason: req.Reason,
		ExpiresAt: req.ExpiresAt, CreatedBy: principalFrom(r.Context()).Ref(),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, map[string]any{
		"id": hold.ID, "source_event_id": hold.SourceEventID, "expires_at": hold.ExpiresAt,
		"created_at": hold.CreatedAt,
	})
}

func (s *Server) handleReleaseKnowledgeHold(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := s.policyFor(w, r, domain.RoleAdmin, domain.ActionAdmin)
	if !ok {
		return
	}
	id := domain.KnowledgeHoldID(r.PathValue("hold_id"))
	if !domain.ValidUUID(id) {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleReleaseKnowledgeHold",
			"malformed hold id"))
		return
	}
	if s.purge == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleReleaseKnowledgeHold",
			"knowledge purge is not configured on this server"))
		return
	}
	if err := s.purge.Release(r.Context(), scope, id, principalFrom(r.Context()).Ref()); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"released": true, "id": id})
}
