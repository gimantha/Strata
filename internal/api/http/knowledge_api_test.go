package http_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/normalize"
)

// ingestForKnowledge puts real source material in place and returns the identifiers a
// claim needs to cite it.
func (h *apiHarness) ingestForKnowledge(t *testing.T, content, key string) (string, string) {
	t.Helper()

	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/events", owner, map[string]any{
		"source_name": h.fixture.Primary.Source.Name,
		"media_type":  normalize.MediaTypePlain,
		"content":     content,
	}, map[string]string{"Idempotency-Key": key})
	if status != http.StatusAccepted {
		t.Fatalf("ingest returned %d: %s", status, body)
	}
	eventID := decode(t, body)["source_event_id"].(string)

	if _, err := h.runner.Process(t.Context(), h.fixture.Primary.Workspace.ID,
		domain.SourceEventID(eventID), false); err != nil {
		t.Fatalf("process: %v", err)
	}
	episodes, err := h.fixture.Store.ListEpisodes(t.Context(), h.fixture.Primary.Workspace.ID,
		domain.SourceEventID(eventID))
	if err != nil || len(episodes) == 0 {
		t.Fatalf("expected episodes: %v", err)
	}
	return eventID, string(episodes[0].ID)
}

func TestIntegrationAssertAndQueryThroughTheAPI(t *testing.T) {
	h := newAPIHarness(t)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	eventID, episodeID := h.ingestForKnowledge(t, "Alice Chen is the CTO of Acme.", "api-know-1")

	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/assertions", owner, map[string]any{
		"source_event_id": eventID,
		"claims": []map[string]any{{
			"subject":       map[string]any{"name": "Alice Chen", "type": "person"},
			"predicate":     "role_at",
			"object_entity": map[string]any{"name": "Acme", "type": "organization"},
			"scope_key":     "CTO",
			"valid_from":    "2026-01-01T00:00:00Z",
			"evidence": []map[string]any{{
				"episode_id":     episodeID,
				"extracted_text": "Alice Chen is the CTO of Acme.",
			}},
		}},
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("assert returned %d: %s", status, body)
	}

	created := decode(t, body)
	assertions := created["assertions"].([]any)
	if len(assertions) != 1 {
		t.Fatalf("expected one assertion: %s", body)
	}
	first := assertions[0].(map[string]any)
	assertionID := first["id"].(string)

	// The predicate was normalized into the registry's canonical form.
	if first["predicate"] != "ROLE_AT" {
		t.Fatalf("predicate should be normalized, got %v", first["predicate"])
	}
	// All four temporal layers are exposed separately.
	temporal := first["temporal"].(map[string]any)
	for _, field := range []string{"observed_at", "recorded_at", "valid_from"} {
		if temporal[field] == nil || temporal[field] == "" {
			t.Fatalf("temporal.%s missing: %s", field, body)
		}
	}

	// The object keeps its type on the wire.
	object := first["object"].(map[string]any)
	if object["kind"] != string(domain.ObjectEntity) {
		t.Fatalf("expected an entity object, got %v", object["kind"])
	}

	// Query it back by predicate.
	status, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/assertions/query", owner,
		map[string]any{"predicates": []string{"role_at"}}, nil)
	if status != http.StatusOK {
		t.Fatalf("query returned %d: %s", status, body)
	}
	if count := decode(t, body)["count"].(float64); count != 1 {
		t.Fatalf("expected 1 result, got %v", count)
	}

	// And fetch it directly.
	status, body = h.do(t, http.MethodGet, "/v1/assertions/"+assertionID, owner, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("get returned %d: %s", status, body)
	}
	if decode(t, body)["id"] != assertionID {
		t.Fatalf("unexpected assertion: %s", body)
	}
}

func TestIntegrationProvenanceEndpointWalksToSource(t *testing.T) {
	h := newAPIHarness(t)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	const statement = "Acme Corporation supplies industrial fasteners."
	eventID, episodeID := h.ingestForKnowledge(t, statement, "api-prov-1")

	_, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/assertions", owner, map[string]any{
		"source_event_id": eventID,
		"claims": []map[string]any{{
			"subject":   map[string]any{"name": "Acme Corporation", "type": "organization"},
			"predicate": "classified_as",
			"object":    map[string]any{"kind": "symbol", "text": "SUPPLIER"},
			"evidence":  []map[string]any{{"episode_id": episodeID, "extracted_text": statement}},
		}},
	}, nil)
	assertionID := decode(t, body)["assertions"].([]any)[0].(map[string]any)["id"].(string)

	status, body := h.do(t, http.MethodGet, "/v1/assertions/"+assertionID+"/provenance", owner, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("provenance returned %d: %s", status, body)
	}

	chain := decode(t, body)
	links := chain["evidence_chain"].([]any)
	if len(links) != 1 {
		t.Fatalf("expected one evidence link: %s", body)
	}
	link := links[0].(map[string]any)

	// Every hop of the chain must be present: episode, artifact, source event, source.
	for _, hop := range []string{"evidence", "episode", "artifact", "source_event", "source"} {
		if link[hop] == nil {
			t.Fatalf("provenance chain is missing its %s hop: %s", hop, body)
		}
	}
	if link["episode"].(map[string]any)["content"] != statement {
		t.Fatal("the chain must reach the archived source text")
	}
	if link["artifact"].(map[string]any)["content_hash"] != domain.ContentHash([]byte(statement)) {
		t.Fatal("the chain must reach the content-addressed artifact")
	}
	if link["source"].(map[string]any)["trust_level"] == "" {
		t.Fatal("the chain must expose how far the source is trusted")
	}
}

func TestIntegrationCorrectionAndHistoricalBeliefThroughTheAPI(t *testing.T) {
	h := newAPIHarness(t)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	firstEvent, firstEpisode := h.ingestForKnowledge(t, "Acme is on the enterprise plan.", "api-corr-1")
	_, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/assertions", owner, map[string]any{
		"source_event_id": firstEvent,
		"claims": []map[string]any{{
			"subject":    map[string]any{"name": "Acme", "type": "organization"},
			"predicate":  "plan_tier",
			"object":     map[string]any{"kind": "symbol", "text": "ENTERPRISE"},
			"valid_from": "2026-01-01T00:00:00Z",
			"evidence":   []map[string]any{{"episode_id": firstEpisode}},
		}},
	}, nil)
	originalID := decode(t, body)["assertions"].([]any)[0].(map[string]any)["id"].(string)
	beforeCorrection := time.Now().UTC()

	// A correction supersedes rather than edits.
	secondEvent, secondEpisode := h.ingestForKnowledge(t, "Correction: Acme is on standard.", "api-corr-2")
	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/assertions", owner, map[string]any{
		"source_event_id": secondEvent,
		"claims": []map[string]any{{
			"subject":    map[string]any{"name": "Acme", "type": "organization"},
			"predicate":  "plan_tier",
			"object":     map[string]any{"kind": "symbol", "text": "STANDARD"},
			"valid_from": "2026-01-01T00:00:00Z",
			"supersedes": []string{originalID},
			"evidence":   []map[string]any{{"episode_id": secondEpisode}},
		}},
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("correction returned %d: %s", status, body)
	}
	if superseded := decode(t, body)["superseded"].([]any); len(superseded) != 1 {
		t.Fatalf("the correction must report what it replaced: %s", body)
	}

	// Current belief is the correction alone.
	_, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/assertions/query", owner,
		map[string]any{"predicates": []string{"plan_tier"}}, nil)
	current := decode(t, body)["assertions"].([]any)
	if len(current) != 1 {
		t.Fatalf("expected one current belief, got %d: %s", len(current), body)
	}
	if current[0].(map[string]any)["object"].(map[string]any)["value"] != "STANDARD" {
		t.Fatalf("current belief should be the correction: %s", body)
	}

	// As of before the correction, the original was believed.
	_, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/assertions/query", owner, map[string]any{
		"predicates": []string{"plan_tier"},
		"known_at":   beforeCorrection.Format(time.RFC3339Nano),
	}, nil)
	historical := decode(t, body)["assertions"].([]any)
	if len(historical) != 1 || historical[0].(map[string]any)["id"] != originalID {
		t.Fatalf("as-of query must return the belief held then: %s", body)
	}
}

func TestIntegrationRetractThroughTheAPI(t *testing.T) {
	h := newAPIHarness(t)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	eventID, episodeID := h.ingestForKnowledge(t, "Acme uses the beta API.", "api-retract-1")
	_, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/assertions", owner, map[string]any{
		"source_event_id": eventID,
		"claims": []map[string]any{{
			"subject":   map[string]any{"name": "Acme", "type": "organization"},
			"predicate": "uses_product",
			"object":    map[string]any{"kind": "symbol", "text": "BETA_API"},
			"evidence":  []map[string]any{{"episode_id": episodeID}},
		}},
	}, nil)
	assertionID := decode(t, body)["assertions"].([]any)[0].(map[string]any)["id"].(string)

	// A retraction must say why.
	status, body := h.do(t, http.MethodPost, "/v1/assertions/"+assertionID+"/retract", owner,
		map[string]any{}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("a retraction without a reason must be refused, got %d: %s", status, body)
	}

	status, body = h.do(t, http.MethodPost, "/v1/assertions/"+assertionID+"/retract", owner,
		map[string]any{"reason": "source reported in error"}, nil)
	if status != http.StatusOK {
		t.Fatalf("retract returned %d: %s", status, body)
	}
	if decode(t, body)["status"] != string(domain.AssertionRetracted) {
		t.Fatalf("expected a retracted claim: %s", body)
	}

	// It is gone from current belief but still fetchable, because it is history.
	_, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/assertions/query", owner,
		map[string]any{"predicates": []string{"uses_product"}}, nil)
	if count := decode(t, body)["count"].(float64); count != 0 {
		t.Fatalf("a retracted claim must not be current belief: %s", body)
	}
	status, _ = h.do(t, http.MethodGet, "/v1/assertions/"+assertionID, owner, nil, nil)
	if status != http.StatusOK {
		t.Fatal("a retracted claim must remain readable: retraction is not deletion")
	}
}

func TestIntegrationPredicateRegistryThroughTheAPI(t *testing.T) {
	h := newAPIHarness(t)
	owner := string(h.fixture.Primary.Principal.ID)
	ws := string(h.fixture.Primary.Workspace.ID)

	status, body := h.do(t, http.MethodPost, "/v1/workspaces/"+ws+"/predicates", owner, map[string]any{
		"name": "current_plan", "functional": true, "conflict_policy": "manual",
		"object_kinds": []string{"symbol"},
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("define predicate returned %d: %s", status, body)
	}
	defined := decode(t, body)
	if defined["name"] != "CURRENT_PLAN" {
		t.Fatalf("predicate names must be normalized, got %v", defined["name"])
	}
	if defined["version"].(float64) != 1 {
		t.Fatalf("a new predicate starts at version 1: %s", body)
	}

	// Changing semantics bumps the version so existing claims keep their meaning.
	_, body = h.do(t, http.MethodPost, "/v1/workspaces/"+ws+"/predicates", owner, map[string]any{
		"name": "current_plan", "functional": false, "conflict_policy": "coexist",
	}, nil)
	if version := decode(t, body)["version"].(float64); version != 2 {
		t.Fatalf("a semantic change must bump the version, got %v", version)
	}

	// An unknown predicate used in a claim is registered as a candidate rather than
	// rejected, which is open mode.
	eventID, episodeID := h.ingestForKnowledge(t, "Acme prefers email contact.", "api-pred-1")
	h.do(t, http.MethodPost, "/v1/graph-spaces/"+string(h.fixture.Primary.GraphSpace.ID)+"/assertions",
		owner, map[string]any{
			"source_event_id": eventID,
			"claims": []map[string]any{{
				"subject":   map[string]any{"name": "Acme", "type": "organization"},
				"predicate": "prefersContactVia",
				"object":    map[string]any{"kind": "symbol", "text": "EMAIL"},
				"evidence":  []map[string]any{{"episode_id": episodeID}},
			}},
		}, nil)

	_, body = h.do(t, http.MethodGet, "/v1/workspaces/"+ws+"/predicates", owner, nil, nil)
	var found bool
	for _, raw := range decode(t, body)["predicates"].([]any) {
		p := raw.(map[string]any)
		if p["name"] == "PREFERS_CONTACT_VIA" {
			found = true
			if p["status"] != string(domain.PredicateCandidate) {
				t.Fatalf("an invented predicate must be registered as a candidate, got %v", p["status"])
			}
			if p["functional"].(bool) {
				t.Fatal("a candidate must not be assumed functional: that would invent contradictions")
			}
		}
	}
	if !found {
		t.Fatalf("the invented predicate should appear in the registry: %s", body)
	}
}

func TestIntegrationEntitiesThroughTheAPI(t *testing.T) {
	h := newAPIHarness(t)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/entities", owner, map[string]any{
		"canonical_name": "Acme Corporation",
		"entity_type":    "organization",
		"aliases":        []string{"Acme", "Acme Corp"},
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create entity returned %d: %s", status, body)
	}
	created := decode(t, body)
	entityID := created["id"].(string)
	if len(created["aliases"].([]any)) != 3 {
		t.Fatalf("the canonical name and both aliases should be searchable: %s", body)
	}

	status, body = h.do(t, http.MethodGet, "/v1/entities/"+entityID, owner, nil, nil)
	if status != http.StatusOK || decode(t, body)["canonical_name"] != "Acme Corporation" {
		t.Fatalf("get entity returned %d: %s", status, body)
	}

	// An alias resolves to the same identity.
	_, body = h.do(t, http.MethodGet, "/v1/graph-spaces/"+gs+"/entities?name=acme%20corp", owner, nil, nil)
	entities := decode(t, body)["entities"].([]any)
	if len(entities) != 1 || entities[0].(map[string]any)["id"] != entityID {
		t.Fatalf("alias lookup should find the entity: %s", body)
	}
}

func TestIntegrationConflictsThroughTheAPI(t *testing.T) {
	h := newAPIHarness(t)
	owner := string(h.fixture.Primary.Principal.ID)
	ws := string(h.fixture.Primary.Workspace.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	h.do(t, http.MethodPost, "/v1/workspaces/"+ws+"/predicates", owner, map[string]any{
		"name": "current_plan", "functional": true, "conflict_policy": "manual",
	}, nil)

	assert := func(event, episode, value string) map[string]any {
		_, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/assertions", owner, map[string]any{
			"source_event_id": event,
			"claims": []map[string]any{{
				"subject":    map[string]any{"name": "Acme", "type": "organization"},
				"predicate":  "current_plan",
				"object":     map[string]any{"kind": "symbol", "text": value},
				"valid_from": "2026-01-01T00:00:00Z",
				"evidence":   []map[string]any{{"episode_id": episode}},
			}},
		}, nil)
		return decode(t, body)
	}

	firstEvent, firstEpisode := h.ingestForKnowledge(t, "Acme is enterprise.", "api-conf-1")
	assert(firstEvent, firstEpisode, "ENTERPRISE")

	secondEvent, secondEpisode := h.ingestForKnowledge(t, "Acme is standard.", "api-conf-2")
	second := assert(secondEvent, secondEpisode, "STANDARD")
	if len(second["conflicts"].([]any)) != 1 {
		t.Fatalf("the contradiction should be recorded: %v", second["conflicts"])
	}

	status, body := h.do(t, http.MethodGet, "/v1/graph-spaces/"+gs+"/conflicts", owner, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list conflicts returned %d: %s", status, body)
	}
	conflicts := decode(t, body)["conflicts"].([]any)
	if len(conflicts) != 1 {
		t.Fatalf("expected one open conflict: %s", body)
	}
	conflictID := conflicts[0].(map[string]any)["id"].(string)

	status, body = h.do(t, http.MethodPost, "/v1/conflicts/"+conflictID+"/resolve", owner,
		map[string]any{"resolution": "resolved_by_human"}, nil)
	if status != http.StatusOK {
		t.Fatalf("resolve returned %d: %s", status, body)
	}

	_, body = h.do(t, http.MethodGet, "/v1/graph-spaces/"+gs+"/conflicts", owner, nil, nil)
	if count := decode(t, body)["count"].(float64); count != 0 {
		t.Fatalf("the conflict should be closed: %s", body)
	}
}

func TestIntegrationKnowledgeEndpointsAreWorkspaceScoped(t *testing.T) {
	h := newAPIHarness(t,
		keyEntry{principalID: "acme-owner", secret: "a", systemRole: domain.RoleAdmin},
		keyEntry{principalID: "globex-owner", secret: "b", systemRole: domain.RoleAdmin},
	)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	eventID, episodeID := h.ingestForKnowledge(t, "Acme is a supplier.", "api-iso-1")
	_, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/assertions", owner, map[string]any{
		"source_event_id": eventID,
		"claims": []map[string]any{{
			"subject":   map[string]any{"name": "Acme", "type": "organization"},
			"predicate": "classified_as",
			"object":    map[string]any{"kind": "symbol", "text": "SUPPLIER"},
			"evidence":  []map[string]any{{"episode_id": episodeID}},
		}},
	}, nil)
	created := decode(t, body)["assertions"].([]any)[0].(map[string]any)
	assertionID := created["id"].(string)
	subjectID := created["subject_id"].(string)

	// Every knowledge read path must report absence to another tenant, not forbidden.
	for _, path := range []string{
		"/v1/assertions/" + assertionID,
		"/v1/assertions/" + assertionID + "/provenance",
		"/v1/entities/" + subjectID,
	} {
		status, body := h.do(t, http.MethodGet, path, "globex-owner", nil, nil)
		if status != http.StatusNotFound {
			t.Fatalf("%s leaked to another tenant with %d: %s", path, status, body)
		}
	}

	status, body := h.do(t, http.MethodPost, "/v1/assertions/"+assertionID+"/retract", "globex-owner",
		map[string]any{"reason": "not mine to retract"}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("another tenant must not be able to retract, got %d: %s", status, body)
	}
}

func TestIntegrationClaimValidationThroughTheAPI(t *testing.T) {
	h := newAPIHarness(t)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)
	eventID, episodeID := h.ingestForKnowledge(t, "Some facts.", "api-valid-1")

	base := func(claim map[string]any) map[string]any {
		return map[string]any{"source_event_id": eventID, "claims": []map[string]any{claim}}
	}

	cases := map[string]map[string]any{
		"no object": {
			"subject": map[string]any{"name": "Acme", "type": "organization"}, "predicate": "x",
		},
		"unknown object kind": {
			"subject": map[string]any{"name": "Acme"}, "predicate": "x",
			"object": map[string]any{"kind": "quaternion", "text": "i"},
		},
		"integer object with no value": {
			"subject": map[string]any{"name": "Acme"}, "predicate": "x",
			"object": map[string]any{"kind": "integer"},
		},
		"malformed date": {
			"subject": map[string]any{"name": "Acme"}, "predicate": "x",
			"object": map[string]any{"kind": "date", "date": "the third of March"},
		},
		"bad memory kind": {
			"subject": map[string]any{"name": "Acme"}, "predicate": "x",
			"object": map[string]any{"kind": "boolean", "boolean": true}, "memory_kind": "vibes",
		},
		"inferred without derivation": {
			"subject": map[string]any{"name": "Acme"}, "predicate": "x",
			"object":          map[string]any{"kind": "boolean", "boolean": true},
			"provenance_mode": "inferred",
			"evidence":        []map[string]any{{"episode_id": episodeID}},
		},
	}

	for name, claim := range cases {
		t.Run(name, func(t *testing.T) {
			status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/assertions", owner,
				base(claim), nil)
			if status != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", status, body)
			}
			if errorCode(t, body) == "" {
				t.Fatalf("error responses need a machine-readable code: %s", body)
			}
		})
	}

	// A claim with no source event cannot be traced, so it is refused.
	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/assertions", owner, map[string]any{
		"claims": []map[string]any{{
			"subject": map[string]any{"name": "Acme"}, "predicate": "x",
			"object": map[string]any{"kind": "boolean", "boolean": true},
		}},
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("a claim without a source event must be refused, got %d: %s", status, body)
	}

	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestIntegrationEntityMergeAndSplitThroughTheAPI(t *testing.T) {
	h := newAPIHarness(t)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	create := func(name string) string {
		status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/entities", owner,
			map[string]any{"canonical_name": name, "entity_type": "organization"}, nil)
		if status != http.StatusCreated {
			t.Fatalf("create entity returned %d: %s", status, body)
		}
		return decode(t, body)["id"].(string)
	}

	left := create("Acme Corp")
	right := create("Acme Corporation Ltd")

	// A merge must say why: it is the most damaging operation here if it is wrong.
	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/entities/merge", owner,
		map[string]any{"from_entity_id": left, "into_entity_id": right}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("a merge without a reason must be refused, got %d: %s", status, body)
	}

	status, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/entities/merge", owner,
		map[string]any{"from_entity_id": left, "into_entity_id": right,
			"reason": "same company, different spelling"}, nil)
	if status != http.StatusOK {
		t.Fatalf("merge returned %d: %s", status, body)
	}
	if decode(t, body)["method"] != string(domain.MethodHumanMerge) {
		t.Fatalf("a merge must be recorded as a human decision: %s", body)
	}

	// The merged identity reports where it went, and both sides share one cluster.
	status, body = h.do(t, http.MethodGet, "/v1/entities/"+left+"/identity", owner, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("identity returned %d: %s", status, body)
	}
	identity := decode(t, body)
	if identity["merged"] != true || identity["canonical_entity_id"] != right {
		t.Fatalf("the merged identity must redirect to the survivor: %s", body)
	}
	if len(identity["cluster"].([]any)) != 2 {
		t.Fatalf("both identities belong to one cluster: %s", body)
	}

	// The merged identity is still readable: nothing was deleted.
	status, _ = h.do(t, http.MethodGet, "/v1/entities/"+left, owner, nil, nil)
	if status != http.StatusOK {
		t.Fatal("a merged identity must remain readable")
	}

	// Undo it.
	status, body = h.do(t, http.MethodPost, "/v1/entities/"+left+"/split", owner,
		map[string]any{"reason": "different subsidiaries after all"}, nil)
	if status != http.StatusOK {
		t.Fatalf("split returned %d: %s", status, body)
	}

	status, body = h.do(t, http.MethodGet, "/v1/entities/"+left+"/identity", owner, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("identity returned %d: %s", status, body)
	}
	if decode(t, body)["merged"] != false {
		t.Fatalf("after a split the identity stands on its own: %s", body)
	}

	// Both decisions survive in the ledger, with the merge marked reverted.
	status, body = h.do(t, http.MethodGet, "/v1/graph-spaces/"+gs+"/resolution-decisions?review=true",
		owner, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("decisions returned %d: %s", status, body)
	}
	var sawRevertedMerge, sawSplit bool
	for _, raw := range decode(t, body)["decisions"].([]any) {
		decision := raw.(map[string]any)
		switch decision["method"] {
		case string(domain.MethodHumanMerge):
			sawRevertedMerge = decision["reverted_at"] != nil
		case string(domain.MethodHumanSplit):
			sawSplit = true
		}
	}
	if !sawRevertedMerge {
		t.Fatalf("the reversed merge must remain, marked reverted: %s", body)
	}
	if !sawSplit {
		t.Fatalf("the split must be recorded: %s", body)
	}
}

func TestIntegrationResolutionDecisionsAreWorkspaceScoped(t *testing.T) {
	h := newAPIHarness(t,
		keyEntry{principalID: "acme-owner", secret: "a", systemRole: domain.RoleAdmin},
		keyEntry{principalID: "globex-owner", secret: "b", systemRole: domain.RoleAdmin},
	)
	owner := string(h.fixture.Primary.Principal.ID)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	_, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/entities", owner,
		map[string]any{"canonical_name": "Acme", "entity_type": "organization"}, nil)
	entityID := decode(t, body)["id"].(string)

	for _, path := range []string{
		"/v1/entities/" + entityID + "/identity",
		"/v1/graph-spaces/" + gs + "/resolution-decisions",
	} {
		status, body := h.do(t, http.MethodGet, path, "globex-owner", nil, nil)
		if status != http.StatusNotFound {
			t.Fatalf("%s leaked to another tenant with %d: %s", path, status, body)
		}
	}

	status, body := h.do(t, http.MethodPost, "/v1/entities/"+entityID+"/split", "globex-owner",
		map[string]any{"reason": "not mine to split"}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("another tenant must not be able to split, got %d: %s", status, body)
	}
}
