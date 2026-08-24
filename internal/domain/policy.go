package domain

import (
	"sort"
	"strings"
	"time"
)

// PolicyAction is what a principal is trying to do (AGENTS.md section 22.2).
//
// Coarse on purpose. A policy language with a hundred verbs is a policy language nobody can
// audit, and the interesting questions here are about *which data*, not which method.
type PolicyAction string

const (
	// ActionRead covers retrieval, context assembly, and reading canonical records.
	ActionRead PolicyAction = "read"
	// ActionWrite covers ingestion and asserting knowledge.
	ActionWrite PolicyAction = "write"
	// ActionExport covers bulk extraction of a graph space. Separate from read because
	// "can look something up" and "can take a copy of everything" are different powers.
	ActionExport PolicyAction = "export"
	// ActionAdmin covers configuration: policies, ontologies, streams, merges.
	ActionAdmin PolicyAction = "admin"
)

var policyActions = []PolicyAction{ActionRead, ActionWrite, ActionExport, ActionAdmin}

func ParsePolicyAction(s string) (PolicyAction, error) {
	return parseEnum("policy action", s, policyActions)
}

// PolicyEffect is what a matching rule does.
type PolicyEffect string

const (
	PolicyAllow PolicyEffect = "allow"
	PolicyDeny  PolicyEffect = "deny"
)

var policyEffects = []PolicyEffect{PolicyAllow, PolicyDeny}

func ParsePolicyEffect(s string) (PolicyEffect, error) {
	return parseEnum("policy effect", s, policyEffects)
}

// PolicyRule is one attribute-based grant or restriction.
//
// A rule matches when every populated condition matches; an empty condition means "any".
// Subject conditions describe who is asking, resource conditions describe what they are
// asking about, and the resource conditions double as query filters — which is the whole
// point. A policy that can only answer yes or no forces the "retrieve everything, hide the
// rest" pattern that AGENTS.md section 22.4 forbids.
type PolicyRule struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Effect      PolicyEffect   `json:"effect"`
	Actions     []PolicyAction `json:"actions,omitempty"`

	// Subject conditions.
	PrincipalIDs   []PrincipalID   `json:"principal_ids,omitempty"`
	PrincipalKinds []PrincipalKind `json:"principal_kinds,omitempty"`
	Roles          []Role          `json:"roles,omitempty"`
	// Purposes restricts a rule to a stated use — the caller says why it is asking, and a
	// rule can require that reason. An unstated purpose matches only rules that name none.
	Purposes []string `json:"purposes,omitempty"`

	// Resource conditions. These narrow queries as well as decide them.
	GraphSpaceIDs   []GraphSpaceID   `json:"graph_space_ids,omitempty"`
	CollectionIDs   []CollectionID   `json:"collection_ids,omitempty"`
	SourceIDs       []SourceID       `json:"source_ids,omitempty"`
	EntityTypes     []string         `json:"entity_types,omitempty"`
	Predicates      []string         `json:"predicates,omitempty"`
	Classifications []Classification `json:"classifications,omitempty"`
	MemoryKinds     []MemoryKind     `json:"memory_kinds,omitempty"`
	// Residencies restricts by where data may be held or served.
	Residencies []string `json:"residencies,omitempty"`

	// MaxClassification is a clearance ceiling rather than a match condition: an allow
	// rule carrying one permits everything up to that level, and the ceiling becomes a
	// query filter.
	MaxClassification Classification `json:"max_classification,omitempty"`

	// Time conditions, for temporary access and retention states.
	NotBefore *time.Time `json:"not_before,omitempty"`
	NotAfter  *time.Time `json:"not_after,omitempty"`
}

func (r PolicyRule) Validate() error {
	const op = "domain.PolicyRule.Validate"

	if strings.TrimSpace(r.Name) == "" {
		return Errorf(CodeInvalidArgument, op, "a rule needs a name")
	}
	if _, err := ParsePolicyEffect(string(r.Effect)); err != nil {
		return err
	}
	for _, action := range r.Actions {
		if _, err := ParsePolicyAction(string(action)); err != nil {
			return err
		}
	}
	for _, role := range r.Roles {
		if _, err := ParseRole(string(role)); err != nil {
			return err
		}
	}
	for _, kind := range r.PrincipalKinds {
		if _, err := ParsePrincipalKind(string(kind)); err != nil {
			return err
		}
	}
	for _, classification := range r.Classifications {
		if _, err := ParseClassification(string(classification)); err != nil {
			return err
		}
	}
	if r.MaxClassification != "" {
		if _, err := ParseClassification(string(r.MaxClassification)); err != nil {
			return err
		}
	}
	for _, kind := range r.MemoryKinds {
		if _, err := ParseMemoryKind(string(kind)); err != nil {
			return err
		}
	}
	if r.NotBefore != nil && r.NotAfter != nil && !r.NotAfter.After(*r.NotBefore) {
		return Errorf(CodeInvalidArgument, op,
			"rule %q is never in force: not_after is not later than not_before", r.Name)
	}
	return nil
}

// PolicySet is a workspace's rules, versioned like an ontology.
//
// Immutable and sequenced for the same reason: an audit record naming a policy version has
// to be checkable against what that version actually said.
type PolicySet struct {
	ID          PolicySetID
	WorkspaceID WorkspaceID
	Version     int
	Name        string
	Notes       string
	Active      bool

	// DefaultClearance is the ceiling for a principal no rule mentions. It is the setting
	// that decides whether this system is closed by default or open by default, so it is
	// stated once, visibly, rather than emerging from rule ordering.
	DefaultClearance Classification `json:"default_clearance"`

	Rules []PolicyRule

	CreatedAt time.Time
	CreatedBy PrincipalRef
}

func (p PolicySet) Validate() error {
	const op = "domain.PolicySet.Validate"

	if IsZero(p.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace is required")
	}
	if strings.TrimSpace(p.Name) == "" {
		return Errorf(CodeInvalidArgument, op, "a policy set needs a name")
	}
	if p.DefaultClearance != "" {
		if _, err := ParseClassification(string(p.DefaultClearance)); err != nil {
			return err
		}
	}

	seen := map[string]bool{}
	for _, rule := range p.Rules {
		if err := rule.Validate(); err != nil {
			return err
		}
		if seen[rule.Name] {
			return Errorf(CodeInvalidArgument, op, "rule %q is defined twice", rule.Name)
		}
		seen[rule.Name] = true
	}
	return nil
}

// AccessRequest is one authorization question.
type AccessRequest struct {
	Principal Principal
	Action    PolicyAction
	Scope     Scope
	// Purpose is the caller's stated reason, when the deployment asks for one.
	Purpose string
	// At is the instant to evaluate time conditions against. Zero means now, supplied by
	// the caller so evaluation stays a pure function.
	At time.Time
	// Residency is where the request is being served from, for data-residency rules.
	Residency string
}

// PolicyFilters are the narrowing a decision requires every query to apply.
//
// This is the part that matters. AGENTS.md section 22.4 forbids retrieving unauthorized data
// and hiding it afterwards, so a decision has to be expressible as query predicates rather
// than as a post-filter. Every field here is designed to become a SQL clause.
type PolicyFilters struct {
	// MaxClassification is the highest sensitivity this principal may see.
	MaxClassification Classification
	// DeniedClassifications are levels excluded regardless of the ceiling, for policies
	// that carve a hole rather than lower a ceiling.
	DeniedClassifications []Classification

	// AllowedSources, when non-empty, restricts reads to these sources. DeniedSources
	// excludes regardless.
	AllowedSources []SourceID
	DeniedSources  []SourceID

	AllowedCollections []CollectionID
	DeniedCollections  []CollectionID

	AllowedEntityTypes []string
	DeniedEntityTypes  []string

	AllowedPredicates []string
	DeniedPredicates  []string

	AllowedMemoryKinds []MemoryKind
	DeniedMemoryKinds  []MemoryKind
}

// Restrictive reports whether these filters narrow anything at all.
func (f PolicyFilters) Restrictive() bool {
	return f.MaxClassification != "" || len(f.DeniedClassifications) > 0 ||
		len(f.AllowedSources) > 0 || len(f.DeniedSources) > 0 ||
		len(f.AllowedCollections) > 0 || len(f.DeniedCollections) > 0 ||
		len(f.AllowedEntityTypes) > 0 || len(f.DeniedEntityTypes) > 0 ||
		len(f.AllowedPredicates) > 0 || len(f.DeniedPredicates) > 0 ||
		len(f.AllowedMemoryKinds) > 0 || len(f.DeniedMemoryKinds) > 0
}

// PermittedClassifications expands the ceiling and exclusions into the concrete set a query
// may match, which is what a SQL `= ANY(...)` needs.
func (f PolicyFilters) PermittedClassifications() []Classification {
	ceiling := f.MaxClassification
	if ceiling == "" {
		ceiling = ClassificationSecret
	}

	denied := map[Classification]bool{}
	for _, classification := range f.DeniedClassifications {
		denied[classification] = true
	}

	out := make([]Classification, 0, len(classifications))
	for _, classification := range classifications {
		if classification.rank() <= ceiling.rank() && !denied[classification] {
			out = append(out, classification)
		}
	}
	return out
}

// Allows reports whether these filters permit a record with the given attributes.
//
// The filters are enforced in SQL; this is for the places that hold a record already and
// must decide whether it may be shown — hydration of a canonical record, an export row, a
// citation. Belt and braces: if a query somewhere forgets to narrow, this still refuses.
func (f PolicyFilters) Allows(classification Classification, source SourceID, predicate string, kind MemoryKind, entityType string) bool {
	if f.MaxClassification != "" && classification != "" &&
		classification.rank() > f.MaxClassification.rank() {
		return false
	}
	if containsValue(f.DeniedClassifications, classification) {
		return false
	}
	if source != "" {
		if containsValue(f.DeniedSources, source) {
			return false
		}
		if len(f.AllowedSources) > 0 && !containsValue(f.AllowedSources, source) {
			return false
		}
	}
	if predicate != "" {
		normalized := NormalizePredicateName(predicate)
		if containsValue(f.DeniedPredicates, normalized) {
			return false
		}
		if len(f.AllowedPredicates) > 0 && !containsValue(f.AllowedPredicates, normalized) {
			return false
		}
	}
	if kind != "" {
		if containsValue(f.DeniedMemoryKinds, kind) {
			return false
		}
		if len(f.AllowedMemoryKinds) > 0 && !containsValue(f.AllowedMemoryKinds, kind) {
			return false
		}
	}
	if entityType != "" {
		normalized := NormalizeEntityType(entityType)
		if containsValue(f.DeniedEntityTypes, normalized) {
			return false
		}
		if len(f.AllowedEntityTypes) > 0 && !containsValue(f.AllowedEntityTypes, normalized) {
			return false
		}
	}
	return true
}

// Decision is the answer, with the narrowing that goes with it.
type Decision struct {
	Allowed bool
	// Rule names what decided it, so a refusal can be explained without reading the whole
	// policy set.
	Rule    string
	Reason  string
	Filters PolicyFilters
	// PolicyVersion records which policy set was evaluated, for the audit trail.
	PolicyVersion int
}

// Evaluate answers one access request against a policy set.
//
// The algorithm is deliberately boring: deny wins, then an explicit allow, then the role's
// baseline. Elaborate resolution schemes — most-specific-match, priority numbers, first-match
// ordering — are where policy bugs live, because two people reading the same rules reach
// different conclusions. Here, if any deny rule matches, the answer is no.
//
// Resource conditions on matching rules become filters rather than blanket refusals. A rule
// denying `restricted` data to readers does not refuse a reader's query; it lowers what the
// query may return, which is the difference between a system people can use and one they
// route around.
func (p PolicySet) Evaluate(req AccessRequest) Decision {
	at := req.At
	if at.IsZero() {
		at = time.Now().UTC()
	}

	role, hasGrant := req.Principal.GrantFor(req.Scope.WorkspaceID)
	decision := Decision{PolicyVersion: p.Version}

	// A grant is still the gate. Policy narrows what a principal may see inside a
	// workspace it already has access to; it never hands out access to one it does not
	// (AGENTS.md section 22.1).
	if !hasGrant && req.Principal.SystemRole != RoleOwner {
		decision.Reason = "no grant for this workspace"
		return decision
	}

	baseline := p.DefaultClearance
	if baseline == "" {
		// Unstated means internal: the level ordinary business data carries. Defaulting to
		// secret would make policy decorative, and defaulting to public would make a
		// fresh workspace useless.
		baseline = ClassificationInternal
	}
	filters := PolicyFilters{MaxClassification: baseline}

	var (
		allowed    bool
		allowedBy  string
		deniedBy   string
		denyReason string
		raised     Classification
	)

	for _, rule := range p.Rules {
		if !rule.matchesSubject(req, role, at) {
			continue
		}
		if !rule.coversAction(req.Action) {
			continue
		}

		if rule.Effect == PolicyDeny {
			// A deny with no resource conditions is a blanket refusal. A deny that names
			// resources is a restriction on what may be returned.
			if !rule.narrows() {
				deniedBy = rule.Name
				denyReason = "denied by rule " + rule.Name
				continue
			}
			rule.applyDeny(&filters)
			continue
		}

		allowed = true
		if allowedBy == "" {
			allowedBy = rule.Name
		}
		if rule.MaxClassification != "" && rule.MaxClassification.rank() > raised.rank() {
			raised = rule.MaxClassification
		}
		rule.applyAllow(&filters)
	}

	if deniedBy != "" {
		decision.Rule = deniedBy
		decision.Reason = denyReason
		return decision
	}
	if raised != "" && raised.rank() > filters.MaxClassification.rank() {
		filters.MaxClassification = raised
	}

	switch {
	case allowed:
		decision.Allowed = true
		decision.Rule = allowedBy
		decision.Reason = "allowed by rule " + allowedBy
	case roleAllows(role, req.Principal.SystemRole, req.Action):
		decision.Allowed = true
		decision.Reason = "allowed by the principal's role"
	default:
		decision.Reason = "the principal's role does not permit " + string(req.Action)
		return decision
	}

	decision.Filters = filters
	normalizeFilters(&decision.Filters)
	return decision
}

// roleAllows is the RBAC baseline policy narrows from (AGENTS.md section 22.2).
func roleAllows(role, systemRole Role, action PolicyAction) bool {
	effective := role
	if systemRole.rank() > effective.rank() {
		effective = systemRole
	}

	switch action {
	case ActionRead:
		return effective.rank() >= RoleReader.rank()
	case ActionWrite:
		return effective.rank() >= RoleWriter.rank()
	case ActionExport, ActionAdmin:
		// Export is an admin power. Being able to look something up is not the same as
		// being able to walk out with everything.
		return effective.rank() >= RoleAdmin.rank()
	default:
		return false
	}
}

func (r PolicyRule) matchesSubject(req AccessRequest, role Role, at time.Time) bool {
	if r.NotBefore != nil && at.Before(*r.NotBefore) {
		return false
	}
	if r.NotAfter != nil && !at.Before(*r.NotAfter) {
		return false
	}
	if len(r.PrincipalIDs) > 0 && !containsValue(r.PrincipalIDs, req.Principal.ID) {
		return false
	}
	if len(r.PrincipalKinds) > 0 && !containsValue(r.PrincipalKinds, req.Principal.Kind) {
		return false
	}
	if len(r.Roles) > 0 && !containsValue(r.Roles, role) {
		return false
	}
	if len(r.Purposes) > 0 && !containsFold(r.Purposes, req.Purpose) {
		return false
	}
	if len(r.Residencies) > 0 && !containsFold(r.Residencies, req.Residency) {
		return false
	}
	if len(r.GraphSpaceIDs) > 0 && !containsValue(r.GraphSpaceIDs, req.Scope.GraphSpaceID) {
		return false
	}
	return true
}

func (r PolicyRule) coversAction(action PolicyAction) bool {
	if len(r.Actions) == 0 {
		return true
	}
	return containsValue(r.Actions, action)
}

// narrows reports whether a rule names resources, which is what makes it a filter rather
// than a verdict.
func (r PolicyRule) narrows() bool {
	return len(r.SourceIDs) > 0 || len(r.CollectionIDs) > 0 || len(r.EntityTypes) > 0 ||
		len(r.Predicates) > 0 || len(r.Classifications) > 0 || len(r.MemoryKinds) > 0
}

func (r PolicyRule) applyDeny(filters *PolicyFilters) {
	filters.DeniedClassifications = append(filters.DeniedClassifications, r.Classifications...)
	filters.DeniedSources = append(filters.DeniedSources, r.SourceIDs...)
	filters.DeniedCollections = append(filters.DeniedCollections, r.CollectionIDs...)
	filters.DeniedEntityTypes = append(filters.DeniedEntityTypes, normalizeTypes(r.EntityTypes)...)
	filters.DeniedPredicates = append(filters.DeniedPredicates, normalizePredicates(r.Predicates)...)
	filters.DeniedMemoryKinds = append(filters.DeniedMemoryKinds, r.MemoryKinds...)
}

func (r PolicyRule) applyAllow(filters *PolicyFilters) {
	// An allow rule naming resources restricts to them. Two allow rules naming different
	// sources permit both, which is why these accumulate rather than intersect.
	filters.AllowedSources = append(filters.AllowedSources, r.SourceIDs...)
	filters.AllowedCollections = append(filters.AllowedCollections, r.CollectionIDs...)
	filters.AllowedEntityTypes = append(filters.AllowedEntityTypes, normalizeTypes(r.EntityTypes)...)
	filters.AllowedPredicates = append(filters.AllowedPredicates, normalizePredicates(r.Predicates)...)
	filters.AllowedMemoryKinds = append(filters.AllowedMemoryKinds, r.MemoryKinds...)
}

// normalizeFilters sorts and deduplicates, so two evaluations of the same policy produce
// byte-identical filters and therefore identical SQL and identical audit records.
func normalizeFilters(f *PolicyFilters) {
	f.DeniedClassifications = dedupe(f.DeniedClassifications)
	f.AllowedSources = dedupe(f.AllowedSources)
	f.DeniedSources = dedupe(f.DeniedSources)
	f.AllowedCollections = dedupe(f.AllowedCollections)
	f.DeniedCollections = dedupe(f.DeniedCollections)
	f.AllowedEntityTypes = dedupe(f.AllowedEntityTypes)
	f.DeniedEntityTypes = dedupe(f.DeniedEntityTypes)
	f.AllowedPredicates = dedupe(f.AllowedPredicates)
	f.DeniedPredicates = dedupe(f.DeniedPredicates)
	f.AllowedMemoryKinds = dedupe(f.AllowedMemoryKinds)
	f.DeniedMemoryKinds = dedupe(f.DeniedMemoryKinds)
}

func dedupe[T ~string](values []T) []T {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[T]struct{}, len(values))
	out := make([]T, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func containsValue[T comparable](values []T, want T) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

func normalizePredicates(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if normalized := NormalizePredicateName(name); normalized != "" {
			out = append(out, normalized)
		}
	}
	return out
}

// DefaultPolicySet is what a workspace has before anyone writes a policy: role-based access
// with an internal clearance ceiling.
//
// Named rather than implicit so "no policy configured" has a describable meaning that shows
// up in an audit record, instead of being an absence somebody has to interpret.
func DefaultPolicySet(ws WorkspaceID) PolicySet {
	return PolicySet{
		WorkspaceID:      ws,
		Version:          0,
		Name:             "default",
		Notes:            "role-based access with an internal clearance ceiling",
		Active:           true,
		DefaultClearance: ClassificationInternal,
	}
}
