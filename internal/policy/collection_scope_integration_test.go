package policy_test

import (
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/normalize"
)

// TestIntegrationCollectionPolicyNarrowsRetrieval covers a gap that made a whole class of
// policy rule decorative.
//
// PolicyFilters has carried AllowedCollections and DeniedCollections since phase 11, and a
// rule naming a collection has been populating them — Restrictive() even returned true for
// them. Nothing applied them. The projections had no collection to filter on and
// applyPolicyFilters had no branch for one, so a policy that denied a collection excluded
// nothing from retrieval. Section 22.4 forbids exactly that: retrieving unauthorized data
// and relying on something downstream to hide it.
//
// Measured before the fix: a query under a denial returned the denied collection's passage
// verbatim.
func TestIntegrationCollectionPolicyNarrowsRetrieval(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	open := h.newCollection(t, "open")
	sealed := h.newCollection(t, "sealed")

	h.ingestInto(t, open, "The Wrenfield depot handles routine dispatch.", "coll-open")
	h.ingestInto(t, sealed, "The Wrenfield depot conceals the Havelock discrepancy.", "coll-sealed")
	// A passage in no collection at all, which is the case an allow rule must not hide.
	h.ingestInto(t, "", "The Wrenfield depot was surveyed in the spring.", "coll-none")

	visible := func(filters domain.PolicyFilters) map[string]bool {
		t.Helper()
		result, err := h.retriever.Query(ctx, domain.QueryRequest{
			Scope: h.acme.Scope(), Query: "Wrenfield depot", Policy: filters, Limit: 50,
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		seen := map[string]bool{}
		for _, item := range result.Items {
			for _, marker := range []string{"dispatch", "Havelock", "surveyed"} {
				if strings.Contains(item.Content, marker) {
					seen[marker] = true
				}
			}
		}
		return seen
	}

	// Without policy, everything is reachable — otherwise the assertions below would pass
	// by finding nothing.
	if all := visible(domain.PolicyFilters{}); !all["dispatch"] || !all["Havelock"] || !all["surveyed"] {
		t.Fatalf("the fixture is not retrievable without policy: %v", all)
	}

	denied := domain.PolicyFilters{DeniedCollections: []domain.CollectionID{sealed}}
	if !denied.Restrictive() {
		t.Fatal("a collection denial does not register as restrictive")
	}
	under := visible(denied)
	if under["Havelock"] {
		t.Error("a denied collection's material was retrieved")
	}
	if !under["dispatch"] {
		t.Error("denying one collection hid another")
	}
	if !under["surveyed"] {
		t.Error("denying a collection hid material that belongs to no collection")
	}

	allowed := domain.PolicyFilters{AllowedCollections: []domain.CollectionID{open}}
	under = visible(allowed)
	if under["Havelock"] {
		t.Error("an allow-list did not exclude a collection outside it")
	}
	if !under["dispatch"] {
		t.Error("an allow-list excluded the collection it names")
	}
	// The same reasoning the entity-type and predicate filters already use: material that
	// is not collection-scoped is not what a collection rule is about, and hiding it would
	// make one narrow rule quietly remove most of the corpus.
	if !under["surveyed"] {
		t.Error("an allow-list hid material that belongs to no collection")
	}
}

// newCollection adds a collection to the primary tenant's graph space.
func (h *harness) newCollection(t *testing.T, slug string) domain.CollectionID {
	t.Helper()

	collection, err := h.fixture.Store.CreateCollection(t.Context(), domain.Collection{
		WorkspaceID: h.acme.Workspace.ID, GraphSpaceID: h.acme.GraphSpace.ID,
		Slug: slug, Name: slug,
	}, h.acme.Principal.ID)
	if err != nil {
		t.Fatalf("create collection %s: %v", slug, err)
	}
	return collection.ID
}

// ingestInto puts material in a collection, or in none when the id is empty.
func (h *harness) ingestInto(t *testing.T, collection domain.CollectionID, text, key string) {
	t.Helper()

	scope := h.acme.Scope()
	scope.CollectionID = collection

	receipt, err := h.gateway.Accept(t.Context(), ingest.Request{
		Scope: scope, Principal: h.acme.Principal.Ref(), SourceID: h.acme.Source.ID,
		MediaType: normalize.MediaTypePlain, Payload: []byte(text), IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", key, err)
	}
	if _, err := h.runner.Process(t.Context(), h.acme.Workspace.ID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process %s: %v", key, err)
	}
}
