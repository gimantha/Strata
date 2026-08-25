// Package s3 stores raw source material in any S3-compatible object store
// (AGENTS.md phase 15, section 40.2).
//
// The filesystem backend is correct and has one limitation that matters for operations: it
// offers no versioning and no replication, so protecting the bytes every claim is cited to
// becomes the deployment's problem. Object stores solve exactly that, which is why this is
// the first phase 15 adapter rather than one of the query backends.
//
// Compatible rather than AWS-specific: the endpoint is configurable and path-style
// addressing is available, so MinIO, Cloudflare R2, Ceph, and Backblaze work the same way
// as S3 itself. The tests run against MinIO for that reason — an adapter claimed to be
// S3-compatible and only ever tested against AWS is an adapter with an untested claim.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/gimantha/strata/internal/store/blob"
)

// Name identifies this backend on artifact rows.
//
// One name for every S3-compatible provider on purpose. The row records which *kind* of
// store holds the bytes so a later migration can find them; recording the vendor would make
// the value change when a deployment moves between providers without the bytes moving at all.
const Name = "s3"

// Options configure the store.
type Options struct {
	// Bucket holds every object. Required.
	Bucket string
	// Prefix namespaces keys inside the bucket, for deployments that share one.
	Prefix string
	// Endpoint points at a non-AWS implementation. Empty uses AWS's own endpoints.
	Endpoint string
	// Region is required by the signing protocol even where the provider ignores it.
	Region string
	// AccessKeyID and SecretAccessKey are static credentials. Both empty falls back to
	// the ambient chain — environment, shared config, instance role — which is how a
	// deployment on AWS avoids having credentials in its configuration at all.
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// PathStyle addresses buckets as endpoint/bucket/key rather than bucket.endpoint.
	// Required by most self-hosted implementations, since virtual-host addressing needs
	// wildcard DNS.
	PathStyle bool
	// Timeout bounds a single operation.
	Timeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.Region == "" {
		// Signing requires a region even when the provider ignores it, and a wrong-but-
		// consistent value is accepted everywhere an ignored value is.
		o.Region = "us-east-1"
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	return o
}

// Store is the S3-backed blob store.
type Store struct {
	client *awss3.Client
	opts   Options
}

// Open builds a client. It does not create the bucket: bucket lifecycle, versioning, and
// replication are the deployment's to configure, and an adapter that quietly created
// buckets would make a typo in configuration look like a working deployment.
func Open(ctx context.Context, opts Options) (*Store, error) {
	const op = "s3.Open"

	opts = opts.withDefaults()
	if opts.Bucket == "" {
		return nil, fmt.Errorf("%s: a bucket is required", op)
	}

	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(opts.Region),
	}
	if opts.AccessKeyID != "" || opts.SecretAccessKey != "" {
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				opts.AccessKeyID, opts.SecretAccessKey, opts.SessionToken)))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot load credentials: %w", op, err)
	}

	client := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = &opts.Endpoint
		}
		o.UsePathStyle = opts.PathStyle
	})

	return &Store{client: client, opts: opts}, nil
}

// Name identifies the backend.
func (s *Store) Name() string { return Name }

// key applies the configured prefix.
func (s *Store) key(key string) string {
	if s.opts.Prefix == "" {
		return key
	}
	return s.opts.Prefix + "/" + key
}

// Put stores data under key.
//
// Idempotent, like the filesystem backend and for the same reason: keys are content
// addresses, so writing identical bytes twice is a normal consequence of an ingestion
// replay rather than a conflict. S3 PUT overwrites, which gives idempotency for free.
func (s *Store) Put(ctx context.Context, key string, data []byte) (blob.Info, error) {
	const op = "s3.Put"

	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	if _, err := s.client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:        &s.opts.Bucket,
		Key:           ptr(s.key(key)),
		Body:          bytes.NewReader(data),
		ContentLength: ptr(int64(len(data))),
	}); err != nil {
		return blob.Info{}, fmt.Errorf("%s: %w", op, err)
	}
	return blob.Info{Key: key, Size: int64(len(data))}, nil
}

// Get retrieves the bytes stored under key.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	const op = "s3.Get"

	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: &s.opts.Bucket,
		Key:    ptr(s.key(key)),
	})
	if err != nil {
		if isNotFound(err) {
			// Translated to the port's own error, so callers keep working against the
			// interface rather than against whichever backend is configured.
			return nil, fmt.Errorf("%s: %w: %s", op, blob.ErrNotFound, key)
		}
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = out.Body.Close() }()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot read the object body: %w", op, err)
	}
	return data, nil
}

// Stat reports an object's size without transferring it.
func (s *Store) Stat(ctx context.Context, key string) (blob.Info, error) {
	const op = "s3.Stat"

	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: &s.opts.Bucket,
		Key:    ptr(s.key(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return blob.Info{}, fmt.Errorf("%s: %w: %s", op, blob.ErrNotFound, key)
		}
		return blob.Info{}, fmt.Errorf("%s: %w", op, err)
	}

	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return blob.Info{Key: key, Size: size}, nil
}

// Delete removes an object.
//
// Absent is not an error, matching the filesystem backend: deletion is idempotent, and a
// caller cleaning up should not have to distinguish "gone now" from "gone already".
func (s *Store) Delete(ctx context.Context, key string) error {
	const op = "s3.Delete"

	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	if _, err := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: &s.opts.Bucket,
		Key:    ptr(s.key(key)),
	}); err != nil && !isNotFound(err) {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// Healthy reports whether the bucket is reachable and readable.
//
// HeadBucket rather than a write: readiness is asked often, and a probe that wrote an
// object would fill a bucket with health checks and would fail a deployment that is
// deliberately read-only during a recovery.
func (s *Store) Healthy(ctx context.Context) error {
	const op = "s3.Healthy"

	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()

	if _, err := s.client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: &s.opts.Bucket}); err != nil {
		return fmt.Errorf("%s: bucket %q is not reachable: %w", op, s.opts.Bucket, err)
	}
	return nil
}

// isNotFound recognizes absence across the shapes S3 implementations report it in.
//
// Three shapes, because they genuinely differ: GetObject returns a typed NoSuchKey,
// HeadObject has no body to carry one and returns a bare 404, and some compatible
// implementations return NotFound as an API error code instead. Matching only the typed
// error would make Stat report every missing object as an infrastructure failure.
func isNotFound(err error) bool {
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}

	var responseErr interface{ HTTPStatusCode() int }
	if errors.As(err, &responseErr) && responseErr.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return false
}

func ptr[T any](v T) *T { return &v }

// Ensure the adapter satisfies the port the rest of the system depends on.
var _ blob.Store = (*Store)(nil)

// RawClient exposes the underlying SDK client for fixture management in tests.
//
// Not part of the blob port and deliberately not used by anything in the running system:
// creating and destroying buckets is an operator's job, not ingestion's.
func (s *Store) RawClient() *awss3.Client { return s.client }
