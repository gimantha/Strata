package ledger

import (
	"reflect"
	"testing"
)

func TestPredicateSpellingVariantsCoverAuxiliariesAndPrepositions(t *testing.T) {
	got := predicateSpellingVariants("HAS_APPROVED_BUDGET")
	want := []string{"APPROVED_BUDGET", "HAS_APPROVED_BUDGET_FOR", "IS_APPROVED_BUDGET",
		"IS_APPROVED_BUDGET_FOR", "APPROVED_BUDGET_FOR"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("variants = %v, want %v", got, want)
	}
	got = predicateSpellingVariants("DELIVERED_PROTOTYPE")
	for _, v := range got {
		if v == "DELIVERED_PROTOTYPE" {
			t.Fatal("a name is not a variant of itself")
		}
	}
	if !hasString(got, "DELIVERED_PROTOTYPE_FOR") || !hasString(got, "HAS_DELIVERED_PROTOTYPE") {
		t.Fatalf("variants = %v", got)
	}
	// Words that merely begin with an auxiliary's letters are left alone.
	if hasString(predicateSpellingVariants("ISSUED_BY"), "SUED_BY") {
		t.Fatal("only a whole auxiliary followed by an underscore is stripped")
	}
}

func hasString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
