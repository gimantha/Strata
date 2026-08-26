package policy_test

import (
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/domain"
)

// TestIntegrationGraphTraversalHonoursPolicy covers two rules that narrowed every other
// retrieval path and did nothing to this one.
//
// The entity leg has always checked entity types, with a comment saying why: an entity
// carries no classification, so its type is the only lever policy has, and "without this a
// rule hiding a type would still leak the names". Traversal reaches the same entities by a
// different route and made no such check, so a denied type was one hop away from any
// permitted one. Memory kinds had the same shape for a different reason — graph_edges had no
// column to filter on.
//
// The two are fixed differently, and the difference is the point. A memory kind belongs to
// the assertion the edge came from, so it travels with the edge and narrows the walk itself.
// An entity type belongs to the entity rather than the edge, so it is checked when a hit is
// hydrated into a name — which is where the disclosure happens, since traversal returns
// opaque identifiers and hydration is what turns one into something a person can read.
func TestIntegrationGraphTraversalHonoursPolicy(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	// The harness seeds "<marker> Holdings" (organization) OPERATES "<marker> Plant"
	// (facility), which is exactly the shape needed: a permitted type with an edge to a
	// denied one.
	h.seed(t, h.acme, "Thornwick Holdings operates the Thornwick Plant.", "Thornwick")

	denied := domain.PolicyFilters{DeniedEntityTypes: []string{"facility"}}

	// The entity leg alone: this is the check that already exists.
	viaEntity, err := h.retriever.Query(ctx, domain.QueryRequest{
		Scope: h.acme.Scope(), Query: "Thornwick Plant", Policy: denied,
		Modes: []domain.RetrievalMode{domain.ModeEntity}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("entity query: %v", err)
	}
	for _, item := range viaEntity.Items {
		if strings.Contains(item.Content, "Thornwick Plant") {
			t.Error("the entity leg disclosed a denied entity type")
		}
	}

	// And the analogous rule on memory kinds, which the edge also does not carry.
	deniedKind := domain.PolicyFilters{
		DeniedMemoryKinds: []domain.MemoryKind{domain.MemorySemantic},
	}
	viaKind, err := h.retriever.Query(ctx, domain.QueryRequest{
		Scope: h.acme.Scope(), Query: "Thornwick Holdings", Policy: deniedKind,
		Modes: []domain.RetrievalMode{domain.ModeEntity, domain.ModeGraph}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("memory kind query: %v", err)
	}
	for _, item := range viaKind.Items {
		if strings.Contains(item.Content, "Thornwick Plant") {
			t.Errorf("traversal disclosed a relationship whose memory kind policy denies: %q",
				item.Content)
		}
	}

	// The graph leg, reaching the same entity by traversal from a permitted one.
	viaGraph, err := h.retriever.Query(ctx, domain.QueryRequest{
		Scope: h.acme.Scope(), Query: "Thornwick Holdings", Policy: denied,
		Modes: []domain.RetrievalMode{domain.ModeEntity, domain.ModeGraph}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("graph query: %v", err)
	}
	for _, item := range viaGraph.Items {
		if strings.Contains(item.Content, "Thornwick Plant") {
			t.Errorf("traversal disclosed an entity whose type policy denies: %q", item.Content)
		}
	}
}
