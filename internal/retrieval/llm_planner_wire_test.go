package retrieval

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/llm/openai"
)

// The other planner tests inject an llm.LLM, which is the right shape for testing planning
// decisions and the wrong shape for testing whether the planner can talk to a provider at
// all: it bypasses the schema, the request encoding, the HTTP client, and every error path
// between them. Everything below drives the real OpenAI adapter over a local server that
// behaves the way a strict provider does.
//
// This is what would have caught the schema that shipped in 332e408 — accepted by every
// scripted test, rejected by any real provider, and degrading so gracefully that the
// failure looked like normal operation.

// strictProvider is an OpenAI-compatible endpoint that enforces the parts of the contract a
// real one enforces: it rejects a schema it cannot accept rather than ignoring the problem.
type strictProvider struct {
	reply      string
	sawSchema  json.RawMessage
	sawStrict  bool
	sawRequest map[string]any
	status     int
}

func (p *strictProvider) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		p.sawRequest = request

		if format, ok := request["response_format"].(map[string]any); ok {
			if schema, ok := format["json_schema"].(map[string]any); ok {
				p.sawStrict, _ = schema["strict"].(bool)
				p.sawSchema, _ = json.Marshal(schema["schema"])
				if reason := strictModeRejection(schema["schema"]); reason != "" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":{"message":"Invalid schema: ` +
						reason + `","type":"invalid_request_error"}}`))
					return
				}
			}
		}
		if p.status != 0 {
			w.WriteHeader(p.status)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream trouble"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` +
			quoteJSON(p.reply) + `}}]}`))
	})
}

// strictModeRejection reports the first rule a real strict validator would reject, using
// the same rules the conformance suite checks.
func strictModeRejection(schema any) string {
	raw, err := json.Marshal(schema)
	if err != nil {
		return "unreadable schema"
	}
	if strings.Contains(string(raw), `"maxLength"`) ||
		strings.Contains(string(raw), `"minItems"`) ||
		strings.Contains(string(raw), `"maxItems"`) {
		return "unsupported keyword"
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return "unreadable schema"
	}
	return missingRequired(root)
}

func missingRequired(node map[string]any) string {
	properties, hasProperties := node["properties"].(map[string]any)
	if hasProperties {
		if additional, ok := node["additionalProperties"]; !ok || additional != false {
			return "additionalProperties must be false"
		}
		required := map[string]bool{}
		if list, ok := node["required"].([]any); ok {
			for _, name := range list {
				if text, ok := name.(string); ok {
					required[text] = true
				}
			}
		}
		for name := range properties {
			if !required[name] {
				return "'" + name + "' is not in required"
			}
		}
		for _, child := range properties {
			if object, ok := child.(map[string]any); ok {
				if reason := missingRequired(object); reason != "" {
					return reason
				}
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		return missingRequired(items)
	}
	return ""
}

func quoteJSON(s string) string {
	encoded, _ := json.Marshal(s)
	return string(encoded)
}

func wiredPlanner(t *testing.T, provider *strictProvider) llmPlanner {
	t.Helper()
	server := httptest.NewServer(provider.handler(t))
	t.Cleanup(server.Close)

	adapter, err := openai.New(openai.Config{
		BaseURL:    server.URL,
		APIKey:     "test-key",
		Model:      "gpt-planner",
		MaxRetries: 1,
		Timeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("build adapter: %v", err)
	}
	return llmPlanner{
		model:     adapter,
		fallback:  heuristicPlanner{hasEmbedder: true},
		timeout:   5 * time.Second,
		hasVector: true,
	}
}

func TestTheSchemaWeSendIsOneAStrictProviderAccepts(t *testing.T) {
	provider := &strictProvider{reply: `{
		"modes": ["lexical", "exact"],
		"mode_reasons": [{"mode": "exact", "reason": "episodes is a requirement"}],
		"sub_queries": [
			{"text": "ignored", "kind": "original", "reason": "as asked"},
			{"text": "supports episodes", "kind": "decomposed", "reason": "the constraint"}
		]
	}`}
	planner := wiredPlanner(t, provider)

	plan := planner.Plan(t.Context(), domain.QueryRequest{
		Query: "which context graph supports episodes",
	})

	if !provider.sawStrict {
		t.Error("the adapter did not ask for strict structured output")
	}
	if len(provider.sawSchema) == 0 {
		t.Fatal("no schema reached the provider")
	}
	// The plan came back from the model rather than the fallback, which is only true if
	// the provider accepted the schema and the response survived the round trip.
	if plan.Planner != "llm/gpt-planner" {
		t.Errorf("planner = %q with note %q; the model was not used",
			plan.Planner, plan.PlannerNote)
	}
	if plan.PlannerNote != "" {
		t.Errorf("unexpected degradation: %s", plan.PlannerNote)
	}
	if len(plan.SubQueries) != 2 {
		t.Fatalf("sub-queries = %d, want 2", len(plan.SubQueries))
	}
	if plan.SubQueries[0].Text != "which context graph supports episodes" {
		t.Errorf("the question as asked did not survive: %q", plan.SubQueries[0].Text)
	}
	if plan.Reasons[domain.ModeExact] != "episodes is a requirement" {
		t.Errorf("mode reason lost over the wire: %q", plan.Reasons[domain.ModeExact])
	}
}

func TestPlanningIsDeterministicOverTheWire(t *testing.T) {
	provider := &strictProvider{reply: `{"modes":["lexical"],"mode_reasons":[],
		"sub_queries":[{"text":"x","kind":"original","reason":"r"}]}`}
	planner := wiredPlanner(t, provider)

	planner.Plan(t.Context(), domain.QueryRequest{Query: "anything"})

	// Planning the same question twice must not produce two different retrieval plans, so
	// the request pins both knobs that would otherwise let it drift.
	if temperature, ok := provider.sawRequest["temperature"].(float64); !ok || temperature != 0 {
		t.Errorf("temperature = %v, want 0", provider.sawRequest["temperature"])
	}
	if _, ok := provider.sawRequest["seed"]; !ok {
		t.Error("no seed was sent; planning could vary between identical questions")
	}
}

func TestAProviderRejectionFallsBackRatherThanFailing(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusTooManyRequests,
		http.StatusInternalServerError} {
		provider := &strictProvider{status: status, reply: "{}"}
		planner := wiredPlanner(t, provider)

		plan := planner.Plan(t.Context(), domain.QueryRequest{Query: "who confirmed the renewal"})

		if len(plan.Modes) == 0 {
			t.Fatalf("HTTP %d left no usable plan", status)
		}
		if plan.Planner != "heuristic" {
			t.Errorf("HTTP %d: planner = %q, want the heuristic fallback", status, plan.Planner)
		}
		if plan.PlannerNote == "" {
			t.Errorf("HTTP %d degraded without saying so in the plan", status)
		}
		if len(plan.SubQueries) != 1 || plan.SubQueries[0].Text != "who confirmed the renewal" {
			t.Errorf("HTTP %d: the question was not searched as asked: %+v", status, plan.SubQueries)
		}
	}
}

func TestGarbageFromAProviderFallsBack(t *testing.T) {
	for name, reply := range map[string]string{
		"not JSON":           `this is not json at all`,
		"empty object":       `{}`,
		"invented retriever": `{"modes":["telepathy"],"mode_reasons":[],"sub_queries":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			planner := wiredPlanner(t, &strictProvider{reply: reply})

			plan := planner.Plan(t.Context(), domain.QueryRequest{Query: "who confirmed it"})

			if len(plan.Modes) == 0 {
				t.Fatal("no usable plan survived")
			}
			if plan.Planner != "heuristic" || plan.PlannerNote == "" {
				t.Errorf("planner = %q, note = %q; want a noted fallback",
					plan.Planner, plan.PlannerNote)
			}
		})
	}
}
