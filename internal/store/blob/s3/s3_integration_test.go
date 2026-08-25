package s3_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/store/blob"
	"github.com/gimantha/strata/internal/store/blob/s3"
	"github.com/gimantha/strata/internal/testsupport/s3test"
)

// TestIntegrationS3MeetsTheBlobContract runs the same suite the filesystem backend runs.
//
// This is what makes the port claim real. Two backends that compile against one interface
// prove nothing about behaviour; two backends that pass the same behavioural suite mean a
// caller can genuinely stop caring which is configured (AGENTS.md section 3).
func TestIntegrationS3MeetsTheBlobContract(t *testing.T) {
	store, err := s3.Open(t.Context(), s3test.Options(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	blob.RunConformance(t, "s3", store)
}

// TestIntegrationS3PrefixNamespacesWithoutLeaking checks the prefix option, which is how two
// deployments share one bucket.
//
// Worth its own test because a prefix that is applied on write and forgotten on read is a
// bug that only appears when a second deployment exists — and then appears as data loss.
func TestIntegrationS3PrefixNamespacesWithoutLeaking(t *testing.T) {
	base := s3test.Options(t)
	ctx := t.Context()

	first := base
	first.Prefix = "deployment-one"
	second := base
	second.Prefix = "deployment-two"

	one, err := s3.Open(ctx, first)
	if err != nil {
		t.Fatalf("open the first: %v", err)
	}
	two, err := s3.Open(ctx, second)
	if err != nil {
		t.Fatalf("open the second: %v", err)
	}

	key := "01a00000-0000-7000-8000-000000000001/sha256/aa/bb/" + strings.Repeat("aa", 32)
	if _, err := one.Put(ctx, key, []byte("belongs to deployment one")); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := one.Get(ctx, key)
	if err != nil {
		t.Fatalf("the writer cannot read its own object: %v", err)
	}
	if string(got) != "belongs to deployment one" {
		t.Fatalf("content changed: %q", got)
	}

	// The same key under a different prefix is a different object, not the same one.
	if _, err := two.Get(ctx, key); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a second deployment read the first's object through a shared bucket: %v", err)
	}
}

// TestIntegrationS3UnhealthyWhenTheBucketIsMissing checks readiness reports a real problem.
//
// A health check that passes against a bucket that does not exist would let a deployment
// come up ready and fail on its first ingest, which is exactly when the failure is most
// expensive.
func TestIntegrationS3UnhealthyWhenTheBucketIsMissing(t *testing.T) {
	opts := s3test.Options(t)
	opts.Bucket = "strata-no-such-bucket-exists-here"

	store, err := s3.Open(t.Context(), opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Healthy(t.Context()); err == nil {
		t.Fatal("a missing bucket reported healthy")
	}
}

// TestOpenRequiresABucket checks configuration is validated at startup rather than at first
// use, so a typo fails the process instead of the first ingest.
func TestOpenRequiresABucket(t *testing.T) {
	if _, err := s3.Open(context.Background(), s3.Options{Endpoint: "http://127.0.0.1:1"}); err == nil {
		t.Fatal("opening with no bucket must fail")
	}
}
