// Package retrieval plans, runs, and fuses candidate generators.
//
// Retrieval is a planner plus independent retrievers plus fusion, not one clever query
// (AGENTS.md section 19). Each retriever is good at something the others are bad at: vectors
// find paraphrase and miss identifiers, lexical finds identifiers and misses paraphrase, the
// graph reaches facts no single passage states. Fusing them is how a query gets served by
// whichever one happens to be right for it.
package retrieval

import (
	"sort"

	"github.com/gimantha/strata/internal/domain"
)

// Weights configure fusion. They are data rather than constants in the ranking code, because
// AGENTS.md section 19.3 requires ranking signals to be inspectable and configurable.
type Weights struct {
	// Mode weights scale each retriever's contribution to the fused score.
	Lexical float64
	Exact   float64
	Vector  float64
	Entity  float64
	Graph   float64

	// RRFConstant damps the influence of top ranks. The conventional 60 keeps a single
	// retriever's first result from dominating everything the others found.
	RRFConstant float64

	// GraphDepthPenalty discounts results the further they were traversed, so a fact two
	// hops away does not outrank one the query directly matched.
	GraphDepthPenalty float64

	// ScoreQualityWeight decides how much a hit's own score, relative to the best its
	// retriever found, modulates its rank credit.
	//
	// Plain RRF is rank-only, which means a retriever that found nothing good still casts a
	// full-strength vote for whatever it ranked first. That is how fusion ends up ranking
	// worse than its most precise input: a confident rank-one match gets pulled down by
	// several unconfident ones. Scaling by within-retriever score ratio keeps the vote but
	// weights it by how sure that retriever was.
	ScoreQualityWeight float64

	// MinSurfaces reserves room in the results for surfaces that ranking alone would shut
	// out.
	//
	// Passages outnumber identities by orders of magnitude, so a purely score-ordered list
	// fills with chunks and an answer of a different kind - the entity the question named,
	// or the neighbour only a graph edge connects - never appears at all. Reserving a slot
	// per surface costs one lower-ranked passage and is the difference between answering
	// "what is Acme" with five sentences mentioning Acme and answering it with Acme.
	MinSurfaces int

	// RelativeFloor drops a retriever's weak tail before fusion, as a fraction of that
	// retriever's own best score.
	//
	// Plain RRF ignores score magnitude entirely: a retriever's tenth-best match, however
	// poor, still contributes rank credit. For a retriever that found nothing good, every
	// one of its results is noise, and letting that noise compete with another retriever's
	// confident answer is how fusion ends up worse than its best input. Comparison is
	// within a retriever, never across, since their scores are not on comparable scales.
	RelativeFloor float64
}

// DefaultWeights favour precise signals over fuzzy ones.
//
// Exact matches rank highest because an identifier query has one right answer. Lexical and
// vector sit close together, since which is better depends entirely on the query. Graph is
// lowest: it reaches things nothing matched directly, which is valuable but speculative.
func DefaultWeights() Weights {
	return Weights{
		Lexical:            1.0,
		Exact:              1.4,
		Vector:             1.0,
		Entity:             1.2,
		Graph:              0.7,
		RRFConstant:        60,
		GraphDepthPenalty:  0.5,
		RelativeFloor:      0.35,
		ScoreQualityWeight: 0.6,
		MinSurfaces:        1,
	}
}

// withDefaults fills anything left unset.
//
// The mode weights are filled too. A zero-valued Weights is the natural way to say "use the
// defaults", and without this every retriever contributed a weight of zero: scores collapsed
// to zero and ranking silently fell through to the tie-breakers. Disabling a retriever is
// done by leaving it out of the request's mode list, not by weighting it zero.
func (w Weights) withDefaults() Weights {
	defaults := DefaultWeights()
	if w.Lexical <= 0 {
		w.Lexical = defaults.Lexical
	}
	if w.Exact <= 0 {
		w.Exact = defaults.Exact
	}
	if w.Vector <= 0 {
		w.Vector = defaults.Vector
	}
	if w.Entity <= 0 {
		w.Entity = defaults.Entity
	}
	if w.Graph <= 0 {
		w.Graph = defaults.Graph
	}
	if w.RRFConstant <= 0 {
		w.RRFConstant = defaults.RRFConstant
	}
	if w.GraphDepthPenalty < 0 {
		w.GraphDepthPenalty = defaults.GraphDepthPenalty
	}
	if w.RelativeFloor <= 0 {
		w.RelativeFloor = defaults.RelativeFloor
	}
	if w.ScoreQualityWeight <= 0 {
		w.ScoreQualityWeight = defaults.ScoreQualityWeight
	}
	if w.MinSurfaces <= 0 {
		w.MinSurfaces = defaults.MinSurfaces
	}
	return w
}

// forMode returns the weight of one retriever.
func (w Weights) forMode(mode domain.RetrievalMode) float64 {
	switch mode {
	case domain.ModeLexical:
		return w.Lexical
	case domain.ModeExact:
		return w.Exact
	case domain.ModeVector:
		return w.Vector
	case domain.ModeEntity:
		return w.Entity
	case domain.ModeGraph:
		return w.Graph
	default:
		return 1
	}
}

// scoreQuality builds a factor that scales a candidate's rank credit by how strong its score
// was relative to the best its own retriever produced.
//
// Comparison stays within a retriever: a cosine similarity and a text rank are not on the
// same scale, and treating them as if they were is the mistake RRF exists to avoid. What is
// comparable is a retriever's confidence in one result versus its confidence in another.
func scoreQuality(candidates []candidate, weight float64) func(candidate) float64 {
	if weight <= 0 {
		return func(candidate) float64 { return 1 }
	}

	best := map[domain.RetrievalMode]float64{}
	for _, c := range candidates {
		if c.score > best[c.mode] {
			best[c.mode] = c.score
		}
	}

	return func(c candidate) float64 {
		top := best[c.mode]
		if top <= 0 {
			// No usable magnitude information, so rank alone decides and every candidate
			// from this retriever is treated equally.
			return 1
		}
		ratio := c.score / top
		if ratio > 1 {
			ratio = 1
		}
		if ratio < 0 {
			ratio = 0
		}
		// A retriever's best hit always votes at full strength; weaker ones are damped
		// towards, but never below, (1 - weight).
		return (1 - weight) + weight*ratio
	}
}

// dropWeakTails removes each retriever's low-scoring results before fusion.
//
// Ranks are re-assigned afterwards so a surviving candidate is credited for its position
// among the results worth keeping, rather than its position in the raw list.
func dropWeakTails(candidates []candidate, floor float64) []candidate {
	if floor <= 0 {
		return candidates
	}

	best := map[domain.RetrievalMode]float64{}
	for _, c := range candidates {
		if c.score > best[c.mode] {
			best[c.mode] = c.score
		}
	}

	kept := make([]candidate, 0, len(candidates))
	nextRank := map[domain.RetrievalMode]int{}
	for _, c := range candidates {
		// A retriever whose scores are all zero or negative carries no magnitude
		// information, so everything it found is kept and rank alone decides.
		if top := best[c.mode]; top > 0 && c.score < top*floor {
			continue
		}
		nextRank[c.mode]++
		c.rank = nextRank[c.mode]
		kept = append(kept, c)
	}
	return kept
}

// candidate is one retriever's view of a record before fusion.
type candidate struct {
	mode  domain.RetrievalMode
	rank  int
	score float64
	hit   domain.Hit
	path  *domain.GraphPath
}

// fuse combines ranked candidate lists using weighted Reciprocal Rank Fusion.
//
// RRF is chosen over score normalization because the retrievers' scores are not comparable:
// a cosine similarity, a ts_rank_cd value, and a trigram similarity live on different scales
// with different distributions, and normalizing them requires assumptions that are wrong in
// different ways for each. Ranks are comparable by construction, and a record that several
// retrievers rank highly rises without any of them having to agree on a number
// (AGENTS.md section 19.3).
func fuse(candidates []candidate, weights Weights, limit int) []domain.RetrievedItem {
	weights = weights.withDefaults()
	candidates = dropWeakTails(candidates, weights.RelativeFloor)
	quality := scoreQuality(candidates, weights.ScoreQualityWeight)

	type accumulator struct {
		item domain.RetrievedItem
	}
	merged := map[string]*accumulator{}

	for _, c := range candidates {
		key := string(c.hit.Surface) + "\x00" + c.hit.RecordID

		entry, ok := merged[key]
		if !ok {
			entry = &accumulator{item: domain.RetrievedItem{
				Surface:  c.hit.Surface,
				RecordID: c.hit.RecordID,
				Content:  c.hit.Content,
				Signals:  map[string]float64{},
				Ranks:    map[domain.RetrievalMode]int{},
			}}
			merged[key] = entry
		}
		if entry.item.Content == "" {
			entry.item.Content = c.hit.Content
		}

		contribution := weights.forMode(c.mode) / (weights.RRFConstant + float64(c.rank))
		contribution *= quality(c)

		// A graph result is discounted by how far it was traversed: reaching something in
		// four hops is weaker evidence of relevance than matching it directly.
		if c.mode == domain.ModeGraph && c.path != nil && c.path.Depth > 0 {
			contribution /= 1 + weights.GraphDepthPenalty*float64(c.path.Depth)
		}

		entry.item.Score += contribution
		entry.item.Signals[string(c.mode)+"_rrf"] = contribution
		entry.item.Signals[string(c.mode)+"_score"] = c.score
		entry.item.Ranks[c.mode] = c.rank
		entry.item.FoundBy = append(entry.item.FoundBy, c.mode)

		if c.path != nil && entry.item.Path == nil {
			entry.item.Path = c.path
		}
	}

	out := make([]domain.RetrievedItem, 0, len(merged))
	for _, entry := range merged {
		sort.Slice(entry.item.FoundBy, func(i, j int) bool {
			return entry.item.FoundBy[i] < entry.item.FoundBy[j]
		})
		out = append(out, entry.item)
	}

	// Ties break on record id so results are stable: an unstable ordering makes retrieval
	// impossible to test and its regressions impossible to see.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if len(out[i].FoundBy) != len(out[j].FoundBy) {
			return len(out[i].FoundBy) > len(out[j].FoundBy)
		}
		return out[i].RecordID < out[j].RecordID
	})

	return diversifyBySurface(out, weights.MinSurfaces, limit)
}

// diversifyBySurface promotes the best result of each absent surface into the returned set.
//
// Only whole surfaces missing from the cut are promoted, at most one result each, and always
// into the tail so ranking still decides the top. The alternative is a result set that is
// uniformly one kind of thing, which is a worse answer than a slightly lower-ranked mixture.
func diversifyBySurface(items []domain.RetrievedItem, minPerSurface, limit int) []domain.RetrievedItem {
	if limit <= 0 || len(items) <= limit {
		return items
	}
	if minPerSurface <= 0 {
		return items[:limit]
	}

	head := make([]domain.RetrievedItem, limit)
	copy(head, items[:limit])

	present := map[domain.Surface]int{}
	for _, item := range head {
		present[item.Surface]++
	}

	// Reserve at most half the result set for promotions, so diversity never overwhelms
	// relevance.
	budget := limit / 2
	for _, item := range items[limit:] {
		if budget <= 0 {
			break
		}
		if present[item.Surface] >= minPerSurface {
			continue
		}

		// Displace the lowest-ranked result of whichever surface is most over-represented,
		// never the top result and never another surface's only representative.
		victim := -1
		for i := len(head) - 1; i > 0; i-- {
			if present[head[i].Surface] > minPerSurface {
				victim = i
				break
			}
		}
		if victim < 0 {
			break
		}

		present[head[victim].Surface]--
		head[victim] = item
		present[item.Surface]++
		budget--
	}

	sort.Slice(head, func(i, j int) bool {
		if head[i].Score != head[j].Score {
			return head[i].Score > head[j].Score
		}
		return head[i].RecordID < head[j].RecordID
	})
	return head
}
