package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/config"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
	"github.com/gimantha/strata/internal/testsupport/s3test"
)

// TestIntegrationIngestArchivesToTheObjectStore is the point of the S3 backend: a full
// ingest, with the raw bytes landing in an object store instead of on a disk, and the
// provenance chain still reaching them.
//
// The conformance suite proves the adapter behaves like the port. This proves the system
// actually works when that adapter is the one configured — the difference between an
// implementation that satisfies an interface and one that is wired in correctly.
func TestIntegrationIngestArchivesToTheObjectStore(t *testing.T) {
	dsn := pgtest.DSN(t)
	opts := s3test.Options(t)

	cfg, err := config.LoadFrom(func(key string) string {
		switch key {
		case "CG_DATABASE_URL":
			return dsn
		case "CG_BLOB_BACKEND":
			return "s3"
		case "CG_S3_BUCKET":
			return opts.Bucket
		case "CG_S3_ENDPOINT":
			return opts.Endpoint
		case "CG_S3_ACCESS_KEY_ID":
			return opts.AccessKeyID
		case "CG_S3_SECRET_ACCESS_KEY":
			return opts.SecretAccessKey
		case "CG_S3_PATH_STYLE":
			return "true"
		case "CG_API_KEYS_FILE":
			return filepath.Join(t.TempDir(), "api-keys.json")
		case "CG_AUTO_MIGRATE":
			return "false"
		case "CG_EMBEDDING_PROVIDER":
			return "hashing"
		case "CG_LOG_LEVEL":
			return "error"
		case "CG_ENV":
			return "test"
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}

	ctx := t.Context()
	application, err := app.New(ctx, cfg)
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = application.Close(closeCtx)
	})

	if got := application.Blobs.Name(); got != "s3" {
		t.Fatalf("the app opened the %q backend with s3 configured", got)
	}
	if err := application.Blobs.Healthy(ctx); err != nil {
		t.Fatalf("the configured object store is not healthy: %v", err)
	}

	scope, principal := newTenant(t, application)

	const statement = "The Kelvinbridge office reviewed delivery schedules with Priya Raman."
	receipt, err := application.Gateway.Accept(ctx, ingest.Request{
		Scope:          scope,
		Principal:      principal,
		SourceName:     "test-source",
		ExternalID:     "object-store-1",
		EventType:      "document.created",
		Operation:      domain.SourceOpUpsert,
		MediaType:      normalize.MediaTypePlain,
		Payload:        []byte(statement),
		IdempotencyKey: "object-store-1",
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	if _, err := application.Runner.Process(ctx, scope.WorkspaceID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	// Processing reads the archived payload back out of the object store, so reaching
	// "processed" already proves a round trip. The artifact row is what a restore and
	// every provenance walk depend on, so check it names the backend that holds the bytes.
	event, err := application.Ledger.GetSourceEvent(ctx, scope.WorkspaceID, receipt.SourceEventID)
	if err != nil {
		t.Fatalf("read source event: %v", err)
	}
	if event.Status != domain.SourceEventProcessed {
		t.Fatalf("the event is %s; the pipeline could not read the archived payload",
			event.Status)
	}

	artifact, err := application.Ledger.GetArtifact(ctx, scope.WorkspaceID, event.RawArtifactID)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if artifact.Storage != "s3" {
		t.Fatalf("the artifact row records storage %q; a later migration would look for "+
			"these bytes in the wrong place", artifact.Storage)
	}

	// And the bytes themselves come back unchanged, which is what every citation
	// ultimately resolves to.
	stored, err := application.Blobs.Get(ctx, artifact.BlobKey)
	if err != nil {
		t.Fatalf("read the archived payload: %v", err)
	}
	if string(stored) != statement {
		t.Fatalf("the archived payload changed in storage: %q", stored)
	}
}
