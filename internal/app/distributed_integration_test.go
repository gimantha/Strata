package app_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/config"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/testsupport/natstest"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

// TestIntegrationDistributedWorkerProcessesPushedWork is phase 14 end to end: a worker
// configured with a broker must pick up work the moment it is committed.
//
// The poll interval is set to five minutes deliberately. Under pure polling this test could
// only pass by waiting five minutes, so finishing in seconds proves the push path ran — and
// because the claim still happens in PostgreSQL, it proves it without weakening any of the
// guarantees the ledger provides (AGENTS.md sections 27.5, 28.1).
func TestIntegrationDistributedWorkerProcessesPushedWork(t *testing.T) {
	dsn := pgtest.DSN(t)
	natsURL := natstest.URL(t)
	cfg := testConfig(t, dsn, natsURL, natstest.Stream(t), t.TempDir(), 5*time.Minute)

	ctx := t.Context()
	application, err := app.New(ctx, cfg)
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = application.Close(closeCtx)
	})

	if application.NATS == nil {
		t.Fatal("a configured broker did not produce a connection")
	}

	scope, principal := newTenant(t, application)

	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		_ = application.RunWorker(workerCtx)
	}()
	t.Cleanup(func() {
		stopWorker()
		<-workerDone
	})

	// Give the consumer time to reach its first long sleep, so what ends it is the
	// notification rather than a poll that happened to be due.
	time.Sleep(500 * time.Millisecond)

	receipt, err := application.Gateway.Accept(ctx, ingest.Request{
		Scope:      scope,
		Principal:  principal,
		SourceName: "test-source",
		ExternalID: "doc-1",
		EventType:  "document.created",
		Operation:  domain.SourceOpUpsert,
		MediaType:  "text/plain",
		Payload: []byte("Ada Lovelace worked on the Analytical Engine with Charles Babbage " +
			"in London during 1843."),
	})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	deadline := time.After(60 * time.Second)
	for {
		event, err := application.Ledger.GetSourceEvent(ctx, scope.WorkspaceID, receipt.SourceEventID)
		if err != nil {
			t.Fatalf("read source event: %v", err)
		}
		if event.Status == domain.SourceEventProcessed {
			return
		}
		if event.Status == domain.SourceEventFailed || event.Status == domain.SourceEventQuarantined {
			t.Fatalf("the pipeline gave up on the event: %s", event.Status)
		}

		select {
		case <-deadline:
			t.Fatalf("the event was still %s after a minute, far inside a five-minute poll "+
				"interval: the broker notification never reached the worker", event.Status)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// TestIntegrationDistributedFleetSharesOneQueue checks the thing horizontal scaling is for:
// two worker processes against one ledger and one broker must divide the work, not repeat it.
func TestIntegrationDistributedFleetSharesOneQueue(t *testing.T) {
	dsn := pgtest.DSN(t)
	natsURL := natstest.URL(t)
	stream := natstest.Stream(t)
	// One blob directory for both, because deployed workers share their artifact store.
	// Giving each its own would mean half the fleet could not read what the other half
	// archived — a misconfiguration, not a distributed-mode property worth testing.
	blobs := t.TempDir()

	ctx := t.Context()

	// Two applications over one database, which is what two deployed workers are.
	first, err := app.New(ctx, testConfig(t, dsn, natsURL, stream, blobs, time.Second))
	if err != nil {
		t.Fatalf("build the first worker: %v", err)
	}
	second, err := app.New(ctx, testConfig(t, dsn, natsURL, stream, blobs, time.Second))
	if err != nil {
		t.Fatalf("build the second worker: %v", err)
	}
	for _, application := range []*app.App{first, second} {
		t.Cleanup(func() {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_ = application.Close(closeCtx)
		})
	}

	scope, principal := newTenant(t, first)

	workerCtx, stopWorkers := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	for _, application := range []*app.App{first, second} {
		go func() {
			defer func() { done <- struct{}{} }()
			_ = application.RunWorker(workerCtx)
		}()
	}
	t.Cleanup(func() {
		stopWorkers()
		<-done
		<-done
	})

	const documents = 12
	receipts := make([]domain.SourceEventID, 0, documents)
	for i := range documents {
		receipt, err := first.Gateway.Accept(ctx, ingest.Request{
			Scope:      scope,
			Principal:  principal,
			SourceName: "test-source",
			ExternalID: fmt.Sprintf("doc-%d", i),
			EventType:  "document.created",
			Operation:  domain.SourceOpUpsert,
			MediaType:  "text/plain",
			Payload: fmt.Appendf(nil,
				"Document %d records that Engineer %d met Reviewer %d in Office %d on the "+
					"fourteenth to agree the schedule for Project %d.", i, i, i, i, i),
		})
		if err != nil {
			t.Fatalf("accept %d: %v", i, err)
		}
		receipts = append(receipts, receipt.SourceEventID)
	}

	deadline := time.After(90 * time.Second)
	for {
		processed := 0
		for _, id := range receipts {
			event, err := first.Ledger.GetSourceEvent(ctx, scope.WorkspaceID, id)
			if err != nil {
				t.Fatalf("read source event: %v", err)
			}
			switch event.Status {
			case domain.SourceEventProcessed:
				processed++
			case domain.SourceEventFailed, domain.SourceEventQuarantined:
				t.Fatalf("the pipeline gave up on %s: %s", id, event.Status)
			}
		}
		if processed == documents {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d of %d documents were processed by the fleet", processed, documents)
		case <-time.After(200 * time.Millisecond):
		}
	}

	// Reaching "processed" is not enough on its own: a fleet that ran every document twice
	// would also get there. The criterion is that scaling out produces no duplicate
	// knowledge, so the check is on what the pipeline wrote, not on how many times it ran.
	//
	// Deliberately not asserting one attempt per item. Concurrent workers touching the same
	// entities do conflict, and retrying is the designed response; a test that forbade it
	// would be testing for an absence of contention rather than for correctness.
	for i, id := range receipts {
		episodes, err := first.Ledger.ListEpisodes(ctx, scope.WorkspaceID, id)
		if err != nil {
			t.Fatalf("list episodes for document %d: %v", i, err)
		}
		if len(episodes) != 1 {
			t.Fatalf("document %d produced %d episodes; the fleet duplicated its knowledge",
				i, len(episodes))
		}

		chunks, err := first.Ledger.ListChunks(ctx, scope.WorkspaceID, id)
		if err != nil {
			t.Fatalf("list chunks for document %d: %v", i, err)
		}
		if len(chunks) == 0 {
			t.Fatalf("document %d reached processed with no chunks", i)
		}
		seen := map[int64]bool{}
		for _, chunk := range chunks {
			if seen[chunk.Sequence] {
				t.Fatalf("document %d has chunk %d twice; reprocessing was not idempotent",
					i, chunk.Sequence)
			}
			seen[chunk.Sequence] = true
		}
	}

	// Nothing may have been abandoned along the way.
	dead, err := first.Ledger.ListOutbox(ctx, scope.WorkspaceID, domain.OutboxDead, 10)
	if err != nil {
		t.Fatalf("list dead-lettered work: %v", err)
	}
	if len(dead) != 0 {
		t.Fatalf("%d work items were dead-lettered under fleet contention", len(dead))
	}
}

// testConfig builds a worker configuration pointed at one database and one broker.
func testConfig(t *testing.T, dsn, natsURL, stream, blobDir string,
	pollInterval time.Duration) config.Config {
	t.Helper()

	cfg, err := config.LoadFrom(func(key string) string {
		switch key {
		case "CG_DATABASE_URL":
			return dsn
		case "CG_NATS_URL":
			return natsURL
		case "CG_NATS_STREAM":
			return stream
		case "CG_BLOB_DIR":
			return filepath.Join(blobDir, "blobs")
		case "CG_API_KEYS_FILE":
			return filepath.Join(t.TempDir(), "api-keys.json")
		case "CG_WORKER_POLL_INTERVAL":
			return pollInterval.String()
		case "CG_WORKER_LEASE":
			return (pollInterval + 30*time.Second).String()
		case "CG_WORKER_IDLE_BACKOFF_MAX":
			return pollInterval.String()
		case "CG_BACKOFF_BASE":
			return "100ms"
		case "CG_BACKOFF_MAX":
			// Workers touching the same entities conflict and retry, which is correct.
			// The production curve reaches minutes by the sixth attempt, though, so
			// inheriting it here would measure the backoff policy rather than the
			// delivery guarantee this test is about.
			return "1s"
		case "CG_AUTO_MIGRATE":
			// pgtest hands back an already-migrated database.
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
		t.Fatalf("build test config: %v", err)
	}
	return cfg
}

// newTenant creates a workspace, graph space, and source through the application's own
// ledger, so the test exercises the same store the worker uses.
func newTenant(t *testing.T, application *app.App) (domain.Scope, domain.PrincipalRef) {
	t.Helper()
	ctx := context.Background()

	principal := domain.Principal{
		ID:          "distributed-owner",
		Kind:        domain.PrincipalUser,
		DisplayName: "distributed owner",
		SystemRole:  domain.RoleAdmin,
	}
	if err := application.Ledger.UpsertPrincipal(ctx, principal); err != nil {
		t.Fatalf("upsert principal: %v", err)
	}

	ws, err := application.Ledger.CreateWorkspace(ctx,
		domain.Workspace{Slug: "acme", Name: "acme"}, principal.ID)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	gs, err := application.Ledger.CreateGraphSpace(ctx, domain.GraphSpace{
		WorkspaceID: ws.ID, Slug: "main", Name: "Main",
	}, principal.ID)
	if err != nil {
		t.Fatalf("create graph space: %v", err)
	}
	if _, err := application.Ledger.CreateSource(ctx, domain.Source{
		WorkspaceID: ws.ID,
		Kind:        domain.SourceKindChat,
		Name:        "test-source",
		TrustLevel:  domain.TrustStandard,
	}, principal.ID); err != nil {
		t.Fatalf("create source: %v", err)
	}

	return domain.Scope{WorkspaceID: ws.ID, GraphSpaceID: gs.ID},
		domain.PrincipalRef{ID: principal.ID, Kind: principal.Kind, DisplayName: principal.DisplayName}
}
