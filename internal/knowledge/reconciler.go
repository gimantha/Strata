package knowledge

import (
	"context"
	"log/slog"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

// Reconciler decides how a new claim relates to what is already believed.
//
// Its central job is to avoid two opposite mistakes. Treating every different value as a
// contradiction destroys legitimate multi-valued facts; treating none of them as
// contradictions lets a graph quietly hold both "the plan is enterprise" and "the plan is
// standard" as current truth. What separates the two cases is predicate semantics,
// overlapping validity, scope, and source authority - not the values themselves
// (AGENTS.md section 14.1).
type Reconciler struct {
	store  Store
	logger *slog.Logger
}

// NewReconciler builds a reconciler.
func NewReconciler(store Store, logger *slog.Logger) *Reconciler {
	return &Reconciler{store: store, logger: logger}
}

// Outcome describes what reconciliation did with a claim.
type Outcome struct {
	// Superseded are earlier claims this one replaced.
	Superseded []domain.AssertionID
	// SupersededSelf is set when the *new* claim was the one superseded, which happens
	// when a late event arrives describing a state the source has already moved past.
	SupersededSelf bool
	// Conflict is set when the disagreement could not be resolved and both claims were
	// kept.
	Conflict *domain.ConflictSet
	// Reason explains the outcome in one line, for logs and the decision trail.
	Reason string
}

// Reconcile compares a committed claim against the believable claims it might contradict.
func (r *Reconciler) Reconcile(ctx context.Context, assertion domain.Assertion, predicate domain.PredicateDefinition, now time.Time) (Outcome, error) {
	// A quarantined claim is not believed, so it cannot contradict anything. Letting it
	// dispute good knowledge would hand untrusted material a veto.
	if assertion.Status == domain.AssertionQuarantined {
		return Outcome{Reason: "quarantined claims do not participate in reconciliation"}, nil
	}
	// A predicate whose values may coexist has nothing to reconcile: liking tea does not
	// stop someone liking coffee.
	if predicate.AllowsMultipleValues() {
		return Outcome{Reason: "predicate permits simultaneous values"}, nil
	}

	overlapping, err := r.store.FindOverlappingClaims(ctx, assertion)
	if err != nil {
		return Outcome{}, err
	}

	var competing []domain.Assertion
	for _, other := range overlapping {
		// The same value is corroboration, not contradiction.
		if other.Object.Key() == assertion.Object.Key() {
			continue
		}
		// Claims whose validity does not actually overlap describe different periods and
		// are both true. The store filters on this, but a claim with no stated validity
		// overlaps everything, so the check is repeated here where the interval helpers
		// live.
		if !domain.ValidityOf(assertion.Temporal).Overlaps(domain.ValidityOf(other.Temporal)) {
			continue
		}
		competing = append(competing, other)
	}
	if len(competing) == 0 {
		return Outcome{Reason: "no competing claim overlaps this one"}, nil
	}

	// Out-of-order handling comes before policy. If the source's own ordering says this
	// claim describes an older state of the record, no policy should let it overwrite the
	// newer one - however it arrived, and however authoritative its source
	// (AGENTS.md section 11.4).
	if stale, superseder := r.isStale(assertion, competing); stale {
		return Outcome{
			SupersededSelf: true,
			Reason: "a later state of this record was already known, from source position " +
				sourceMarker(superseder),
		}, nil
	}

	// A source correcting itself is not a disagreement. If the same source already told us
	// something else about this slot and its own ordering puts that earlier, the new claim
	// is the correction and the old one is history — no policy, no authority weighting, no
	// conflict set. This is the counterpart of the staleness check above: if an older claim
	// from a source loses to a newer one, a newer claim from that source wins.
	//
	// Without it every ordinary database column update ends as a recorded contradiction
	// between two values the source never held simultaneously.
	var selfCorrected []domain.AssertionID
	if corrected, rest := r.partitionSelfCorrections(assertion, competing); len(corrected) > 0 {
		outcome, err := r.supersede(ctx, assertion, corrected, now,
			"the same source reported a later value for this record")
		if err != nil {
			return Outcome{}, err
		}
		if len(rest) == 0 {
			return outcome, nil
		}
		// Some competing claims came from elsewhere. Those are a real disagreement and
		// still go through policy, carrying the corrections already made.
		selfCorrected = outcome.Superseded
		competing = rest
	}

	var outcome Outcome
	switch predicate.ConflictPolicy {
	case domain.ConflictPolicyLatestWins:
		outcome, err = r.supersede(ctx, assertion, competing, now,
			"the predicate's policy is that the latest claim wins")

	case domain.ConflictPolicyHighestAuthority:
		outcome, err = r.resolveByAuthority(ctx, assertion, competing, now)

	default:
		// Functional predicates with no resolution policy, and anything explicitly marked
		// for manual review, keep both claims and record the disagreement.
		outcome, err = r.conflict(ctx, assertion, competing,
			"overlapping values for a predicate that does not permit them")
	}
	if err != nil {
		return Outcome{}, err
	}
	outcome.Superseded = append(selfCorrected, outcome.Superseded...)
	return outcome, nil
}

// isStale reports whether the source has already told us about a later state of this record.
//
// This is what makes redelivery order irrelevant. A CDC stream that sends update 102 before
// update 101 converges to the same state either way: whichever arrives second is compared
// against what is already known, and the older one loses regardless of when it showed up.
func (r *Reconciler) isStale(assertion domain.Assertion, competing []domain.Assertion) (bool, domain.Assertion) {
	position := domain.SourcePositionOf(assertion)
	position.SourceID = assertion.SourceID()
	if !position.Comparable() {
		return false, domain.Assertion{}
	}

	for _, other := range competing {
		otherPosition := domain.SourcePositionOf(other)
		otherPosition.SourceID = other.SourceID()
		if domain.CompareSourcePosition(position, otherPosition) == domain.OrderBefore {
			return true, other
		}
	}
	return false, domain.Assertion{}
}

// partitionSelfCorrections splits competing claims into ones this source has superseded by
// reporting a later value, and everything else.
func (r *Reconciler) partitionSelfCorrections(assertion domain.Assertion, competing []domain.Assertion) (corrected, rest []domain.Assertion) {
	position := domain.SourcePositionOf(assertion)
	position.SourceID = assertion.SourceID()
	if !position.Comparable() || position.SourceID == "" {
		return nil, competing
	}

	for _, other := range competing {
		otherPosition := domain.SourcePositionOf(other)
		otherPosition.SourceID = other.SourceID()

		sameSource := otherPosition.SourceID != "" && otherPosition.SourceID == position.SourceID
		if sameSource && domain.CompareSourcePosition(position, otherPosition) == domain.OrderAfter {
			corrected = append(corrected, other)
			continue
		}
		rest = append(rest, other)
	}
	return corrected, rest
}

// resolveByAuthority prefers the more trusted source, and refuses to choose between equals.
func (r *Reconciler) resolveByAuthority(ctx context.Context, assertion domain.Assertion, competing []domain.Assertion, now time.Time) (Outcome, error) {
	mine, err := r.store.SourceAuthority(ctx, assertion.WorkspaceID, assertion.SourceEventID)
	if err != nil {
		return Outcome{}, err
	}

	var (
		outranked []domain.Assertion
		blocked   []domain.Assertion
	)
	for _, other := range competing {
		theirs, err := r.store.SourceAuthority(ctx, other.WorkspaceID, other.SourceEventID)
		if err != nil {
			return Outcome{}, err
		}
		switch {
		case mine.MoreAuthoritativeThan(theirs):
			outranked = append(outranked, other)
		case theirs.MoreAuthoritativeThan(mine):
			// A more authoritative claim already stands. The new one is recorded but does
			// not displace it.
			blocked = append(blocked, other)
		default:
			// Equal authority settles nothing. Picking one would be arbitrary, and
			// arbitrary is exactly what a conflict set exists to avoid.
			blocked = append(blocked, other)
		}
	}

	if len(blocked) > 0 {
		return r.conflict(ctx, assertion, competing,
			"sources of equal or greater authority disagree")
	}
	return r.supersede(ctx, assertion, outranked, now,
		"this claim comes from a more authoritative source")
}

// supersede marks earlier claims as replaced.
func (r *Reconciler) supersede(ctx context.Context, assertion domain.Assertion, competing []domain.Assertion, now time.Time, reason string) (Outcome, error) {
	if len(competing) == 0 {
		return Outcome{Reason: reason}, nil
	}

	ids := make([]domain.AssertionID, 0, len(competing))
	for _, other := range competing {
		ids = append(ids, other.ID)
	}

	// Supersession is applied directly rather than by re-committing the claim. The claim
	// already exists, so a second commit would collide on its fingerprint and be treated
	// as a replay - taking the supersession with it.
	superseded, err := r.store.SupersedeWithLink(ctx, assertion.WorkspaceID, assertion.ID, ids, now)
	if err != nil {
		return Outcome{}, err
	}

	if r.logger != nil {
		r.logger.InfoContext(ctx, "earlier claims superseded",
			slog.String("predicate", assertion.Predicate.Name),
			slog.Int("superseded", len(superseded)),
			slog.String("reason", reason))
	}
	return Outcome{Superseded: superseded, Reason: reason}, nil
}

// conflict records a disagreement without resolving it.
func (r *Reconciler) conflict(ctx context.Context, assertion domain.Assertion, competing []domain.Assertion, reason string) (Outcome, error) {
	members := make([]domain.AssertionID, 0, len(competing)+1)
	for _, other := range competing {
		members = append(members, other.ID)
	}
	members = append(members, assertion.ID)

	set, err := r.store.CreateConflictSet(ctx, domain.ConflictSet{
		WorkspaceID:  assertion.WorkspaceID,
		GraphSpaceID: assertion.GraphSpaceID,
		SubjectID:    assertion.SubjectID,
		Predicate:    assertion.Predicate.Name,
		ScopeKey:     assertion.ScopeKey,
		Reason:       reason,
		Resolution:   domain.ConflictOpen,
	}, members)
	if err != nil {
		return Outcome{}, err
	}

	if r.logger != nil {
		r.logger.WarnContext(ctx, "conflicting claims recorded rather than resolved",
			slog.String("subject_id", string(assertion.SubjectID)),
			slog.String("predicate", assertion.Predicate.Name),
			slog.String("conflict_set_id", string(set.ID)),
			slog.String("reason", reason))
	}
	return Outcome{Conflict: &set, Reason: reason}, nil
}

// sourceMarker renders a claim's position for an explanation.
func sourceMarker(a domain.Assertion) string {
	switch {
	case a.Temporal.SourceSequence != "":
		return a.Temporal.SourceSequence
	case a.Temporal.SourceVersion != "":
		return a.Temporal.SourceVersion
	case a.Temporal.SourceCommitTime != nil:
		return a.Temporal.SourceCommitTime.UTC().Format(time.RFC3339)
	default:
		return "unknown"
	}
}
