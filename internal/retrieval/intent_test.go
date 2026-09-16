package retrieval

import (
	"context"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

func TestInterpretPreservesIdentifiersInsideQuestions(t *testing.T) {
	intent := Interpret("Why did case CASE-42 move after error ERR_7731X?")
	if len(intent.FocusTerms) != 2 || intent.FocusTerms[0] != "CASE-42" || intent.FocusTerms[1] != "ERR_7731X" {
		t.Fatalf("focus terms = %v", intent.FocusTerms)
	}
	if !contains(intent.Requirements, "decision_trace") {
		t.Fatalf("requirements = %v", intent.Requirements)
	}
}

func TestInterpretFindsNamesInsideQuestions(t *testing.T) {
	intent := Interpret("Why did Alice Chen leave Acme Corporation?")
	if len(intent.EntityTerms) != 2 || intent.EntityTerms[0] != "Alice Chen" || intent.EntityTerms[1] != "Acme Corporation" {
		t.Fatalf("entity terms = %v", intent.EntityTerms)
	}
}

func TestInterpretSeparatesWorldAndKnowledgeTime(t *testing.T) {
	intent := Interpret("What did we believe on 2026-04-10 about the state valid at 2026-03-25?")
	wantKnown := time.Date(2026, 4, 10, 23, 59, 59, 999999999, time.UTC)
	wantValid := time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)
	if intent.Temporal.KnownAt == nil || !intent.Temporal.KnownAt.Equal(wantKnown) {
		t.Fatalf("known_at = %v", intent.Temporal.KnownAt)
	}
	if intent.Temporal.ValidAt == nil || !intent.Temporal.ValidAt.Equal(wantValid) {
		t.Fatalf("valid_at = %v", intent.Temporal.ValidAt)
	}
}

func TestExplicitTemporalFiltersWin(t *testing.T) {
	explicit := Interpret("valid at 2026-01-01").Temporal
	inferred := Interpret("known at 2026-02-02").Temporal
	got := MergeTemporal(explicit, inferred)
	if got.ValidAt != explicit.ValidAt || got.KnownAt == nil {
		t.Fatalf("merged temporal = %+v", got)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestInterpretIgnoresFunctionWordsAndCalendarWords(t *testing.T) {
	intent := Interpret("For Project-001, why was the supplier changed on January 25? Return the recorded reason code.")
	if len(intent.FocusTerms) != 1 || intent.FocusTerms[0] != "Project-001" {
		t.Fatalf("focus terms = %v", intent.FocusTerms)
	}
	// "For", "January", and "Return" are capitalized and name nothing. A sub-query for any
	// of them retrieves whatever happens to contain a common word.
	if len(intent.EntityTerms) != 0 {
		t.Fatalf("entity terms = %v", intent.EntityTerms)
	}
}

func TestInterpretKeepsNamesThatFollowFunctionWords(t *testing.T) {
	intent := Interpret("For Alice Chen, what changed at New Horizons Ltd in March?")
	if !contains(intent.EntityTerms, "Alice Chen") || !contains(intent.EntityTerms, "New Horizons Ltd") {
		t.Fatalf("entity terms = %v", intent.EntityTerms)
	}
	if contains(intent.EntityTerms, "For") || contains(intent.EntityTerms, "March") {
		t.Fatalf("entity terms = %v", intent.EntityTerms)
	}
}

func TestPlanQueuesIdentifiersBeforeNamePhrases(t *testing.T) {
	plan := heuristicPlanner{hasEmbedder: true}.Plan(context.Background(), domain.QueryRequest{
		Query: "Why did Alice Chen, Bob Stone and Carol Diaz object to CASE-42?",
	})
	if !containsSubQuery(plan.SubQueries, "CASE-42") {
		t.Fatalf("the identifier must survive the sub-query cap, got %+v", plan.SubQueries)
	}
	if len(plan.SubQueries) > domain.MaxSubQueries {
		t.Fatalf("%d sub-queries exceeds the cap", len(plan.SubQueries))
	}
}
