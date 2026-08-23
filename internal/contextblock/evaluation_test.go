package contextblock_test

import (
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/contextblock"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/knowledge"
)

// evaluationCorpus is deliberately redundant. Real corpora are: the same fact restated in a
// summary, a ticket, and a follow-up. A selection policy that cannot tell restatement from
// new information looks fine on a corpus where every document says something different.
var evaluationCorpus = []struct {
	key  string
	text string
}{
	{"supply-1", "Acme Corporation supplies industrial fasteners to the Portland plant."},
	{"supply-2", "Acme Corporation is the supplier of industrial fasteners for the Portland plant."},
	{"supply-3", "Industrial fasteners at the Portland plant are supplied by Acme Corporation."},
	{"supply-4", "The Portland plant sources its industrial fasteners from Acme Corporation."},
	{"shift", "The Portland plant runs a night shift on Thursdays, at roughly forty percent of weekday throughput."},
	{"route", "Globex Industries took over the Salem distribution route in March, with a smaller fleet and denser coverage."},
	{"count", "Fastener stock at Portland is counted weekly, and discrepancies above two percent trigger a manual recount."},
	{"contact", "Priya Raman is the account manager for the Portland plant and handles supplier escalations."},
}

// TestIntegrationSelectionBeatsNaiveTopKAtEqualBudget measures the metrics AGENTS.md
// section 31.5 asks for: useful fact coverage, redundancy, citation coverage, and token
// cost. The baseline is what a system without a selection policy would do — take retrieval's
// ranking and fill the budget from the top.
//
// The comparison is the point. "Our assembler produces good context" is unfalsifiable;
// "our assembler covers more distinct facts with less repetition than top-k at the same
// budget" is a claim that can fail, and this test is where it would.
func TestIntegrationSelectionBeatsNaiveTopKAtEqualBudget(t *testing.T) {
	h := newHarness(t)

	var event domain.SourceEventID
	var episode domain.EpisodeID
	for _, doc := range evaluationCorpus {
		e, ep := h.ingest(t, doc.text, "eval-"+doc.key)
		if event == "" {
			event, episode = e, ep
		}
	}
	h.claim(t, event, episode, knowledge.Claim{
		Subject:      knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
		Predicate:    "supplies_to",
		ObjectEntity: &knowledge.EntityRef{Name: "Portland Plant", Type: "facility"},
	})

	// Large enough that selection has room to make choices. At a few hundred tokens the
	// citation lines alone consume most of the block and every policy produces the same
	// two items, which measures nothing.
	const budget = 800
	const query = "what should I know about the Portland plant and its fasteners"

	assembled, err := h.assembler.Assemble(t.Context(), domain.ContextRequest{
		Scope: h.scope(), Query: query, TokenBudget: budget, Explain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline := h.naiveTopK(t, query, budget)

	selected := measure(assembled.Items)
	naive := measure(baseline)

	t.Logf("metric            assembled   top-k")
	t.Logf("distinct facts    %-11d %d", selected.facts, naive.facts)
	t.Logf("mean redundancy   %-11.3f %.3f", selected.redundancy, naive.redundancy)
	t.Logf("items             %-11d %d", selected.items, naive.items)
	t.Logf("tokens used       %-11d %d", assembled.Budget.Used, budget)

	if selected.items == 0 {
		t.Fatal("assembly produced nothing to measure")
	}
	if selected.facts < naive.facts {
		t.Fatalf("selection covered fewer distinct facts than top-k: %d against %d",
			selected.facts, naive.facts)
	}
	if selected.redundancy > naive.redundancy {
		t.Fatalf("selection repeated itself more than top-k: %.3f against %.3f",
			selected.redundancy, naive.redundancy)
	}

	// Citation coverage is the one metric with an absolute target rather than a
	// comparative one: anything below total is a claim someone cannot check.
	markers := map[int]bool{}
	for _, citation := range assembled.Citations {
		markers[citation.Marker] = true
	}
	for _, item := range assembled.Items {
		if !markers[item.Marker] {
			t.Fatalf("item %d was rendered with no citation", item.Marker)
		}
	}
}

// naiveTopK is the baseline: retrieval order, no selection, fill until the budget is spent.
func (h *harness) naiveTopK(t *testing.T, query string, budget int) []domain.ContextItem {
	t.Helper()

	// Assembly with everything that makes it smart turned off: redundancy tolerated,
	// no coverage bonus, no section shares, and a candidate pool no deeper than what
	// fits. What remains is rank order under a budget.
	naive := contextblock.New(h.retriever(), h.fixture.Store, contextblock.Options{
		Weights: contextblock.Weights{
			Relevance:        1,
			Confidence:       0.0001,
			Evidence:         0.0001,
			Temporal:         0.0001,
			Priority:         0.0001,
			Diversity:        0.0001,
			RedundancyCutoff: 1,
			CoverageBonus:    0.0001,
			DisputedPenalty:  0.0001,
			SectionShare:     map[domain.ContextSection]float64{},
		},
	}, nil, nil)

	block, err := naive.Assemble(t.Context(), domain.ContextRequest{
		Scope: h.scope(), Query: query, TokenBudget: budget,
	})
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	return block.Items
}

type metrics struct {
	items      int
	facts      int
	redundancy float64
}

// measure counts distinct facts and mean pairwise overlap.
//
// "Distinct facts" is approximated by the set of content words: two items that share almost
// all their words are counted once. Crude, and it is the same crudeness applied to both
// sides of the comparison, which is what makes the comparison fair.
func measure(items []domain.ContextItem) metrics {
	m := metrics{items: len(items)}
	if len(items) == 0 {
		return m
	}

	sets := make([]map[string]struct{}, 0, len(items))
	for _, item := range items {
		sets = append(sets, wordsOf(item.Text))
	}

	var distinct []map[string]struct{}
	for _, set := range sets {
		novel := true
		for _, kept := range distinct {
			if jaccard(set, kept) > 0.5 {
				novel = false
				break
			}
		}
		if novel {
			distinct = append(distinct, set)
		}
	}
	m.facts = len(distinct)

	pairs, total := 0, 0.0
	for i := range sets {
		for j := i + 1; j < len(sets); j++ {
			total += jaccard(sets[i], sets[j])
			pairs++
		}
	}
	if pairs > 0 {
		m.redundancy = total / float64(pairs)
	}
	return m
}

func wordsOf(text string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, field := range strings.Fields(strings.ToLower(text)) {
		word := strings.Trim(field, ".,;:!?\"'()[]{}")
		if len(word) < 3 {
			continue
		}
		out[word] = struct{}{}
	}
	return out
}

func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	for word := range a {
		if _, ok := b[word]; ok {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}
