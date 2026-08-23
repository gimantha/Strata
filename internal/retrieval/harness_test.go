package retrieval_test

import (
	"context"
	"testing"

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

type harness struct {
	fixture   *pgtest.Fixture
	gateway   *ingest.Gateway
	runner    *pipeline.Runner
	service   *knowledge.Service
	projector *projection.Projector
	retriever *retrieval.Retriever
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	f := pgtest.NewFixture(t)
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	// Feature hashing rather than the pure-hash mock: the vector leg needs real cosine
	// structure for its results to mean anything. It does not generalize across synonyms,
	// which is stated plainly in the package and is why the fixture does not test for that.
	embedder := hashing.New()
	projector := projection.New(f.Store, embedder, projection.Options{}, nil, nil)
	service := knowledge.New(f.Store, knowledge.Options{}, nil, nil)

	stages := pipeline.DefaultStages(f.Store, blobs, pipeline.StageConfig{
		ChunkMaxTokens:     256,
		ChunkOverlapTokens: 16,
		Tokenizer:          normalize.DefaultTokenizer,
		Projector:          projector,
	})

	return &harness{
		fixture:   f,
		gateway:   ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1}, nil, nil, nil),
		runner:    pipeline.NewRunner(f.Store, 1, stages, nil, nil, nil),
		service:   service,
		projector: projector,
		retriever: retrieval.New(f.Store, embedder, retrieval.Options{}, nil, nil),
	}
}

func (h *harness) scope() domain.Scope { return h.fixture.Primary.Scope() }

// firstEvent returns the earliest ingested event, for tests that need something to attach a
// claim to.
func (h *harness) firstEvent(t *testing.T) domain.SourceEventID {
	t.Helper()

	events, err := h.fixture.Store.ListSourceEventIDsAfter(context.Background(),
		h.scope().WorkspaceID, nil, "", 1)
	if err != nil || len(events) == 0 {
		t.Fatalf("list events: %v", err)
	}
	return events[0].ID
}

// loadFixture ingests the corpus and records the relationship the graph leg needs, returning
// a map from projected record id back to the document key it came from.
func (h *harness) loadFixture(t *testing.T) map[string]string {
	t.Helper()
	ctx := context.Background()

	keyByRecord := map[string]string{}
	var firstEvent domain.SourceEventID

	for _, doc := range fixtureCorpus {
		receipt, err := h.gateway.Accept(ctx, ingest.Request{
			Scope:          h.scope(),
			Principal:      h.fixture.Primary.Principal.Ref(),
			SourceID:       h.fixture.Primary.Source.ID,
			MediaType:      normalize.MediaTypePlain,
			Payload:        []byte(doc.text),
			IdempotencyKey: "fixture-" + doc.key,
		})
		if err != nil {
			t.Fatalf("ingest %s: %v", doc.key, err)
		}
		if _, err := h.runner.Process(ctx, h.scope().WorkspaceID, receipt.SourceEventID, false); err != nil {
			t.Fatalf("process %s: %v", doc.key, err)
		}
		if firstEvent == "" {
			firstEvent = receipt.SourceEventID
		}

		chunks, err := h.fixture.Store.ListChunks(ctx, h.scope().WorkspaceID, receipt.SourceEventID)
		if err != nil {
			t.Fatalf("list chunks: %v", err)
		}
		for _, chunk := range chunks {
			keyByRecord[string(chunk.ID)] = doc.key
		}
	}

	// One relationship, so the graph leg has something to traverse. It is stated as a
	// claim rather than as prose precisely so no single passage answers the relational
	// query directly.
	if _, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope:         h.scope(),
		Principal:     h.fixture.Primary.Principal.Ref(),
		SourceEventID: firstEvent,
		Claims: []knowledge.Claim{{
			Subject:      knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
			Predicate:    "SUPPLIES",
			ObjectEntity: &knowledge.EntityRef{Name: "Globex Industries", Type: "organization"},
		}},
	}); err != nil {
		t.Fatalf("assert relationship: %v", err)
	}

	// Project the claim and the identities it created.
	if _, err := h.projector.ProjectEvent(ctx, h.scope(), firstEvent); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := h.projector.ProjectEntities(ctx, h.scope()); err != nil {
		t.Fatalf("project entities: %v", err)
	}

	return keyByRecord
}
