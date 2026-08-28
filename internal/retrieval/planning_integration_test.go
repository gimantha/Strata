package retrieval_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/llm"
	"github.com/gimantha/strata/internal/retrieval"
)

// planningModel returns a fixed plan, standing in for a real one.
//
// Scripted rather than live, because the property under test is what retrieval does with a
// reshaped question — not whether a particular model reshapes it well. A test that called a
// provider would measure the provider and fail when it changed its mind.
type planningModel struct{ raw string }

func (planningModel) Name() string  { return "scripted" }
func (planningModel) Model() string { return "scripted-planner" }

func (planningModel) Generate(context.Context, llm.GenerateRequest) (llm.GenerateResponse, error) {
	return llm.GenerateResponse{}, nil
}

func (m planningModel) GenerateStructured(context.Context,
	llm.StructuredRequest) (llm.StructuredResponse, error) {
	return llm.StructuredResponse{Raw: json.RawMessage(m.raw)}, nil
}

// TestIntegrationPlanningFindsWhatShapeAloneMisses is the case that motivated the feature.
//
// "which support episodes" is a requirement, not a topic. Embeddings are poor at
// requirements — a document about context graphs that mentions episodes and one that does
// not sit close together — and the heuristic planner cannot tell the difference, because it
// reads shape rather than meaning. A planner that decomposes the question searches for the
// constraint directly, and the fusion of both searches ranks the qualifying document above
// the one that merely shares a topic.
func TestIntegrationPlanningFindsWhatShapeAloneMisses(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Two documents on the same topic. Only one meets the requirement, and it does not use
	// the question's wording for the rest.
	h.ingestOne(t, "planner-with",
		"Strata segments every source event into episodes before chunking them.")
	h.ingestOne(t, "planner-without",
		"A context graph implementation stores entities, assertions and their relationships.")

	const question = "what is the context graph implementation which support episodes?"

	// The scripted planner does what a real one should: keeps the question, and adds a
	// search for the constraint the question actually turns on.
	planned := retrieval.New(h.fixture.Store, h.fixture.Store.Indexes(), h.embedder,
		retrieval.Options{
			PlanningModel: planningModel{raw: `{
				"modes": ["lexical", "vector"],
				"mode_reasons": [{"mode": "lexical", "reason": "the constraint is a literal word"}],
				"sub_queries": [
					{"text": "ignored", "kind": "original", "reason": "as asked"},
					{"text": "episodes", "kind": "decomposed",
					 "reason": "the requirement the question turns on"}
				]
			}`},
		}, nil, nil)

	rank := func(r *retrieval.Retriever, marker string) int {
		t.Helper()
		result, err := r.Query(ctx, domain.QueryRequest{
			Scope: h.scope(), Query: question, Limit: 10, Explain: true,
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		for i, item := range result.Items {
			if strings.Contains(item.Content, marker) {
				return i + 1
			}
		}
		return 0
	}

	withPlanning := rank(planned, "episodes")
	if withPlanning == 0 {
		t.Fatal("planning did not find the document that meets the requirement")
	}
	// The document that only shares a topic must not outrank it.
	if other := rank(planned, "entities, assertions"); other != 0 && other < withPlanning {
		t.Errorf("the document without episodes ranked %d, above the one with them at %d",
			other, withPlanning)
	}

	// The plan says what it did, which is what makes a rewritten result auditable: a
	// reader can see the search that produced it was not the question they asked.
	result, err := planned.Query(ctx, domain.QueryRequest{
		Scope: h.scope(), Query: question, Limit: 10, Explain: true,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if result.Plan == nil {
		t.Fatal("an explained query returned no plan")
	}
	if result.Plan.Planner != "llm/scripted-planner" {
		t.Errorf("the plan does not name the planner: %q", result.Plan.Planner)
	}
	if len(result.Plan.SubQueries) != 2 {
		t.Fatalf("expected two searches in the plan, got %d", len(result.Plan.SubQueries))
	}
	if result.Plan.SubQueries[0].Text != question {
		t.Errorf("the plan does not record the question as asked: %q",
			result.Plan.SubQueries[0].Text)
	}
	if result.Plan.SubQueries[1].Text != "episodes" {
		t.Errorf("the plan does not record the decomposed search: %q",
			result.Plan.SubQueries[1].Text)
	}
}
