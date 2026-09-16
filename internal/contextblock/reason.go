package contextblock

import (
	"context"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

// deriveProofs computes only conclusions the typed ledger can establish exactly. These are
// query-time proofs with cited inputs, never new observed assertions.
func (h *hydrator) deriveProofs(ctx context.Context, candidates []candidate) ([]candidate, error) {
	// Proofs are derived about the question's subject. A delta between two budgets of
	// some other record, or an absence proof for it, would be marked required and shown
	// as if it answered the question.
	pool := focusedOnly(candidates)
	if hasRequirement(h.requirements, "comparison_inputs") {
		derived, err := h.comparisonProofs(ctx, pool)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, derived...)
	}
	if hasRequirement(h.requirements, "decision_trace") {
		candidates = append(candidates, h.decisionProofs(pool)...)
	}
	if hasRequirement(h.requirements, "absence_proof") && !expansionPolicyUnsupported(h.req.Policy) {
		derived, err := h.absenceProofs(ctx, pool)
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
		left, leftUnit, leftOK := numeric(first.assertion.Object)
		right, rightUnit, rightOK := numeric(last.assertion.Object)
		// Two amounts in different units are not a difference; two in the same unit are,
		// whatever kind the extractor happened to store them as.
		if !leftOK || !rightOK || !strings.EqualFold(leftUnit, rightUnit) || left.Cmp(right) == 0 {
			continue
		}
		delta := new(big.Rat).Sub(right, left)
		subject, err := h.entity(ctx, last.assertion.SubjectID)
		if err != nil {
			return nil, err
		}
		predicate := strings.ToLower(strings.ReplaceAll(last.assertion.Predicate.Name, "_", " "))
		deltaText := formatRat(delta)
		if leftUnit != "" {
			deltaText += " " + leftUnit
		}
		text := fmt.Sprintf("%s %s changed from %s to %s; exact delta %s",
			subject.CanonicalName, predicate, first.assertion.Object.Display(), last.assertion.Object.Display(), deltaText)
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

// numeric reads an object as an exact number with an optional unit.
//
// Typed numbers are taken as they are. Text is accepted too when it is a number with a unit
// or currency attached, because that is how extractors store money and measurements when
// the schema offers no unit field: "USD 12100" is a quantity, and refusing to subtract two
// of them because they are spelled as strings loses exactly the arithmetic a question about
// a change asks for. The unit is returned so mismatched quantities are never differenced.
func numeric(object domain.AssertionObject) (*big.Rat, string, bool) {
	switch object.Kind {
	case domain.ObjectInteger:
		return new(big.Rat).SetInt64(object.Integer), "", true
	case domain.ObjectDecimal:
		value, ok := new(big.Rat).SetString(object.Decimal)
		return value, "", ok
	case domain.ObjectDuration:
		return new(big.Rat).SetInt64(int64(object.Duration)), "", true
	case domain.ObjectString, domain.ObjectSymbol:
		return quantityOf(object.Text)
	default:
		return nil, "", false
	}
}

// quantity matches "USD 12,100", "$12100.50", "12100 USD", "45 %", and a bare number.
var quantity = regexp.MustCompile(`^\s*(?:([A-Z]{3}|[$€£¥])\s*)?(-?\d[\d,]*(?:\.\d+)?)(?:\s*([A-Z]{3}|%|[A-Za-z]+))?\s*$`)

func quantityOf(text string) (*big.Rat, string, bool) {
	match := quantity.FindStringSubmatch(text)
	if match == nil {
		return nil, "", false
	}
	unit := match[1]
	if unit == "" {
		unit = match[3]
	} else if match[3] != "" {
		// A prefix and a suffix is not a quantity: "USD 100 EUR" means nothing.
		return nil, "", false
	}
	value, ok := new(big.Rat).SetString(strings.ReplaceAll(match[2], ",", ""))
	if !ok {
		return nil, "", false
	}
	return value, unit, true
}

func formatRat(value *big.Rat) string {
	if value.IsInt() {
		return value.Num().String()
	}
	return value.FloatString(6)
}
