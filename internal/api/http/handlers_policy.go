package http

import (
	"net/http"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/policy"
)

type policyRuleBody struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Effect      string   `json:"effect"`
	Actions     []string `json:"actions,omitempty"`

	PrincipalIDs   []string `json:"principal_ids,omitempty"`
	PrincipalKinds []string `json:"principal_kinds,omitempty"`
	Roles          []string `json:"roles,omitempty"`
	Purposes       []string `json:"purposes,omitempty"`

	GraphSpaceIDs   []string `json:"graph_space_ids,omitempty"`
	CollectionIDs   []string `json:"collection_ids,omitempty"`
	SourceIDs       []string `json:"source_ids,omitempty"`
	EntityTypes     []string `json:"entity_types,omitempty"`
	Predicates      []string `json:"predicates,omitempty"`
	Classifications []string `json:"classifications,omitempty"`
	MemoryKinds     []string `json:"memory_kinds,omitempty"`
	Residencies     []string `json:"residencies,omitempty"`

	MaxClassification string     `json:"max_classification,omitempty"`
	NotBefore         *time.Time `json:"not_before,omitempty"`
	NotAfter          *time.Time `json:"not_after,omitempty"`
}

type definePolicyRequest struct {
	Name             string           `json:"name"`
	Notes            string           `json:"notes,omitempty"`
	DefaultClearance string           `json:"default_clearance,omitempty"`
	Rules            []policyRuleBody `json:"rules,omitempty"`
	Activate         bool             `json:"activate,omitempty"`
}

// handleDefinePolicy appends an immutable policy version (AGENTS.md section 22.2).
func (s *Server) handleDefinePolicy(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleAdmin)
	if !ok {
		return
	}
	if s.policy == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleDefinePolicy",
			"policy is not configured on this server"))
		return
	}

	var req definePolicyRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	define := policy.DefineRequest{
		Scope:     scope,
		Principal: principalFrom(r.Context()).Ref(),
		Name:      req.Name,
		Notes:     req.Notes,
		Activate:  req.Activate,
	}
	if req.DefaultClearance != "" {
		parsed, err := domain.ParseClassification(req.DefaultClearance)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		define.DefaultClearance = parsed
	}
	for _, body := range req.Rules {
		rule, err := toPolicyRule(body)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		define.Rules = append(define.Rules, rule)
	}

	set, err := s.policy.Define(r.Context(), define)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, policySetJSON(set))
}

func toPolicyRule(body policyRuleBody) (domain.PolicyRule, error) {
	effect, err := domain.ParsePolicyEffect(body.Effect)
	if err != nil {
		return domain.PolicyRule{}, err
	}

	rule := domain.PolicyRule{
		Name:        body.Name,
		Description: body.Description,
		Effect:      effect,
		Purposes:    body.Purposes,
		EntityTypes: body.EntityTypes,
		Predicates:  body.Predicates,
		Residencies: body.Residencies,
		NotBefore:   body.NotBefore,
		NotAfter:    body.NotAfter,
	}

	for _, action := range body.Actions {
		parsed, err := domain.ParsePolicyAction(action)
		if err != nil {
			return domain.PolicyRule{}, err
		}
		rule.Actions = append(rule.Actions, parsed)
	}
	for _, id := range body.PrincipalIDs {
		rule.PrincipalIDs = append(rule.PrincipalIDs, domain.PrincipalID(id))
	}
	for _, kind := range body.PrincipalKinds {
		parsed, err := domain.ParsePrincipalKind(kind)
		if err != nil {
			return domain.PolicyRule{}, err
		}
		rule.PrincipalKinds = append(rule.PrincipalKinds, parsed)
	}
	for _, role := range body.Roles {
		parsed, err := domain.ParseRole(role)
		if err != nil {
			return domain.PolicyRule{}, err
		}
		rule.Roles = append(rule.Roles, parsed)
	}
	for _, id := range body.GraphSpaceIDs {
		rule.GraphSpaceIDs = append(rule.GraphSpaceIDs, domain.GraphSpaceID(id))
	}
	for _, id := range body.CollectionIDs {
		rule.CollectionIDs = append(rule.CollectionIDs, domain.CollectionID(id))
	}
	for _, id := range body.SourceIDs {
		rule.SourceIDs = append(rule.SourceIDs, domain.SourceID(id))
	}
	for _, classification := range body.Classifications {
		parsed, err := domain.ParseClassification(classification)
		if err != nil {
			return domain.PolicyRule{}, err
		}
		rule.Classifications = append(rule.Classifications, parsed)
	}
	for _, kind := range body.MemoryKinds {
		parsed, err := domain.ParseMemoryKind(kind)
		if err != nil {
			return domain.PolicyRule{}, err
		}
		rule.MemoryKinds = append(rule.MemoryKinds, parsed)
	}
	if body.MaxClassification != "" {
		parsed, err := domain.ParseClassification(body.MaxClassification)
		if err != nil {
			return domain.PolicyRule{}, err
		}
		rule.MaxClassification = parsed
	}
	return rule, nil
}

// handleActivePolicy reports the policy currently in force.
func (s *Server) handleActivePolicy(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleAdmin)
	if !ok {
		return
	}
	if s.policy == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleActivePolicy",
			"policy is not configured on this server"))
		return
	}

	set, err := s.policy.Active(r.Context(), scope.WorkspaceID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, policySetJSON(set))
}

// handleListPolicies returns the policy history.
func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleAdmin)
	if !ok {
		return
	}
	if s.policy == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleListPolicies",
			"policy is not configured on this server"))
		return
	}

	sets, err := s.policy.List(r.Context(), scope.WorkspaceID, intParam(r, "limit", 50))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	out := make([]map[string]any, 0, len(sets))
	for _, set := range sets {
		out = append(out, policySetJSON(set))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"policies": out, "count": len(out)})
}

type explainPolicyRequest struct {
	PrincipalID string `json:"principal_id"`
	Action      string `json:"action,omitempty"`
	Purpose     string `json:"purpose,omitempty"`
}

// handleExplainPolicy answers "what would this principal be allowed to see".
//
// A hypothetical, so it is not recorded as a decision. Filling the audit log with questions
// nobody acted on would make the log harder to read at exactly the moment it matters.
func (s *Server) handleExplainPolicy(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleAdmin)
	if !ok {
		return
	}
	if s.policy == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleExplainPolicy",
			"policy is not configured on this server"))
		return
	}

	var req explainPolicyRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	subject := principalFrom(r.Context())
	if req.PrincipalID != "" {
		resolved, err := s.identity.PrincipalForID(r.Context(), domain.PrincipalID(req.PrincipalID))
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		subject = resolved
	}

	action := domain.ActionRead
	if req.Action != "" {
		parsed, err := domain.ParsePolicyAction(req.Action)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		action = parsed
	}

	decision, set, err := s.policy.Explain(r.Context(), domain.AccessRequest{
		Principal: subject, Action: action, Scope: scope, Purpose: req.Purpose,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"principal_id":   req.PrincipalID,
		"action":         string(action),
		"allowed":        decision.Allowed,
		"rule":           decision.Rule,
		"reason":         decision.Reason,
		"policy_version": set.Version,
		"filters":        policyFiltersJSON(decision.Filters),
	})
}

type setClearanceRequest struct {
	MaxClassification string `json:"max_classification"`
}

// handleSetClearance records a principal's clearance ceiling in this workspace.
func (s *Server) handleSetClearance(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleAdmin)
	if !ok {
		return
	}
	if s.policy == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleSetClearance",
			"policy is not configured on this server"))
		return
	}

	var req setClearanceRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	var clearance domain.Classification
	if req.MaxClassification != "" {
		parsed, err := domain.ParseClassification(req.MaxClassification)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		clearance = parsed
	}

	principal := domain.PrincipalID(r.PathValue("principal_id"))
	if err := s.policy.SetClearance(r.Context(), scope, principalFrom(r.Context()).Ref(),
		principal, clearance); err != nil {
		s.writeError(w, r, err)
		return
	}

	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"principal_id":       string(principal),
		"max_classification": string(clearance),
	})
}

func policySetJSON(set domain.PolicySet) map[string]any {
	rules := make([]map[string]any, 0, len(set.Rules))
	for _, rule := range set.Rules {
		entry := map[string]any{"name": rule.Name, "effect": string(rule.Effect)}
		if len(rule.Actions) > 0 {
			actions := make([]string, 0, len(rule.Actions))
			for _, action := range rule.Actions {
				actions = append(actions, string(action))
			}
			entry["actions"] = actions
		}
		if len(rule.Roles) > 0 {
			roles := make([]string, 0, len(rule.Roles))
			for _, role := range rule.Roles {
				roles = append(roles, string(role))
			}
			entry["roles"] = roles
		}
		if len(rule.Classifications) > 0 {
			levels := make([]string, 0, len(rule.Classifications))
			for _, classification := range rule.Classifications {
				levels = append(levels, string(classification))
			}
			entry["classifications"] = levels
		}
		if rule.MaxClassification != "" {
			entry["max_classification"] = string(rule.MaxClassification)
		}
		if rule.Description != "" {
			entry["description"] = rule.Description
		}
		rules = append(rules, entry)
	}

	return map[string]any{
		"id":                string(set.ID),
		"version":           set.Version,
		"name":              set.Name,
		"notes":             set.Notes,
		"active":            set.Active,
		"default_clearance": string(set.DefaultClearance),
		"rules":             rules,
		"created_at":        set.CreatedAt,
	}
}

func policyFiltersJSON(filters domain.PolicyFilters) map[string]any {
	out := map[string]any{}
	if filters.MaxClassification != "" {
		out["max_classification"] = string(filters.MaxClassification)
	}
	permitted := filters.PermittedClassifications()
	levels := make([]string, 0, len(permitted))
	for _, classification := range permitted {
		levels = append(levels, string(classification))
	}
	out["permitted_classifications"] = levels

	if len(filters.DeniedSources) > 0 {
		out["denied_sources"] = idStringsOf(filters.DeniedSources)
	}
	if len(filters.AllowedSources) > 0 {
		out["allowed_sources"] = idStringsOf(filters.AllowedSources)
	}
	if len(filters.DeniedPredicates) > 0 {
		out["denied_predicates"] = filters.DeniedPredicates
	}
	if len(filters.AllowedPredicates) > 0 {
		out["allowed_predicates"] = filters.AllowedPredicates
	}
	if len(filters.DeniedEntityTypes) > 0 {
		out["denied_entity_types"] = filters.DeniedEntityTypes
	}
	return out
}

func idStringsOf[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}
