// Package knowledge turns claims into committed canonical knowledge.
//
// It sits between ingestion and the ledger: it resolves predicates and subjects, decides
// whether a new claim contradicts an existing one, and commits everything atomically with
// its evidence. Extraction in phase 3 calls this; so does a human asserting a fact
// directly.
package knowledge

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gimantha/strata/internal/domain"
)

// Store is the ledger surface this service needs, declared by its consumer.
type Store interface {
	EnsurePredicate(ctx context.Context, ws domain.WorkspaceID, name string) (domain.PredicateDefinition, error)
	GetPredicateByName(ctx context.Context, ws domain.WorkspaceID, name string) (domain.PredicateDefinition, error)

	CreateEntity(ctx context.Context, e domain.Entity) (domain.Entity, error)
	GetEntity(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) (domain.Entity, error)
	FindEntitiesByName(ctx context.Context, scope domain.Scope, name string) ([]domain.Entity, error)

	CommitKnowledge(ctx context.Context, commit domain.KnowledgeCommit) (domain.KnowledgeCommitResult, error)
	GetAssertion(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) (domain.Assertion, error)
	QueryAssertions(ctx context.Context, q domain.AssertionQuery) ([]domain.Assertion, error)
	FindOverlappingClaims(ctx context.Context, a domain.Assertion) ([]domain.Assertion, error)
	RetractAssertion(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID, at time.Time, reason string, actor domain.PrincipalID) (domain.Assertion, error)
	ProvenanceChain(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) (domain.ProvenanceChain, error)
	CreateConflictSet(ctx context.Context, set domain.ConflictSet, members []domain.AssertionID) (domain.ConflictSet, error)
	GetSourceEvent(ctx context.Context, ws domain.WorkspaceID, id domain.SourceEventID) (domain.SourceEvent, error)
}

// Service commits and queries canonical knowledge.
type Service struct {
	store  Store
	now    func() time.Time
	logger *slog.Logger
	tracer trace.Tracer
}

// Options configures the service.
type Options struct {
	// Now supplies knowledge time, injectable so temporal tests are deterministic.
	Now func() time.Time
}

// New builds the service.
func New(store Store, opts Options, logger *slog.Logger, tracer trace.Tracer) *Service {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer("knowledge")
	}
	return &Service{store: store, now: now, logger: logger, tracer: tracer}
}

// EntityRef names a subject or object entity, either by identifier or by name.
type EntityRef struct {
	ID   domain.EntityID
	Name string
	Type string
}

// EvidenceInput points a claim at the source material behind it.
type EvidenceInput struct {
	EpisodeID     domain.EpisodeID
	ChunkID       *domain.ChunkID
	QuoteStart    *int
	QuoteEnd      *int
	ExtractedText string
	Confidence    float64
	ModelRunID    *domain.ModelRunID
}

// Claim is one fact to record.
type Claim struct {
	Subject   EntityRef
	Predicate string
	Object    domain.AssertionObject
	// ObjectEntity resolves an entity-valued object by name instead of identifier.
	ObjectEntity *EntityRef

	MemoryKind domain.MemoryKind
	ScopeKey   string

	EventTime     *time.Time
	ValidFrom     *time.Time
	ValidTo       *time.Time
	EffectiveFrom *time.Time
	EffectiveTo   *time.Time
	ActiveFrom    *time.Time
	ActiveUntil   *time.Time
	DecayStartsAt *time.Time
	ExpiresAt     *time.Time

	Confidence          float64
	ConfidenceBreakdown *domain.ConfidenceBreakdown
	ProvenanceMode      domain.ProvenanceMode
	Classification      domain.Classification

	Evidence []EvidenceInput
	// Supersedes names claims this one corrects. The correction and the supersession
	// commit together.
	Supersedes []domain.AssertionID

	// Quarantine holds a claim out of current belief without discarding it, for material
	// that cannot be trusted at face value (AGENTS.md section 24). A quarantined claim is
	// stored with its evidence and can be reviewed, but it is not believed and does not
	// contradict anything.
	Quarantine       bool
	QuarantineReason string
}

// DerivationInput describes what produced reasoned claims.
type DerivationInput struct {
	Method            string
	RuleName          string
	RuleVersion       string
	ModelRunID        *domain.ModelRunID
	InputAssertionIDs []domain.AssertionID
	Parameters        map[string]any
}

// AssertRequest commits a batch of claims from one source event.
type AssertRequest struct {
	Scope         domain.Scope
	Principal     domain.PrincipalRef
	SourceEventID domain.SourceEventID
	Claims        []Claim
	Derivation    *DerivationInput
}

// AssertResult reports what was committed.
type AssertResult struct {
	Assertions []domain.Assertion
	Entities   []domain.Entity
	Duplicates int
	Superseded []domain.AssertionID
	Conflicts  []domain.ConflictSet
}

// Assert commits claims to the ledger.
//
// Subjects are resolved, predicates registered, the claim written with its evidence, and
// anything it corrects marked superseded - all in one transaction. Conflicts are detected
// afterwards, because deciding a claim contradicts another requires the claim to exist.
func (s *Service) Assert(ctx context.Context, req AssertRequest) (AssertResult, error) {
	const op = "knowledge.Assert"

	ctx, span := s.tracer.Start(ctx, "knowledge.Assert", trace.WithAttributes(
		attribute.String("strata.workspace_id", string(req.Scope.WorkspaceID)),
		attribute.Int("strata.claims", len(req.Claims)),
	))
	defer span.End()

	if domain.IsZero(req.Scope.WorkspaceID) || domain.IsZero(req.Scope.GraphSpaceID) {
		return AssertResult{}, domain.Errorf(domain.CodeInternal, op,
			"scope was not resolved before asserting knowledge")
	}
	if len(req.Claims) == 0 {
		return AssertResult{}, domain.Errorf(domain.CodeInvalidArgument, op, "at least one claim is required")
	}
	if domain.IsZero(req.SourceEventID) {
		return AssertResult{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"source_event_id is required: knowledge must be traceable to an ingestion")
	}

	// The event must exist in this workspace. Without this a caller could attach claims
	// to another tenant's event and corrupt its provenance chain.
	event, err := s.store.GetSourceEvent(ctx, req.Scope.WorkspaceID, req.SourceEventID)
	if err != nil {
		return AssertResult{}, err
	}

	now := s.now().UTC()
	commit := domain.KnowledgeCommit{
		Scope:         req.Scope,
		SourceEventID: req.SourceEventID,
		Actor:         req.Principal,
		SupersededAt:  now,
	}

	var derivationID *domain.DerivationID
	if req.Derivation != nil {
		derivation := domain.Derivation{
			ID:                domain.NewDerivationID(),
			WorkspaceID:       req.Scope.WorkspaceID,
			GraphSpaceID:      req.Scope.GraphSpaceID,
			Method:            req.Derivation.Method,
			RuleName:          req.Derivation.RuleName,
			RuleVersion:       req.Derivation.RuleVersion,
			ModelRunID:        req.Derivation.ModelRunID,
			InputAssertionIDs: req.Derivation.InputAssertionIDs,
			Parameters:        req.Derivation.Parameters,
		}
		if err := derivation.Validate(); err != nil {
			return AssertResult{}, err
		}
		commit.Derivations = append(commit.Derivations, derivation)
		derivationID = &derivation.ID
	}

	var (
		result     AssertResult
		predicates = map[string]domain.PredicateDefinition{}
	)

	for i, claim := range req.Claims {
		built, created, predicate, err := s.buildAssertion(ctx, req, claim, event, derivationID, now)
		if err != nil {
			return AssertResult{}, domain.Wrap(err, domain.CodeOf(err), op,
				"claim "+itoa(i)+" could not be prepared")
		}
		predicates[predicate.Name] = predicate
		commit.Entities = append(commit.Entities, created...)

		evidence := make([]domain.Evidence, 0, len(claim.Evidence))
		for _, input := range claim.Evidence {
			evidence = append(evidence, domain.Evidence{
				WorkspaceID:   req.Scope.WorkspaceID,
				EpisodeID:     input.EpisodeID,
				ChunkID:       input.ChunkID,
				ArtifactID:    &event.RawArtifactID,
				SourceEventID: req.SourceEventID,
				QuoteStart:    input.QuoteStart,
				QuoteEnd:      input.QuoteEnd,
				ExtractedText: input.ExtractedText,
				ModelRunID:    input.ModelRunID,
				Confidence:    orOne(input.Confidence),
			})
		}

		commit.Assertions = append(commit.Assertions, domain.AssertionCommit{
			Assertion:     built,
			Evidence:      evidence,
			SupersedesIDs: claim.Supersedes,
		})
	}

	committed, err := s.store.CommitKnowledge(ctx, commit)
	if err != nil {
		return AssertResult{}, err
	}
	result.Assertions = committed.Assertions
	result.Entities = committed.Entities
	result.Duplicates = committed.Duplicates
	result.Superseded = committed.Superseded

	// Conflict detection runs after the commit: a claim has to exist before it can be
	// compared with what it might contradict, and the comparison must never delete
	// either side (AGENTS.md section 14.2).
	for _, assertion := range committed.Assertions {
		// A quarantined claim is not believed, so it cannot contradict anything. Letting
		// it open conflict sets would let untrusted material cast doubt on good knowledge.
		if assertion.Status == domain.AssertionQuarantined {
			continue
		}
		predicate, ok := predicates[assertion.Predicate.Name]
		if !ok || predicate.AllowsMultipleValues() {
			continue
		}
		conflict, err := s.reconcile(ctx, assertion, predicate, now)
		if err != nil {
			return AssertResult{}, err
		}
		if conflict != nil {
			result.Conflicts = append(result.Conflicts, *conflict)
		}
	}

	if s.logger != nil {
		s.logger.InfoContext(ctx, "knowledge committed",
			slog.Int("assertions", len(result.Assertions)),
			slog.Int("duplicates", result.Duplicates),
			slog.Int("superseded", len(result.Superseded)),
			slog.Int("conflicts", len(result.Conflicts)))
	}
	return result, nil
}

// buildAssertion resolves a claim's references and assembles the record to commit.
func (s *Service) buildAssertion(ctx context.Context, req AssertRequest, claim Claim, event domain.SourceEvent, derivationID *domain.DerivationID, now time.Time) (domain.Assertion, []domain.Entity, domain.PredicateDefinition, error) {
	const op = "knowledge.buildAssertion"

	var created []domain.Entity

	subject, newSubject, err := s.resolveEntity(ctx, req.Scope, claim.Subject)
	if err != nil {
		return domain.Assertion{}, nil, domain.PredicateDefinition{}, err
	}
	if newSubject != nil {
		created = append(created, *newSubject)
	}

	object := claim.Object
	if claim.ObjectEntity != nil {
		objectEntity, newObject, err := s.resolveEntity(ctx, req.Scope, *claim.ObjectEntity)
		if err != nil {
			return domain.Assertion{}, nil, domain.PredicateDefinition{}, err
		}
		if newObject != nil {
			created = append(created, *newObject)
		}
		object = domain.ObjectOfEntity(objectEntity)
	}

	predicate, err := s.store.EnsurePredicate(ctx, req.Scope.WorkspaceID, claim.Predicate)
	if err != nil {
		return domain.Assertion{}, nil, domain.PredicateDefinition{}, err
	}

	memoryKind := claim.MemoryKind
	if memoryKind == "" {
		memoryKind = predicate.DefaultMemoryKind
	}
	provenance := claim.ProvenanceMode
	if provenance == "" {
		provenance = domain.ProvenanceExtracted
	}
	// Sensitivity is inherited and may only be raised, never quietly lowered
	// (AGENTS.md section 22.3).
	classification := domain.MostRestrictive(
		domain.MostRestrictive(event.Classification, predicate.Sensitivity),
		claim.Classification)

	assertion := domain.Assertion{
		ID:           domain.NewAssertionID(),
		WorkspaceID:  req.Scope.WorkspaceID,
		GraphSpaceID: req.Scope.GraphSpaceID,
		SubjectID:    subject,
		Predicate:    predicate.Ref(),
		Object:       object,
		MemoryKind:   memoryKind,
		ScopeKey:     claim.ScopeKey,
		Temporal: domain.TemporalCoordinates{
			EventTime:     firstTime(claim.EventTime, event.EventTime),
			ValidFrom:     claim.ValidFrom,
			ValidTo:       claim.ValidTo,
			EffectiveFrom: claim.EffectiveFrom,
			EffectiveTo:   claim.EffectiveTo,
			// Knowledge time is the system's, never the caller's: observed when the
			// event arrived, recorded when this claim was written.
			ObservedAt:       event.ObservedAt,
			RecordedAt:       now,
			SourceTime:       event.SourceTime,
			SourceCommitTime: event.SourceCommitTime,
			SourceSequence:   event.SourceSequence,
			SourceVersion:    event.SourceVersion,
			ActiveFrom:       claim.ActiveFrom,
			ActiveUntil:      claim.ActiveUntil,
			DecayStartsAt:    claim.DecayStartsAt,
			ExpiresAt:        claim.ExpiresAt,
		}.Normalize(),
		Confidence:          orOne(claim.Confidence),
		ConfidenceBreakdown: claim.ConfidenceBreakdown,
		Status:              statusFor(claim),
		ProvenanceMode:      provenance,
		DerivationID:        derivationID,
		SourceEventID:       req.SourceEventID,
		Classification:      classification,
		CreatedBy:           req.Principal,
	}
	if len(claim.Supersedes) == 1 {
		// Record the chain on the claim itself so a reader can walk backwards without a
		// separate lookup.
		assertion.SupersedesID = &claim.Supersedes[0]
	}
	assertion.Fingerprint = assertion.ComputeFingerprint()

	if err := assertion.Validate(); err != nil {
		return domain.Assertion{}, nil, domain.PredicateDefinition{}, err
	}
	_ = op
	return assertion, created, predicate, nil
}

// resolveEntity turns a reference into an identity, creating one when the name is new.
//
// An ambiguous name is an error rather than a guess: silently picking one of several
// matching identities is how a knowledge graph quietly merges two different people.
// Real resolution, with aliases and embeddings, arrives in phase 4.
func (s *Service) resolveEntity(ctx context.Context, scope domain.Scope, ref EntityRef) (domain.EntityID, *domain.Entity, error) {
	const op = "knowledge.resolveEntity"

	if !domain.IsZero(ref.ID) {
		entity, err := s.store.GetEntity(ctx, scope.WorkspaceID, ref.ID)
		if err != nil {
			return "", nil, err
		}
		if entity.GraphSpaceID != scope.GraphSpaceID {
			return "", nil, domain.Errorf(domain.CodeNotFound, op, "entity not found in this graph space")
		}
		return entity.ID, nil, nil
	}

	if ref.Name == "" {
		return "", nil, domain.Errorf(domain.CodeInvalidArgument, op,
			"an entity reference needs an id or a name")
	}

	matches, err := s.store.FindEntitiesByName(ctx, scope, ref.Name)
	if err != nil {
		return "", nil, err
	}
	if ref.Type != "" {
		filtered := matches[:0]
		for _, m := range matches {
			if m.EntityType == ref.Type {
				filtered = append(filtered, m)
			}
		}
		matches = filtered
	}

	switch len(matches) {
	case 1:
		return matches[0].ID, nil, nil
	case 0:
		entityType := ref.Type
		if entityType == "" {
			entityType = "unknown"
		}
		entity := domain.Entity{
			ID:            domain.NewEntityID(),
			WorkspaceID:   scope.WorkspaceID,
			GraphSpaceID:  scope.GraphSpaceID,
			CanonicalName: ref.Name,
			EntityType:    entityType,
		}
		if err := entity.Validate(); err != nil {
			return "", nil, err
		}
		return entity.ID, &entity, nil
	default:
		return "", nil, domain.Errorf(domain.CodeConflict, op,
			"%q matches %d entities in this graph space; specify an entity id",
			ref.Name, len(matches))
	}
}

// reconcile decides what to do about a new claim that may contradict existing ones.
//
// Phase 2 handles the unambiguous cases: values that may coexist are left alone, and a
// predicate whose policy is latest-wins supersedes what it replaces. Anything else that
// genuinely overlaps becomes a conflict set, so the disagreement is recorded rather than
// resolved by guesswork. Authority-weighted and out-of-order resolution arrive with the
// full reconciler in phase 5 (AGENTS.md sections 14, 36).
func (s *Service) reconcile(ctx context.Context, assertion domain.Assertion, predicate domain.PredicateDefinition, now time.Time) (*domain.ConflictSet, error) {
	overlapping, err := s.store.FindOverlappingClaims(ctx, assertion)
	if err != nil {
		return nil, err
	}

	var competing []domain.AssertionID
	for _, other := range overlapping {
		// Same value is corroboration, not contradiction.
		if other.Object.Key() == assertion.Object.Key() {
			continue
		}
		competing = append(competing, other.ID)
	}
	if len(competing) == 0 {
		return nil, nil
	}

	if predicate.ConflictPolicy == domain.ConflictPolicyLatestWins {
		// The newer claim wins on knowledge time; the older one stays queryable as of
		// any earlier instant.
		if _, err := s.store.CommitKnowledge(ctx, domain.KnowledgeCommit{
			Scope:         domain.Scope{WorkspaceID: assertion.WorkspaceID, GraphSpaceID: assertion.GraphSpaceID},
			SourceEventID: assertion.SourceEventID,
			SupersededAt:  now,
			Assertions: []domain.AssertionCommit{{
				Assertion:     assertion,
				SupersedesIDs: competing,
			}},
		}); err != nil {
			return nil, err
		}
		return nil, nil
	}

	set, err := s.store.CreateConflictSet(ctx, domain.ConflictSet{
		WorkspaceID:  assertion.WorkspaceID,
		GraphSpaceID: assertion.GraphSpaceID,
		SubjectID:    assertion.SubjectID,
		Predicate:    assertion.Predicate.Name,
		ScopeKey:     assertion.ScopeKey,
		Reason: "overlapping values for a " + string(predicate.ConflictPolicy) +
			" predicate that does not permit multiple simultaneous values",
		Resolution: domain.ConflictOpen,
	}, append(competing, assertion.ID))
	if err != nil {
		return nil, err
	}

	if s.logger != nil {
		s.logger.WarnContext(ctx, "conflicting claims recorded rather than resolved",
			slog.String("subject_id", string(assertion.SubjectID)),
			slog.String("predicate", assertion.Predicate.Name),
			slog.String("conflict_set_id", string(set.ID)),
			slog.Int("claims", len(competing)+1))
	}
	return &set, nil
}

// Retract withdraws a claim without replacing it.
func (s *Service) Retract(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID, reason string, actor domain.PrincipalID) (domain.Assertion, error) {
	return s.store.RetractAssertion(ctx, ws, id, s.now().UTC(), reason, actor)
}

// Query answers temporal and structural questions about committed knowledge.
func (s *Service) Query(ctx context.Context, q domain.AssertionQuery) ([]domain.Assertion, error) {
	return s.store.QueryAssertions(ctx, q)
}

// Get loads one claim.
func (s *Service) Get(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) (domain.Assertion, error) {
	return s.store.GetAssertion(ctx, ws, id)
}

// Provenance walks a claim back to the source material behind it.
func (s *Service) Provenance(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) (domain.ProvenanceChain, error) {
	return s.store.ProvenanceChain(ctx, ws, id)
}

// statusFor decides the status a new claim is committed with.
func statusFor(claim Claim) domain.AssertionStatus {
	if claim.Quarantine {
		return domain.AssertionQuarantined
	}
	return domain.AssertionActive
}

func orOne(v float64) float64 {
	if v <= 0 {
		return 1
	}
	return v
}

func firstTime(values ...*time.Time) *time.Time {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}
