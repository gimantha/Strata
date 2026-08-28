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
	"github.com/gimantha/strata/internal/testsupport/opensearchtest"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

// TestIntegrationRetrievalWorksWithTheLexicalProjectionElsewhere is the deployment-level
// claim for the third backend: the ledger in PostgreSQL, the text index in OpenSearch, and
// everything above the ports unaware.
//
// The conformance suites prove the adapter behaves like the port; this proves the system
// works when it is the one configured. Both halves are needed, and the S3 and Qdrant
// backends each needed the same pair.
func TestIntegrationRetrievalWorksWithTheLexicalProjectionElsewhere(t *testing.T) {
	dsn := pgtest.DSN(t)
	url := opensearchtest.URL(t)
	indexName := opensearchtest.Index(t)

	cfg, err := config.LoadFrom(func(key string) string {
		switch key {
		case "CG_DATABASE_URL":
			return dsn
		case "CG_LEXICAL_BACKEND":
			return "opensearch"
		case "CG_OPENSEARCH_URL":
			return url
		case "CG_OPENSEARCH_INDEX":
			return indexName
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

	names := application.Indexes.Names()
	if names[domain.ProjectionLexical] != "opensearch" {
		t.Fatalf("the lexical projection reports %q", names[domain.ProjectionLexical])
	}
	if names[domain.ProjectionVector] != "postgres" {
		t.Fatalf("the vector projection moved too: %q", names[domain.ProjectionVector])
	}

	scope, principal := newTenant(t, application)

	receipt, err := application.Gateway.Accept(ctx, ingest.Request{
		Scope: scope, Principal: principal, SourceName: "test-source",
		ExternalID: "opensearch-1", EventType: "document.created",
		Operation: domain.SourceOpUpsert, MediaType: normalize.MediaTypePlain,
		Payload:        []byte("The Kelvinbridge depot logged fault ERR_7731X during dispatch."),
		IdempotencyKey: "opensearch-1",
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := application.Runner.Process(ctx, scope.WorkspaceID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	// The lexical leg alone, so this cannot pass on the vector index's behalf.
	found, err := application.Retriever.Query(ctx, domain.QueryRequest{
		Scope: scope, Query: "depot logged fault", Principal: principal,
		Modes: []domain.RetrievalMode{domain.ModeLexical}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("lexical query: %v", err)
	}
	if len(found.Items) == 0 {
		t.Error("the lexical leg returned nothing with the projection in OpenSearch")
	}

	// And the exact leg, which is the half a prefix-matching engine could not serve and
	// the reason this backend was chosen over a lighter one.
	found, err = application.Retriever.Query(ctx, domain.QueryRequest{
		Scope: scope, Query: "ERR_7731X", Principal: principal,
		Modes: []domain.RetrievalMode{domain.ModeExact}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("exact query: %v", err)
	}
	if len(found.Items) == 0 {
		t.Error("an identifier was not findable through the exact leg")
	}

	// A rebuild reaches OpenSearch too, since Purge and the replay both go through the
	// port. This is what section 40's drill depends on.
	before, err := application.Indexes.Counts(ctx, scope.WorkspaceID)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if before[domain.ProjectionLexical] == 0 {
		t.Fatal("the recovery drill would report no lexical records while retrieval finds them")
	}
	if _, err := application.Projector.Rebuild(ctx, scope.WorkspaceID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	after, err := application.Indexes.Counts(ctx, scope.WorkspaceID)
	if err != nil {
		t.Fatalf("counts after rebuild: %v", err)
	}
	if after[domain.ProjectionLexical] != before[domain.ProjectionLexical] {
		t.Errorf("a rebuild left %d lexical records where there were %d",
			after[domain.ProjectionLexical], before[domain.ProjectionLexical])
	}
}
