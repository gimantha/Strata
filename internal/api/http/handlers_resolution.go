package http

import (
	"net/http"

	"github.com/gimantha/strata/internal/domain"
)

type mergeEntitiesRequest struct {
	// FromEntityID is redirected into IntoEntityID. It is not deleted.
	FromEntityID string `json:"from_entity_id"`
	IntoEntityID string `json:"into_entity_id"`
	Reason       string `json:"reason"`
}

// handleMergeEntities redirects one identity into another.
//
// Nothing is collapsed: the merged identity keeps its row, its names, and every assertion
// that referenced it. Only a pointer changes, which is what makes the operation reversible
// (AGENTS.md section 12.3).
func (s *Server) handleMergeEntities(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleAdmin)
	if !ok {
		return
	}

	var req mergeEntitiesRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if !domain.ValidUUID(domain.EntityID(req.FromEntityID)) ||
		!domain.ValidUUID(domain.EntityID(req.IntoEntityID)) {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleMergeEntities",
			"from_entity_id and into_entity_id must both be entity ids"))
		return
	}
	if req.Reason == "" {
		// A merge is the most damaging operation available if it is wrong. Requiring a
		// reason is cheap and makes the decision ledger worth reading.
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleMergeEntities",
			"a merge must record why it was made"))
		return
	}

	decision, err := s.ledger.MergeEntities(r.Context(), scope.WorkspaceID,
		domain.EntityID(req.FromEntityID), domain.EntityID(req.IntoEntityID),
		domain.ResolutionDecision{
			WorkspaceID:  scope.WorkspaceID,
			GraphSpaceID: scope.GraphSpaceID,
			Confidence:   1,
			ActorID:      principalFrom(r.Context()).ID,
			Reason:       req.Reason,
		})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, toDecisionResponse(decision))
}

type splitEntityRequest struct {
	Reason string `json:"reason"`
}

// handleSplitEntity undoes a merge, restoring an identity to standing on its own.
func (s *Server) handleSplitEntity(w http.ResponseWriter, r *http.Request) {
	const op = "http.handleSplitEntity"

	raw := r.PathValue("entity_id")
	if !domain.ValidUUID(domain.EntityID(raw)) {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, op, "malformed entity id"))
		return
	}
	id := domain.EntityID(raw)

	principal := principalFrom(r.Context())
	ws, err := s.ledger.ResolveEntityWorkspace(r.Context(), id, grantedWorkspaces(principal))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.identity.AuthorizeWorkspace(r.Context(), principal, ws, domain.RoleAdmin); err != nil {
		s.writeError(w, r, err)
		return
	}

	var req splitEntityRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.Reason == "" {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, op,
			"a split must record why it was made"))
		return
	}

	entity, err := s.ledger.GetEntity(r.Context(), ws, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	decision, err := s.ledger.SplitEntity(r.Context(), ws, id, domain.ResolutionDecision{
		WorkspaceID:  ws,
		GraphSpaceID: entity.GraphSpaceID,
		Confidence:   1,
		ActorID:      principal.ID,
		Reason:       req.Reason,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, toDecisionResponse(decision))
}

// handleListResolutionDecisions returns the decision ledger.
//
// Passing review=true narrows it to the decisions a human should look at: ambiguous
// outcomes the resolver refused to guess, and merges or splits someone performed.
func (s *Server) handleListResolutionDecisions(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleReader)
	if !ok {
		return
	}

	reviewOnly := r.URL.Query().Get("review") == "true"
	decisions, err := s.ledger.ListResolutionDecisions(r.Context(), scope, reviewOnly, 0)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	out := make([]map[string]any, 0, len(decisions))
	for _, decision := range decisions {
		out = append(out, toDecisionResponse(decision))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"decisions": out, "count": len(out)})
}

// handleEntityIdentity reports how an identity relates to others: what it merged into, what
// merged into it, and the stable keys bound to it.
func (s *Server) handleEntityIdentity(w http.ResponseWriter, r *http.Request) {
	const op = "http.handleEntityIdentity"

	raw := r.PathValue("entity_id")
	if !domain.ValidUUID(domain.EntityID(raw)) {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, op, "malformed entity id"))
		return
	}
	id := domain.EntityID(raw)

	ws, err := s.ledger.ResolveEntityWorkspace(r.Context(), id, grantedWorkspaces(principalFrom(r.Context())))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	canonical, err := s.ledger.CanonicalEntityID(r.Context(), ws, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	cluster, err := s.ledger.IdentityCluster(r.Context(), ws, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	identifiers, err := s.ledger.ListIdentifiers(r.Context(), ws, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	members := make([]string, 0, len(cluster))
	for _, member := range cluster {
		members = append(members, string(member))
	}
	keys := make([]map[string]any, 0, len(identifiers))
	for _, identifier := range identifiers {
		keys = append(keys, map[string]any{
			"kind":      string(identifier.Kind),
			"namespace": identifier.Namespace,
			"value":     identifier.Value,
		})
	}

	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"entity_id":           string(id),
		"canonical_entity_id": string(canonical),
		"merged":              canonical != id,
		"cluster":             members,
		"identifiers":         keys,
	})
}

func toDecisionResponse(d domain.ResolutionDecision) map[string]any {
	candidates := make([]map[string]any, 0, len(d.Candidates))
	for _, candidate := range d.Candidates {
		candidates = append(candidates, map[string]any{
			"entity_id": string(candidate.EntityID),
			"name":      candidate.Name,
			"score":     candidate.Score,
			"features":  candidate.Features,
		})
	}

	out := map[string]any{
		"id":               string(d.ID),
		"method":           string(d.Method),
		"mention_text":     d.MentionText,
		"mention_type":     d.MentionType,
		"chosen_entity_id": string(d.ChosenEntityID),
		"confidence":       d.Confidence,
		"resolver_version": d.ResolverVersion,
		"human_override":   d.HumanOverride,
		"candidates":       candidates,
		"created_at":       d.CreatedAt.UTC().Format(timeFormat),
	}
	if !domain.IsZero(d.PreviousEntityID) {
		out["previous_entity_id"] = string(d.PreviousEntityID)
	}
	if d.Reason != "" {
		out["reason"] = d.Reason
	}
	if d.ActorID != "" {
		out["actor_id"] = string(d.ActorID)
	}
	if d.RevertedAt != nil {
		out["reverted_at"] = d.RevertedAt.UTC().Format(timeFormat)
	}
	return out
}
