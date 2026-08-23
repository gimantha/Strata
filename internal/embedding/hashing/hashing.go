// Package hashing is a feature-hashing embedder: a real technique, not a test double.
//
// Each token is hashed into a bucket and the resulting bag-of-words vector is normalized, so
// two texts sharing vocabulary land near each other under cosine distance. That makes it a
// usable zero-dependency baseline for vector retrieval, and it makes the vector leg of
// hybrid search testable without a network call.
//
// What it cannot do is generalize. "bolts" and "fasteners" hash to unrelated buckets, so it
// finds no similarity between them where a trained model would. Anywhere semantic
// generalization matters, this is a floor rather than a substitute.
package hashing

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"

	"github.com/gimantha/strata/internal/embedding"
)

// Embedder projects text into a fixed-width bag-of-words space.
type Embedder struct {
	dimensions int
	model      string
}

// New builds the embedder at the projection's width.
func New() *Embedder {
	return &Embedder{dimensions: embedding.Dimensions, model: "hashing-bow-v1"}
}

// WithDimensions overrides the width, for tests.
func (e *Embedder) WithDimensions(n int) *Embedder {
	e.dimensions = n
	return e
}

func (e *Embedder) Name() string    { return "hashing" }
func (e *Embedder) Model() string   { return e.model }
func (e *Embedder) Version() int    { return 1 }
func (e *Embedder) Dimensions() int { return e.dimensions }

func (e *Embedder) Embed(ctx context.Context, texts []string) ([]embedding.Vector, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	out := make([]embedding.Vector, 0, len(texts))
	for _, text := range texts {
		out = append(out, e.vectorize(text))
	}
	return out, nil
}

// vectorize builds a normalized term-frequency vector over hashed buckets.
func (e *Embedder) vectorize(text string) embedding.Vector {
	vector := make(embedding.Vector, e.dimensions)

	for _, token := range tokenize(text) {
		digest := fnv.New32a()
		_, _ = digest.Write([]byte(token))
		sum := digest.Sum32()

		bucket := int(sum % uint32(e.dimensions))
		// A second hash bit decides the sign, which keeps collisions from always adding
		// constructively and inflating similarity between unrelated texts.
		if sum&0x80000000 != 0 {
			vector[bucket]--
		} else {
			vector[bucket]++
		}
	}

	// Sublinear scaling: a term repeated ten times is not ten times as informative, and
	// without damping a long document's frequent words drown out everything else.
	for i, component := range vector {
		if component != 0 {
			sign := float32(1)
			if component < 0 {
				sign = -1
				component = -component
			}
			vector[i] = sign * float32(1+math.Log(float64(component)))
		}
	}
	return vector.Normalize()
}

// tokenize lowercases and splits on non-alphanumerics, keeping runs of digits and letters
// together so an identifier such as ERR_7731X survives as two informative tokens rather than
// being discarded.
func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})

	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if len(field) > 1 {
			out = append(out, field)
		}
	}
	return out
}
