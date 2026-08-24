package http_test

import (
	"net/http"
	"testing"

	"github.com/gimantha/strata/internal/domain"
)

// TestIntegrationChangesEndpointAcceptsAPushedBatch covers the push half of the connector
// contract: an upstream that can only write to us lands in the same place as one we read.
func TestIntegrationChangesEndpointAcceptsAPushedBatch(t *testing.T) {
	h := newAPIHarness(t)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	batch := map[string]any{
		"source_name": h.fixture.Primary.Source.Name,
		"stream":      "public.customers",
		"changes": []map[string]any{
			{
				"stream":    "public.customers",
				"operation": "insert",
				"key":       map[string]any{"id": 42},
				"after":     map[string]any{"id": 42, "name": "Acme Corporation", "tier": "STANDARD"},
				"offset":    "0/1000",
				"sequence":  "1",
			},
			{
				"stream":    "public.customers",
				"operation": "update",
				"key":       map[string]any{"id": 42},
				"before":    map[string]any{"id": 42, "name": "Acme Corporation", "tier": "STANDARD"},
				"after":     map[string]any{"id": 42, "name": "Acme Corporation", "tier": "PREMIUM"},
				"offset":    "0/2000",
				"sequence":  "2",
			},
		},
	}

	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/changes", owner, batch, nil)
	if status != http.StatusAccepted {
		// 202, not 200: the changes are durable, but turning them into knowledge happens
		// on the pipeline.
		t.Fatalf("expected 202, got %d: %s", status, body)
	}

	response := decode(t, body)
	if response["accepted"].(float64) != 2 {
		t.Fatalf("expected two accepted changes: %s", body)
	}

	// Re-pushing the same batch must not create new events.
	status, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/changes", owner, batch, nil)
	if status != http.StatusAccepted {
		t.Fatalf("expected 202 on replay, got %d: %s", status, body)
	}
	replay := decode(t, body)
	if replay["accepted"].(float64) != 0 || replay["duplicates"].(float64) != 2 {
		t.Fatalf("a replayed batch should be recognized as duplicate: %s", body)
	}
}

func TestIntegrationChangesEndpointValidatesAndScopes(t *testing.T) {
	h := newAPIHarness(t,
		keyEntry{principalID: "acme-owner", secret: "a", systemRole: domain.RoleAdmin},
		keyEntry{principalID: "globex-owner", secret: "b", systemRole: domain.RoleAdmin},
	)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	valid := map[string]any{
		"source_name": h.fixture.Primary.Source.Name,
		"changes": []map[string]any{{
			"stream": "public.customers", "operation": "insert",
			"key": map[string]any{"id": 1}, "after": map[string]any{"id": 1},
		}},
	}

	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/changes", "globex-owner", valid, nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant, got %d: %s", status, body)
	}

	status, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/changes", owner,
		map[string]any{"source_name": h.fixture.Primary.Source.Name, "changes": []any{}}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for an empty batch, got %d: %s", status, body)
	}

	status, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/changes", owner,
		map[string]any{
			"source_name": h.fixture.Primary.Source.Name,
			"changes": []map[string]any{{
				// No key: the change cannot be tied to the record it changed.
				"stream": "public.customers", "operation": "insert",
				"after": map[string]any{"id": 1},
			}},
		}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for a keyless change, got %d: %s", status, body)
	}
}
