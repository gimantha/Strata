package benchmarks_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/benchmarks"
	"github.com/gimantha/strata/internal/config"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// deployment is a real Strata instance over a real database, assembled the way a process
// assembles it. Benchmarks measure the wiring as deployed, not a stripped-down path that
// exists only in tests.
type deployment struct {
	app       *app.App
	scope     domain.Scope
	principal domain.PrincipalRef
	corpus    benchmarks.Corpus
}

// newDeployment builds an instance with the hashing embedder.
//
// hashing rather than a hosted model on purpose: it computes locally, so the numbers
// measure this system rather than somebody's API, and it is reproducible on any machine.
// A deployment benchmarking a real embedding model should say which one — that is exactly
// what section 39's reporting requirement is for.
func newDeployment(tb testing.TB, corpus benchmarks.Corpus) *deployment {
	tb.Helper()

	dsn := pgtest.DSN(tb)
	cfg, err := config.LoadFrom(func(key string) string {
		switch key {
		case "CG_DATABASE_URL":
			return dsn
		case "CG_BLOB_DIR":
			return filepath.Join(tb.TempDir(), "blobs")
		case "CG_API_KEYS_FILE":
			return filepath.Join(tb.TempDir(), "api-keys.json")
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
		tb.Fatalf("configure: %v", err)
	}

	ctx := context.Background()
	application, err := app.New(ctx, cfg)
	if err != nil {
		tb.Fatalf("build app: %v", err)
	}
	tb.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = application.Close(closeCtx)
	})

	principal := domain.Principal{
		ID: "bench-owner", Kind: domain.PrincipalUser,
		DisplayName: "benchmark owner", SystemRole: domain.RoleAdmin,
	}
	if err := application.Ledger.UpsertPrincipal(ctx, principal); err != nil {
		tb.Fatalf("upsert principal: %v", err)
	}
	ws, err := application.Ledger.CreateWorkspace(ctx,
		domain.Workspace{Slug: "bench", Name: "bench"}, principal.ID)
	if err != nil {
		tb.Fatalf("create workspace: %v", err)
	}
	gs, err := application.Ledger.CreateGraphSpace(ctx, domain.GraphSpace{
		WorkspaceID: ws.ID, Slug: "main", Name: "Main",
	}, principal.ID)
	if err != nil {
		tb.Fatalf("create graph space: %v", err)
	}
	if _, err := application.Ledger.CreateSource(ctx, domain.Source{
		WorkspaceID: ws.ID, Kind: domain.SourceKindDocument,
		Name: "bench-source", TrustLevel: domain.TrustStandard,
	}, principal.ID); err != nil {
		tb.Fatalf("create source: %v", err)
	}

	return &deployment{
		app:       application,
		scope:     domain.Scope{WorkspaceID: ws.ID, GraphSpaceID: gs.ID},
		principal: domain.PrincipalRef{ID: principal.ID, Kind: principal.Kind},
		corpus:    corpus,
	}
}

// accept durably records one document, which is the operation section 39's ingest target
// measures: archived, committed, and queued for processing, but not yet processed. That
// boundary is the API's promise — a caller waits for durability, never for extraction.
func (d *deployment) accept(tb testing.TB, doc benchmarks.Document) domain.SourceEventID {
	tb.Helper()

	receipt, err := d.app.Gateway.Accept(context.Background(), ingest.Request{
		Scope:          d.scope,
		Principal:      d.principal,
		SourceName:     "bench-source",
		ExternalID:     doc.ExternalID,
		EventType:      "document.created",
		Operation:      domain.SourceOpUpsert,
		MediaType:      normalize.MediaTypePlain,
		Payload:        []byte(doc.Content),
		IdempotencyKey: doc.ExternalID,
	})
	if err != nil {
		tb.Fatalf("accept %s: %v", doc.ExternalID, err)
	}
	return receipt.SourceEventID
}

// load ingests and fully processes the corpus, returning how long each phase took.
//
// Processing runs at the configured worker concurrency rather than one document at a time,
// because a single-threaded projection lag figure describes a deployment nobody runs.
func (d *deployment) load(tb testing.TB) (ingestDuration, processDuration time.Duration) {
	tb.Helper()

	docs := d.corpus.Generate()

	start := time.Now()
	events := make([]domain.SourceEventID, 0, len(docs))
	for _, doc := range docs {
		events = append(events, d.accept(tb, doc))
	}
	ingestDuration = time.Since(start)

	start = time.Now()
	workers := min(runtime.NumCPU(), 8)
	queue := make(chan domain.SourceEventID)
	var (
		wg       sync.WaitGroup
		failures = make(chan error, len(events))
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range queue {
				if _, err := d.app.Runner.Process(context.Background(),
					d.scope.WorkspaceID, id, false); err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	for _, id := range events {
		queue <- id
	}
	close(queue)
	wg.Wait()
	close(failures)
	for err := range failures {
		tb.Fatalf("process: %v", err)
	}
	processDuration = time.Since(start)

	d.relate(tb, docs, events)
	return ingestDuration, processDuration
}

// relate records the entity-to-entity claims each document implies.
//
// Asserted directly rather than extracted by a model. The corpus already states which
// entities each document mentions, so deriving the graph from that gives a known shape —
// a benchmark whose graph depends on what a model happened to find measures the model.
// It also means these numbers are reproducible on a machine with no model configured,
// which is the point of using the hashing embedder too.
func (d *deployment) relate(tb testing.TB, docs []benchmarks.Document, events []domain.SourceEventID) {
	tb.Helper()
	ctx := context.Background()

	predicates := []string{"WORKS_AT", "LOCATED_IN", "REPORTS_TO", "SUPPLIES"}

	for i, doc := range docs {
		episodes, err := d.app.Ledger.ListEpisodes(ctx, d.scope.WorkspaceID, events[i])
		if err != nil || len(episodes) == 0 {
			tb.Fatalf("expected an episode for %s: %v", doc.ExternalID, err)
		}

		claims := make([]knowledge.Claim, 0, len(doc.Mentions))
		for j, mention := range doc.Mentions {
			object := doc.Mentions[(j+1)%len(doc.Mentions)]
			if object.Name == mention.Name {
				continue
			}
			claims = append(claims, knowledge.Claim{
				Subject:      knowledge.EntityRef{Name: mention.Name, Type: mention.Type},
				Predicate:    predicates[j%len(predicates)],
				ObjectEntity: &knowledge.EntityRef{Name: object.Name, Type: object.Type},
				ScopeKey:     doc.ExternalID,
				Evidence:     []knowledge.EvidenceInput{{EpisodeID: episodes[0].ID}},
			})
		}
		if len(claims) == 0 {
			continue
		}

		if _, err := d.app.Knowledge.Assert(ctx, knowledge.AssertRequest{
			Scope:         d.scope,
			Principal:     d.principal,
			SourceEventID: events[i],
			Claims:        claims,
		}); err != nil {
			tb.Fatalf("assert relations for %s: %v", doc.ExternalID, err)
		}
	}
}

// conditions renders everything section 39 requires alongside a number.
//
// Printed with every result rather than written once in a README, because the number and
// the conditions get copied separately and only one of them is meaningful alone.
func conditions(corpus benchmarks.Corpus) string {
	return fmt.Sprintf(
		"dataset: %s | hardware: %s/%s, %d cpu | index: pgvector ivfflat + pg_trgm + tsvector "+
			"(migrations 0006-0013) | embedding: hashing-bow-v1, 1536 dimensions",
		corpus.Describe(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
}

// minSamples is the smallest sample this harness will report percentiles from.
//
// Go runs a benchmark body several times with a growing iteration count to calibrate. Those
// warm-up passes are real measurements of nothing, and printing a p95 from a single sample
// puts a number in the record that looks like a result.
const minSamples = 20

// report prints one measurement with its conditions attached.
func report(tb testing.TB, corpus benchmarks.Corpus, name string, lines ...string) {
	tb.Helper()

	tb.Logf("\n%s\n  %s", name, conditions(corpus))
	for _, line := range lines {
		tb.Logf("  %s", line)
	}
}

// reportable reports whether a sample is large enough to describe.
func reportable(samples int) bool { return samples >= minSamples }

// percentile returns the value at a percentile of a sorted-in-place sample.
//
// Nearest-rank rather than interpolated: with a few hundred samples the difference is
// noise, and reporting an interpolated figure implies a precision the sample does not have.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(float64(len(sorted))*p/100 + 0.5)
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// target reads a configurable threshold, so a deployment can assert its own targets rather
// than inheriting the ones this machine happens to meet (AGENTS.md section 39).
func target(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return parsed
}
