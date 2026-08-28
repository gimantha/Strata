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
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/testsupport/neo4jtest"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

// TestIntegrationRetrievalWorksWithTheGraphProjectionElsewhere completes the set: the ledger
// in PostgreSQL, the graph in Neo4j, and traversal answering a question no passage states.
//
// The relational query is the one that matters here. A claim connects two entities and no
// document says so in those words, so reaching the second from the first is only possible by
// following an edge — which means the answer comes from Neo4j and the names come from the
// ledger, exactly as the graph seam intended.
func TestIntegrationRetrievalWorksWithTheGraphProjectionElsewhere(t *testing.T) {
	dsn := pgtest.DSN(t)
	uri := neo4jtest.URI(t)
	user, password := neo4jtest.Credentials()

	cfg, err := config.LoadFrom(func(key string) string {
		switch key {
		case "CG_DATABASE_URL":
			return dsn
		case "CG_GRAPH_BACKEND":
			return "neo4j"
		case "CG_NEO4J_URI":
			return uri
		case "CG_NEO4J_USER":
			return user
		case "CG_NEO4J_PASSWORD":
			return password
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
	if names[domain.ProjectionGraph] != "neo4j" {
		t.Fatalf("the graph projection reports %q", names[domain.ProjectionGraph])
	}
	if names[domain.ProjectionLexical] != "postgres" {
		t.Fatalf("the lexical projection moved too: %q", names[domain.ProjectionLexical])
	}

	scope, principal := newTenant(t, application)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = application.Indexes.Graph.Purge(cleanupCtx, scope.WorkspaceID)
	})

	receipt, err := application.Gateway.Accept(ctx, ingest.Request{
		Scope: scope, Principal: principal, SourceName: "test-source",
		ExternalID: "neo4j-1", EventType: "document.created",
		Operation: domain.SourceOpUpsert, MediaType: normalize.MediaTypePlain,
		Payload:        []byte("Acme Corporation was reviewed this quarter."),
		IdempotencyKey: "neo4j-1",
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := application.Runner.Process(ctx, scope.WorkspaceID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	episodes, err := application.Ledger.ListEpisodes(ctx, scope.WorkspaceID, receipt.SourceEventID)
	if err != nil || len(episodes) == 0 {
		t.Fatalf("expected an episode: %v", err)
	}

	// A relationship stated as a claim rather than as prose, so no passage answers the
	// query directly and only traversal can.
	if _, err := application.Knowledge.Assert(ctx, knowledge.AssertRequest{
		Scope: scope, Principal: principal, SourceEventID: receipt.SourceEventID,
		Claims: []knowledge.Claim{{
			Subject:      knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
			Predicate:    "SUPPLIES",
			ObjectEntity: &knowledge.EntityRef{Name: "Globex Industries", Type: "organization"},
			Evidence:     []knowledge.EvidenceInput{{EpisodeID: episodes[0].ID}},
		}},
	}); err != nil {
		t.Fatalf("assert: %v", err)
	}
	if _, err := application.Projector.ProjectEvent(ctx, scope, receipt.SourceEventID); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := application.Projector.ProjectEntities(ctx, scope); err != nil {
		t.Fatalf("project entities: %v", err)
	}

	found, err := application.Retriever.Query(ctx, domain.QueryRequest{
		Scope: scope, Query: "Acme Corporation", Principal: principal,
		Modes: []domain.RetrievalMode{domain.ModeEntity, domain.ModeGraph}, Limit: 20,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	var reachedGlobex bool
	for _, item := range found.Items {
		if item.Content == "Globex Industries" || item.Path != nil {
			if item.Content == "Globex Industries" {
				reachedGlobex = true
				// The name came from the ledger, and the path from Neo4j.
				if item.Path == nil {
					t.Error("a graph hit carries no path; it is not explainable")
				}
			}
		}
	}
	if !reachedGlobex {
		t.Error("traversal did not reach the entity only an edge connects")
	}

	if count, err := application.Indexes.Counts(ctx, scope.WorkspaceID); err != nil {
		t.Fatalf("counts: %v", err)
	} else if count[domain.ProjectionGraph] == 0 {
		t.Error("the recovery drill would report no edges while traversal follows them")
	}
}
