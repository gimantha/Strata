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
	"github.com/gimantha/strata/internal/resolution"
)

// Store is the ledger surface this service needs, declared by its consumer.
type Store interface {
	EnsurePredicate(ctx context.Context, ws domain.WorkspaceID, name string) (domain.PredicateDefinition, error)
	GetPredicateByName(ctx context.Context, ws domain.WorkspaceID, name string) (domain.PredicateDefinition, error)

	CreateEntity(ctx context.Context, e domain.Entity) (domain.Entity, error)
	GetEntity(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) (domain.Entity, error)
	FindEntitiesByName(ctx context.Context, scope domain.Scope, name string) ([]domain.Entity, error)
	CanonicalEntityID(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) (domain.EntityID, error)
	IdentityCluster(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) ([]domain.EntityID, error)

	CommitKnowledge(ctx context.Context, commit domain.KnowledgeCommit) (domain.KnowledgeCommitResult, error)
	GetAssertion(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) (domain.Assertion, error)
	QueryAssertions(ctx context.Context, q domain.AssertionQuery) ([]domain.Assertion, error)
	FindOverlappingClaims(ctx context.Context, a domain.Assertion) ([]domain.Assertion, error)
	SourceAuthority(ctx context.Context, ws domain.WorkspaceID, eventID domain.SourceEventID) (domain.TrustLevel, error)
	SupersedeAssertions(ctx context.Context, ws domain.WorkspaceID, ids []domain.AssertionID, at time.Time) ([]domain.AssertionID, error)
	SupersedeWithLink(ctx context.Context, ws domain.WorkspaceID, superseding domain.AssertionID, superseded []domain.AssertionID, at time.Time) ([]domain.AssertionID, error)
	RetractAssertion(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID, at time.Time, reason string, actor domain.PrincipalID) (domain.Assertion, error)
	ProvenanceChain(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) (domain.ProvenanceChain, error)
	CreateConflictSet(ctx context.Context, set domain.ConflictSet, members []domain.AssertionID) (domain.ConflictSet, error)
	GetSourceEvent(ctx context.Context, ws domain.WorkspaceID, id domain.SourceEventID) (domain.SourceEvent, error)
}

// Service commits and queries canonical knowledge.
type Service struct {
	store      Store
	resolver   Resolver
	reconciler *Reconciler
	now        func() time.Time
	logger     *slog.Logger
	tracer     trace.Tracer
}

// Resolver decides which identity a mention refers to. It is an interface so the service
// depends on the decision, not on how it was reached.
type Resolver interface {
	Resolve(ctx context.Context, scope domain.Scope, mention domain.Mention) (resolution.Result, error)
}

// Options configures the service.
type Options struct {
	// Now supplies knowledge time, injectable so temporal tests are deterministic.
	Now func() time.Time
	// Resolver decides identity. When absent the service builds the default one, which
	// keeps callers that do not care about resolution tuning from having to wire it.
	Resolver Resolver
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

	resolver := opts.Resolver
	if resolver == nil {
		if resolvable, ok := store.(resolution.Store); ok {
			resolver = resolution.New(resolvable, resolution.DefaultOptions(), logger)
		}
	}
	return &Service{
		store:      store,
		resolver:   resolver,
		reconciler: NewReconciler(store, logger),
		now:        now,
		logger:     logger,
		tracer:     tracer,
	}
}

// EntityRef names a subject or object entity.
//
// The fields beyond name exist because a name is weak evidence of identity. When a source
// supplies its own primary key or a business key, the resolver uses that instead, which is
// the difference between recognizing a record and guessing at one
// (AGENTS.md section 12.1).
type EntityRef struct {
	ID      domain.EntityID
	Name    string
	Type    string
	Aliases []string

	// SourceID and ExternalID are the upstream system's identity for this record.
	SourceID   *domain.SourceID
	ExternalID string

	// DomainKeys are configured business keys such as an email address.
	DomainKeys []domain.DomainKey
}

// toMention converts a reference into what the resolver works with.
func (r EntityRef) toMention(sourceEventID domain.SourceEventID) domain.Mention {
	return domain.Mention{
		Name:          r.Name,
		EntityType:    r.Type,
		Aliases:       r.Aliases,
		SourceID:      r.SourceID,
		ExternalID:    r.ExternalID,
		DomainKeys:    r.DomainKeys,
		SourceEventID: sourceEventID,
	}
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
	// SupersededOnArrival names claims that were already out of date when they arrived,
	// because the source had told us about a later state of the same record first. They
	// are recorded but never became current belief (AGENTS.md section 11.4).
	SupersededOnArrival []domain.AssertionID
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

	// Reconciliation runs after the commit: a claim has to exist before it can be compared
	// with what it might contradict, and the comparison never deletes either side
	// (AGENTS.md section 14.2).
	for _, assertion := range committed.Assertions {
		predicate, ok := predicates[assertion.Predicate.Name]
		if !ok {
			continue
		}

		outcome, err := s.reconciler.Reconcile(ctx, assertion.WithSourceID(event.SourceID),
			predicate, now)
		if err != nil {
			return AssertResult{}, err
		}

		result.Superseded = append(result.Superseded, outcome.Superseded...)
		if outcome.Conflict != nil {
			result.Conflicts = append(result.Conflicts, *outcome.Conflict)
		}
		if outcome.SupersededSelf {
			// A late arrival describing a state the source has already moved past. It is
			// recorded, because it is what the source said, but it was never current
			// belief and must not become it.
			if _, err := s.store.SupersedeAssertions(ctx, assertion.WorkspaceID,
				[]domain.AssertionID{assertion.ID}, now); err != nil {
				return AssertResult{}, err
			}
			result.SupersededOnArrival = append(result.SupersededOnArrival, assertion.ID)
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

	subject, err := s.resolveEntity(ctx, req.Scope, claim.Subject, req.SourceEventID)
	if err != nil {
		return domain.Assertion{}, nil, domain.PredicateDefinition{}, err
	}

	object := claim.Object
	if claim.ObjectEntity != nil {
		objectEntity, err := s.resolveEntity(ctx, req.Scope, *claim.ObjectEntity, req.SourceEventID)
		if err != nil {
			return domain.Assertion{}, nil, domain.PredicateDefinition{}, err
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
	// Identities are created by the resolver, which persists them along with the decision
	// that produced them, so nothing further is created as part of the commit.
	return assertion, nil, predicate, nil
}

// resolveEntity turns a reference into an identity.
//
// An explicit identifier is used as given. Everything else goes through the resolution
// ladder, which prefers stable keys over names and would rather create a duplicate than
// merge two identities that might not be the same thing. Duplicates are mergeable later;
// a wrong merge corrupts every fact about both (AGENTS.md section 12).
func (s *Service) resolveEntity(ctx context.Context, scope domain.Scope, ref EntityRef, sourceEventID domain.SourceEventID) (domain.EntityID, error) {
	const op = "knowledge.resolveEntity"

	if !domain.IsZero(ref.ID) {
		entity, err := s.store.GetEntity(ctx, scope.WorkspaceID, ref.ID)
		if err != nil {
			return "", err
		}
		if entity.GraphSpaceID != scope.GraphSpaceID {
			return "", domain.Errorf(domain.CodeNotFound, op, "entity not found in this graph space")
		}
		// Follow any merge, so a claim about a redirected identity lands on the one that
		// now represents it.
		return s.store.CanonicalEntityID(ctx, scope.WorkspaceID, entity.ID)
	}

	if s.resolver == nil {
		return "", domain.Errorf(domain.CodeInternal, op, "no entity resolver is configured")
	}
	if ref.Name == "" && ref.ExternalID == "" && len(ref.DomainKeys) == 0 {
		return "", domain.Errorf(domain.CodeInvalidArgument, op,
			"an entity reference needs an id, a name, or a key")
	}

	result, err := s.resolver.Resolve(ctx, scope, ref.toMention(sourceEventID))
	if err != nil {
		return "", err
	}
	return result.EntityID, nil
}

// Retract withdraws a claim without replacing it.
func (s *Service) Retract(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID, reason string, actor domain.PrincipalID) (domain.Assertion, error) {
	return s.store.RetractAssertion(ctx, ws, id, s.now().UTC(), reason, actor)
}

// Query answers temporal and structural questions about committed knowledge.
//
// Entity filters are expanded through identity clusters first. A merge redirects an
// identity without rewriting the assertions that named it, so asking about either side of a
// merge has to reach the facts recorded under both - otherwise merging two identities would
// appear to lose half of what is known about them (AGENTS.md section 12.3).
func (s *Service) Query(ctx context.Context, q domain.AssertionQuery) ([]domain.Assertion, error) {
	expandedSubjects, err := s.expandIdentities(ctx, q.Scope.WorkspaceID, q.SubjectIDs)
	if err != nil {
		return nil, err
	}
	q.SubjectIDs = expandedSubjects

	expandedObjects, err := s.expandIdentities(ctx, q.Scope.WorkspaceID, q.ObjectEntityIDs)
	if err != nil {
		return nil, err
	}
	q.ObjectEntityIDs = expandedObjects

	return s.store.QueryAssertions(ctx, q)
}

// expandIdentities replaces each identity with every identity that resolves to the same
// thing.
func (s *Service) expandIdentities(ctx context.Context, ws domain.WorkspaceID, ids []domain.EntityID) ([]domain.EntityID, error) {
	if len(ids) == 0 {
		return ids, nil
	}

	seen := map[domain.EntityID]bool{}
	out := make([]domain.EntityID, 0, len(ids))
	for _, id := range ids {
		cluster, err := s.store.IdentityCluster(ctx, ws, id)
		if err != nil {
			if domain.IsCode(err, domain.CodeNotFound) {
				// An unknown identity matches nothing, which a query should report as no
				// results rather than as an error.
				continue
			}
			return nil, err
		}
		for _, member := range cluster {
			if !seen[member] {
				seen[member] = true
				out = append(out, member)
			}
		}
	}
	return out, nil
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
