package openai_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/llm"
	"github.com/gimantha/strata/internal/llm/openai"
)

// stub is an OpenAI-compatible endpoint under test control. CI never talks to a real
// provider (AGENTS.md section 32.4).
func stub(t *testing.T, handler http.HandlerFunc) *openai.Provider {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	provider, err := openai.New(openai.Config{
		BaseURL:    server.URL,
		APIKey:     "test-key",
		Model:      "gpt-test",
		MaxRetries: 2,
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("build provider: %v", err)
	}
	return provider
}

func okResponse(content string) string {
	return `{"model":"gpt-test-2026","choices":[{"message":{"content":` +
		jsonString(content) + `},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
}

// jsonString renders a value as a JSON string literal.
func jsonString(s string) string {
	encoded, _ := json.Marshal(s)
	return string(encoded)
}

func TestGenerateStructuredSendsSchemaAndReturnsRawJSON(t *testing.T) {
	var captured map[string]any

	provider := stub(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)

		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("credentials were not sent: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, okResponse(`{"entities":[]}`))
	})

	resp, err := provider.GenerateStructured(context.Background(), llm.StructuredRequest{
		GenerateRequest: llm.GenerateRequest{
			Messages:  []llm.Message{{Role: llm.RoleSystem, Content: "be exact"}},
			MaxTokens: 100,
		},
		SchemaName: "extraction_result",
		Schema:     json.RawMessage(`{"type":"object"}`),
	})
	if err != nil {
		t.Fatalf("structured call: %v", err)
	}

	if string(resp.Raw) != `{"entities":[]}` {
		t.Fatalf("raw output was altered: %s", resp.Raw)
	}
	// The adapter reports what actually served the request, which may be a pinned
	// snapshot rather than the alias that was asked for.
	if resp.Model != "gpt-test-2026" {
		t.Fatalf("expected the served model, got %q", resp.Model)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Fatalf("token accounting lost: %+v", resp.Usage)
	}

	// Structured output must be requested as a schema, not merely as JSON.
	format, ok := captured["response_format"].(map[string]any)
	if !ok || format["type"] != "json_schema" {
		t.Fatalf("the request must ask for schema-constrained output: %v", captured["response_format"])
	}
	schema := format["json_schema"].(map[string]any)
	if schema["strict"] != true {
		t.Fatal("strict mode must be requested")
	}
	if schema["name"] != "extraction_result" {
		t.Fatalf("schema name lost: %v", schema["name"])
	}
}

func TestGenerateStructuredOmitsTemperatureWhenUnset(t *testing.T) {
	// Some models reject any temperature but their default, so an unset value must not be
	// sent as zero.
	var captured map[string]any
	provider := stub(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		io.WriteString(w, okResponse(`{}`))
	})

	if _, err := provider.GenerateStructured(context.Background(), llm.StructuredRequest{
		GenerateRequest: llm.GenerateRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}},
	}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if _, present := captured["temperature"]; present {
		t.Fatal("an unset temperature must be omitted rather than sent as zero")
	}
}

func TestRefusalIsNotTreatedAsOutput(t *testing.T) {
	provider := stub(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"gpt-test","choices":[{"message":{"content":"","refusal":"I cannot help with that"},"finish_reason":"stop"}]}`)
	})

	_, err := provider.GenerateStructured(context.Background(), llm.StructuredRequest{
		GenerateRequest: llm.GenerateRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}}},
	})
	if err == nil {
		t.Fatal("a refusal is not usable output and must be an error")
	}
	// A refusal is a decision, not an outage: retrying it would just repeat it.
	if domain.ClassifyError(err).Retryable() {
		t.Fatalf("a refusal must not be retryable, classified as %s", domain.ClassifyError(err))
	}
}

func TestRetriesTransientFailuresOnly(t *testing.T) {
	t.Run("server error is retried", func(t *testing.T) {
		var attempts atomic.Int32
		provider := stub(t, func(w http.ResponseWriter, r *http.Request) {
			if attempts.Add(1) < 3 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			io.WriteString(w, okResponse(`{"ok":true}`))
		})

		resp, err := provider.GenerateStructured(context.Background(), llm.StructuredRequest{
			GenerateRequest: llm.GenerateRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}}},
		})
		if err != nil {
			t.Fatalf("a transient failure should have been retried: %v", err)
		}
		if string(resp.Raw) != `{"ok":true}` {
			t.Fatalf("unexpected output: %s", resp.Raw)
		}
		if attempts.Load() != 3 {
			t.Fatalf("expected 3 attempts, got %d", attempts.Load())
		}
	})

	t.Run("bad request is not retried", func(t *testing.T) {
		var attempts atomic.Int32
		provider := stub(t, func(w http.ResponseWriter, r *http.Request) {
			attempts.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"bad schema","type":"invalid_request_error"}}`)
		})

		_, err := provider.GenerateStructured(context.Background(), llm.StructuredRequest{
			GenerateRequest: llm.GenerateRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}}},
		})
		if err == nil {
			t.Fatal("a rejected request must fail")
		}
		// Retrying a request the provider considers malformed just burns quota.
		if attempts.Load() != 1 {
			t.Fatalf("a bad request must not be retried, saw %d attempts", attempts.Load())
		}
	})

	t.Run("rate limiting is retryable", func(t *testing.T) {
		provider := stub(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		})
		_, err := provider.GenerateStructured(context.Background(), llm.StructuredRequest{
			GenerateRequest: llm.GenerateRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}}},
		})
		if !domain.IsCode(err, domain.CodeRateLimited) {
			t.Fatalf("expected rate_limited, got %s", domain.CodeOf(err))
		}
		if !domain.ClassifyError(err).Retryable() {
			t.Fatal("rate limiting must be retryable")
		}
	})
}

func TestCredentialProblemsDoNotLeakTheResponseBody(t *testing.T) {
	provider := stub(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Incorrect API key sk-secret-value-12345"}}`)
	})

	_, err := provider.GenerateStructured(context.Background(), llm.StructuredRequest{
		GenerateRequest: llm.GenerateRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}}},
	})
	if err == nil {
		t.Fatal("a credential failure must be an error")
	}
	// A provider echoing part of a key back must not end up in our logs.
	if strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("the error leaked provider response content: %v", err)
	}
}

func TestMalformedProviderResponseIsAnError(t *testing.T) {
	cases := map[string]string{
		"not json":   `not json at all`,
		"no choices": `{"model":"gpt-test","choices":[]}`,
		"api error":  `{"error":{"message":"overloaded","type":"server_error"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			provider := stub(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, body)
			})
			if _, err := provider.GenerateStructured(context.Background(), llm.StructuredRequest{
				GenerateRequest: llm.GenerateRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}}},
			}); err == nil {
				t.Fatal("a malformed provider response must be an error")
			}
		})
	}
}

func TestContextCancellationStopsRetrying(t *testing.T) {
	var attempts atomic.Int32
	provider := stub(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	if _, err := provider.GenerateStructured(ctx, llm.StructuredRequest{
		GenerateRequest: llm.GenerateRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}}},
	}); err == nil {
		t.Fatal("expected the call to fail")
	}
	if attempts.Load() > 3 {
		t.Fatalf("cancellation should stop the retry loop, saw %d attempts", attempts.Load())
	}
}

func TestConfigurationRequiresAModel(t *testing.T) {
	if _, err := openai.New(openai.Config{}); err == nil {
		t.Fatal("a provider with no model configured must not be constructible")
	}
	// Any OpenAI-compatible endpoint works; that is the point of the adapter.
	provider, err := openai.New(openai.Config{Model: "llama3", BaseURL: "http://localhost:11434/v1"})
	if err != nil {
		t.Fatalf("a self-hosted endpoint must be usable: %v", err)
	}
	if provider.Name() != "openai" || provider.Model() != "llama3" {
		t.Fatalf("unexpected identity: %s/%s", provider.Name(), provider.Model())
	}
}
