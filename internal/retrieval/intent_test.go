package retrieval

import (
	"testing"
	"time"
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
