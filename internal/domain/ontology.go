package domain

import (
	"sort"
	"strings"
	"time"
)

// OntologyMode decides whether a graph space constrains what may be asserted in it
// (AGENTS.md section 8).
type OntologyMode string

const (
	// OntologyOpen accepts invented entity types and predicate names, normalizing them
	// through the registry. This is how a corpus is explored before anyone knows what
	// shape it has.
	OntologyOpen OntologyMode = "open"
	// OntologyGuided validates every claim against a bound ontology version. What the
	// schema does not describe does not silently become knowledge.
	OntologyGuided OntologyMode = "guided"
)

var ontologyModes = []OntologyMode{OntologyOpen, OntologyGuided}

func ParseOntologyMode(s string) (OntologyMode, error) {
	return parseEnum("ontology mode", s, ontologyModes)
}

// OntologyStatus tracks a version's place in the sequence.
type OntologyStatus string

const (
	// OntologyDraft can be validated against but not bound to a graph space.
	OntologyDraft OntologyStatus = "draft"
	// OntologyActive is bindable.
	OntologyActive OntologyStatus = "active"
	// OntologySuperseded was replaced. It stays readable because assertions validated
	// under it name it, and a claim whose schema cannot be looked up is a claim nobody
	// can re-check.
	OntologySuperseded OntologyStatus = "superseded"
)

var ontologyStatuses = []OntologyStatus{OntologyDraft, OntologyActive, OntologySuperseded}

func ParseOntologyStatus(s string) (OntologyStatus, error) {
	return parseEnum("ontology status", s, ontologyStatuses)
}

// EntityTypeDef names a kind of thing the ontology recognizes.
type EntityTypeDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Aliases are other spellings that mean this type. Sources disagree about whether a
	// company is an "organization", an "org", or a "company", and rejecting a claim over
	// that is pedantry rather than validation.
	Aliases []string `json:"aliases,omitempty"`
}

// PredicateConstraint is what an ontology says about one predicate.
//
// It carries the same semantics as a registry entry, because binding an ontology to a graph
// space should not create a second, competing description of what a predicate means.
type PredicateConstraint struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// SubjectTypes and ObjectTypes restrict which entity types may appear on each side.
	// Empty means any type the ontology defines.
	SubjectTypes []string `json:"subject_types,omitempty"`
	ObjectTypes  []string `json:"object_types,omitempty"`
	// ObjectKinds restricts the typed object column. Empty means any kind.
	ObjectKinds []ObjectKind `json:"object_kinds,omitempty"`
	// AllowedValues closes the vocabulary for symbol objects. Empty leaves it open.
	AllowedValues []string `json:"allowed_values,omitempty"`

	Functional        bool           `json:"functional,omitempty"`
	InverseFunctional bool           `json:"inverse_functional,omitempty"`
	Symmetric         bool           `json:"symmetric,omitempty"`
	Transitive        bool           `json:"transitive,omitempty"`
	TemporalPolicy    TemporalPolicy `json:"temporal_policy,omitempty"`
	ConflictPolicy    ConflictPolicy `json:"conflict_policy,omitempty"`
	DefaultMemoryKind MemoryKind     `json:"default_memory_kind,omitempty"`
	Sensitivity       Classification `json:"sensitivity,omitempty"`
}

// OntologyVersion is an immutable schema (AGENTS.md section 8).
//
// Immutable because assertions record the version they were validated under. Editing a
// version in place would silently change what those assertions were checked against, which
// is the same class of mistake as editing a committed claim.
type OntologyVersion struct {
	ID          OntologyVersionID
	WorkspaceID WorkspaceID
	Version     int
	Name        string
	Notes       string
	Status      OntologyStatus

	EntityTypes []EntityTypeDef
	Predicates  []PredicateConstraint

	CreatedAt time.Time
	CreatedBy PrincipalRef
}

func (o OntologyVersion) Validate() error {
	const op = "domain.OntologyVersion.Validate"

	if IsZero(o.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace is required")
	}
	if strings.TrimSpace(o.Name) == "" {
		return Errorf(CodeInvalidArgument, op, "an ontology version needs a name")
	}
	if len(o.EntityTypes) == 0 && len(o.Predicates) == 0 {
		// An empty schema in guided mode rejects everything, which looks like a broken
		// pipeline rather than a configuration mistake.
		return Errorf(CodeInvalidArgument, op,
			"an ontology version must define at least one entity type or predicate")
	}
	if _, err := ParseOntologyStatus(string(o.Status)); o.Status != "" && err != nil {
		return err
	}

	seenTypes := map[string]string{}
	for _, entityType := range o.EntityTypes {
		name := NormalizeEntityType(entityType.Name)
		if name == "" {
			return Errorf(CodeInvalidArgument, op, "an entity type needs a name")
		}
		if _, clash := seenTypes[name]; clash {
			return Errorf(CodeInvalidArgument, op, "entity type %q is defined twice", name)
		}
		seenTypes[name] = name
		for _, alias := range entityType.Aliases {
			normalized := NormalizeEntityType(alias)
			if normalized == "" {
				continue
			}
			if owner, clash := seenTypes[normalized]; clash && owner != name {
				return Errorf(CodeInvalidArgument, op,
					"alias %q maps to both %q and %q", normalized, owner, name)
			}
			seenTypes[normalized] = name
		}
	}

	seenPredicates := map[string]bool{}
	for _, predicate := range o.Predicates {
		name := NormalizePredicateName(predicate.Name)
		if name == "" {
			return Errorf(CodeInvalidArgument, op, "a predicate constraint needs a name")
		}
		if seenPredicates[name] {
			return Errorf(CodeInvalidArgument, op, "predicate %q is constrained twice", name)
		}
		seenPredicates[name] = true

		for _, kind := range predicate.ObjectKinds {
			if _, err := ParseObjectKind(string(kind)); err != nil {
				return err
			}
		}
		if predicate.TemporalPolicy != "" {
			if _, err := ParseTemporalPolicy(string(predicate.TemporalPolicy)); err != nil {
				return err
			}
		}
		if predicate.ConflictPolicy != "" {
			if _, err := ParseConflictPolicy(string(predicate.ConflictPolicy)); err != nil {
				return err
			}
		}
		// A type named on a predicate but never defined is almost always a typo, and in
		// guided mode it would reject every claim using that predicate.
		for _, side := range [][]string{predicate.SubjectTypes, predicate.ObjectTypes} {
			for _, typeName := range side {
				if _, known := seenTypes[NormalizeEntityType(typeName)]; !known {
					return Errorf(CodeInvalidArgument, op,
						"predicate %q references undefined entity type %q", name, typeName)
				}
			}
		}
	}
	return nil
}

// NormalizeEntityType produces the canonical form: lower_snake_case.
//
// Entity types come from models and from humans, and "Organization", "organisation", and
// "ORG" should not become three types. Predicate names use UPPER_SNAKE for the same reason
// in the other direction; what matters is that each has exactly one canonical form.
func NormalizeEntityType(name string) string {
	var b strings.Builder
	prevSeparator := true
	prevLower := false

	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'A' && r <= 'Z':
			if prevLower && !prevSeparator {
				b.WriteByte('_')
			}
			b.WriteRune(r + 32)
			prevSeparator, prevLower = false, false
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			prevSeparator, prevLower = false, r >= 'a' && r <= 'z'
		default:
			if !prevSeparator && b.Len() > 0 {
				b.WriteByte('_')
				prevSeparator = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

// OntologyBinding is what a graph space validates against.
type OntologyBinding struct {
	Mode    OntologyMode
	Version *OntologyVersion
}

// Guided reports whether claims in this space must satisfy a schema.
func (b OntologyBinding) Guided() bool { return b.Mode == OntologyGuided && b.Version != nil }

// ViolationDisposition decides what happens to a claim that fails validation.
//
// Both outcomes satisfy the rule that an invalid candidate is never silently committed; they
// differ in who can act on it. A caller stating a claim directly can fix and resend, so
// rejection returns the problem to them. A model proposing a claim has no such loop, and
// discarding its output would lose what the source actually said — so it is held, visible,
// for a human to accept or drop.
type ViolationDisposition string

const (
	DispositionReject     ViolationDisposition = "reject"
	DispositionQuarantine ViolationDisposition = "quarantine"
)

var dispositions = []ViolationDisposition{DispositionReject, DispositionQuarantine}

func ParseViolationDisposition(s string) (ViolationDisposition, error) {
	return parseEnum("violation disposition", s, dispositions)
}

// ViolationCode names one way a claim can fail its schema.
type ViolationCode string

const (
	ViolationUnknownEntityType ViolationCode = "unknown_entity_type"
	ViolationUnknownPredicate  ViolationCode = "unknown_predicate"
	ViolationSubjectType       ViolationCode = "subject_type_not_allowed"
	ViolationObjectType        ViolationCode = "object_type_not_allowed"
	ViolationObjectKind        ViolationCode = "object_kind_not_allowed"
	ViolationValue             ViolationCode = "value_not_allowed"
)

// Violation is one failed check, named specifically enough to act on.
type Violation struct {
	Code   ViolationCode `json:"code"`
	Detail string        `json:"detail"`
}

func (v Violation) String() string { return string(v.Code) + ": " + v.Detail }

// ClaimShape is what validation needs from a candidate claim.
//
// Deliberately not the claim itself: validation runs before entities are resolved and
// before an assertion exists, on candidates from extraction as readily as on requests from
// the API.
type ClaimShape struct {
	SubjectType string
	Predicate   string
	Object      AssertionObject
	// ObjectType is the entity type of an entity-valued object.
	ObjectType string
}

// Check validates a claim shape against this version.
//
// It returns every violation rather than the first. A candidate with a misspelled subject
// type and an out-of-vocabulary value has two problems, and reporting them one round trip
// at a time wastes everyone's time.
func (o OntologyVersion) Check(shape ClaimShape) []Violation {
	var violations []Violation

	types := o.typeIndex()
	subjectType := NormalizeEntityType(shape.SubjectType)
	if subjectType != "" {
		if _, known := types[subjectType]; !known {
			violations = append(violations, Violation{
				Code:   ViolationUnknownEntityType,
				Detail: "subject type " + quote(shape.SubjectType) + " is not in the ontology",
			})
		}
	}

	objectType := NormalizeEntityType(shape.ObjectType)
	if objectType != "" {
		if _, known := types[objectType]; !known {
			violations = append(violations, Violation{
				Code:   ViolationUnknownEntityType,
				Detail: "object type " + quote(shape.ObjectType) + " is not in the ontology",
			})
		}
	}

	predicateName := NormalizePredicateName(shape.Predicate)
	constraint, known := o.Predicate(predicateName)
	if !known {
		return append(violations, Violation{
			Code:   ViolationUnknownPredicate,
			Detail: "predicate " + quote(predicateName) + " is not in the ontology",
		})
	}

	if len(constraint.SubjectTypes) > 0 && subjectType != "" {
		if !containsType(types, constraint.SubjectTypes, subjectType) {
			violations = append(violations, Violation{
				Code: ViolationSubjectType,
				Detail: predicateName + " takes a subject of type " +
					strings.Join(constraint.SubjectTypes, " or ") + ", not " + quote(subjectType),
			})
		}
	}

	if shape.Object.Kind == ObjectEntity && len(constraint.ObjectTypes) > 0 && objectType != "" {
		if !containsType(types, constraint.ObjectTypes, objectType) {
			violations = append(violations, Violation{
				Code: ViolationObjectType,
				Detail: predicateName + " takes an object of type " +
					strings.Join(constraint.ObjectTypes, " or ") + ", not " + quote(objectType),
			})
		}
	}

	if len(constraint.ObjectKinds) > 0 && shape.Object.Kind != "" {
		allowed := false
		for _, kind := range constraint.ObjectKinds {
			if kind == shape.Object.Kind {
				allowed = true
				break
			}
		}
		if !allowed {
			kinds := make([]string, 0, len(constraint.ObjectKinds))
			for _, kind := range constraint.ObjectKinds {
				kinds = append(kinds, string(kind))
			}
			violations = append(violations, Violation{
				Code: ViolationObjectKind,
				Detail: predicateName + " takes an object of kind " +
					strings.Join(kinds, " or ") + ", not " + string(shape.Object.Kind),
			})
		}
	}

	if len(constraint.AllowedValues) > 0 && shape.Object.Kind == ObjectSymbol {
		allowed := false
		for _, value := range constraint.AllowedValues {
			if strings.EqualFold(value, shape.Object.Text) {
				allowed = true
				break
			}
		}
		if !allowed {
			violations = append(violations, Violation{
				Code: ViolationValue,
				Detail: quote(shape.Object.Text) + " is not one of " +
					strings.Join(constraint.AllowedValues, ", "),
			})
		}
	}
	return violations
}

// Predicate looks up a constraint by name, normalizing first.
func (o OntologyVersion) Predicate(name string) (PredicateConstraint, bool) {
	normalized := NormalizePredicateName(name)
	for _, constraint := range o.Predicates {
		if NormalizePredicateName(constraint.Name) == normalized {
			return constraint, true
		}
	}
	return PredicateConstraint{}, false
}

// ResolveEntityType maps a spelling onto a defined type, following aliases.
func (o OntologyVersion) ResolveEntityType(name string) (string, bool) {
	canonical, ok := o.typeIndex()[NormalizeEntityType(name)]
	return canonical, ok
}

// typeIndex maps every accepted spelling to its canonical type name.
func (o OntologyVersion) typeIndex() map[string]string {
	index := make(map[string]string, len(o.EntityTypes)*2)
	for _, entityType := range o.EntityTypes {
		canonical := NormalizeEntityType(entityType.Name)
		if canonical == "" {
			continue
		}
		index[canonical] = canonical
		for _, alias := range entityType.Aliases {
			if normalized := NormalizeEntityType(alias); normalized != "" {
				index[normalized] = canonical
			}
		}
	}
	return index
}

func containsType(index map[string]string, allowed []string, candidate string) bool {
	resolved, ok := index[candidate]
	if !ok {
		resolved = candidate
	}
	for _, name := range allowed {
		if index[NormalizeEntityType(name)] == resolved {
			return true
		}
	}
	return false
}

// EntityTypeNames returns the defined type names, sorted, for prompts and listings.
func (o OntologyVersion) EntityTypeNames() []string {
	names := make([]string, 0, len(o.EntityTypes))
	for _, entityType := range o.EntityTypes {
		if name := NormalizeEntityType(entityType.Name); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// PredicateNames returns the constrained predicate names, sorted.
func (o OntologyVersion) PredicateNames() []string {
	names := make([]string, 0, len(o.Predicates))
	for _, constraint := range o.Predicates {
		if name := NormalizePredicateName(constraint.Name); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ToPredicateDefinition renders a constraint as a registry entry.
//
// Binding an ontology registers its predicates, so open-mode discovery and guided-mode
// declaration end up in the same registry rather than two descriptions that can disagree.
func (c PredicateConstraint) ToPredicateDefinition(ws WorkspaceID) PredicateDefinition {
	definition := PredicateDefinition{
		WorkspaceID:       ws,
		Name:              NormalizePredicateName(c.Name),
		Description:       c.Description,
		SubjectTypes:      normalizeTypes(c.SubjectTypes),
		ObjectTypes:       normalizeTypes(c.ObjectTypes),
		ObjectKinds:       c.ObjectKinds,
		Functional:        c.Functional,
		InverseFunctional: c.InverseFunctional,
		Symmetric:         c.Symmetric,
		Transitive:        c.Transitive,
		TemporalPolicy:    c.TemporalPolicy,
		ConflictPolicy:    c.ConflictPolicy,
		DefaultMemoryKind: c.DefaultMemoryKind,
		Sensitivity:       c.Sensitivity,
		Status:            PredicateApproved,
	}
	if definition.TemporalPolicy == "" {
		definition.TemporalPolicy = TemporalPolicyStateful
	}
	if definition.ConflictPolicy == "" {
		// A declared predicate defaults to coexisting for the same reason a discovered
		// one does: assuming exclusivity invents contradictions that were never stated.
		definition.ConflictPolicy = ConflictPolicyCoexist
	}
	if definition.DefaultMemoryKind == "" {
		definition.DefaultMemoryKind = MemorySemantic
	}
	if definition.Sensitivity == "" {
		definition.Sensitivity = ClassificationInternal
	}
	return definition
}

func normalizeTypes(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if normalized := NormalizeEntityType(name); normalized != "" {
			out = append(out, normalized)
		}
	}
	return out
}

func quote(s string) string { return "\"" + s + "\"" }
