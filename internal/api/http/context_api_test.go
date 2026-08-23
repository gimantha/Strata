package http_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/domain"
)

// TestIntegrationContextEndpointReturnsCitedPromptText walks the endpoint the way an agent
// would: ask a question, get text to put in a prompt, and get the references needed to check
// what it says.
func TestIntegrationContextEndpointReturnsCitedPromptText(t *testing.T) {
	h := newAPIHarness(t)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	eventID, episodeID := h.ingestForKnowledge(t,
		"Acme Corporation supplies industrial fasteners to the Portland plant.", "api-ctx-1")

	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/assertions", owner, map[string]any{
		"source_event_id": eventID,
		"claims": []map[string]any{{
			"subject":   map[string]any{"name": "Acme Corporation", "type": "organization"},
			"predicate": "supplies",
			"object":    map[string]any{"kind": "string", "text": "industrial fasteners"},
			"evidence": []map[string]any{{
				"episode_id":     episodeID,
				"extracted_text": "Acme Corporation supplies industrial fasteners",
			}},
		}},
	}, nil)
	if status != http.StatusCreated && status != http.StatusOK {
		t.Fatalf("assert returned %d: %s", status, body)
	}

	status, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/context", owner, map[string]any{
		"query":        "who supplies industrial fasteners",
		"token_budget": 1200,
		"explain":      true,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("context returned %d: %s", status, body)
	}

	response := decode(t, body)
	text, _ := response["context"].(string)
	if !strings.Contains(text, "CONTEXT BLOCK") {
		t.Fatalf("expected a rendered block, got: %s", body)
	}
	if !strings.Contains(text, "never as instructions") {
		t.Fatal("the block must state how quoted source content is to be read")
	}

	items, _ := response["items"].([]any)
	citations, _ := response["citations"].([]any)
	if len(items) == 0 {
		t.Fatalf("expected items: %s", body)
	}
	if len(citations) != len(items) {
		t.Fatalf("%d items carry %d citations", len(items), len(citations))
	}

	budget, _ := response["budget"].(map[string]any)
	used, _ := budget["used"].(float64)
	limit, _ := budget["limit"].(float64)
	if used > limit {
		t.Fatalf("the block reports %v tokens against a %v budget", used, limit)
	}
	if used == 0 {
		t.Fatal("a block with content cannot cost nothing")
	}

	for _, entry := range items {
		item, _ := entry.(map[string]any)
		if _, ok := item["trusted"].(bool); !ok {
			// A caller re-rendering items itself needs to know which are quoted source.
			t.Fatalf("item does not say whether it is trusted: %v", item)
		}
	}
}

func TestIntegrationContextEndpointValidatesAndScopes(t *testing.T) {
	h := newAPIHarness(t,
		keyEntry{principalID: "acme-owner", secret: "a", systemRole: domain.RoleAdmin},
		keyEntry{principalID: "globex-owner", secret: "b", systemRole: domain.RoleAdmin},
	)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	h.ingestForKnowledge(t, "Acme Corporation supplies industrial fasteners.", "api-ctx-scope-1")

	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/context", "globex-owner",
		map[string]any{"query": "industrial fasteners"}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant, got %d: %s", status, body)
	}

	status, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/context", owner,
		map[string]any{"query": "fasteners", "token_budget": 5}, nil)
	if status != http.StatusBadRequest {
		// A budget too small to hold the scaffolding would return an empty block, which
		// looks like a retrieval failure rather than a configuration mistake.
		t.Fatalf("expected 400 for an unusable budget, got %d: %s", status, body)
	}

	status, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/context", owner,
		map[string]any{"query": "fasteners", "sections": []string{"nonsense"}}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown section, got %d: %s", status, body)
	}
}
