package retrieval_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/retrieval"
)

// The evaluation fixture behind the phase 7 acceptance criterion: hybrid retrieval must
// demonstrably outperform the individual modes (AGENTS.md section 36).
//
// The corpus and queries are chosen so that no single retriever can win. Each query below is
// served well by one mode and poorly by at least one other, which is the situation hybrid
// retrieval exists for. A fixture where one retriever answered everything would prove
// nothing except that the fixture was chosen badly.

// document is one piece of source material in the fixture.
type document struct {
	key  string
	text string
}

// fixtureCorpus is deliberately varied and large enough for a top-5 cut to be selective.
//
// Eight documents made recall@5 nearly free - returning five of eight is most of the corpus -
// so the fixture would have reported every retriever as excellent. The distractors below are
// not padding: several share vocabulary with the queries without answering them, which is
// what makes ranking, rather than mere matching, the thing being measured.
var fixtureCorpus = []document{
	{"supply", "Acme Corporation supplies industrial fasteners to Globex Industries under a long term agreement."},
	{"error", "Deployment aborted. The build failed with error code ERR_7731X during the linking stage."},
	{"part", "Replacement part number AF-2291-B is compatible with the older turbine housings."},
	{"turbine", "Globex Industries manufactures turbines at its Berlin plant for the energy sector."},
	{"refund", "Standard refunds are available for thirty days from the delivery date, without exception."},
	{"escalation", "Escalate to the account team when a customer disputes a charge above ten thousand dollars."},
	{"audit", "The annual compliance audit reviewed procurement records for the aerospace division."},
	{"contract", "The master services agreement was countersigned by both parties in March."},

	// Distractors sharing vocabulary with the queries without answering them.
	{"fastener-policy", "Fastener inventory policy requires quarterly counts of industrial stock in every warehouse."},
	{"other-error", "A separate incident logged error code ERR_2210A while the deployment pipeline was idle."},
	{"other-part", "Part number AF-1180-C was withdrawn from the catalogue after the supplier changed."},
	{"berlin-office", "The Berlin office relocated to a larger building near the central station last spring."},
	{"energy-market", "Energy sector demand for turbines rose sharply across European manufacturing."},
	{"delivery", "Delivery windows are confirmed by email on the working day before dispatch."},
	{"charges", "Disputed charges under one thousand dollars are written off without review."},
	{"agreement-terms", "Agreement terms may be renegotiated annually by either party with notice."},
	{"procurement", "Procurement records are retained for seven years to satisfy the compliance regime."},
	{"aerospace", "The aerospace division qualified two new suppliers during the last review cycle."},
	{"linking", "Linking stages of the build were parallelised to shorten the overall pipeline."},
	{"housing", "Turbine housings manufactured before 2019 use a different mounting standard."},
	{"account-team", "The account team meets weekly to review escalations raised by support."},
	{"warehouse", "Warehouse staff scan every pallet on arrival to keep inventory counts accurate."},
	{"invoice", "Invoices are issued on the first working day of the month following delivery."},
	{"support-hours", "Support is available between eight and eighteen hundred on working days."},
	{"onboarding", "New customers complete an onboarding call before their first shipment."},
	{"catalogue", "The product catalogue is republished quarterly with revised part numbers."},
	{"supplier-review", "Supplier reviews score delivery reliability, defect rate, and responsiveness."},
	{"logistics", "Logistics partners are selected per region based on cost and transit time."},
	{"quality", "Quality inspections sample one in fifty units from each production batch."},
	{"training", "Training records are audited annually alongside the compliance review."},
}

// evaluationQuery is one question with the records that should be found for it.
type evaluationQuery struct {
	name string
	text string
	// wantKeys are the corpus documents whose chunks should be retrieved, and entity names
	// that should be reached. A query is answered if any expected item appears in the top
	// results.
	wantKeys []string
	// why records what this query is testing, so a failure says something useful.
	why string
}

var evaluationQueries = []evaluationQuery{
	{
		name:     "exact error code",
		text:     "ERR_7731X",
		wantKeys: []string{"error"},
		why:      "identifiers are mangled by stemming and have no useful embedding neighbourhood",
	},
	{
		name:     "exact part number",
		text:     "AF-2291-B",
		wantKeys: []string{"part"},
		why:      "punctuated part numbers survive neither tokenization nor embedding",
	},
	{
		name:     "prose with shared vocabulary",
		text:     "industrial fasteners agreement",
		wantKeys: []string{"supply"},
		why:      "ordinary full-text territory",
	},
	{
		name:     "wording the document does not use",
		text:     "turbine manufacturing plant Berlin energy",
		wantKeys: []string{"turbine"},
		why:      "overlapping but reordered vocabulary, where ranking differs between retrievers",
	},
	{
		// Only the identity counts here. Allowing a chunk that merely mentions the name
		// would let lexical satisfy a query about identity resolution, and the fixture
		// would stop measuring what it claims to.
		name:     "entity by name",
		text:     "Globex Industries",
		wantKeys: []string{"Globex Industries"},
		why:      "a name should reach the identity itself, not only passages mentioning it",
	},
	{
		// The query names Acme; the expected answer is Globex, which no passage connects
		// to it in these words. Only traversal of the SUPPLIES edge gets there.
		name:     "relationship no passage states",
		text:     "Acme Corporation",
		wantKeys: []string{"Globex Industries"},
		why:      "reachable only by following an edge from the entity the query names",
	},
	{
		name:     "policy question",
		text:     "refund window delivery",
		wantKeys: []string{"refund"},
		why:      "plain prose retrieval",
	},
	{
		name:     "escalation threshold",
		text:     "disputes charge ten thousand dollars",
		wantKeys: []string{"escalation"},
		why:      "numbers embedded in prose, which tokenizers treat inconsistently",
	},
}

// evaluation runs the fixture and reports how each mode scores.
type evaluation struct {
	mode      string
	recallAt5 float64
	mrr       float64
	answered  int
	total     int
}

func TestIntegrationHybridOutperformsIndividualModes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	keyByRecord := h.loadFixture(t)

	modes := map[string][]domain.RetrievalMode{
		"lexical": {domain.ModeLexical},
		"exact":   {domain.ModeExact},
		"vector":  {domain.ModeVector},
		"entity":  {domain.ModeEntity},
		// Graph alone cannot start: it expands from entities other retrievers found, so it
		// is measured paired with the entity lookup that seeds it.
		"entity+graph": {domain.ModeEntity, domain.ModeGraph},
		// Hybrid is the planner's own choice of modes, which is what a caller gets by
		// default.
		"hybrid": nil,
	}

	results := make([]evaluation, 0, len(modes))
	for name, selected := range modes {
		results = append(results, h.evaluate(t, ctx, name, selected, keyByRecord))
	}
	sort.Slice(results, func(i, j int) bool { return results[i].recallAt5 > results[j].recallAt5 })

	t.Log("retrieval quality on the fixture corpus:")
	t.Logf("  %-14s %-10s %-8s %s", "mode", "recall@5", "mrr", "answered")
	for _, result := range results {
		t.Logf("  %-14s %-10.3f %-8.3f %d/%d",
			result.mode, result.recallAt5, result.mrr, result.answered, result.total)
	}

	var hybrid evaluation
	for _, result := range results {
		if result.mode == "hybrid" {
			hybrid = result
		}
	}
	if hybrid.total == 0 {
		t.Fatal("the hybrid evaluation did not run")
	}

	// The acceptance criterion. Hybrid must beat every individual mode on recall, and not
	// lose to any of them on ranking quality - fusing retrievers is worthless if it merely
	// averages them.
	for _, result := range results {
		if result.mode == "hybrid" {
			continue
		}
		if hybrid.recallAt5 <= result.recallAt5 {
			t.Errorf("hybrid recall@5 %.3f does not beat %s at %.3f",
				hybrid.recallAt5, result.mode, result.recallAt5)
		}
		if hybrid.mrr < result.mrr {
			t.Errorf("hybrid MRR %.3f is worse than %s at %.3f",
				hybrid.mrr, result.mode, result.mrr)
		}
	}

	// And it must answer most of the fixture, or "best of a bad set" would pass.
	//
	// The bar is not 1.0, and the query that misses is worth naming. "refund window
	// delivery" should find the refund policy, but a distractor reading "Delivery windows
	// are confirmed by email" matches two of the three terms adjacently, while the target
	// matches two non-adjacently. Under bag-of-words similarity the distractor is genuinely
	// the better match; separating them needs a model that understands "refund window" as a
	// concept, which feature hashing cannot do by construction. That is a limitation of the
	// test embedder, not a defect in fusion, and pretending otherwise by deleting the query
	// would make this fixture flatter.
	if hybrid.recallAt5 < 0.8 {
		t.Errorf("hybrid should answer most fixture queries, recall@5 was %.3f",
			hybrid.recallAt5)
	}
}

// evaluate runs every fixture query under one mode selection.
func (h *harness) evaluate(t *testing.T, ctx context.Context, name string, modes []domain.RetrievalMode, keyByRecord map[string]string) evaluation {
	t.Helper()

	out := evaluation{mode: name, total: len(evaluationQueries)}

	for _, query := range evaluationQueries {
		result, err := h.retriever.Query(ctx, domain.QueryRequest{
			Scope: h.scope(),
			Query: query.text,
			Modes: modes,
			Limit: 5,
		})
		if err != nil {
			t.Fatalf("%s / %s: %v", name, query.name, err)
		}

		rank := firstRelevantRank(result.Items, query.wantKeys, keyByRecord)
		if rank > 0 {
			out.answered++
			out.recallAt5 += 1
			out.mrr += 1 / float64(rank)
		}
	}

	out.recallAt5 /= float64(out.total)
	out.mrr /= float64(out.total)
	return out
}

// firstRelevantRank returns the 1-based position of the first expected item, or 0.
func firstRelevantRank(items []domain.RetrievedItem, wantKeys []string, keyByRecord map[string]string) int {
	want := map[string]bool{}
	for _, key := range wantKeys {
		want[key] = true
	}

	for i, item := range items {
		// A chunk maps back to the document it came from; an entity is matched by name.
		if key, ok := keyByRecord[item.RecordID]; ok && want[key] {
			return i + 1
		}
		// An entity's projected content carries a type suffix - "Globex Industries
		// (organization)" - so match on the name rather than the whole string.
		if item.Surface == domain.SurfaceEntity {
			for key := range want {
				if strings.HasPrefix(item.Content, key) {
					return i + 1
				}
			}
		}
	}
	return 0
}

func TestIntegrationPlannerExplainsItsChoices(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.loadFixture(t)

	// An identifier query should reach for exact matching without being told to.
	result, err := h.retriever.Query(ctx, domain.QueryRequest{
		Scope: h.scope(), Query: "ERR_7731X", Explain: true, Limit: 5,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Plan == nil {
		t.Fatal("an explained query must return its plan")
	}
	if !hasMode(result.Plan.Modes, domain.ModeExact) {
		t.Fatalf("an identifier query should plan exact matching, got %v", result.Plan.Modes)
	}
	if result.Plan.Reasons[domain.ModeExact] == "" {
		t.Fatal("the plan must say why each mode ran")
	}

	// Prose should not.
	result, err = h.retriever.Query(ctx, domain.QueryRequest{
		Scope:   h.scope(),
		Query:   "what are the standard refund arrangements for delivered goods",
		Explain: true, Limit: 5,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if hasMode(result.Plan.Modes, domain.ModeExact) {
		t.Fatalf("a prose query should not plan exact matching, got %v", result.Plan.Modes)
	}

	// Every result explains which retrievers found it and what they contributed.
	if len(result.Items) == 0 {
		t.Fatal("the prose query should return something")
	}
	top := result.Items[0]
	if len(top.FoundBy) == 0 {
		t.Fatal("a result must record which retrievers found it")
	}
	if len(top.Signals) == 0 {
		t.Fatal("a result must expose the signals behind its score")
	}
	for _, line := range retrieval.Explain(*result.Plan) {
		if !strings.Contains(line, ":") {
			t.Fatalf("plan explanations should be readable, got %q", line)
		}
	}
}

func TestIntegrationResultsAreStableAcrossRuns(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.loadFixture(t)

	// Unstable ordering makes retrieval impossible to test and its regressions invisible.
	var first []string
	for run := range 3 {
		result, err := h.retriever.Query(ctx, domain.QueryRequest{
			Scope: h.scope(), Query: "Globex Industries", Limit: 10,
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}

		var ids []string
		for _, item := range result.Items {
			ids = append(ids, fmt.Sprintf("%s:%s", item.Surface, item.RecordID))
		}
		if run == 0 {
			first = ids
			continue
		}
		if strings.Join(ids, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d returned a different order:\n%v\n%v", run, first, ids)
		}
	}
}

func hasMode(modes []domain.RetrievalMode, want domain.RetrievalMode) bool {
	for _, mode := range modes {
		if mode == want {
			return true
		}
	}
	return false
}
