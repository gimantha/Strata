package retrieval_test

import (
	"context"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/knowledge"
)

// The other half of the phase 7 acceptance criterion: temporal and workspace filters must be
// applied correctly (AGENTS.md section 36).

func TestIntegrationRetrievalIsWorkspaceScoped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.loadFixture(t)
	other := h.fixture.NewTenant(t, "globex")

	// Every retriever the planner might choose must respect the boundary, so the query is
	// run with each mode explicitly as well as through the planner.
	for _, modes := range [][]domain.RetrievalMode{
		nil,
		{domain.ModeLexical},
		{domain.ModeExact},
		{domain.ModeVector},
		{domain.ModeEntity},
		{domain.ModeEntity, domain.ModeGraph},
	} {
		result, err := h.retriever.Query(ctx, domain.QueryRequest{
			Scope: other.Scope(),
			Query: "Acme Corporation industrial fasteners ERR_7731X",
			Modes: modes,
			Limit: 20,
		})
		if err != nil {
			t.Fatalf("query with modes %v: %v", modes, err)
		}
		if len(result.Items) != 0 {
			t.Fatalf("modes %v leaked %d results across the workspace boundary",
				modes, len(result.Items))
		}
	}

	// The owning tenant still sees its own data, so the filter is scoping rather than
	// simply breaking retrieval.
	result, err := h.retriever.Query(ctx, domain.QueryRequest{
		Scope: h.scope(), Query: "industrial fasteners", Limit: 5,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result.Items) == 0 {
		t.Fatal("the owning tenant must still retrieve its own records")
	}
}

func TestIntegrationTemporalFiltersNarrowResults(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.loadFixture(t)
	eventID := h.firstEvent(t)

	// A claim that held only during 2025.
	past := date(2025, time.January, 1)
	ended := date(2026, time.January, 1)
	if _, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: h.scope(), Principal: h.fixture.Primary.Principal.Ref(), SourceEventID: eventID,
		Claims: []knowledge.Claim{{
			Subject:   knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
			Predicate: "CERTIFIED_UNDER",
			Object:    domain.ObjectOfSymbol("SCHEME_2025"),
			ValidFrom: &past,
			ValidTo:   &ended,
		}},
	}); err != nil {
		t.Fatalf("assert: %v", err)
	}
	if _, err := h.projector.ProjectEvent(ctx, h.scope(), eventID); err != nil {
		t.Fatalf("project: %v", err)
	}

	query := func(validAt *time.Time) int {
		result, err := h.retriever.Query(ctx, domain.QueryRequest{
			Scope:    h.scope(),
			Query:    "certified under scheme 2025",
			Modes:    []domain.RetrievalMode{domain.ModeLexical},
			Temporal: domain.TemporalQuery{ValidAt: validAt},
			Limit:    10,
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		found := 0
		for _, item := range result.Items {
			if item.Surface == domain.SurfaceAssertion {
				found++
			}
		}
		return found
	}

	if query(nil) == 0 {
		t.Fatal("without a temporal filter the claim should be retrievable")
	}
	if got := query(ptr(date(2025, time.June, 1))); got == 0 {
		t.Fatal("the claim held in mid-2025 and should be retrievable as of then")
	}
	if got := query(ptr(date(2026, time.June, 1))); got != 0 {
		t.Fatalf("the claim had ended by mid-2026 and must not be retrievable as of then, got %d", got)
	}
	// The interval is half-open, so the instant it ends is already outside it.
	if got := query(&ended); got != 0 {
		t.Fatalf("validity is half-open, so the end instant is excluded, got %d", got)
	}
	if got := query(ptr(date(2024, time.June, 1))); got != 0 {
		t.Fatalf("the claim had not begun in 2024, got %d", got)
	}
}

func TestIntegrationFiltersApplyToEveryRetriever(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.loadFixture(t)

	// A surface filter must narrow every retriever, not only the one that happens to
	// support it, or results would depend on which retriever found them.
	result, err := h.retriever.Query(ctx, domain.QueryRequest{
		Scope:   h.scope(),
		Query:   "industrial fasteners",
		Filters: domain.QueryFilters{Surfaces: []domain.Surface{domain.SurfaceChunk}},
		Limit:   20,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result.Items) == 0 {
		t.Fatal("the filtered query should still return chunks")
	}
	for _, item := range result.Items {
		// Graph and entity retrievers return identities; the surface filter must exclude
		// them here rather than let them through a side door.
		if item.Surface != domain.SurfaceChunk {
			t.Fatalf("surface filter leaked a %s result", item.Surface)
		}
	}
}

func TestIntegrationQueryValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	cases := map[string]domain.QueryRequest{
		"no workspace": {Query: "anything"},
		"empty query":  {Scope: h.scope()},
		"bad mode": {
			Scope: h.scope(), Query: "x",
			Modes: []domain.RetrievalMode{domain.RetrievalMode("telepathy")},
		},
		"impossible confidence": {
			Scope: h.scope(), Query: "x",
			Filters: domain.QueryFilters{MinConfidence: 2},
		},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := h.retriever.Query(ctx, req); err == nil {
				t.Fatal("expected the request to be rejected")
			} else if !domain.IsCode(err, domain.CodeInvalidArgument) {
				t.Fatalf("expected invalid_argument, got %s", domain.CodeOf(err))
			}
		})
	}

	// Limits are bounded rather than rejected, so a caller asking for too much gets the
	// maximum instead of an error.
	h.loadFixture(t)
	result, err := h.retriever.Query(ctx, domain.QueryRequest{
		Scope: h.scope(), Query: "industrial", Limit: 100_000,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result.Items) > domain.MaxQueryLimit {
		t.Fatalf("an excessive limit must be capped, got %d results", len(result.Items))
	}
}

func date(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func ptr[T any](v T) *T { return &v }
