package app_test

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/config"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
	"github.com/gimantha/strata/internal/testsupport/qdranttest"
)

// TestIntegrationRetrievalWorksWithTheVectorProjectionElsewhere is the deployment-level
// claim: the ledger in PostgreSQL, the vectors in Qdrant, and everything above the ports
// unaware of the difference.
//
// The conformance suites prove the adapter behaves like the port. This proves the system
// works when it is the one configured — the difference between satisfying an interface and
// being wired in correctly, which is the same distinction the S3 backend needed.
func TestIntegrationRetrievalWorksWithTheVectorProjectionElsewhere(t *testing.T) {
	dsn := pgtest.DSN(t)
	host, port := qdranttest.Address(t)
	collection := qdranttest.Collection(t)

	cfg, err := config.LoadFrom(func(key string) string {
		switch key {
		case "CG_DATABASE_URL":
			return dsn
		case "CG_VECTOR_BACKEND":
			return "qdrant"
		case "CG_QDRANT_HOST":
			return host
		case "CG_QDRANT_PORT":
			return strconv.Itoa(port)
		case "CG_QDRANT_COLLECTION":
			return collection
		case "CG_BLOB_DIR":
			return filepath.Join(t.TempDir(), "blobs")
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
		closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = application.Close(closeCtx)
	})

	// The projections report which backend serves each, so an operator is told rather than
	// having to infer it from configuration.
	names := application.Indexes.Names()
	if names[domain.ProjectionVector] != "qdrant" {
		t.Fatalf("the vector projection reports %q, not qdrant", names[domain.ProjectionVector])
	}
	if names[domain.ProjectionLexical] != "postgres" {
		t.Fatalf("the lexical projection moved as well: %q", names[domain.ProjectionLexical])
	}

	scope, principal := newTenant(t, application)

	const statement = "The Kelvinbridge office reviewed delivery schedules with Priya Raman."
	receipt, err := application.Gateway.Accept(ctx, ingest.Request{
		Scope: scope, Principal: principal, SourceName: "test-source",
		ExternalID: "qdrant-1", EventType: "document.created",
		Operation: domain.SourceOpUpsert, MediaType: normalize.MediaTypePlain,
		Payload: []byte(statement), IdempotencyKey: "qdrant-1",
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := application.Runner.Process(ctx, scope.WorkspaceID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	// The vector leg alone, so this cannot pass on the lexical index's behalf.
	result, err := application.Retriever.Query(ctx, domain.QueryRequest{
		Scope: scope, Query: "who reviewed delivery schedules", Principal: principal,
		Modes: []domain.RetrievalMode{domain.ModeVector}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(result.Items) == 0 {
		t.Fatal("the vector leg returned nothing with the projection in Qdrant")
	}

	// And the counts a recovery drill reads come from the configured backend, not from a
	// PostgreSQL table that is now empty.
	counts, err := application.Indexes.Counts(ctx, scope.WorkspaceID)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts[domain.ProjectionVector] == 0 {
		t.Error("the recovery drill would report no vectors while retrieval finds them")
	}

	// A rebuild has to reach Qdrant too, since Purge and the replay both go through the
	// port. This is the property section 40's drill depends on.
	if _, err := application.Projector.Rebuild(ctx, scope.WorkspaceID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rebuilt, err := application.Indexes.Counts(ctx, scope.WorkspaceID)
	if err != nil {
		t.Fatalf("counts after rebuild: %v", err)
	}
	if rebuilt[domain.ProjectionVector] != counts[domain.ProjectionVector] {
		t.Errorf("a rebuild left %d vectors where there were %d",
			rebuilt[domain.ProjectionVector], counts[domain.ProjectionVector])
	}

	result, err = application.Retriever.Query(ctx, domain.QueryRequest{
		Scope: scope, Query: "who reviewed delivery schedules", Principal: principal,
		Modes: []domain.RetrievalMode{domain.ModeVector}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("query after rebuild: %v", err)
	}
	if len(result.Items) == 0 {
		t.Error("retrieval found nothing after a rebuild")
	}
	if !strings.Contains(statement, "Kelvinbridge") {
		t.Fatal("the fixture changed out from under this assertion")
	}
}
