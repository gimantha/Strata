package contextblock

import (
	"sort"

	"github.com/gimantha/strata/internal/domain"
)

// Weights tune budget-aware selection (AGENTS.md section 20.2).
//
// Every one of these is a constant chosen by measurement on the evaluation corpus, which
// means they are the part of this design most likely to be wrong somewhere else. They are
// exposed for that reason.
type Weights struct {
	// Relevance is how much the fused retrieval score counts.
	Relevance float64
	// Confidence, Evidence, Temporal, and Priority scale a candidate by properties of
	// the claim rather than of the match.
	Confidence float64
	Evidence   float64
	Temporal   float64
	Priority   float64

	// Diversity is the MMR trade-off: 0 ranks purely by relevance, 1 purely by novelty.
	Diversity float64
	// RedundancyCutoff drops a candidate outright above this similarity to something
	// already selected, rather than merely ranking it lower.
	//
	// 0.5 is not a guess. On the evaluation corpus, four differently worded statements of
	// the same fact score 0.58 to 0.70 against each other, while genuinely different
	// facts that share vocabulary top out at 0.31. The gap between those two clusters is
	// wide, and the cutoff sits in it.
	RedundancyCutoff float64
	// CoverageBonus rewards the first claim about a new subject or predicate.
	CoverageBonus float64
	// DisputedPenalty discounts a contradicted claim. It is a discount and not an
	// exclusion: a disputed fact the asker needs is worth showing, annotated.
	DisputedPenalty float64

	// SectionShare caps what fraction of the budget one section may take on the first
	// pass. Leftovers are redistributed, so a query with no conflicts does not waste the
	// conflict share.
	SectionShare map[domain.ContextSection]float64
}

// DefaultWeights returns the tuned defaults.
func DefaultWeights() Weights {
	return Weights{
		Relevance:  1.0,
		Confidence: 0.6,
		Evidence:   0.5,
		Temporal:   0.7,
		Priority:   0.3,

		Diversity:        0.35,
		RedundancyCutoff: 0.5,
		CoverageBonus:    0.25,
		DisputedPenalty:  0.15,

		// Facts get the largest share because ten stated facts beat one long passage
		// that implies three of them. Excerpts earn their share by being checkable.
		SectionShare: map[domain.ContextSection]float64{
			domain.SectionFacts:     0.45,
			domain.SectionExcerpts:  0.30,
			domain.SectionGraph:     0.12,
			domain.SectionHistory:   0.08,
			domain.SectionConflicts: 0.15,
		},
	}
}

// withDefaults fills anything left unset.
//
// A zero weight means "unset", never "disable". Suppressing a section is done by leaving it
// out of the request, and suppressing a factor is done by setting it near zero rather than
// at zero — the distinction cost phase 7 a whole round of meaningless measurements.
func (w Weights) withDefaults() Weights {
	defaults := DefaultWeights()
	if w.Relevance <= 0 {
		w.Relevance = defaults.Relevance
	}
	if w.Confidence <= 0 {
		w.Confidence = defaults.Confidence
	}
	if w.Evidence <= 0 {
		w.Evidence = defaults.Evidence
	}
	if w.Temporal <= 0 {
		w.Temporal = defaults.Temporal
	}
	if w.Priority <= 0 {
		w.Priority = defaults.Priority
	}
	if w.Diversity <= 0 {
		w.Diversity = defaults.Diversity
	}
	if w.RedundancyCutoff <= 0 {
		w.RedundancyCutoff = defaults.RedundancyCutoff
	}
	if w.CoverageBonus <= 0 {
		w.CoverageBonus = defaults.CoverageBonus
	}
	if w.DisputedPenalty <= 0 {
		w.DisputedPenalty = defaults.DisputedPenalty
	}
	if len(w.SectionShare) == 0 {
		w.SectionShare = defaults.SectionShare
	}
	return w
}

// merit is a candidate's standing before redundancy and coverage, which depend on what has
// already been chosen.
func (w Weights) merit(c candidate) float64 {
	score := w.Relevance*c.relevance +
		w.Confidence*c.confidence +
		w.Evidence*c.evidence +
		w.Temporal*c.temporal +
		w.Priority*c.priority
	if c.conflict != nil {
		score *= 1 - w.DisputedPenalty
	}
	return score
}

// selector runs budget-aware greedy selection with redundancy reduction.
type selector struct {
	weights   Weights
	estimator Estimator
	req       domain.ContextRequest
	// cost is what a candidate actually spends once rendered, which is its text plus the
	// reference line it obliges. Charging only for the text made selection pick items the
	// renderer then had to drop, and on small budgets the citations cost more than the
	// content does.
	cost func(candidate) int

	coveredSubjects   map[domain.EntityID]struct{}
	coveredPredicates map[string]struct{}
	dropped           []domain.DroppedItem
}

// selection is one chosen candidate plus the arithmetic behind choosing it.
type selection struct {
	candidate  candidate
	score      float64
	redundancy float64
	signals    map[string]float64
}

// choose picks candidates greedily, highest marginal value first.
//
// Greedy rather than optimal: the exact version is a knapsack problem with an objective that
// changes as items are added, and the approximation is within a few percent while staying
// explainable — which matters more here, since anyone debugging a prompt needs to know why a
// fact is missing.
func (s *selector) choose(candidates []candidate, budget int) []selection {
	if len(candidates) == 0 {
		return nil
	}

	s.coveredSubjects = map[domain.EntityID]struct{}{}
	s.coveredPredicates = map[string]struct{}{}
	s.rescaleRelevance(candidates)

	remaining := make([]candidate, 0, len(candidates))
	for _, c := range candidates {
		if !s.req.Wants(c.section) {
			s.dropped = append(s.dropped, domain.DroppedItem{
				Surface: c.surface, RecordID: c.recordID, Section: c.section,
				Reason: domain.DropSectionExcluded, Relevance: c.relevance,
			})
			continue
		}
		remaining = append(remaining, c)
	}

	state := &fill{
		caps:   s.sectionCaps(budget),
		spent:  map[domain.ContextSection]int{},
		budget: budget,
	}

	// Two passes. The first respects each section's share, so a flood of excerpts cannot
	// push out every fact. The second offers whatever the first left unspent to the
	// candidates the shares turned away, because a query with no conflicts should not
	// waste the conflict share on nothing.
	chosen := s.fillPass(remaining, nil, state)
	if len(state.deferred) > 0 && state.budget > state.total && len(chosen) < s.req.MaxItems {
		state.caps = nil
		chosen = s.fillPass(state.deferred, chosen, state)
	}

	for _, c := range state.deferred {
		s.dropped = append(s.dropped, domain.DroppedItem{
			Surface: c.surface, RecordID: c.recordID, Section: c.section,
			Reason: domain.DropBudget, Relevance: c.relevance,
			Detail: "no budget left after the section shares were filled",
		})
	}
	return chosen
}

// fill carries budget state across the two selection passes.
type fill struct {
	caps     map[domain.ContextSection]int
	spent    map[domain.ContextSection]int
	total    int
	budget   int
	deferred []candidate
}

func (s *selector) fillPass(remaining []candidate, chosen []selection, state *fill) []selection {
	state.deferred = nil

	for len(chosen) < s.req.MaxItems && len(remaining) > 0 {
		pick, index, redundancy := s.best(remaining, chosen)
		if index < 0 {
			break
		}
		remaining = removeAt(remaining, index)

		tokens := s.cost(pick)
		if state.total+tokens > state.budget {
			// Too big for what is left. Skipped rather than terminal: a shorter
			// candidate further down may still fit, and stopping here would leave
			// budget unspent because one long passage happened to rank next.
			s.dropped = append(s.dropped, domain.DroppedItem{
				Surface: pick.surface, RecordID: pick.recordID, Section: pick.section,
				Reason: domain.DropBudget, Relevance: pick.relevance,
				Detail: "does not fit in the remaining budget",
			})
			continue
		}
		if cap, ok := state.caps[pick.section]; ok && state.spent[pick.section]+tokens > cap {
			state.deferred = append(state.deferred, pick)
			continue
		}

		chosen = append(chosen, selection{
			candidate: pick, score: s.weights.merit(pick), redundancy: redundancy,
			signals: s.signalsFor(pick, redundancy),
		})
		state.spent[pick.section] += tokens
		state.total += tokens
		s.cover(pick)
	}

	if len(chosen) >= s.req.MaxItems {
		for _, c := range remaining {
			s.dropped = append(s.dropped, domain.DroppedItem{
				Surface: c.surface, RecordID: c.recordID, Section: c.section,
				Reason: domain.DropItemLimit, Relevance: c.relevance,
			})
		}
	}
	return chosen
}

// rescaleRelevance maps fused scores onto [0,1] against the best in this batch.
//
// Fused RRF scores are small — a strong result is around 0.05 — and only comparable within
// one response. Without rescaling, the relevance weight would be swamped by every other
// factor, which is a quieter version of the bug that made phase 7's first measurements
// meaningless.
func (s *selector) rescaleRelevance(candidates []candidate) {
	best := 0.0
	for _, c := range candidates {
		if c.relevance > best {
			best = c.relevance
		}
	}
	if best <= 0 {
		return
	}
	for i := range candidates {
		candidates[i].relevance /= best
	}
}

// best returns the highest marginal-value candidate, its index, and its redundancy.
//
// Marginal value is MMR: relevance discounted by similarity to what is already in, plus a
// bonus for covering a subject or predicate nothing else covers. Ten facts about ten things
// beat ten phrasings of one thing (AGENTS.md section 20.2).
func (s *selector) best(remaining []candidate, chosen []selection) (candidate, int, float64) {
	bestIndex, bestValue, bestRedundancy := -1, 0.0, 0.0

	for i, c := range remaining {
		overlap := 0.0
		for _, already := range chosen {
			if sim := redundancy(c, already.candidate); sim > overlap {
				overlap = sim
			}
		}
		if overlap >= s.weights.RedundancyCutoff {
			continue
		}

		value := (1-s.weights.Diversity)*s.weights.merit(c) -
			s.weights.Diversity*overlap +
			s.weights.CoverageBonus*s.novelty(c)

		if bestIndex < 0 || value > bestValue {
			bestIndex, bestValue, bestRedundancy = i, value, overlap
		}
	}

	if bestIndex < 0 {
		// Everything left is a near-duplicate of something already chosen.
		for _, c := range remaining {
			s.dropped = append(s.dropped, domain.DroppedItem{
				Surface: c.surface, RecordID: c.recordID, Section: c.section,
				Reason: domain.DropRedundant, Relevance: c.relevance,
				Redundancy: s.maxSimilarity(c, chosen),
			})
		}
		return candidate{}, -1, 0
	}
	return remaining[bestIndex], bestIndex, bestRedundancy
}

func (s *selector) maxSimilarity(c candidate, chosen []selection) float64 {
	worst := 0.0
	for _, already := range chosen {
		if sim := redundancy(c, already.candidate); sim > worst {
			worst = sim
		}
	}
	return worst
}

// novelty is the fraction of a candidate's subjects and predicate not yet covered.
func (s *selector) novelty(c candidate) float64 {
	total, fresh := 0, 0
	for _, subject := range c.subjects {
		total++
		if _, seen := s.coveredSubjects[subject]; !seen {
			fresh++
		}
	}
	if c.predicate != "" {
		total++
		if _, seen := s.coveredPredicates[c.predicate]; !seen {
			fresh++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(fresh) / float64(total)
}

func (s *selector) cover(c candidate) {
	for _, subject := range c.subjects {
		s.coveredSubjects[subject] = struct{}{}
	}
	if c.predicate != "" {
		s.coveredPredicates[c.predicate] = struct{}{}
	}
}

// sectionCaps converts shares into token ceilings for the requested sections.
func (s *selector) sectionCaps(budget int) map[domain.ContextSection]int {
	caps := make(map[domain.ContextSection]int, len(s.weights.SectionShare))
	for section, share := range s.weights.SectionShare {
		if !s.req.Wants(section) {
			continue
		}
		caps[section] = int(float64(budget) * share)
	}
	return caps
}

func (s *selector) signalsFor(c candidate, redundancy float64) map[string]float64 {
	if !s.req.Explain {
		return nil
	}
	return map[string]float64{
		"relevance":  c.relevance,
		"confidence": c.confidence,
		"evidence":   c.evidence,
		"temporal":   c.temporal,
		"priority":   c.priority,
		"novelty":    s.novelty(c),
		"redundancy": redundancy,
		"merit":      s.weights.merit(c),
	}
}

func removeAt(items []candidate, index int) []candidate {
	return append(items[:index:index], items[index+1:]...)
}

// orderForRendering groups selections by section, keeping score order within each.
func orderForRendering(chosen []selection) []selection {
	order := map[domain.ContextSection]int{}
	for i, section := range domain.ContextSections() {
		order[section] = i
	}

	out := append([]selection(nil), chosen...)
	sort.SliceStable(out, func(i, j int) bool {
		if order[out[i].candidate.section] != order[out[j].candidate.section] {
			return order[out[i].candidate.section] < order[out[j].candidate.section]
		}
		return out[i].score > out[j].score
	})
	return out
}
