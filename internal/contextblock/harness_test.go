package contextblock_test

import (
	"context"
	"testing"

	"github.com/gimantha/strata/internal/contextblock"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding/hashing"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/pipeline"
	"github.com/gimantha/strata/internal/projection"
	"github.com/gimantha/strata/internal/retrieval"
	"github.com/gimantha/strata/internal/store/blob"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// harness runs assembly against everything below it: real ingestion, real projections, real
// retrieval, real PostgreSQL. The unit tests already cover the arithmetic with fakes; what
// these exercise is whether a citation still resolves once the ids come from the ledger
// rather than from a fixture.
type harness struct {
	fixture       *pgtest.Fixture
	gateway       *ingest.Gateway
	runner        *pipeline.Runner
	service       *knowledge.Service
	projector     *projection.Projector
	retrieverImpl *retrieval.Retriever
	assembler     *contextblock.Assembler
}

// retriever exposes the shared retriever so a test can build a differently configured
// assembler over the same corpus.
func (h *harness) retriever() *retrieval.Retriever { return h.retrieverImpl }

func newHarness(t *testing.T) *harness {
	t.Helper()

	f := pgtest.NewFixture(t)
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	embedder := hashing.New()
	projector := projection.New(f.Store, f.Store, f.Store.Indexes(), embedder, projection.Options{}, nil, nil)
	retriever := retrieval.New(f.Store, f.Store.Indexes(), embedder, retrieval.Options{}, nil, nil)

	stages := pipeline.DefaultStages(f.Store, blobs, pipeline.StageConfig{
		ChunkMaxTokens:     256,
		ChunkOverlapTokens: 16,
		Tokenizer:          normalize.DefaultTokenizer,
		Projector:          projector,
	})

	return &harness{
		fixture:       f,
		gateway:       ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1}, nil, nil, nil),
		runner:        pipeline.NewRunner(f.Store, 1, stages, nil, nil, nil),
		service:       knowledge.New(f.Store, knowledge.Options{}, nil, nil),
		projector:     projector,
		retrieverImpl: retriever,
		assembler:     contextblock.New(retriever, f.Store, contextblock.Options{}, nil, nil),
	}
}

func (h *harness) scope() domain.Scope { return h.fixture.Primary.Scope() }

// ingest puts a document through the whole pipeline and returns its event and first episode.
func (h *harness) ingest(t *testing.T, text, key string) (domain.SourceEventID, domain.EpisodeID) {
	t.Helper()
	ctx := context.Background()

	receipt, err := h.gateway.Accept(ctx, ingest.Request{
		Scope:          h.scope(),
		Principal:      h.fixture.Primary.Principal.Ref(),
		SourceID:       h.fixture.Primary.Source.ID,
		MediaType:      normalize.MediaTypePlain,
		Payload:        []byte(text),
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", key, err)
	}
	if _, err := h.runner.Process(ctx, h.scope().WorkspaceID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process %s: %v", key, err)
	}

	episodes, err := h.fixture.Store.ListEpisodes(ctx, h.scope().WorkspaceID, receipt.SourceEventID)
	if err != nil || len(episodes) == 0 {
		t.Fatalf("list episodes for %s: %v", key, err)
	}
	return receipt.SourceEventID, episodes[0].ID
}

// claim records one fact against real source material, with real evidence.
func (h *harness) claim(t *testing.T, event domain.SourceEventID, episode domain.EpisodeID, c knowledge.Claim) domain.AssertionID {
	t.Helper()
	ctx := context.Background()

	if len(c.Evidence) == 0 {
		c.Evidence = []knowledge.EvidenceInput{{EpisodeID: episode, ExtractedText: c.Predicate}}
	}

	result, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope:         h.scope(),
		Principal:     h.fixture.Primary.Principal.Ref(),
		SourceEventID: event,
		Claims:        []knowledge.Claim{c},
	})
	if err != nil {
		t.Fatalf("assert: %v", err)
	}
	if len(result.Assertions) == 0 {
		t.Fatal("assert returned no assertion")
	}

	if _, err := h.projector.ProjectEvent(ctx, h.scope(), event); err != nil {
		t.Fatalf("project event: %v", err)
	}
	if _, err := h.projector.ProjectEntities(ctx, h.scope()); err != nil {
		t.Fatalf("project entities: %v", err)
	}
	return result.Assertions[0].ID
}
