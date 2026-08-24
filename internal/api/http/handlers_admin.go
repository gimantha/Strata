package http

import (
	"net/http"

	"github.com/gimantha/strata/internal/domain"
)

type createWorkspaceRequest struct {
	Slug     string         `json:"slug"`
	Name     string         `json:"name"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type workspaceResponse struct {
	ID        string         `json:"id"`
	Slug      string         `json:"slug"`
	Name      string         `json:"name"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt string         `json:"created_at"`
}

func toWorkspaceResponse(ws domain.Workspace) workspaceResponse {
	return workspaceResponse{
		ID:        string(ws.ID),
		Slug:      ws.Slug,
		Name:      ws.Name,
		Metadata:  ws.Metadata,
		CreatedAt: ws.CreatedAt.UTC().Format(timeFormat),
	}
}

const timeFormat = "2006-01-02T15:04:05.000Z07:00"

// handleCreateWorkspace creates a tenant. This is the one operation gated by a system
// role rather than a workspace grant, since no workspace exists yet to be granted on.
// The creator becomes its owner in the same transaction.
func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	principal := principalFrom(r.Context())
	if err := s.identity.AuthorizeSystem(principal, domain.RoleAdmin); err != nil {
		s.writeError(w, r, err)
		return
	}

	var req createWorkspaceRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	ws, err := s.ledger.CreateWorkspace(r.Context(), domain.Workspace{
		Slug: req.Slug, Name: req.Name, Metadata: req.Metadata,
	}, principal.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, toWorkspaceResponse(ws))
}

// handleListWorkspaces lists only what the caller has been granted. There is no
// list-everything path (AGENTS.md section 22.1).
func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	principal := principalFrom(r.Context())

	list, err := s.ledger.ListWorkspaces(r.Context(), principal.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]workspaceResponse, 0, len(list))
	for _, ws := range list {
		out = append(out, toWorkspaceResponse(ws))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"workspaces": out})
}

type createGraphSpaceRequest struct {
	Slug     string         `json:"slug"`
	Name     string         `json:"name"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type graphSpaceResponse struct {
	ID          string         `json:"id"`
	WorkspaceID string         `json:"workspace_id"`
	Slug        string         `json:"slug"`
	Name        string         `json:"name"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreatedAt   string         `json:"created_at"`
}

func toGraphSpaceResponse(gs domain.GraphSpace) graphSpaceResponse {
	return graphSpaceResponse{
		ID:          string(gs.ID),
		WorkspaceID: string(gs.WorkspaceID),
		Slug:        gs.Slug,
		Name:        gs.Name,
		Metadata:    gs.Metadata,
		CreatedAt:   gs.CreatedAt.UTC().Format(timeFormat),
	}
}

func (s *Server) handleCreateGraphSpace(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.authorizedWorkspace(w, r, domain.RoleAdmin)
	if !ok {
		return
	}

	var req createGraphSpaceRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	gs, err := s.ledger.CreateGraphSpace(r.Context(), domain.GraphSpace{
		WorkspaceID: ws, Slug: req.Slug, Name: req.Name, Metadata: req.Metadata,
	}, principalFrom(r.Context()).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, toGraphSpaceResponse(gs))
}

func (s *Server) handleListGraphSpaces(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.authorizedWorkspace(w, r, domain.RoleReader)
	if !ok {
		return
	}

	list, err := s.ledger.ListGraphSpaces(r.Context(), ws)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]graphSpaceResponse, 0, len(list))
	for _, gs := range list {
		out = append(out, toGraphSpaceResponse(gs))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"graph_spaces": out})
}

type createCollectionRequest struct {
	Slug     string         `json:"slug"`
	Name     string         `json:"name"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func (s *Server) handleCreateCollection(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleAdmin)
	if !ok {
		return
	}

	var req createCollectionRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	collection, err := s.ledger.CreateCollection(r.Context(), domain.Collection{
		WorkspaceID:  scope.WorkspaceID,
		GraphSpaceID: scope.GraphSpaceID,
		Slug:         req.Slug,
		Name:         req.Name,
		Metadata:     req.Metadata,
	}, principalFrom(r.Context()).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, map[string]any{
		"id":             string(collection.ID),
		"workspace_id":   string(collection.WorkspaceID),
		"graph_space_id": string(collection.GraphSpaceID),
		"slug":           collection.Slug,
		"name":           collection.Name,
		"created_at":     collection.CreatedAt.UTC().Format(timeFormat),
	})
}

type createSourceRequest struct {
	Kind           string         `json:"kind"`
	Name           string         `json:"name"`
	URI            string         `json:"uri,omitempty"`
	TrustLevel     string         `json:"trust_level,omitempty"`
	Classification string         `json:"classification,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

type sourceResponse struct {
	ID             string `json:"id"`
	WorkspaceID    string `json:"workspace_id"`
	Kind           string `json:"kind"`
	Name           string `json:"name"`
	URI            string `json:"uri,omitempty"`
	TrustLevel     string `json:"trust_level"`
	Classification string `json:"classification"`
	CreatedAt      string `json:"created_at"`
}

func toSourceResponse(src domain.Source) sourceResponse {
	return sourceResponse{
		ID:             string(src.ID),
		WorkspaceID:    string(src.WorkspaceID),
		Kind:           string(src.Kind),
		Name:           src.Name,
		URI:            src.URI,
		TrustLevel:     string(src.TrustLevel),
		Classification: string(src.Classification),
		CreatedAt:      src.CreatedAt.UTC().Format(timeFormat),
	}
}

func (s *Server) handleCreateSource(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.authorizedWorkspace(w, r, domain.RoleAdmin)
	if !ok {
		return
	}

	var req createSourceRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	kind, err := domain.ParseSourceKind(req.Kind)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	src := domain.Source{
		WorkspaceID: ws,
		Kind:        kind,
		Name:        req.Name,
		URI:         req.URI,
		Metadata:    req.Metadata,
	}
	if req.TrustLevel != "" {
		trust, err := domain.ParseTrustLevel(req.TrustLevel)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		src.TrustLevel = trust
	}
	if req.Classification != "" {
		classification, err := domain.ParseClassification(req.Classification)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		src.Classification = classification
	}

	created, err := s.ledger.CreateSource(r.Context(), src, principalFrom(r.Context()).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, toSourceResponse(created))
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.authorizedWorkspace(w, r, domain.RoleReader)
	if !ok {
		return
	}

	list, err := s.ledger.ListSources(r.Context(), ws)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]sourceResponse, 0, len(list))
	for _, src := range list {
		out = append(out, toSourceResponse(src))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"sources": out})
}

type createGrantRequest struct {
	PrincipalID string `json:"principal_id"`
	Role        string `json:"role"`
}

// handleCreateGrant grants a principal access to a workspace. Only an owner may widen
// access, so a workspace admin cannot quietly add collaborators.
func (s *Server) handleCreateGrant(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.authorizedWorkspace(w, r, domain.RoleOwner)
	if !ok {
		return
	}

	var req createGrantRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	role, err := domain.ParseRole(req.Role)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.PrincipalID == "" {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleCreateGrant",
			"principal_id is required"))
		return
	}

	if err := s.ledger.Grant(r.Context(), domain.PrincipalID(req.PrincipalID), ws, role,
		principalFrom(r.Context()).ID); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, map[string]any{
		"principal_id": req.PrincipalID,
		"workspace_id": string(ws),
		"role":         string(role),
	})
}

// authorizedWorkspace resolves the {workspace_id} path parameter and checks the
// caller's grant on it.
func (s *Server) authorizedWorkspace(w http.ResponseWriter, r *http.Request, need domain.Role) (domain.WorkspaceID, bool) {
	const op = "http.authorizedWorkspace"

	raw := r.PathValue("workspace_id")
	if !domain.ValidUUID(domain.WorkspaceID(raw)) {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, op, "malformed workspace id"))
		return "", false
	}
	ws := domain.WorkspaceID(raw)

	if err := s.identity.AuthorizeWorkspace(r.Context(), principalFrom(r.Context()), ws, need); err != nil {
		// Report the workspace as missing rather than forbidden, so its existence is
		// not disclosed to a caller with no grant.
		s.writeError(w, r, domain.Errorf(domain.CodeWorkspaceNotFound, op, "workspace not found"))
		return "", false
	}
	return ws, true
}

// authorizedGraphSpace resolves the {graph_space_id} path parameter into an authorized
// scope. The workspace comes from the stored graph space, never from the request.
func (s *Server) authorizedGraphSpace(w http.ResponseWriter, r *http.Request, need domain.Role) (domain.Scope, bool) {
	scope, err := s.identity.ResolveGraphSpace(r.Context(),
		principalFrom(r.Context()), domain.GraphSpaceID(r.PathValue("graph_space_id")), need)
	if err != nil {
		s.writeError(w, r, err)
		return domain.Scope{}, false
	}
	return scope, true
}

// policyFor resolves the graph space and evaluates the caller's access in one step.
//
// Every read path calls this instead of authorizedGraphSpace, because a role check alone
// answers "may this principal use this graph space" and not "which of its contents may they
// see". The returned filters are what the handler must pass into its queries; nothing
// downstream is allowed to fetch first and filter later (AGENTS.md section 22.4).
func (s *Server) policyFor(w http.ResponseWriter, r *http.Request, need domain.Role, action domain.PolicyAction) (domain.Scope, domain.PolicyFilters, bool) {
	scope, ok := s.authorizedGraphSpace(w, r, need)
	if !ok {
		return domain.Scope{}, domain.PolicyFilters{}, false
	}
	if s.policy == nil {
		// No policy service configured: role-based access alone, and no narrowing. Stated
		// here rather than assumed, so a misconfigured deployment fails open visibly in
		// one place instead of invisibly in twelve.
		return scope, domain.PolicyFilters{}, true
	}

	decision, err := s.policy.Authorize(r.Context(), domain.AccessRequest{
		Principal: principalFrom(r.Context()),
		Action:    action,
		Scope:     scope,
		Purpose:   r.Header.Get("X-Strata-Purpose"),
	})
	if err != nil {
		s.writeError(w, r, err)
		return domain.Scope{}, domain.PolicyFilters{}, false
	}
	if !decision.Allowed {
		s.writeError(w, r, domain.Errorf(domain.CodePermissionDenied, "http.policyFor",
			"%s", decision.Reason))
		return domain.Scope{}, domain.PolicyFilters{}, false
	}
	return scope, decision.Filters, true
}

// purposeOf reads the caller's stated reason for asking, when policy requires one.
func purposeOf(r *http.Request) string { return r.Header.Get("X-Strata-Purpose") }
