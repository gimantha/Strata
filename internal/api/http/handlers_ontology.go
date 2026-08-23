package http

import (
	"net/http"
	"strconv"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ontology"
)

// intParam reads a positive integer query parameter, falling back to a default.
//
// A malformed value falls back rather than failing: a mistyped limit should not turn a
// read into an error the caller has to handle.
func intParam(r *http.Request, name string, fallback int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

type entityTypeBody struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Aliases     []string `json:"aliases,omitempty"`
}

type predicateConstraintBody struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	SubjectTypes  []string `json:"subject_types,omitempty"`
	ObjectTypes   []string `json:"object_types,omitempty"`
	ObjectKinds   []string `json:"object_kinds,omitempty"`
	AllowedValues []string `json:"allowed_values,omitempty"`

	Functional        bool   `json:"functional,omitempty"`
	InverseFunctional bool   `json:"inverse_functional,omitempty"`
	Symmetric         bool   `json:"symmetric,omitempty"`
	Transitive        bool   `json:"transitive,omitempty"`
	TemporalPolicy    string `json:"temporal_policy,omitempty"`
	ConflictPolicy    string `json:"conflict_policy,omitempty"`
	DefaultMemoryKind string `json:"default_memory_kind,omitempty"`
	Sensitivity       string `json:"sensitivity,omitempty"`
}

type defineOntologyRequest struct {
	Name        string                    `json:"name"`
	Notes       string                    `json:"notes,omitempty"`
	EntityTypes []entityTypeBody          `json:"entity_types,omitempty"`
	Predicates  []predicateConstraintBody `json:"predicates,omitempty"`

	Activate           bool `json:"activate,omitempty"`
	RegisterPredicates bool `json:"register_predicates,omitempty"`
}

// handleDefineOntology appends an immutable schema version (AGENTS.md section 8).
func (s *Server) handleDefineOntology(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleAdmin)
	if !ok {
		return
	}
	if s.ontology == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleDefineOntology",
			"ontology management is not configured on this server"))
		return
	}

	var req defineOntologyRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	define := ontology.DefineRequest{
		Scope:              scope,
		Principal:          principalFrom(r.Context()).Ref(),
		Name:               req.Name,
		Notes:              req.Notes,
		Activate:           req.Activate,
		RegisterPredicates: req.RegisterPredicates,
	}
	for _, entityType := range req.EntityTypes {
		define.EntityTypes = append(define.EntityTypes, domain.EntityTypeDef{
			Name: entityType.Name, Description: entityType.Description, Aliases: entityType.Aliases,
		})
	}
	for _, predicate := range req.Predicates {
		constraint, err := toConstraint(predicate)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		define.Predicates = append(define.Predicates, constraint)
	}

	version, err := s.ontology.Define(r.Context(), define)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusCreated, ontologyVersionJSON(version))
}

func toConstraint(body predicateConstraintBody) (domain.PredicateConstraint, error) {
	constraint := domain.PredicateConstraint{
		Name:              body.Name,
		Description:       body.Description,
		SubjectTypes:      body.SubjectTypes,
		ObjectTypes:       body.ObjectTypes,
		AllowedValues:     body.AllowedValues,
		Functional:        body.Functional,
		InverseFunctional: body.InverseFunctional,
		Symmetric:         body.Symmetric,
		Transitive:        body.Transitive,
	}

	for _, kind := range body.ObjectKinds {
		parsed, err := domain.ParseObjectKind(kind)
		if err != nil {
			return domain.PredicateConstraint{}, err
		}
		constraint.ObjectKinds = append(constraint.ObjectKinds, parsed)
	}
	if body.TemporalPolicy != "" {
		parsed, err := domain.ParseTemporalPolicy(body.TemporalPolicy)
		if err != nil {
			return domain.PredicateConstraint{}, err
		}
		constraint.TemporalPolicy = parsed
	}
	if body.ConflictPolicy != "" {
		parsed, err := domain.ParseConflictPolicy(body.ConflictPolicy)
		if err != nil {
			return domain.PredicateConstraint{}, err
		}
		constraint.ConflictPolicy = parsed
	}
	if body.DefaultMemoryKind != "" {
		parsed, err := domain.ParseMemoryKind(body.DefaultMemoryKind)
		if err != nil {
			return domain.PredicateConstraint{}, err
		}
		constraint.DefaultMemoryKind = parsed
	}
	if body.Sensitivity != "" {
		parsed, err := domain.ParseClassification(body.Sensitivity)
		if err != nil {
			return domain.PredicateConstraint{}, err
		}
		constraint.Sensitivity = parsed
	}
	return constraint, nil
}

// handleListOntologyVersions returns the schema history, newest first.
func (s *Server) handleListOntologyVersions(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleReader)
	if !ok {
		return
	}
	if s.ontology == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleListOntologyVersions",
			"ontology management is not configured on this server"))
		return
	}

	versions, err := s.ontology.List(r.Context(), scope.WorkspaceID, intParam(r, "limit", 50))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	out := make([]map[string]any, 0, len(versions))
	for _, version := range versions {
		out = append(out, ontologyVersionJSON(version))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"versions": out, "count": len(out)})
}

type bindOntologyRequest struct {
	Mode              string `json:"mode"`
	OntologyVersionID string `json:"ontology_version_id,omitempty"`
}

// handleBindOntology puts a graph space into open or guided mode.
func (s *Server) handleBindOntology(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleAdmin)
	if !ok {
		return
	}
	if s.ontology == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleBindOntology",
			"ontology management is not configured on this server"))
		return
	}

	var req bindOntologyRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	mode, err := domain.ParseOntologyMode(req.Mode)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var versionID *domain.OntologyVersionID
	if req.OntologyVersionID != "" {
		id := domain.OntologyVersionID(req.OntologyVersionID)
		versionID = &id
	}

	if err := s.ontology.Bind(r.Context(), scope, mode, versionID); err != nil {
		s.writeError(w, r, err)
		return
	}

	binding, err := s.ontology.Binding(r.Context(), scope)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, bindingJSON(binding))
}

// handleOntologyBinding reports what a graph space validates against.
func (s *Server) handleOntologyBinding(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleReader)
	if !ok {
		return
	}
	if s.ontology == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleOntologyBinding",
			"ontology management is not configured on this server"))
		return
	}

	binding, err := s.ontology.Binding(r.Context(), scope)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, bindingJSON(binding))
}

// handleValidateOntology reports what a version would refuse, without changing anything.
//
// The useful operation on a schema change is not "apply" but "tell me what this would
// break" — run on real data, before binding, with the answer in hand.
func (s *Server) handleValidateOntology(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleReader)
	if !ok {
		return
	}
	if s.ontology == nil {
		s.writeError(w, r, domain.Errorf(domain.CodeInternal, "http.handleValidateOntology",
			"ontology management is not configured on this server"))
		return
	}

	versionID := domain.OntologyVersionID(r.PathValue("ontology_version_id"))
	report, err := s.ontology.Validate(r.Context(), scope, versionID, intParam(r, "limit", 0))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	violations := make([]map[string]any, 0, len(report.Violations))
	for _, violation := range report.Violations {
		reasons := make([]map[string]string, 0, len(violation.Violations))
		for _, reason := range violation.Violations {
			reasons = append(reasons, map[string]string{
				"code": string(reason.Code), "detail": reason.Detail,
			})
		}
		violations = append(violations, map[string]any{
			"assertion_id": string(violation.AssertionID),
			"subject":      violation.Subject,
			"predicate":    violation.Predicate,
			"object":       violation.Object,
			"violations":   reasons,
		})
	}

	byCode := make(map[string]int, len(report.ByCode))
	for code, count := range report.ByCode {
		byCode[string(code)] = count
	}

	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"ontology_version": ontologyVersionJSON(report.Version),
		"checked":          report.Checked,
		"conforming":       report.Conforming,
		"violating":        len(report.Violations),
		"by_code":          byCode,
		"violations":       violations,
	})
}

func ontologyVersionJSON(version domain.OntologyVersion) map[string]any {
	entityTypes := make([]map[string]any, 0, len(version.EntityTypes))
	for _, entityType := range version.EntityTypes {
		entry := map[string]any{"name": domain.NormalizeEntityType(entityType.Name)}
		if entityType.Description != "" {
			entry["description"] = entityType.Description
		}
		if len(entityType.Aliases) > 0 {
			entry["aliases"] = entityType.Aliases
		}
		entityTypes = append(entityTypes, entry)
	}

	predicates := make([]map[string]any, 0, len(version.Predicates))
	for _, constraint := range version.Predicates {
		entry := map[string]any{"name": domain.NormalizePredicateName(constraint.Name)}
		if len(constraint.SubjectTypes) > 0 {
			entry["subject_types"] = constraint.SubjectTypes
		}
		if len(constraint.ObjectTypes) > 0 {
			entry["object_types"] = constraint.ObjectTypes
		}
		if len(constraint.ObjectKinds) > 0 {
			kinds := make([]string, 0, len(constraint.ObjectKinds))
			for _, kind := range constraint.ObjectKinds {
				kinds = append(kinds, string(kind))
			}
			entry["object_kinds"] = kinds
		}
		if len(constraint.AllowedValues) > 0 {
			entry["allowed_values"] = constraint.AllowedValues
		}
		if constraint.Functional {
			entry["functional"] = true
		}
		predicates = append(predicates, entry)
	}

	return map[string]any{
		"id":           string(version.ID),
		"version":      version.Version,
		"name":         version.Name,
		"notes":        version.Notes,
		"status":       string(version.Status),
		"entity_types": entityTypes,
		"predicates":   predicates,
		"created_at":   version.CreatedAt,
	}
}

func bindingJSON(binding domain.OntologyBinding) map[string]any {
	out := map[string]any{"mode": string(binding.Mode), "guided": binding.Guided()}
	if binding.Version != nil {
		out["ontology_version"] = ontologyVersionJSON(*binding.Version)
	}
	return out
}
