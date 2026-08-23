package hashing

import (
	"context"
	"testing"
)

// cosine of two normalized vectors.
func cosine(a, b []float32) float64 {
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

func TestSharedVocabularyProducesSimilarVectors(t *testing.T) {
	e := New()
	vectors, err := e.Embed(context.Background(), []string{
		"Acme Corporation supplies industrial fasteners",
		"Acme Corporation supplies industrial fasteners to Globex",
		"Turbine maintenance schedules for the Berlin plant",
	})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}

	related := cosine(vectors[0], vectors[1])
	unrelated := cosine(vectors[0], vectors[2])

	if related <= unrelated {
		t.Fatalf("texts sharing vocabulary should be closer: %.3f vs %.3f", related, unrelated)
	}
	if related < 0.5 {
		t.Fatalf("near-identical texts should be clearly similar, got %.3f", related)
	}
}

func TestEmbeddingIsDeterministic(t *testing.T) {
	e := New()
	ctx := context.Background()

	first, _ := e.Embed(ctx, []string{"the same text"})
	second, _ := e.Embed(ctx, []string{"the same text"})

	for i := range first[0] {
		if first[0][i] != second[0][i] {
			t.Fatal("the same text must always embed identically")
		}
	}
}

func TestDoesNotGeneralizeAcrossSynonyms(t *testing.T) {
	// Stated as a test because it is the honest limit of this embedder, and anyone reading
	// results from it needs to know: shared meaning without shared words scores near zero.
	e := New()
	vectors, _ := e.Embed(context.Background(), []string{
		"industrial fasteners",
		"bolts and screws",
	})
	if similarity := cosine(vectors[0], vectors[1]); similarity > 0.2 {
		t.Fatalf("feature hashing should not find synonyms similar, got %.3f", similarity)
	}
}

func TestVectorsAreNormalizedAndValid(t *testing.T) {
	e := New()
	vectors, err := e.Embed(context.Background(), []string{"some ordinary text here"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if err := vectors[0].Validate(e.Dimensions()); err != nil {
		t.Fatalf("vector should be valid: %v", err)
	}
	if magnitude := cosine(vectors[0], vectors[0]); magnitude < 0.99 || magnitude > 1.01 {
		t.Fatalf("vectors should be unit length, got %.4f", magnitude)
	}

	// Empty text has no direction, and must not produce a non-finite vector.
	empty, err := e.Embed(context.Background(), []string{""})
	if err != nil {
		t.Fatalf("embed empty: %v", err)
	}
	if err := empty[0].Validate(e.Dimensions()); err != nil {
		t.Fatalf("an empty text must still yield a valid vector: %v", err)
	}
}

func TestIdentifiersSurviveTokenization(t *testing.T) {
	// An error code must contribute something, since exact identifiers are a large share of
	// what people search for.
	e := New().WithDimensions(256)
	vectors, _ := e.Embed(context.Background(), []string{
		"build failed with ERR_7731X",
		"build failed with ERR_9999Z",
	})
	if similarity := cosine(vectors[0], vectors[1]); similarity > 0.95 {
		t.Fatalf("different error codes should not be near-identical, got %.3f", similarity)
	}
}
