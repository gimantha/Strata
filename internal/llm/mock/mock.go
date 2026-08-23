// Package mock is a deterministic LLM provider for tests and CI.
//
// AGENTS.md section 32.4 requires that CI never depend on a live model. This provider
// makes extraction fully testable: the same input always produces the same output, and a
// test can make the provider return whatever a real one might - including malformed
// output, refusals, and content that tries to smuggle instructions through.
package mock

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/gimantha/strata/internal/llm"
)

// Provider is a scripted LLM.
type Provider struct {
	mu sync.Mutex

	name  string
	model string

	// responder decides what to return for a request. Tests replace it to script
	// behavior; the default returns an empty but valid extraction result.
	responder func(req llm.StructuredRequest) (llm.StructuredResponse, error)

	// calls records every request, so a test can assert on what the prompt actually
	// contained - which is how injection defenses are verified.
	calls []llm.StructuredRequest
}

// New builds a provider that returns an empty, valid extraction result.
func New() *Provider {
	return &Provider{
		name:  "mock",
		model: "mock-extractor-v1",
		responder: func(llm.StructuredRequest) (llm.StructuredResponse, error) {
			return llm.StructuredResponse{
				Raw:          json.RawMessage(`{"entities":[],"assertions":[],"temporal":[]}`),
				FinishReason: "stop",
				Model:        "mock-extractor-v1",
			}, nil
		},
	}
}

func (p *Provider) Name() string  { return p.name }
func (p *Provider) Model() string { return p.model }

// RespondWith scripts the provider to return fixed JSON for every structured request.
func (p *Provider) RespondWith(raw string) *Provider {
	return p.RespondFunc(func(llm.StructuredRequest) (llm.StructuredResponse, error) {
		return llm.StructuredResponse{
			Raw:          json.RawMessage(raw),
			FinishReason: "stop",
			Model:        p.model,
			Usage:        llm.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		}, nil
	})
}

// RespondFunc scripts arbitrary behavior, including errors.
func (p *Provider) RespondFunc(fn func(req llm.StructuredRequest) (llm.StructuredResponse, error)) *Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.responder = fn
	return p
}

// FailWith scripts a provider error, for testing transient failure handling.
func (p *Provider) FailWith(err error) *Provider {
	return p.RespondFunc(func(llm.StructuredRequest) (llm.StructuredResponse, error) {
		return llm.StructuredResponse{}, err
	})
}

func (p *Provider) Generate(ctx context.Context, req llm.GenerateRequest) (llm.GenerateResponse, error) {
	if err := ctx.Err(); err != nil {
		return llm.GenerateResponse{}, err
	}
	return llm.GenerateResponse{Text: "", FinishReason: "stop", Model: p.model}, nil
}

func (p *Provider) GenerateStructured(ctx context.Context, req llm.StructuredRequest) (llm.StructuredResponse, error) {
	if err := ctx.Err(); err != nil {
		return llm.StructuredResponse{}, err
	}

	p.mu.Lock()
	p.calls = append(p.calls, req)
	responder := p.responder
	p.mu.Unlock()

	return responder(req)
}

// Calls returns every structured request the provider received.
func (p *Provider) Calls() []llm.StructuredRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]llm.StructuredRequest, len(p.calls))
	copy(out, p.calls)
	return out
}

// LastPrompt returns the full text of the most recent request, for asserting on how source
// material was delimited.
func (p *Provider) LastPrompt() (string, error) {
	calls := p.Calls()
	if len(calls) == 0 {
		return "", fmt.Errorf("mock provider received no requests")
	}

	var combined string
	for _, m := range calls[len(calls)-1].Messages {
		combined += string(m.Role) + ":\n" + m.Content + "\n"
	}
	return combined, nil
}
