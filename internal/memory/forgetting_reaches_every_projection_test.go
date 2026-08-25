package memory_test

import (
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/memory"
)

// TestIntegrationForgettingReachesEveryProjection covers a gap that made soft forgetting
// half-true.
//
// Phase 12 gave the memory service a projector so a deactivation would reach the
// projections rather than only the ledger. It reached one of them. The projector skips
// re-embedding text whose content hash is unchanged — the expensive part, correctly avoided
// — but it skipped the whole vector write along with it, and a deactivation changes only
// lifecycle, never the rendered sentence. So the lexical index learned that a claim had
// stopped being current and the vector index did not, which means "forget this" left the
// claim reachable through one of the five retrieval paths.
//
// The test reads both projections directly rather than going through retrieval, because
// the symptom is invisible from there: vector hits carry no content, so a test matching on
// text passes while the leak is happening.
func TestIntegrationForgettingReachesEveryProjection(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	claim := h.observe(t, "Acme prefers Thursday deliveries.", "forget-projections-1",
		knowledge.Claim{
			Subject:    knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
			Predicate:  "PREFERS",
			Object:     domain.ObjectOfString("Thursday deliveries"),
			MemoryKind: domain.MemoryPreference,
		})

	activeUntil := func(table string) (*time.Time, bool) {
		t.Helper()

		var (
			until *time.Time
			found bool
		)
		rows, err := h.fixture.Store.Pool().Query(ctx,
			`SELECT active_until FROM `+table+` WHERE record_id = $1 AND surface = 'assertion'`,
			string(claim.ID))
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		defer rows.Close()
		for rows.Next() {
			found = true
			if err := rows.Scan(&until); err != nil {
				t.Fatalf("scan %s: %v", table, err)
			}
		}
		return until, found
	}

	for _, table := range []string{"vector_records", "lexical_records"} {
		if _, ok := activeUntil(table); !ok {
			t.Fatalf("the claim was never projected into %s, so this test would prove nothing",
				table)
		}
	}

	if _, err := h.memory.Forget(ctx, memory.ForgetRequest{
		Scope:       h.scope(),
		Actor:       h.fixture.Primary.Principal.Ref(),
		AssertionID: claim.ID,
		Kind:        domain.ForgetDeactivate,
		Reason:      "the customer asked us to stop using this",
	}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// Both projections, not one. A claim that is inactive in the lexical index and active
	// in the vector index has not been forgotten; it has been forgotten in one place.
	for _, table := range []string{"lexical_records", "vector_records"} {
		until, _ := activeUntil(table)
		if until == nil {
			t.Errorf("%s still records the claim as active after a deactivation", table)
		}
	}

	// And reactivation clears it again in both, so the reversibility that makes
	// deactivation safe is not itself half-applied.
	if _, err := h.memory.Reactivate(ctx, h.scope(),
		h.fixture.Primary.Principal.Ref(), claim.ID); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	for _, table := range []string{"lexical_records", "vector_records"} {
		if until, _ := activeUntil(table); until != nil {
			t.Errorf("%s still records the claim as deactivated after a reactivation", table)
		}
	}
}
