package ledger_test

import (
	"context"
	"testing"

	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestIntegrationPredicateSpellingsResolveToOneRegistryEntry(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()
	ws := f.Primary.Scope().WorkspaceID

	first, err := f.Store.EnsurePredicate(ctx, ws, "HAS_APPROVED_BUDGET")
	if err != nil {
		t.Fatal(err)
	}
	// The same relation spelled without the auxiliary is not a second predicate.
	second, err := f.Store.EnsurePredicate(ctx, ws, "approved budget")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("APPROVED_BUDGET was registered separately from HAS_APPROVED_BUDGET: %s vs %s", second.ID, first.ID)
	}
	// And it is recorded as an alias, so later lookups by either spelling resolve.
	resolved, err := f.Store.GetPredicateByName(ctx, ws, "APPROVED_BUDGET")
	if err != nil || resolved.ID != first.ID {
		t.Fatalf("alias lookup = %v %v", resolved.ID, err)
	}
	// A trailing preposition is the same grammar difference.
	third, err := f.Store.EnsurePredicate(ctx, ws, "DELIVERED_PROTOTYPE_FOR")
	if err != nil {
		t.Fatal(err)
	}
	fourth, err := f.Store.EnsurePredicate(ctx, ws, "DELIVERED_PROTOTYPE")
	if err != nil {
		t.Fatal(err)
	}
	if fourth.ID != third.ID {
		t.Fatal("DELIVERED_PROTOTYPE and DELIVERED_PROTOTYPE_FOR must share one registry entry")
	}
	// Unrelated names that merely begin with an auxiliary's letters stay distinct.
	issued, err := f.Store.EnsurePredicate(ctx, ws, "ISSUED_BY")
	if err != nil {
		t.Fatal(err)
	}
	if issued.ID == first.ID || issued.ID == third.ID {
		t.Fatal("ISSUED_BY must not be merged into anything")
	}
}
