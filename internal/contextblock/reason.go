package contextblock

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

// deriveProofs computes only conclusions the typed ledger can establish exactly. These are
// query-time proofs with cited inputs, never new observed assertions.
func (h *hydrator) deriveProofs(ctx context.Context, candidates []candidate) ([]candidate, error) {
	if hasRequirement(h.requirements, "comparison_inputs") {
		derived, err := h.comparisonProofs(ctx, candidates)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, derived...)
	}
	if hasRequirement(h.requirements, "decision_trace") {
		candidates = append(candidates, h.decisionProofs(candidates)...)
	}
	if hasRequirement(h.requirements, "absence_proof") && !expansionPolicyUnsupported(h.req.Policy) {
		derived, err := h.absenceProofs(ctx, candidates)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, derived...)
	}
	return candidates, nil
}

func (h *hydrator) comparisonProofs(ctx context.Context, candidates []candidate) ([]candidate, error) {
	groups := assertionGroups(candidates)
	var out []candidate
	for slot, values := range groups {
		if len(values) < 2 {
			continue
		}
		sort.SliceStable(values, func(i, j int) bool {
			left, right := assertionTime(*values[i].assertion), assertionTime(*values[j].assertion)
			if left.Equal(right) {
				return values[i].assertion.ID < values[j].assertion.ID
			}
			return left.Before(right)
		})
		first, last := values[0], values[len(values)-1]
		left, leftOK := numeric(first.assertion.Object)
		right, rightOK := numeric(last.assertion.Object)
		if !leftOK || !rightOK || left.Cmp(right) == 0 {
			continue
		}
		delta := new(big.Rat).Sub(right, left)
		subject, err := h.entity(ctx, last.assertion.SubjectID)
		if err != nil {
			return nil, err
		}
		predicate := strings.ToLower(strings.ReplaceAll(last.assertion.Predicate.Name, "_", " "))
		text := fmt.Sprintf("%s %s changed from %s to %s; exact delta %s",
			subject.CanonicalName, predicate, first.assertion.Object.Display(), last.assertion.Object.Display(), formatRat(delta))
		out = append(out, proofCandidate("comparison:"+slot, text,
			[]domain.AssertionID{first.assertion.ID, last.assertion.ID}))
	}
	return out, nil
}

func (h *hydrator) decisionProofs(candidates []candidate) []candidate {
	var out []candidate
	seen := map[domain.DerivationID]bool{}
	for _, value := range candidates {
		if value.derivation == nil || value.assertion == nil || len(value.supportIDs) == 0 || seen[value.derivation.ID] {
			continue
		}
		seen[value.derivation.ID] = true
		method := value.derivation.Method
		if value.derivation.RuleName != "" {
			method += " rule " + value.derivation.RuleName
		}
		text := fmt.Sprintf("%s was derived by %s from %d cited assertion", value.text, method, len(value.supportIDs))
		if len(value.supportIDs) != 1 {
			text += "s"
		}
		inputs := append([]domain.AssertionID(nil), value.supportIDs...)
		inputs = append(inputs, value.assertion.ID)
		out = append(out, proofCandidate("decision:"+string(value.derivation.ID), text, inputs))
	}
	return out
}

func (h *hydrator) absenceProofs(ctx context.Context, candidates []candidate) ([]candidate, error) {
	groups := assertionGroups(candidates)
	var out []candidate
	for slot, history := range groups {
		if len(history) == 0 {
			continue
		}
		a := history[0].assertion
		at := h.asOf()
		current, err := h.store.QueryAssertions(ctx, domain.AssertionQuery{
			Scope: h.req.Scope, SubjectIDs: []domain.EntityID{a.SubjectID},
			Predicates: []string{a.Predicate.Name}, ScopeKey: a.ScopeKey,
			Statuses: []domain.AssertionStatus{domain.AssertionActive, domain.AssertionDisputed},
			ValidAt:  &at, ActiveAt: h.req.Temporal.ActiveAt,
			Classifications: h.req.Policy.PermittedClassifications(), SourceIDs: h.req.Policy.AllowedSources,
			MemoryKinds: h.req.Policy.AllowedMemoryKinds, Limit: 1,
		})
		if err != nil {
			return nil, err
		}
		if len(current) > 0 {
			continue
		}
		subject, err := h.entity(ctx, a.SubjectID)
		if err != nil {
			return nil, err
		}
		predicate := strings.ToLower(strings.ReplaceAll(a.Predicate.Name, "_", " "))
		inputs := make([]domain.AssertionID, 0, len(history))
		for _, value := range history {
			inputs = append(inputs, value.assertion.ID)
		}
		text := fmt.Sprintf("No active %s assertion was found for %s in canonical slot %q at %s; the check inspected %d historical assertion(s) in this graph space",
			predicate, subject.CanonicalName, domain.DisplayContextQualifiers(a.ScopeKey), h.asOf().UTC().Format(time.RFC3339), len(inputs))
		out = append(out, proofCandidate("absence:"+slot, text, inputs))
	}
	return out, nil
}

func assertionGroups(candidates []candidate) map[string][]candidate {
	out := map[string][]candidate{}
	seen := map[string]bool{}
	for _, value := range candidates {
		if value.assertion == nil || seen[string(value.assertion.ID)] {
			continue
		}
		seen[string(value.assertion.ID)] = true
		out[value.slot] = append(out[value.slot], value)
	}
	return out
}

func proofCandidate(id, text string, supports []domain.AssertionID) candidate {
	return candidate{
		section: domain.SectionProofs, text: text, surface: domain.SurfaceProof, recordID: id,
		relevance: 1, confidence: 1, evidence: 1, temporal: 1, priority: 1, required: true,
		citation: domain.Citation{Surface: domain.SurfaceProof, SupportAssertionIDs: supports},
		terms:    terms(text),
	}
}

func assertionTime(a domain.Assertion) time.Time {
	for _, value := range []*time.Time{a.Temporal.EffectiveFrom, a.Temporal.ValidFrom, a.Temporal.EventTime} {
		if value != nil {
			return value.UTC()
		}
	}
	return a.Temporal.RecordedAt.UTC()
}

func numeric(object domain.AssertionObject) (*big.Rat, bool) {
	switch object.Kind {
	case domain.ObjectInteger:
		return new(big.Rat).SetInt64(object.Integer), true
	case domain.ObjectDecimal:
		value, ok := new(big.Rat).SetString(object.Decimal)
		return value, ok
	case domain.ObjectDuration:
		return new(big.Rat).SetInt64(int64(object.Duration)), true
	default:
		return nil, false
	}
}

func formatRat(value *big.Rat) string {
	if value.IsInt() {
		return value.Num().String()
	}
	return value.FloatString(6)
}
