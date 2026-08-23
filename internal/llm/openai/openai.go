// Package openai adapts any OpenAI-compatible chat-completions endpoint to the llm
// interface.
//
// "OpenAI-compatible" is the point: the base URL is configuration, so the same adapter
// serves OpenAI, Azure OpenAI, vLLM, Ollama, LM Studio, and the various gateways. No type
// declared here escapes the package (AGENTS.md sections 2.11, 13.2).
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/llm"
)

// Config configures the adapter. The API key is held in memory only and never persisted
// or logged (AGENTS.md section 13.2).
type Config struct {
	BaseURL string
	APIKey  string
	Model   string
	// Organization and Project are optional OpenAI routing headers.
	Organization string
	Project      string
	Timeout      time.Duration
	MaxRetries   int
	// HTTPClient allows a caller to supply an instrumented client; tests inject a stub.
	HTTPClient *http.Client
}

// Provider is an OpenAI-compatible chat-completions client.
type Provider struct {
	cfg    Config
	client *http.Client
}

// DefaultBaseURL is the public OpenAI endpoint.
const DefaultBaseURL = "https://api.openai.com/v1"

// New builds the adapter.
func New(cfg Config) (*Provider, error) {
	const op = "openai.New"

	if cfg.Model == "" {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op, "a model must be configured")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &Provider{cfg: cfg, client: client}, nil
}

func (p *Provider) Name() string  { return "openai" }
func (p *Provider) Model() string { return p.cfg.Model }

// chatRequest is the wire shape. It stays unexported so it cannot leak upward.
type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	MaxTokens      int             `json:"max_completion_tokens,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	Seed           *int            `json:"seed,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *jsonSchema `json:"json_schema,omitempty"`
}

type jsonSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	// Strict asks the provider to guarantee conformance. Local validation still runs
	// regardless: a provider's promise is not a substitute for checking.
	Strict bool `json:"strict"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func (p *Provider) Generate(ctx context.Context, req llm.GenerateRequest) (llm.GenerateResponse, error) {
	body := p.buildRequest(req, nil)
	parsed, err := p.call(ctx, body)
	if err != nil {
		return llm.GenerateResponse{}, err
	}
	choice := parsed.Choices[0]
	return llm.GenerateResponse{
		Text:         choice.Message.Content,
		FinishReason: choice.FinishReason,
		Usage:        usageOf(parsed),
		Model:        parsed.Model,
	}, nil
}

func (p *Provider) GenerateStructured(ctx context.Context, req llm.StructuredRequest) (llm.StructuredResponse, error) {
	const op = "openai.GenerateStructured"

	format := &responseFormat{Type: "json_object"}
	if len(req.Schema) > 0 {
		format = &responseFormat{
			Type: "json_schema",
			JSONSchema: &jsonSchema{
				Name:   orDefault(req.SchemaName, "extraction_result"),
				Schema: req.Schema,
				Strict: true,
			},
		}
	}

	parsed, err := p.call(ctx, p.buildRequest(req.GenerateRequest, format))
	if err != nil {
		return llm.StructuredResponse{}, err
	}
	choice := parsed.Choices[0]

	// A refusal is a legitimate answer, not a transport failure. It must not be retried
	// and must not be parsed as output.
	if choice.Message.Refusal != "" {
		return llm.StructuredResponse{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"the model refused the request")
	}

	return llm.StructuredResponse{
		Raw:          json.RawMessage(choice.Message.Content),
		FinishReason: choice.FinishReason,
		Usage:        usageOf(parsed),
		Model:        parsed.Model,
	}, nil
}

func (p *Provider) buildRequest(req llm.GenerateRequest, format *responseFormat) chatRequest {
	messages := make([]chatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		messages = append(messages, chatMessage{Role: string(m.Role), Content: m.Content})
	}

	out := chatRequest{
		Model:          p.cfg.Model,
		Messages:       messages,
		MaxTokens:      req.MaxTokens,
		Seed:           req.Seed,
		ResponseFormat: format,
	}
	// Only send a temperature when one was chosen: some models reject any value but their
	// default.
	if req.Temperature > 0 {
		temperature := req.Temperature
		out.Temperature = &temperature
	}
	return out
}

// call performs the request, retrying only what is safe to retry.
func (p *Provider) call(ctx context.Context, body chatRequest) (chatResponse, error) {
	const op = "openai.call"

	encoded, err := json.Marshal(body)
	if err != nil {
		return chatResponse{}, domain.Wrap(err, domain.CodeInternal, op, "cannot encode request")
	}

	var lastErr error
	for attempt := 0; attempt <= p.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			// Back off between attempts, and stop immediately if the caller gave up.
			select {
			case <-ctx.Done():
				return chatResponse{}, domain.Wrap(ctx.Err(), domain.CodeProviderUnavailable, op,
					"cancelled while retrying")
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}

		parsed, retryable, err := p.attempt(ctx, encoded)
		if err == nil {
			return parsed, nil
		}
		lastErr = err
		if !retryable {
			return chatResponse{}, err
		}
	}
	return chatResponse{}, lastErr
}

// attempt performs one HTTP call, reporting whether a failure is worth retrying.
func (p *Provider) attempt(ctx context.Context, encoded []byte) (chatResponse, bool, error) {
	const op = "openai.attempt"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.cfg.BaseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return chatResponse{}, false, domain.Wrap(err, domain.CodeInternal, op, "cannot build request")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	if p.cfg.Organization != "" {
		httpReq.Header.Set("OpenAI-Organization", p.cfg.Organization)
	}
	if p.cfg.Project != "" {
		httpReq.Header.Set("OpenAI-Project", p.cfg.Project)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		// A transport failure says nothing about the request's validity, so it is
		// retryable.
		return chatResponse{}, true, domain.Wrap(err, domain.CodeProviderUnavailable, op,
			"provider is unreachable")
	}
	defer resp.Body.Close()

	// Bound the response: a provider must not be able to exhaust memory here.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return chatResponse{}, true, domain.Wrap(err, domain.CodeProviderUnavailable, op,
			"cannot read provider response")
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return chatResponse{}, true, domain.Errorf(domain.CodeRateLimited, op,
			"provider rate limited the request")
	case resp.StatusCode >= 500:
		return chatResponse{}, true, domain.Errorf(domain.CodeProviderUnavailable, op,
			"provider returned status %d", resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// Never echo the body: it can contain request context, and the credential problem
		// is the whole message.
		return chatResponse{}, false, domain.Errorf(domain.CodeProviderUnavailable, op,
			"provider rejected the credentials")
	case resp.StatusCode >= 400:
		return chatResponse{}, false, domain.Errorf(domain.CodeInvalidArgument, op,
			"provider rejected the request with status %d", resp.StatusCode)
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return chatResponse{}, false, domain.Wrap(err, domain.CodeProviderUnavailable, op,
			"provider returned a malformed response")
	}
	if parsed.Error != nil {
		return chatResponse{}, false, domain.Errorf(domain.CodeProviderUnavailable, op,
			"provider reported an error: %s", parsed.Error.Type)
	}
	if len(parsed.Choices) == 0 {
		return chatResponse{}, false, domain.Errorf(domain.CodeProviderUnavailable, op,
			"provider returned no choices")
	}
	return parsed, false, nil
}

// maxResponseBytes bounds a provider response.
const maxResponseBytes = 8 << 20

func usageOf(r chatResponse) llm.Usage {
	return llm.Usage{
		PromptTokens:     r.Usage.PromptTokens,
		CompletionTokens: r.Usage.CompletionTokens,
		TotalTokens:      r.Usage.TotalTokens,
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
