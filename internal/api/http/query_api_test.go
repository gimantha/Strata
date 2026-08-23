package http_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/domain"
)

// TestIntegrationQueryEndpointReturnsFusedResults exercises the whole path the API exposes:
// ingest, process into projections, then query. It is deliberately end-to-end, because the
// interesting failures here are wiring failures — a retriever that never runs, a projection
// stage that never fired — and a unit test on fusion cannot see either.
func TestIntegrationQueryEndpointReturnsFusedResults(t *testing.T) {
	h := newAPIHarness(t)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	h.ingestForKnowledge(t,
		"Acme Corporation supplies industrial fasteners to the Portland plant.", "api-query-1")
	h.ingestForKnowledge(t,
		"The Portland plant runs a night shift on Thursdays.", "api-query-2")

	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/query", owner, map[string]any{
		"query":   "who supplies industrial fasteners",
		"limit":   5,
		"explain": true,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("query returned %d: %s", status, body)
	}

	response := decode(t, body)
	results, _ := response["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("expected results, got: %s", body)
	}

	top, _ := results[0].(map[string]any)
	content, _ := top["content"].(string)
	if !strings.Contains(content, "fasteners") {
		t.Fatalf("top result should mention fasteners, got %q", content)
	}
	if score, _ := top["score"].(float64); score <= 0 {
		// Every fused score being zero is the signature of unset retriever weights, which
		// still returns plausible-looking results ranked by nothing at all.
		t.Fatalf("expected a positive fused score, got %v", top["score"])
	}
	if found, _ := top["found_by"].([]any); len(found) == 0 {
		t.Fatal("explain should report which retrievers found the result")
	}
	if _, ok := top["signals"]; !ok {
		t.Fatal("explain should report per-retriever signals")
	}
	if _, ok := response["plan"]; !ok {
		t.Fatal("explain should report the query plan")
	}
}

// TestIntegrationQueryEndpointIsGraphSpaceScoped holds the invariant that matters most here:
// retrieval reads from projections, and a projection is one more place tenant scope could be
// dropped without anyone noticing until it matters.
func TestIntegrationQueryEndpointIsGraphSpaceScoped(t *testing.T) {
	h := newAPIHarness(t,
		keyEntry{principalID: "acme-owner", secret: "a", systemRole: domain.RoleAdmin},
		keyEntry{principalID: "globex-owner", secret: "b", systemRole: domain.RoleAdmin},
	)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	h.ingestForKnowledge(t, "Acme Corporation supplies industrial fasteners.", "api-query-scope-1")

	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/query", "globex-owner", map[string]any{
		"query": "industrial fasteners",
	}, nil)
	if status != http.StatusNotFound {
		// Absence, not denial: confirming the graph space exists is itself information.
		t.Fatalf("expected 404 for another tenant, got %d: %s", status, body)
	}

	status, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/query", owner, map[string]any{
		"query": "",
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for an empty query, got %d: %s", status, body)
	}
}
