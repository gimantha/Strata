// Package embedding is the provider-independent embedding interface.
//
// It mirrors internal/llm: provider types stay inside the adapter packages, and everything
// above speaks only the types declared here (AGENTS.md sections 2.11, 5).
package embedding

import (
	"context"
	"math"

	"github.com/gimantha/strata/internal/domain"
)

// Dimensions is the vector width the projection schema is built for.
//
// pgvector needs a fixed dimension on any indexed column, so this is a deployment-wide
// constant rather than a per-model one. 1536 matches the common hosted embedding models;
// running a model of another width means altering the column, which is an additive
// migration and a re-embed, not a change to canonical knowledge
// (AGENTS.md section 17).
const Dimensions = 1536

// Vector is a single embedding.
type Vector []float32

// Embedder turns text into vectors.
type Embedder interface {
	// Name identifies the provider for vector records.
	Name() string
	// Model identifies the configured model. It is stored on every vector record, because
	// a vector is only comparable with others from the same model.
	Model() string
	// Version distinguishes re-embeddings by the same model, so a projection rebuild after
	// a provider changes its weights is distinguishable from the original.
	Version() int
	// Dimensions reports the width this embedder produces.
	Dimensions() int
	// Embed returns one vector per input, in the same order.
	Embed(ctx context.Context, texts []string) ([]Vector, error)
}

// MaxBatch bounds how many texts are sent in one request.
const MaxBatch = 96

// Validate checks a vector is usable before it reaches the projection.
func (v Vector) Validate(expected int) error {
	const op = "embedding.Vector.Validate"

	if len(v) != expected {
		return domain.Errorf(domain.CodeInvalidArgument, op,
			"embedding has %d dimensions, expected %d", len(v), expected)
	}
	for _, component := range v {
		// A NaN or infinity poisons every distance computation it takes part in, and the
		// failure surfaces far from here as nonsensical search results.
		if math.IsNaN(float64(component)) || math.IsInf(float64(component), 0) {
			return domain.Errorf(domain.CodeInvalidArgument, op,
				"embedding contains a non-finite value")
		}
	}
	return nil
}

// Normalize scales a vector to unit length.
//
// Cosine distance is unaffected by magnitude, but normalizing makes stored vectors
// comparable under inner product too, and keeps values in a range where float32 precision
// is uniform. A zero vector is returned unchanged, since it has no direction to preserve.
func (v Vector) Normalize() Vector {
	var sum float64
	for _, component := range v {
		sum += float64(component) * float64(component)
	}
	if sum == 0 {
		return v
	}

	magnitude := float32(math.Sqrt(sum))
	out := make(Vector, len(v))
	for i, component := range v {
		out[i] = component / magnitude
	}
	return out
}
