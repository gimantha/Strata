// Package llm is the provider-independent model interface.
//
// Provider-specific types must not escape this package tree (AGENTS.md sections 2.11,
// 13.2). Everything above it - extraction, and embeddings when phase 6 arrives - speaks
// only the types declared here, so swapping providers is a wiring change rather than a
// rewrite.
package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Role identifies who is speaking in a conversation.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn of a prompt.
type Message struct {
	Role    Role
	Content string
}

// GenerateRequest asks a model for free text.
type GenerateRequest struct {
	Messages  []Message
	MaxTokens int
	// Temperature is a pointer because zero is a meaningful value and the most useful one:
	// it asks for greedy decoding. A plain float64 cannot express the difference between
	// "the caller wants 0" and "the caller said nothing", and conflating them sends neither.
	Temperature *float64
	// Seed makes a provider's sampling reproducible where it supports one. Extraction
	// sets it so the same input tends to produce the same output.
	Seed *int
}

// GenerateResponse is free-text output.
type GenerateResponse struct {
	Text         string
	FinishReason string
	Usage        Usage
	Model        string
}

// StructuredRequest asks a model for output conforming to a JSON schema.
//
// Schema-constrained extraction is preferred over open generation: a model that can only
// answer in a fixed shape has far less room to act on instructions hidden in the source
// (AGENTS.md section 13.3).
type StructuredRequest struct {
	GenerateRequest
	SchemaName string
	Schema     json.RawMessage
}

// StructuredResponse carries the raw JSON the provider returned.
//
// It is deliberately not decoded here. Validation belongs to the caller that owns the
// schema, and a provider adapter must never be the thing that decides output is
// acceptable.
type StructuredResponse struct {
	Raw          json.RawMessage
	FinishReason string
	Usage        Usage
	Model        string
}

// Usage reports token accounting where the provider supplies it.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// LLM is the model abstraction (AGENTS.md section 13.2).
type LLM interface {
	// Name identifies the provider for model-run records.
	Name() string
	// Model identifies the configured model.
	Model() string
	Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error)
	GenerateStructured(ctx context.Context, req StructuredRequest) (StructuredResponse, error)
}

// HashRequest produces the stable request hash recorded on a model run.
//
// It covers the prompt and the schema, so two runs with the same hash really did ask the
// same question. The hash is stored instead of the prompt because the prompt embeds source
// material.
func HashRequest(req StructuredRequest) string {
	h := sha256.New()
	for _, m := range req.Messages {
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write([]byte(m.Content))
		h.Write([]byte{0})
	}
	h.Write([]byte(req.SchemaName))
	h.Write([]byte{0})
	h.Write(req.Schema)
	return hex.EncodeToString(h.Sum(nil))
}

// HashResponse produces the response hash recorded on a model run.
func HashResponse(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Timing measures a call's latency.
type Timing struct {
	Started time.Time
}

// StartTiming begins measuring.
func StartTiming(now func() time.Time) Timing {
	if now == nil {
		now = time.Now
	}
	return Timing{Started: now()}
}

// Elapsed reports how long the call took.
func (t Timing) Elapsed(now func() time.Time) time.Duration {
	if now == nil {
		now = time.Now
	}
	return now().Sub(t.Started)
}
