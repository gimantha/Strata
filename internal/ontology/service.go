// Package ontology manages schema versions and the graph spaces bound to them
// (AGENTS.md section 8, phase 9).
//
// Two modes coexist. Open mode lets extraction invent entity types and predicate names,
// normalized through the registry, which is how a corpus is explored before anyone knows
// its shape. Guided mode validates every claim against a bound version, so what the schema
// does not describe does not silently become knowledge.
//
// The point of keeping both is that they are answers to different questions. A new corpus
// needs discovery; a production pipeline needs a contract. Forcing one mode on both makes
// exploration rigid or production sloppy.
package ontology

import (
	"context"
	"log/slog"

	"github.com/gimantha/strata/internal/domain"
)

// Store is the persistence surface, declared by its consumer.
type Store interface {
	CreateOntologyVersion(ctx context.Context, version domain.OntologyVersion) (domain.OntologyVersion, error)
	GetOntologyVersion(ctx context.Context, ws domain.WorkspaceID, id domain.OntologyVersionID) (domain.OntologyVersion, error)
	LatestOntologyVersion(ctx context.Context, ws domain.WorkspaceID) (domain.OntologyVersion, error)
	ListOntologyVersions(ctx context.Context, ws domain.WorkspaceID, limit int) ([]domain.OntologyVersion, error)
	SupersedeOntologyVersions(ctx context.Context, ws domain.WorkspaceID, keep domain.OntologyVersionID) error

	BindGraphSpace(ctx context.Context, ws domain.WorkspaceID, id domain.GraphSpaceID, mode domain.OntologyMode, version *domain.OntologyVersionID) error
	GraphSpaceBinding(ctx context.Context, ws domain.WorkspaceID, id domain.GraphSpaceID) (domain.OntologyBinding, error)
	GetGraphSpace(ctx context.Context, id domain.GraphSpaceID) (domain.GraphSpace, error)

	DefinePredicate(ctx context.Context, definition domain.PredicateDefinition, actor domain.PrincipalID) (domain.PredicateDefinition, error)
	GetEntity(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) (domain.Entity, error)
	QueryAssertions(ctx context.Context, q domain.AssertionQuery) ([]domain.Assertion, error)
}

// Service defines schema versions and reports what they would accept.
type Service struct {
	store  Store
	logger *slog.Logger
}

func New(store Store, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{store: store, logger: logger}
}

// DefineRequest creates a new immutable version.
type DefineRequest struct {
	Scope     domain.Scope
	Principal domain.PrincipalRef

	Name        string
	Notes       string
	EntityTypes []domain.EntityTypeDef
	Predicates  []domain.PredicateConstraint

	// Activate supersedes every other active version. A workspace usually wants exactly
	// one current schema, and leaving several active makes "the ontology" ambiguous.
	Activate bool
	// RegisterPredicates writes the constraints into the predicate registry, so guided
	// declaration and open-mode discovery share one description of what a predicate
	// means rather than maintaining two that can disagree.
	RegisterPredicates bool
}

// Define appends a version.
func (s *Service) Define(ctx context.Context, req DefineRequest) (domain.OntologyVersion, error) {
	version := domain.OntologyVersion{
		WorkspaceID: req.Scope.WorkspaceID,
		Name:        req.Name,
		Notes:       req.Notes,
		EntityTypes: req.EntityTypes,
		Predicates:  req.Predicates,
		Status:      domain.OntologyActive,
		CreatedBy:   req.Principal,
	}
	if !req.Activate {
		version.Status = domain.OntologyDraft
	}
	if err := version.Validate(); err != nil {
		return domain.OntologyVersion{}, err
	}

	created, err := s.store.CreateOntologyVersion(ctx, version)
	if err != nil {
		return domain.OntologyVersion{}, err
	}

	if req.Activate {
		if err := s.store.SupersedeOntologyVersions(ctx, req.Scope.WorkspaceID, created.ID); err != nil {
			return domain.OntologyVersion{}, err
		}
	}
	if req.RegisterPredicates {
		for _, constraint := range created.Predicates {
			definition := constraint.ToPredicateDefinition(req.Scope.WorkspaceID)
			if _, err := s.store.DefinePredicate(ctx, definition, req.Principal.ID); err != nil {
				return domain.OntologyVersion{}, err
			}
		}
	}

	s.logger.InfoContext(ctx, "ontology version defined",
		slog.Int("version", created.Version),
		slog.String("name", created.Name),
		slog.Int("entity_types", len(created.EntityTypes)),
		slog.Int("predicates", len(created.Predicates)),
		slog.String("status", string(created.Status)))
	return created, nil
}

// Get reads one version.
func (s *Service) Get(ctx context.Context, ws domain.WorkspaceID, id domain.OntologyVersionID) (domain.OntologyVersion, error) {
	return s.store.GetOntologyVersion(ctx, ws, id)
}

// Latest returns the newest version in a workspace.
func (s *Service) Latest(ctx context.Context, ws domain.WorkspaceID) (domain.OntologyVersion, error) {
	return s.store.LatestOntologyVersion(ctx, ws)
}

// List returns the schema history, newest first.
func (s *Service) List(ctx context.Context, ws domain.WorkspaceID, limit int) ([]domain.OntologyVersion, error) {
	return s.store.ListOntologyVersions(ctx, ws, limit)
}

// Binding reports what a graph space validates against.
func (s *Service) Binding(ctx context.Context, scope domain.Scope) (domain.OntologyBinding, error) {
	return s.store.GraphSpaceBinding(ctx, scope.WorkspaceID, scope.GraphSpaceID)
}

// Bind puts a graph space into open or guided mode.
//
// Binding is deliberately not retroactive: claims already committed keep the version they
// were checked against, or none. Re-validating history on a schema change would rewrite what
// the system believed at the time, which is the mistake the whole ledger is built to avoid.
// Report first with Validate, then decide.
func (s *Service) Bind(ctx context.Context, scope domain.Scope, mode domain.OntologyMode, versionID *domain.OntologyVersionID) error {
	const op = "ontology.Bind"

	if mode == domain.OntologyGuided {
		if versionID == nil {
			return domain.Errorf(domain.CodeInvalidArgument, op,
				"guided mode requires an ontology version")
		}
		version, err := s.store.GetOntologyVersion(ctx, scope.WorkspaceID, *versionID)
		if err != nil {
			return err
		}
		if version.Status == domain.OntologyDraft {
			// A draft is a proposal. Binding one would put claims under a schema nobody
			// has committed to.
			return domain.Errorf(domain.CodeInvalidArgument, op,
				"ontology version %d is a draft; activate it before binding",
				version.Version)
		}
	}

	if err := s.store.BindGraphSpace(ctx, scope.WorkspaceID, scope.GraphSpaceID, mode, versionID); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "graph space bound",
		slog.String("graph_space_id", string(scope.GraphSpaceID)),
		slog.String("mode", string(mode)))
	return nil
}

// ValidationReport says what a version would do to knowledge that already exists.
type ValidationReport struct {
	Version    domain.OntologyVersion
	Checked    int
	Conforming int
	Violations []AssertionViolation
	// ByCode counts violations by kind, which is what says whether a schema is nearly
	// right or fundamentally wrong about the corpus.
	ByCode map[domain.ViolationCode]int
}

// AssertionViolation is one existing claim a version would refuse.
type AssertionViolation struct {
	AssertionID domain.AssertionID
	Subject     string
	Predicate   string
	Object      string
	Violations  []domain.Violation
}

// Validate reports how committed knowledge measures up against a version.
//
// This is the migration tool. A schema change is cheap to declare and expensive to discover
// wrong, so the useful operation is not "apply" but "tell me what this would refuse" — run
// before binding, on real data, with the answer in hand.
//
// It never writes. Nothing is quarantined, superseded, or corrected by validating; the
// report is input to a decision, not the decision.
func (s *Service) Validate(ctx context.Context, scope domain.Scope, versionID domain.OntologyVersionID, limit int) (ValidationReport, error) {
	version, err := s.store.GetOntologyVersion(ctx, scope.WorkspaceID, versionID)
	if err != nil {
		return ValidationReport{}, err
	}

	if limit <= 0 || limit > domain.MaxAssertionLimit {
		limit = domain.MaxAssertionLimit
	}
	assertions, err := s.store.QueryAssertions(ctx, domain.AssertionQuery{
		Scope: scope,
		// Current belief only. A superseded claim was refused or replaced already, and
		// reporting it as a violation would drown the answer in history.
		Statuses: []domain.AssertionStatus{domain.AssertionActive, domain.AssertionDisputed},
		Limit:    limit,
	})
	if err != nil {
		return ValidationReport{}, err
	}

	report := ValidationReport{Version: version, ByCode: map[domain.ViolationCode]int{}}
	types := map[domain.EntityID]string{}

	for _, assertion := range assertions {
		report.Checked++

		subjectType, subjectName, err := s.describeEntity(ctx, scope, assertion.SubjectID, types)
		if err != nil {
			return ValidationReport{}, err
		}
		shape := domain.ClaimShape{
			SubjectType: subjectType,
			Predicate:   assertion.Predicate.Name,
			Object:      assertion.Object,
		}
		if assertion.Object.Kind == domain.ObjectEntity {
			objectType, _, err := s.describeEntity(ctx, scope, assertion.Object.EntityID, types)
			if err != nil {
				return ValidationReport{}, err
			}
			shape.ObjectType = objectType
		}

		violations := version.Check(shape)
		if len(violations) == 0 {
			report.Conforming++
			continue
		}
		for _, violation := range violations {
			report.ByCode[violation.Code]++
		}
		report.Violations = append(report.Violations, AssertionViolation{
			AssertionID: assertion.ID,
			Subject:     subjectName,
			Predicate:   assertion.Predicate.Name,
			Object:      assertion.Object.Display(),
			Violations:  violations,
		})
	}
	return report, nil
}

// describeEntity caches type and name lookups across a validation run.
func (s *Service) describeEntity(ctx context.Context, scope domain.Scope, id domain.EntityID, cache map[domain.EntityID]string) (string, string, error) {
	if domain.IsZero(id) {
		return "", "", nil
	}
	if cached, ok := cache[id]; ok {
		return cached, "", nil
	}
	entity, err := s.store.GetEntity(ctx, scope.WorkspaceID, id)
	if err != nil {
		if domain.IsCode(err, domain.CodeNotFound) {
			return "", "", nil
		}
		return "", "", err
	}
	cache[id] = entity.EntityType
	return entity.EntityType, entity.CanonicalName, nil
}
