// Package s3test provides a real S3-compatible object store for integration tests.
//
// Real, never a mock, for the same reason the ledger tests use real PostgreSQL and the
// distributed tests use a real broker: the behaviour under test is how an object store
// reports absence, overwrites, and empty objects, and a fake would reproduce whatever its
// author already believed. Absence in particular is reported three different ways by real
// implementations, which is exactly the kind of detail a fake smooths over.
//
// A server is resolved in this order:
//
//  1. TEST_S3_ENDPOINT, if set (CI service container, or a shared dev server).
//  2. A server already listening on 127.0.0.1:19000 — the port scripts/dev-minio.sh uses.
//  3. Otherwise the test skips, unless CG_REQUIRE_S3=1 turns the skip into a failure so CI
//     cannot silently lose the coverage.
package s3test

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/gimantha/strata/internal/store/blob/s3"
)

// devEndpoint is what scripts/dev-minio.sh publishes. Deliberately not 9000: a developer's
// own MinIO should not have this project's buckets created in it.
const devEndpoint = "http://127.0.0.1:19000"

// Credentials match scripts/dev-minio.sh. Test credentials for a local container, which is
// why they are in the source rather than in a secret manager.
const (
	devAccessKey = "strata"
	devSecretKey = "strata-secret"
)

var (
	resolveOnce sync.Once
	resolved    string
	resolveErr  error
)

// Endpoint returns a reachable object store, skipping or failing when none is available.
func Endpoint(t testing.TB) string {
	t.Helper()

	resolveOnce.Do(resolve)
	if resolveErr != nil {
		if os.Getenv("CG_REQUIRE_S3") == "1" {
			t.Fatalf("CG_REQUIRE_S3=1 but no S3-compatible store is available: %v", resolveErr)
		}
		t.Skipf("skipping object-store test: none available (%v)", resolveErr)
	}
	return resolved
}

// Available reports whether a store can be reached, without failing a test.
func Available() bool {
	resolveOnce.Do(resolve)
	return resolveErr == nil
}

func resolve() {
	candidates := []string{}
	if endpoint := os.Getenv("TEST_S3_ENDPOINT"); endpoint != "" {
		candidates = append(candidates, endpoint)
	}
	candidates = append(candidates, devEndpoint)

	for _, endpoint := range candidates {
		if err := probe(endpoint); err == nil {
			resolved = endpoint
			return
		}
	}
	resolveErr = fmt.Errorf("nothing listening on %v; run scripts/dev-minio.sh start "+
		"or set TEST_S3_ENDPOINT", candidates)
}

// probe checks the endpoint accepts connections.
func probe(endpoint string) error {
	host := endpoint
	for _, scheme := range []string{"http://", "https://"} {
		if len(host) > len(scheme) && host[:len(scheme)] == scheme {
			host = host[len(scheme):]
			break
		}
	}
	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Options returns a configuration pointed at a fresh bucket for this test.
//
// A bucket per test, because these stores are shared and an object left behind by one test
// is a dirty fixture for the next — which looks like a flake and is really a leak.
func Options(t testing.TB) s3.Options {
	t.Helper()

	endpoint := Endpoint(t)
	bucket := bucketName(t)

	opts := s3.Options{
		Bucket:          bucket,
		Endpoint:        endpoint,
		Region:          "us-east-1",
		AccessKeyID:     credential("TEST_S3_ACCESS_KEY", devAccessKey),
		SecretAccessKey: credential("TEST_S3_SECRET_KEY", devSecretKey),
		// Path style, because a self-hosted store has no wildcard DNS for virtual-host
		// addressing and every compatible implementation supports the path form.
		PathStyle: true,
		Timeout:   15 * time.Second,
	}

	client := rawClient(t, opts)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: &bucket}); err != nil {
		t.Fatalf("create test bucket %s: %v", bucket, err)
	}
	t.Cleanup(func() { emptyAndDelete(client, bucket) })

	return opts
}

// bucketName derives a legal, unique bucket name from the test.
//
// S3 bucket names are lowercase, 3-63 characters, and may not contain underscores, so a Go
// test name cannot be used directly.
func bucketName(t testing.TB) string {
	cleaned := make([]rune, 0, len(t.Name()))
	for _, r := range t.Name() {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			cleaned = append(cleaned, r)
		case r >= 'A' && r <= 'Z':
			cleaned = append(cleaned, r+('a'-'A'))
		default:
			cleaned = append(cleaned, '-')
		}
	}
	name := fmt.Sprintf("t-%s-%d", string(cleaned), os.Getpid())
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

func credential(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// rawClient builds an SDK client for fixture management, which the adapter deliberately
// does not expose: creating and destroying buckets is not something ingestion should be
// able to do.
func rawClient(t testing.TB, opts s3.Options) *awss3.Client {
	t.Helper()

	store, err := s3.Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("open object store: %v", err)
	}
	return store.RawClient()
}

// emptyAndDelete removes a test bucket. Best effort: a leftover bucket in a dev container
// is untidy, and failing a passing test over it would be worse.
func emptyAndDelete(client *awss3.Client, bucket string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pages := awss3.NewListObjectsV2Paginator(client, &awss3.ListObjectsV2Input{Bucket: &bucket})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return
		}
		for _, object := range page.Contents {
			_, _ = client.DeleteObject(ctx, &awss3.DeleteObjectInput{
				Bucket: &bucket, Key: object.Key,
			})
		}
	}
	_, _ = client.DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: &bucket})
}
