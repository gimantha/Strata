// Package memory manages the lifecycle of knowledge: what is still in scope, what has
// decayed, and what repeated observation has turned into a stable fact
// (AGENTS.md section 21).
//
// The distinction this package exists to hold is that knowledge stops being *useful* long
// before it stops being *true*. A note that someone is staying at a hotel tonight is true
// forever and worth surfacing for a day. Every operation here touches the context clock or
// derives new claims; none of them edit what the ledger says happened.
package memory

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/projection"
)

// Store is the canonical surface this package reads and writes.
type Store interface {
	QueryAssertions(ctx context.Context, q domain.AssertionQuery) ([]domain.Assertion, error)
	GetAssertion(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) (domain.Assertion, error)
	GetSourceEvent(ctx context.Context, ws domain.WorkspaceID, id domain.SourceEventID) (domain.SourceEvent, error)
	DeactivateAssertion(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID, at time.Time, reason string, actor domain.PrincipalID) (domain.Assertion, error)
	ReactivateAssertion(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID, actor domain.PrincipalID) (domain.Assertion, error)
	GetCheckpoint(ctx context.Context, ws domain.WorkspaceID, projection string) (domain.ProjectionCheckpoint, error)
	SaveCheckpoint(ctx context.Context, checkpoint domain.ProjectionCheckpoint) error
}

// Committer writes derived knowledge through the ordinary path.
//
// Consolidation has no privileged route into the ledger: a derived fact is committed by the
// same code that commits an extracted one, so evidence, supersession, conflict detection, and
// ontology validation apply to conclusions exactly as they do to observations.
type Committer interface {
	Assert(ctx context.Context, req knowledge.AssertRequest) (knowledge.AssertResult, error)
}

// Projector refreshes the retrieval projections for one source event.
//
// Lifecycle changes have to reach retrieval or they have not happened: a claim that is
// "deactivated" in the ledger and still surfacing as current context is deactivated in name
// only. Projections are derived, so the way to update one is to re-derive it through the
// normal path rather than to patch the projected row.
type Projector interface {
	ProjectEvent(ctx context.Context, scope domain.Scope, eventID domain.SourceEventID) (projection.Stats, error)
}

// Options configure the service.
type Options struct {
	Rule domain.ConsolidationRule
	// HalfLife tunes decay scoring.
	HalfLife time.Duration
	Clock    func() time.Time
}

// Service consolidates, deactivates, and scores memory.
type Service struct {
	store     Store
	committer Committer
	projector Projector
	opts      Options
	logger    *slog.Logger
	tracer    trace.Tracer
}

func New(store Store, committer Committer, projector Projector, opts Options, logger *slog.Logger, tracer trace.Tracer) *Service {
	opts.Rule = opts.Rule.Normalize()
	if opts.HalfLife <= 0 {
		opts.HalfLife = domain.DecayHalfLife
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer("memory")
	}
	return &Service{
		store: store, committer: committer, projector: projector,
		opts: opts, logger: logger, tracer: tracer,
	}
}

// checkpointName is the consolidation job's cursor, kept alongside the projection
// checkpoints because it is the same kind of thing: a derived store's position in the ledger.
const checkpointName = "consolidation"

// ConsolidateRequest asks for one consolidation pass.
type ConsolidateRequest struct {
	Scope     domain.Scope
	Principal domain.PrincipalRef
	// Rule overrides the service default for this pass.
	Rule domain.ConsolidationRule
	// Limit bounds how many observations are examined.
	Limit int
	// DryRun reports what would be derived without writing anything, which is how a rule
	// change is evaluated before it is adopted.
	DryRun bool
}

// ConsolidateResult reports what a pass found.
type ConsolidateResult struct {
	Examined int
	Groups   int
	// Qualified are groups that met the rule, whether or not they were written.
	Qualified []domain.ObservationGroup
	Derived   []domain.Assertion
	// Existing counts conclusions already drawn: consolidation is idempotent, and a
	// second pass over unchanged observations derives nothing new.
	Existing int
}

// Consolidate turns repeated observation into stable semantic facts (AGENTS.md section 21.1).
//
// It groups episodic claims that say the same thing about the same subject, and where a group
// has been observed often enough, commits one derived assertion recording that conclusion —
// with a derivation naming every observation it rests on. The observations are left exactly
// as they are: consolidation adds a conclusion, it does not replace the evidence for it.
func (s *Service) Consolidate(ctx context.Context, req ConsolidateRequest) (ConsolidateResult, error) {
	ctx, span := s.tracer.Start(ctx, "memory.Consolidate", trace.WithAttributes(
		attribute.String("strata.workspace_id", string(req.Scope.WorkspaceID)),
		attribute.Bool("strata.dry_run", req.DryRun),
	))
	defer span.End()

	rule := req.Rule.Normalize()
	limit := req.Limit
	if limit <= 0 || limit > domain.MaxAssertionLimit {
		limit = domain.MaxAssertionLimit
	}

	observations, err := s.store.QueryAssertions(ctx, domain.AssertionQuery{
		Scope: req.Scope,
		// Only current belief. Consolidating a superseded observation would conclude
		// something from evidence the system has already stopped believing.
		Statuses:      []domain.AssertionStatus{domain.AssertionActive},
		MemoryKinds:   rule.Kinds,
		MinConfidence: rule.MinConfidence,
		Limit:         limit,
	})
	if err != nil {
		return ConsolidateResult{}, err
	}

	groups := s.group(ctx, req.Scope, observations)
	result := ConsolidateResult{Examined: len(observations), Groups: len(groups)}

	// Sorted, so a pass over the same data derives the same facts in the same order and a
	// dry run matches the write that follows it.
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		group := groups[key]
		if !group.Qualifies(rule) {
			continue
		}
		result.Qualified = append(result.Qualified, group)

		if req.DryRun {
			continue
		}

		derived, existed, err := s.derive(ctx, req, group)
		if err != nil {
			return ConsolidateResult{}, err
		}
		if existed {
			result.Existing++
			continue
		}
		result.Derived = append(result.Derived, derived...)
	}

	if !req.DryRun {
		s.advanceCheckpoint(ctx, req.Scope, len(result.Derived))
	}

	span.SetAttributes(
		attribute.Int("strata.examined", result.Examined),
		attribute.Int("strata.qualified", len(result.Qualified)),
		attribute.Int("strata.derived", len(result.Derived)),
	)
	s.logger.InfoContext(ctx, "consolidation pass complete",
		slog.Int("examined", result.Examined),
		slog.Int("groups", result.Groups),
		slog.Int("qualified", len(result.Qualified)),
		slog.Int("derived", len(result.Derived)),
		slog.Int("already_derived", result.Existing),
		slog.Bool("dry_run", req.DryRun))
	return result, nil
}

// group gathers observations that say the same thing.
func (s *Service) group(ctx context.Context, scope domain.Scope, observations []domain.Assertion) map[string]domain.ObservationGroup {
	groups := map[string]domain.ObservationGroup{}
	sources := map[domain.SourceEventID]domain.SourceID{}

	for _, observation := range observations {
		group := domain.ObservationGroup{
			SubjectID: observation.SubjectID,
			Predicate: observation.Predicate.Name,
			ScopeKey:  observation.ScopeKey,
			ObjectKey: observation.Object.Key(),
		}
		key := group.Key()

		existing, ok := groups[key]
		if !ok {
			existing = group
			existing.Sources = map[domain.SourceID]struct{}{}
		}
		existing.Members = append(existing.Members, observation)

		// Which source an observation came from decides whether repetition is
		// corroboration or one upstream talking to itself.
		source, known := sources[observation.SourceEventID]
		if !known {
			if event, err := s.store.GetSourceEvent(ctx, scope.WorkspaceID,
				observation.SourceEventID); err == nil {
				source = event.SourceID
			}
			sources[observation.SourceEventID] = source
		}
		if source != "" {
			existing.Sources[source] = struct{}{}
		}

		groups[key] = existing
	}
	return groups
}

// derive commits one consolidated fact.
//
// The derived claim is semantic, carries a derivation naming every observation behind it, and
// reuses the earliest observation's source event so provenance still reaches real source
// material. Nothing about the observations changes.
func (s *Service) derive(ctx context.Context, req ConsolidateRequest, group domain.ObservationGroup) ([]domain.Assertion, bool, error) {
	if len(group.Members) == 0 {
		return nil, false, nil
	}

	// Ordered by knowledge time so the derivation's inputs read as the sequence that
	// produced the conclusion, and so the fingerprint is stable across passes.
	members := append([]domain.Assertion(nil), group.Members...)
	sort.Slice(members, func(i, j int) bool {
		return members[i].Temporal.RecordedAt.Before(members[j].Temporal.RecordedAt)
	})

	inputs := make([]domain.AssertionID, 0, len(members))
	for _, member := range members {
		inputs = append(inputs, member.ID)
	}

	first := members[0]
	claim := knowledge.Claim{
		Subject:   knowledge.EntityRef{ID: first.SubjectID},
		Predicate: first.Predicate.Name,
		Object:    first.Object,
		ScopeKey:  first.ScopeKey,
		// Semantic, not episodic: the point of consolidation is that a pattern across
		// events has become a fact about the world rather than a record of occasions.
		MemoryKind:     domain.MemorySemantic,
		ProvenanceMode: domain.ProvenanceDerived,
		Confidence:     group.Confidence(),
		ValidFrom:      group.EarliestValidFrom(),
		Classification: first.Classification,
	}

	result, err := s.committer.Assert(ctx, knowledge.AssertRequest{
		Scope:         req.Scope,
		Principal:     req.Principal,
		SourceEventID: first.SourceEventID,
		Claims:        []knowledge.Claim{claim},
		Derivation: &knowledge.DerivationInput{
			Method:            DerivationMethod,
			RuleName:          "repeated-observation",
			RuleVersion:       ConsolidationRuleVersion,
			InputAssertionIDs: inputs,
			Parameters: map[string]any{
				"observations": len(members),
				"sources":      len(group.Sources),
				"summary":      group.Summary(),
			},
		},
	})
	if err != nil {
		return nil, false, err
	}

	// A second pass over unchanged observations produces the same claim, which collides on
	// its fingerprint. Idempotence comes from the ledger rather than from bookkeeping here.
	if result.Duplicates > 0 && len(result.Assertions) == result.Duplicates {
		return nil, true, nil
	}
	return result.Assertions, false, nil
}

// DerivationMethod names the mechanism on every consolidated claim, so "which facts did
// consolidation produce" is a query rather than an inference.
const DerivationMethod = "consolidation"

// ConsolidationRuleVersion identifies the derivation rule. Bump it when the grouping or
// confidence arithmetic changes in a way that should re-derive existing conclusions.
const ConsolidationRuleVersion = "1"

// advanceCheckpoint records that a pass ran.
func (s *Service) advanceCheckpoint(ctx context.Context, scope domain.Scope, derived int) {
	now := s.opts.Clock()
	checkpoint := domain.ProjectionCheckpoint{
		WorkspaceID:      scope.WorkspaceID,
		Projection:       checkpointName,
		LastRecordedAt:   &now,
		RecordsProjected: int64(derived),
	}
	if err := s.store.SaveCheckpoint(ctx, checkpoint); err != nil {
		// A lost checkpoint costs a redundant pass, not correctness: consolidation is
		// idempotent, so running it twice derives nothing the first run did not.
		s.logger.WarnContext(ctx, "cannot save the consolidation checkpoint",
			slog.String("error", err.Error()))
	}
}

// ForgetRequest asks for one lifecycle change.
type ForgetRequest struct {
	Scope       domain.Scope
	Actor       domain.PrincipalRef
	AssertionID domain.AssertionID
	Kind        domain.ForgetKind
	Reason      string
}

// Forget takes knowledge out of use in the way the caller names (AGENTS.md section 21.4).
//
// The kind is required and unvalued kinds are refused, because the four ways of forgetting
// differ in what survives: deactivation is reversible and keeps everything, retraction says
// the claim was wrong, and the two deletion workflows destroy records. Making the caller name
// which one they mean is the whole point of not having a `delete` flag.
func (s *Service) Forget(ctx context.Context, req ForgetRequest) (domain.Assertion, error) {
	const op = "memory.Forget"

	kind, err := domain.ParseForgetKind(string(req.Kind))
	if err != nil {
		return domain.Assertion{}, err
	}
	if kind.Destructive() {
		// Retention deletion and privacy erasure need erasure jobs, projection sweeps, and
		// their own authorization (AGENTS.md section 23). Refusing here is better than
		// quietly doing something weaker and letting a caller believe data was destroyed.
		return domain.Assertion{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"%s is a deletion workflow and is not available through this operation", kind)
	}
	if kind == domain.ForgetRetract {
		return domain.Assertion{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"retraction is a knowledge-time correction; use the retract endpoint")
	}

	if req.Reason == "" {
		// A deactivation with no reason is indistinguishable later from an accident.
		return domain.Assertion{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"a reason is required: deactivation is reversible and someone will want to know why")
	}

	deactivated, err := s.store.DeactivateAssertion(ctx, req.Scope.WorkspaceID, req.AssertionID,
		s.opts.Clock(), req.Reason, req.Actor.ID)
	if err != nil {
		return domain.Assertion{}, err
	}

	s.reproject(ctx, req.Scope, deactivated)
	s.logger.InfoContext(ctx, "assertion deactivated",
		slog.String("assertion_id", string(req.AssertionID)),
		slog.String("reason", req.Reason))
	return deactivated, nil
}

// Reactivate puts deactivated knowledge back in scope.
//
// The counterpart that makes deactivation safe to use. Without it, "soft" forgetting would be
// soft in name only, and operators would hesitate to use it for exactly the transient
// material it exists for.
func (s *Service) Reactivate(ctx context.Context, scope domain.Scope, actor domain.PrincipalRef, id domain.AssertionID) (domain.Assertion, error) {
	restored, err := s.store.ReactivateAssertion(ctx, scope.WorkspaceID, id, actor.ID)
	if err != nil {
		return domain.Assertion{}, err
	}
	s.reproject(ctx, scope, restored)
	s.logger.InfoContext(ctx, "assertion reactivated",
		slog.String("assertion_id", string(id)))
	return restored, nil
}

// reproject re-derives the projections for a claim whose lifecycle changed.
//
// Through the same path a rebuild uses, so there is no second implementation to drift. A
// failure here is logged rather than returned: the ledger change is committed and correct,
// and the projection catches up on the next pass — but it is a real gap in the meantime, so
// it is reported as an error rather than a note.
func (s *Service) reproject(ctx context.Context, scope domain.Scope, assertion domain.Assertion) {
	if s.projector == nil {
		s.logger.WarnContext(ctx, "no projector configured; retrieval will keep surfacing "+
			"this claim until the projections are rebuilt",
			slog.String("assertion_id", string(assertion.ID)))
		return
	}
	if domain.IsZero(assertion.SourceEventID) {
		return
	}

	eventScope := scope
	if domain.IsZero(eventScope.GraphSpaceID) {
		eventScope.GraphSpaceID = assertion.GraphSpaceID
	}
	if _, err := s.projector.ProjectEvent(ctx, eventScope, assertion.SourceEventID); err != nil {
		s.logger.ErrorContext(ctx, "lifecycle change did not reach the projections",
			slog.String("assertion_id", string(assertion.ID)),
			slog.String("error", err.Error()))
	}
}

// Score reports the ranking weight a claim's lifecycle currently carries, for tooling that
// wants to explain why an old memory ranks where it does.
func (s *Service) Score(assertion domain.Assertion) float64 {
	return domain.LifecycleOf(assertion.Temporal).DecayWeight(s.opts.Clock(), s.opts.HalfLife)
}
