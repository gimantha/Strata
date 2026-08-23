// Package mock is a deterministic embedder for tests and CI.
//
// It produces stable vectors from a hash of the text, so the same input always embeds
// identically and CI never needs a live provider (AGENTS.md section 32.4). Similar texts do
// not get similar vectors - that is not what this is for. It exists so projection, replay,
// and scope-filtering behavior can be tested exactly.
package mock

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"sync"

	"github.com/gimantha/strata/internal/embedding"
)

// Embedder is a deterministic hash-based embedder.
type Embedder struct {
	mu sync.Mutex

	model      string
	version    int
	dimensions int
	calls      int
	// texts records everything embedded, so a test can assert on what was sent.
	texts []string
}

// New builds the embedder.
func New() *Embedder {
	return &Embedder{model: "mock-embedding-v1", version: 1, dimensions: embedding.Dimensions}
}

// WithDimensions overrides the width, for tests that need a mismatch.
func (e *Embedder) WithDimensions(n int) *Embedder {
	e.dimensions = n
	return e
}

// WithModel overrides the model identity, for testing re-embedding under a new model.
func (e *Embedder) WithModel(model string, version int) *Embedder {
	e.model, e.version = model, version
	return e
}

func (e *Embedder) Name() string    { return "mock" }
func (e *Embedder) Model() string   { return e.model }
func (e *Embedder) Version() int    { return e.version }
func (e *Embedder) Dimensions() int { return e.dimensions }

// Calls reports how many batches were embedded.
func (e *Embedder) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// Texts returns everything that was embedded.
func (e *Embedder) Texts() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.texts))
	copy(out, e.texts)
	return out
}

func (e *Embedder) Embed(ctx context.Context, texts []string) ([]embedding.Vector, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	e.mu.Lock()
	e.calls++
	e.texts = append(e.texts, texts...)
	dimensions := e.dimensions
	model := e.model
	e.mu.Unlock()

	out := make([]embedding.Vector, 0, len(texts))
	for _, text := range texts {
		out = append(out, deterministicVector(model+"\x00"+text, dimensions))
	}
	return out, nil
}

// deterministicVector expands a hash of the text into a stable pseudo-random unit vector.
func deterministicVector(seed string, dimensions int) embedding.Vector {
	vector := make(embedding.Vector, dimensions)
	digest := fnv.New64a()

	for i := range vector {
		digest.Reset()
		_, _ = digest.Write([]byte(seed))
		var index [4]byte
		binary.LittleEndian.PutUint32(index[:], uint32(i))
		_, _ = digest.Write(index[:])

		// Map the hash into [-1, 1) so vectors spread across the space rather than
		// clustering in one orthant.
		vector[i] = float32(digest.Sum64()%2_000_001)/1_000_000 - 1
	}
	return vector.Normalize()
}
