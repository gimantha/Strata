package http

import (
	"net/http"

	"github.com/gimantha/strata/internal/domain"
)

type createEntityRequest struct {
	CanonicalName string         `json:"canonical_name"`
	EntityType    string         `json:"entity_type"`
	Aliases       []string       `json:"aliases,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

type entityResponse struct {
	ID            string         `json:"id"`
	WorkspaceID   string         `json:"workspace_id"`
	GraphSpaceID  string         `json:"graph_space_id"`
	CanonicalName string         `json:"canonical_name"`
	EntityType    string         `json:"entity_type"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	Aliases       []string       `json:"aliases,omitempty"`
	CreatedAt     string         `json:"created_at"`
	RetiredAt     string         `json:"retired_at,omitempty"`
}

func toEntityResponse(e domain.Entity, aliases []domain.EntityAlias) entityResponse {
	out := entityResponse{
		ID:            string(e.ID),
		WorkspaceID:   string(e.WorkspaceID),
		GraphSpaceID:  string(e.GraphSpaceID),
		CanonicalName: e.CanonicalName,
		EntityType:    e.EntityType,
		Metadata:      e.Metadata,
		CreatedAt:     e.CreatedAt.UTC().Format(timeFormat),
	}
	if e.RetiredAt != nil {
		out.RetiredAt = e.RetiredAt.UTC().Format(timeFormat)
	}
	for _, alias := range aliases {
		out.Aliases = append(out.Aliases, alias.Alias)
	}
	return out
}

// handleCreateEntity records a stable identity. Facts about it are assertions, not
// columns here (AGENTS.md section 6.7).
func (s *Server) handleCreateEntity(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleWriter)
	if !ok {
		return
	}

	var req createEntityRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	entity, err := s.ledger.CreateEntity(r.Context(), domain.Entity{
		WorkspaceID:   scope.WorkspaceID,
		GraphSpaceID:  scope.GraphSpaceID,
		CanonicalName: req.CanonicalName,
		EntityType:    req.EntityType,
		Metadata:      req.Metadata,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	for _, alias := range req.Aliases {
		if err := s.ledger.AddAlias(r.Context(), domain.EntityAlias{
			WorkspaceID: scope.WorkspaceID,
			EntityID:    entity.ID,
			Alias:       alias,
			Confidence:  1,
		}); err != nil {
			s.writeError(w, r, err)
			return
		}
	}

	aliases, err := s.ledger.ListAliases(r.Context(), scope.WorkspaceID, entity.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, toEntityResponse(entity, aliases))
}

// handleGetEntity returns one identity with its known names.
func (s *Server) handleGetEntity(w http.ResponseWriter, r *http.Request) {
	const op = "http.handleGetEntity"

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

	entity, err := s.ledger.GetEntity(r.Context(), ws, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	aliases, err := s.ledger.ListAliases(r.Context(), ws, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, toEntityResponse(entity, aliases))
}

// handleListEntities lists identities in a graph space, optionally by name or type.
func (s *Server) handleListEntities(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleReader)
	if !ok {
		return
	}

	query := r.URL.Query()
	var (
		entities []domain.Entity
		err      error
	)
	if name := query.Get("name"); name != "" {
		// Name lookup can legitimately return several identities. Which one is meant is
		// entity resolution's problem in phase 4, not something to guess here.
		entities, err = s.ledger.FindEntitiesByName(r.Context(), scope, name)
	} else {
		entities, err = s.ledger.ListEntities(r.Context(), scope, query.Get("type"), 0)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	out := make([]entityResponse, 0, len(entities))
	for _, e := range entities {
		out = append(out, toEntityResponse(e, nil))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"entities": out, "count": len(out)})
}

type definePredicateRequest struct {
	Name              string   `json:"name"`
	Description       string   `json:"description,omitempty"`
	SubjectTypes      []string `json:"subject_types,omitempty"`
	ObjectTypes       []string `json:"object_types,omitempty"`
	ObjectKinds       []string `json:"object_kinds,omitempty"`
	Functional        bool     `json:"functional,omitempty"`
	InverseFunctional bool     `json:"inverse_functional,omitempty"`
	Symmetric         bool     `json:"symmetric,omitempty"`
	Transitive        bool     `json:"transitive,omitempty"`
	TemporalPolicy    string   `json:"temporal_policy,omitempty"`
	ConflictPolicy    string   `json:"conflict_policy,omitempty"`
	DefaultMemoryKind string   `json:"default_memory_kind,omitempty"`
	Sensitivity       string   `json:"sensitivity,omitempty"`
	Status            string   `json:"status,omitempty"`
}

type predicateResponse struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Description       string   `json:"description,omitempty"`
	SubjectTypes      []string `json:"subject_types,omitempty"`
	ObjectTypes       []string `json:"object_types,omitempty"`
	ObjectKinds       []string `json:"object_kinds,omitempty"`
	Functional        bool     `json:"functional"`
	InverseFunctional bool     `json:"inverse_functional"`
	Symmetric         bool     `json:"symmetric"`
	Transitive        bool     `json:"transitive"`
	TemporalPolicy    string   `json:"temporal_policy"`
	ConflictPolicy    string   `json:"conflict_policy"`
	DefaultMemoryKind string   `json:"default_memory_kind"`
	Sensitivity       string   `json:"sensitivity"`
	Status            string   `json:"status"`
	Version           int      `json:"version"`
}

func toPredicateResponse(p domain.PredicateDefinition) predicateResponse {
	kinds := make([]string, 0, len(p.ObjectKinds))
	for _, k := range p.ObjectKinds {
		kinds = append(kinds, string(k))
	}
	return predicateResponse{
		ID:                string(p.ID),
		Name:              p.Name,
		Description:       p.Description,
		SubjectTypes:      p.SubjectTypes,
		ObjectTypes:       p.ObjectTypes,
		ObjectKinds:       kinds,
		Functional:        p.Functional,
		InverseFunctional: p.InverseFunctional,
		Symmetric:         p.Symmetric,
		Transitive:        p.Transitive,
		TemporalPolicy:    string(p.TemporalPolicy),
		ConflictPolicy:    string(p.ConflictPolicy),
		DefaultMemoryKind: string(p.DefaultMemoryKind),
		Sensitivity:       string(p.Sensitivity),
		Status:            string(p.Status),
		Version:           p.Version,
	}
}

// handleDefinePredicate sets a predicate's semantics. Changing them bumps the version, so
// claims validated under the old definition are not silently reinterpreted.
func (s *Server) handleDefinePredicate(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.authorizedWorkspace(w, r, domain.RoleAdmin)
	if !ok {
		return
	}

	var req definePredicateRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	def := domain.PredicateDefinition{
		WorkspaceID:       ws,
		Name:              req.Name,
		Description:       req.Description,
		SubjectTypes:      req.SubjectTypes,
		ObjectTypes:       req.ObjectTypes,
		Functional:        req.Functional,
		InverseFunctional: req.InverseFunctional,
		Symmetric:         req.Symmetric,
		Transitive:        req.Transitive,
	}
	for _, kind := range req.ObjectKinds {
		parsed, err := domain.ParseObjectKind(kind)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		def.ObjectKinds = append(def.ObjectKinds, parsed)
	}
	if req.TemporalPolicy != "" {
		parsed, err := domain.ParseTemporalPolicy(req.TemporalPolicy)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		def.TemporalPolicy = parsed
	}
	if req.ConflictPolicy != "" {
		parsed, err := domain.ParseConflictPolicy(req.ConflictPolicy)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		def.ConflictPolicy = parsed
	}
	if req.DefaultMemoryKind != "" {
		parsed, err := domain.ParseMemoryKind(req.DefaultMemoryKind)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		def.DefaultMemoryKind = parsed
	}
	if req.Sensitivity != "" {
		parsed, err := domain.ParseClassification(req.Sensitivity)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		def.Sensitivity = parsed
	}
	if req.Status != "" {
		parsed, err := domain.ParsePredicateStatus(req.Status)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		def.Status = parsed
	}

	stored, err := s.ledger.DefinePredicate(r.Context(), def, principalFrom(r.Context()).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, toPredicateResponse(stored))
}

// handleListPredicates returns the registry, including names extraction invented.
func (s *Server) handleListPredicates(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.authorizedWorkspace(w, r, domain.RoleReader)
	if !ok {
		return
	}

	predicates, err := s.ledger.ListPredicates(r.Context(), ws)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]predicateResponse, 0, len(predicates))
	for _, p := range predicates {
		out = append(out, toPredicateResponse(p))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"predicates": out, "count": len(out)})
}

// handleListConflicts returns recorded disagreements.
func (s *Server) handleListConflicts(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleReader)
	if !ok {
		return
	}

	openOnly := r.URL.Query().Get("status") != "all"
	conflicts, err := s.ledger.ListConflictSets(r.Context(), scope, openOnly, 0)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	out := make([]map[string]any, 0, len(conflicts))
	for _, c := range conflicts {
		entry := map[string]any{
			"id":         string(c.ID),
			"subject_id": string(c.SubjectID),
			"predicate":  c.Predicate,
			"scope_key":  c.ScopeKey,
			"reason":     c.Reason,
			"resolution": string(c.Resolution),
			"created_at": c.CreatedAt.UTC().Format(timeFormat),
		}
		if c.ResolvedAt != nil {
			entry["resolved_at"] = c.ResolvedAt.UTC().Format(timeFormat)
			entry["resolved_by"] = string(c.ResolvedBy)
		}
		out = append(out, entry)
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"conflicts": out, "count": len(out)})
}

type resolveConflictRequest struct {
	Resolution string `json:"resolution"`
}

// handleResolveConflict closes a disagreement, returning surviving claims to active.
func (s *Server) handleResolveConflict(w http.ResponseWriter, r *http.Request) {
	const op = "http.handleResolveConflict"

	raw := r.PathValue("conflict_id")
	if !domain.ValidUUID(domain.ConflictSetID(raw)) {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, op, "malformed conflict id"))
		return
	}
	id := domain.ConflictSetID(raw)

	principal := principalFrom(r.Context())
	ws, err := s.ledger.ResolveConflictSetWorkspace(r.Context(), id, grantedWorkspaces(principal))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.identity.AuthorizeWorkspace(r.Context(), principal, ws, domain.RoleWriter); err != nil {
		s.writeError(w, r, err)
		return
	}

	var req resolveConflictRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	resolution, err := domain.ParseConflictResolution(req.Resolution)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if resolution == domain.ConflictOpen {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, op,
			"resolving a conflict requires a resolution other than open"))
		return
	}

	if err := s.ledger.ResolveConflictSet(r.Context(), ws, id, resolution,
		s.now(), principal.ID); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"id":         string(id),
		"resolution": string(resolution),
	})
}
