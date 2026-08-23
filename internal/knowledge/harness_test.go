package knowledge_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/pipeline"
	"github.com/gimantha/strata/internal/store/blob"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// clock is a settable clock so knowledge time is deterministic in tests. Temporal
// behavior is the thing under test here, so it cannot depend on wall time.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

type harness struct {
	fixture *pgtest.Fixture
	service *knowledge.Service
	clock   *clock
	gateway *ingest.Gateway
	runner  *pipeline.Runner
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	f := pgtest.NewFixture(t)
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("create blob store: %v", err)
	}

	c := &clock{now: time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC)}

	return &harness{
		fixture: f,
		clock:   c,
		service: knowledge.New(f.Store, knowledge.Options{Now: c.Now}, nil, nil),
		gateway: ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1}, nil, nil, nil),
		runner: pipeline.NewRunner(f.Store, 1, pipeline.DefaultStages(f.Store, blobs, pipeline.StageConfig{
			ChunkMaxTokens: 128, ChunkOverlapTokens: 16,
		}), nil, nil, nil),
	}
}

func (h *harness) scope() domain.Scope { return h.fixture.Primary.Scope() }

func (h *harness) principal() domain.PrincipalRef { return h.fixture.Primary.Principal.Ref() }

// ingestEpisode puts real source material in the ledger and processes it, so claims made
// against it have a genuine provenance chain rather than a synthetic one.
func (h *harness) ingestEpisode(t *testing.T, content, idempotencyKey string) (domain.SourceEventID, domain.EpisodeID, domain.ChunkID) {
	t.Helper()
	ctx := context.Background()

	receipt, err := h.gateway.Accept(ctx, ingest.Request{
		Scope:          h.scope(),
		Principal:      h.principal(),
		SourceID:       h.fixture.Primary.Source.ID,
		MediaType:      normalize.MediaTypePlain,
		Payload:        []byte(content),
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := h.runner.Process(ctx, h.scope().WorkspaceID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	episodes, err := h.fixture.Store.ListEpisodes(ctx, h.scope().WorkspaceID, receipt.SourceEventID)
	if err != nil || len(episodes) == 0 {
		t.Fatalf("expected episodes for the ingested content: %v", err)
	}
	chunks, err := h.fixture.Store.ListChunks(ctx, h.scope().WorkspaceID, receipt.SourceEventID)
	if err != nil || len(chunks) == 0 {
		t.Fatalf("expected chunks for the ingested content: %v", err)
	}
	return receipt.SourceEventID, episodes[0].ID, chunks[0].ID
}

// assertOne commits a single claim and returns it.
func (h *harness) assertOne(t *testing.T, eventID domain.SourceEventID, claim knowledge.Claim) domain.Assertion {
	t.Helper()

	result, err := h.service.Assert(context.Background(), knowledge.AssertRequest{
		Scope:         h.scope(),
		Principal:     h.principal(),
		SourceEventID: eventID,
		Claims:        []knowledge.Claim{claim},
	})
	if err != nil {
		t.Fatalf("assert: %v", err)
	}
	if len(result.Assertions) != 1 {
		t.Fatalf("expected 1 assertion, got %d", len(result.Assertions))
	}
	return result.Assertions[0]
}

// query runs a temporal query and returns the matching claims.
func (h *harness) query(t *testing.T, q domain.AssertionQuery) []domain.Assertion {
	t.Helper()

	q.Scope = h.scope()
	found, err := h.service.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return found
}

// objectsOf renders the object values of a result set for assertions in tests.
func objectsOf(assertions []domain.Assertion) []string {
	out := make([]string, 0, len(assertions))
	for _, a := range assertions {
		out = append(out, a.Object.Display())
	}
	return out
}

func date(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func ptr[T any](v T) *T { return &v }
