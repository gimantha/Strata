package projection_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding"
	embeddingmock "github.com/gimantha/strata/internal/embedding/mock"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/pipeline"
	"github.com/gimantha/strata/internal/projection"
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
	embedder  *embeddingmock.Embedder
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	f := pgtest.NewFixture(t)
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("create blob store: %v", err)
	}

	embedder := embeddingmock.New()
	service := knowledge.New(f.Store, knowledge.Options{}, nil, nil)
	projector := projection.New(f.Store, embedder, projection.Options{}, nil, nil)

	stages := pipeline.DefaultStages(f.Store, blobs, pipeline.StageConfig{
		ChunkMaxTokens:     128,
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
		embedder:  embedder,
	}
}

func (h *harness) scope() domain.Scope { return h.fixture.Primary.Scope() }

// ingest puts content through the pipeline, projections included.
func (h *harness) ingest(t *testing.T, content, key string) domain.SourceEventID {
	t.Helper()
	ctx := context.Background()

	receipt, err := h.gateway.Accept(ctx, ingest.Request{
		Scope:          h.scope(),
		Principal:      h.fixture.Primary.Principal.Ref(),
		SourceID:       h.fixture.Primary.Source.ID,
		MediaType:      normalize.MediaTypePlain,
		Payload:        []byte(content),
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := h.runner.Process(ctx, h.scope().WorkspaceID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	return receipt.SourceEventID
}

// assertClaim records a fact and projects it.
func (h *harness) assertClaim(t *testing.T, eventID domain.SourceEventID, claim knowledge.Claim) domain.Assertion {
	t.Helper()
	ctx := context.Background()

	result, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope:         h.scope(),
		Principal:     h.fixture.Primary.Principal.Ref(),
		SourceEventID: eventID,
		Claims:        []knowledge.Claim{claim},
	})
	if err != nil {
		t.Fatalf("assert: %v", err)
	}
	if _, err := h.projector.ProjectEvent(ctx, h.scope(), eventID); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := h.projector.ProjectEntities(ctx, h.scope()); err != nil {
		t.Fatalf("project entities: %v", err)
	}
	return result.Assertions[0]
}

// embedQuery produces a query vector the same way the projection did.
func (h *harness) embedQuery(t *testing.T, text string) []float32 {
	t.Helper()

	vectors, err := h.embedder.Embed(context.Background(), []string{text})
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	return vectors[0]
}

func TestIntegrationPipelineProjectsChunksToBothIndexes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	const content = "Acme Corporation supplies industrial fasteners to the aerospace sector."
	h.ingest(t, content, "project-1")

	counts, err := h.fixture.Store.CountProjected(ctx, h.scope().WorkspaceID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if counts["lexical"] == 0 || counts["vector"] == 0 {
		t.Fatalf("chunks should reach both projections, got %v", counts)
	}

	// The lexical projection finds it by words.
	hits, err := h.fixture.Store.SearchLexical(ctx, domain.LexicalQuery{
		Scope: h.scope(), Text: "industrial fasteners",
	})
	if err != nil {
		t.Fatalf("lexical search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("lexical search should find the indexed chunk")
	}
	if hits[0].Score <= 0 {
		t.Fatal("a lexical hit must carry a rank")
	}

	// The vector projection finds it by its embedding.
	vectorHits, err := h.fixture.Store.SearchVectors(ctx, domain.VectorQuery{
		Scope:     h.scope(),
		Embedding: h.embedQuery(t, content),
		Surfaces:  []domain.Surface{domain.SurfaceChunk},
	})
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(vectorHits) == 0 {
		t.Fatal("vector search should find the indexed chunk")
	}
	if vectorHits[0].Score < 0.99 {
		t.Fatalf("an exact text match should be near-identical, got %v", vectorHits[0].Score)
	}
}

func TestIntegrationLexicalHandlesExactIdentifiers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.ingest(t, "The build failed with error code ERR_7731X on part number AF-2291-B.", "exact-1")

	// This is precisely where vectors are weak and lexical retrieval earns its place: an
	// error code has no useful semantic neighbourhood (AGENTS.md section 18).
	hits, err := h.fixture.Store.SearchLexical(ctx, domain.LexicalQuery{
		Scope: h.scope(), Text: "ERR_7731X", Exact: true,
	})
	if err != nil {
		t.Fatalf("exact search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("an exact identifier must be findable")
	}
	if !strings.Contains(hits[0].Content, "ERR_7731X") {
		t.Fatalf("the hit should contain the identifier: %q", hits[0].Content)
	}

	// A part number with punctuation that stemming would mangle.
	hits, err = h.fixture.Store.SearchLexical(ctx, domain.LexicalQuery{
		Scope: h.scope(), Text: "AF-2291-B", Exact: true,
	})
	if err != nil {
		t.Fatalf("exact search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("a part number must be findable")
	}

	// Something absent returns nothing rather than a weak match.
	hits, err = h.fixture.Store.SearchLexical(ctx, domain.LexicalQuery{
		Scope: h.scope(), Text: "ERR_9999Z", Exact: true,
	})
	if err != nil {
		t.Fatalf("exact search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("an absent identifier must not match, got %d hits", len(hits))
	}
}

func TestIntegrationGraphExpansionWalksBothDirections(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	eventID := h.ingest(t, "Acme supplies Globex. Globex supplies Initech.", "graph-1")

	acme := h.assertClaim(t, eventID, knowledge.Claim{
		Subject:      knowledge.EntityRef{Name: "Acme", Type: "organization"},
		Predicate:    "SUPPLIES",
		ObjectEntity: &knowledge.EntityRef{Name: "Globex", Type: "organization"},
	})
	h.assertClaim(t, eventID, knowledge.Claim{
		Subject:      knowledge.EntityRef{Name: "Globex", Type: "organization"},
		Predicate:    "SUPPLIES",
		ObjectEntity: &knowledge.EntityRef{Name: "Initech", Type: "organization"},
	})

	// One hop from Acme reaches Globex.
	hits, err := h.fixture.Store.ExpandGraph(ctx, domain.GraphExpandQuery{
		Scope: h.scope(), Roots: []domain.EntityID{acme.SubjectID}, Depth: 1,
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	names := namesOf(hits)
	if !contains(names, "Globex") {
		t.Fatalf("one hop should reach Globex, got %v", names)
	}
	if contains(names, "Initech") {
		t.Fatalf("Initech is two hops away and must not appear at depth 1: %v", names)
	}

	// Two hops reach Initech, and the path is explainable.
	hits, err = h.fixture.Store.ExpandGraph(ctx, domain.GraphExpandQuery{
		Scope: h.scope(), Roots: []domain.EntityID{acme.SubjectID}, Depth: 2,
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	names = namesOf(hits)
	if !contains(names, "Initech") {
		t.Fatalf("two hops should reach Initech, got %v", names)
	}
	for _, hit := range hits {
		if hit.Depth > 0 && domain.IsZero(hit.ViaAssertion) {
			t.Fatalf("%s was reached without recording which claim connected it", hit.Name)
		}
	}

	// Traversal follows edges in reverse too: "who supplies Initech" is the same graph
	// seen from the other end.
	var initech domain.EntityID
	for _, hit := range hits {
		if hit.Name == "Initech" {
			initech = hit.EntityID
		}
	}
	hits, err = h.fixture.Store.ExpandGraph(ctx, domain.GraphExpandQuery{
		Scope: h.scope(), Roots: []domain.EntityID{initech}, Depth: 2,
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if !contains(namesOf(hits), "Acme") {
		t.Fatalf("reverse traversal should reach Acme, got %v", namesOf(hits))
	}
}

func TestIntegrationGraphExpansionIsBounded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	eventID := h.ingest(t, "A chain of companies.", "bounded-1")

	// A chain longer than the depth ceiling, plus a cycle back to the start.
	names := []string{"C0", "C1", "C2", "C3", "C4", "C5", "C6", "C7"}
	var first domain.EntityID
	for i := 0; i < len(names)-1; i++ {
		claim := h.assertClaim(t, eventID, knowledge.Claim{
			Subject:      knowledge.EntityRef{Name: names[i], Type: "organization"},
			Predicate:    "SUPPLIES",
			ObjectEntity: &knowledge.EntityRef{Name: names[i+1], Type: "organization"},
		})
		if i == 0 {
			first = claim.SubjectID
		}
	}
	// Close the loop, so an unbounded walk would never terminate.
	h.assertClaim(t, eventID, knowledge.Claim{
		Subject:      knowledge.EntityRef{Name: "C7", Type: "organization"},
		Predicate:    "SUPPLIES",
		ObjectEntity: &knowledge.EntityRef{Name: "C0", Type: "organization"},
	})

	// A request beyond the ceiling is clamped rather than honoured: an unbounded traversal
	// of a connected graph reaches everything, which is not an answer.
	hits, err := h.fixture.Store.ExpandGraph(ctx, domain.GraphExpandQuery{
		Scope: h.scope(), Roots: []domain.EntityID{first}, Depth: 100,
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	for _, hit := range hits {
		if hit.Depth > domain.MaxGraphDepth {
			t.Fatalf("%s was returned at depth %d, past the ceiling", hit.Name, hit.Depth)
		}
	}

	// The cycle terminates rather than looping.
	if len(hits) == 0 {
		t.Fatal("expansion should return results despite the cycle")
	}
	seen := map[domain.EntityID]bool{}
	for _, hit := range hits {
		if seen[hit.EntityID] {
			t.Fatalf("%s was returned twice; the walk is not deduplicating", hit.Name)
		}
		seen[hit.EntityID] = true
	}
}

// Scenario I from AGENTS.md section 37: drop every projection, replay from the ledger, and
// obtain semantically equivalent retrieval results.
func TestIntegrationScenarioIProjectionRebuild(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ws := h.scope().WorkspaceID

	// Build up a workspace with several events, claims, and relationships.
	for i, content := range []string{
		"Acme Corporation supplies industrial fasteners.",
		"Globex Industries manufactures turbines in Berlin.",
		"Initech provides software to Acme Corporation.",
	} {
		eventID := h.ingest(t, content, "rebuild-"+string(rune('a'+i)))
		h.assertClaim(t, eventID, knowledge.Claim{
			Subject:      knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
			Predicate:    "SUPPLIES",
			ObjectEntity: &knowledge.EntityRef{Name: "Globex Industries", Type: "organization"},
			Evidence:     nil,
		})
	}

	before, err := h.fixture.Store.CountProjected(ctx, ws)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if before["vector"] == 0 || before["lexical"] == 0 || before["graph"] == 0 {
		t.Fatalf("expected all three projections populated, got %v", before)
	}

	// Capture what retrieval returns before the rebuild.
	lexicalBefore := h.lexicalResults(t, "industrial fasteners")
	vectorBefore := h.vectorResults(t, "Acme Corporation supplies industrial fasteners.")
	graphBefore := h.graphResults(t)

	// Drop every derived record. The ledger is untouched.
	if err := h.fixture.Store.DeleteProjections(ctx, ws); err != nil {
		t.Fatalf("delete projections: %v", err)
	}
	empty, err := h.fixture.Store.CountProjected(ctx, ws)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if empty["vector"] != 0 || empty["lexical"] != 0 || empty["graph"] != 0 {
		t.Fatalf("projections should be empty, got %v", empty)
	}
	if len(h.lexicalResults(t, "industrial fasteners")) != 0 {
		t.Fatal("retrieval should return nothing once the projections are gone")
	}

	// Replay from the ledger alone.
	stats, err := h.projector.Rebuild(ctx, ws)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if stats.Events == 0 {
		t.Fatal("the rebuild should have replayed the events")
	}

	after, err := h.fixture.Store.CountProjected(ctx, ws)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	for name, count := range before {
		if after[name] != count {
			t.Fatalf("%s projection rebuilt to %d records, was %d", name, after[name], count)
		}
	}

	// Retrieval returns equivalent results.
	if got := h.lexicalResults(t, "industrial fasteners"); !equalResults(got, lexicalBefore) {
		t.Fatalf("lexical results differ after rebuild:\nbefore %v\nafter  %v", lexicalBefore, got)
	}
	if got := h.vectorResults(t, "Acme Corporation supplies industrial fasteners."); !equalResults(got, vectorBefore) {
		t.Fatalf("vector results differ after rebuild:\nbefore %v\nafter  %v", vectorBefore, got)
	}
	if got := h.graphResults(t); !equalResults(got, graphBefore) {
		t.Fatalf("graph expansion differs after rebuild:\nbefore %v\nafter  %v", graphBefore, got)
	}

	// The rebuild recorded where it got to, and that it was a rebuild.
	checkpoints, err := h.fixture.Store.ListCheckpoints(ctx, ws)
	if err != nil {
		t.Fatalf("checkpoints: %v", err)
	}
	if len(checkpoints) != 3 {
		t.Fatalf("expected a checkpoint per projection, got %d", len(checkpoints))
	}
	for _, checkpoint := range checkpoints {
		if checkpoint.RebuiltAt == nil {
			t.Fatalf("%s checkpoint should record the rebuild", checkpoint.Projection)
		}
		if checkpoint.LastRecordedAt == nil {
			t.Fatalf("%s checkpoint should record how far it consumed", checkpoint.Projection)
		}
	}
}

func TestIntegrationRebuildSkipsUnchangedEmbeddings(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.ingest(t, "Acme Corporation supplies industrial fasteners.", "reuse-1")
	firstCalls := h.embedder.Calls()
	if firstCalls == 0 {
		t.Fatal("the first projection should have embedded something")
	}

	// Re-projecting the same event without dropping anything must not pay to embed
	// identical text again.
	events, err := h.fixture.Store.ListSourceEventIDsAfter(ctx, h.scope().WorkspaceID, nil, "", 10)
	if err != nil || len(events) == 0 {
		t.Fatalf("list events: %v", err)
	}
	stats, err := h.projector.ProjectEvent(ctx, h.scope(), events[0].ID)
	if err != nil {
		t.Fatalf("re-project: %v", err)
	}
	if stats.Embedded != 0 {
		t.Fatalf("unchanged text should not be re-embedded, embedded %d", stats.Embedded)
	}
	if stats.Reused == 0 {
		t.Fatal("the reuse should be reported")
	}

	// A full rebuild drops the vectors first, so it genuinely re-embeds.
	if _, err := h.projector.Rebuild(ctx, h.scope().WorkspaceID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if h.embedder.Calls() <= firstCalls {
		t.Fatal("a rebuild after deletion must re-embed")
	}
}

func TestIntegrationProjectionsAreWorkspaceScoped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.ingest(t, "Acme Corporation supplies industrial fasteners.", "scope-1")
	other := h.fixture.NewTenant(t, "globex")

	// Another tenant's searches reach none of it, through any projection.
	lexical, err := h.fixture.Store.SearchLexical(ctx, domain.LexicalQuery{
		Scope: other.Scope(), Text: "industrial fasteners",
	})
	if err != nil {
		t.Fatalf("lexical: %v", err)
	}
	if len(lexical) != 0 {
		t.Fatalf("lexical search leaked across tenants, got %d hits", len(lexical))
	}

	vector, err := h.fixture.Store.SearchVectors(ctx, domain.VectorQuery{
		Scope:     other.Scope(),
		Embedding: h.embedQuery(t, "Acme Corporation supplies industrial fasteners."),
	})
	if err != nil {
		t.Fatalf("vector: %v", err)
	}
	if len(vector) != 0 {
		t.Fatalf("vector search leaked across tenants, got %d hits", len(vector))
	}

	// A rebuild of one workspace does not touch another's projections.
	countsBefore, err := h.fixture.Store.CountProjected(ctx, h.scope().WorkspaceID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if _, err := h.projector.Rebuild(ctx, other.Workspace.ID); err != nil {
		t.Fatalf("rebuild other tenant: %v", err)
	}
	countsAfter, err := h.fixture.Store.CountProjected(ctx, h.scope().WorkspaceID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if countsAfter["lexical"] != countsBefore["lexical"] {
		t.Fatal("rebuilding one workspace must not disturb another's projections")
	}
}

func TestIntegrationVectorSearchFiltersBeforeRanking(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	const content = "Confidential salary information for the engineering team."
	h.ingest(t, content, "filter-1")
	probe := h.embedQuery(t, content)

	// Without a filter the record is found.
	hits, err := h.fixture.Store.SearchVectors(ctx, domain.VectorQuery{
		Scope: h.scope(), Embedding: probe,
	})
	if err != nil {
		t.Fatalf("vector: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("the record should be findable")
	}

	// A classification filter excludes it before ranking, rather than ranking it and
	// hoping a later stage drops it.
	hits, err = h.fixture.Store.SearchVectors(ctx, domain.VectorQuery{
		Scope:          h.scope(),
		Embedding:      probe,
		Classification: []domain.Classification{domain.ClassificationSecret},
	})
	if err != nil {
		t.Fatalf("vector: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("the filter should have excluded it, got %d hits", len(hits))
	}

	// A score floor keeps an unrelated question from getting confident-looking answers,
	// which nearest-neighbour search would otherwise always produce.
	hits, err = h.fixture.Store.SearchVectors(ctx, domain.VectorQuery{
		Scope:     h.scope(),
		Embedding: h.embedQuery(t, "something entirely unrelated to any of this"),
		MinScore:  0.9,
	})
	if err != nil {
		t.Fatalf("vector: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("a score floor should exclude distant neighbours, got %d hits", len(hits))
	}
}

func TestIntegrationProjectionWorksWithoutAnEmbedder(t *testing.T) {
	// Lexical and graph retrieval need no model, and a deployment without one should not
	// lose them.
	f := pgtest.NewFixture(t)
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	projector := projection.New(f.Store, nil, projection.Options{}, nil, nil)
	runner := pipeline.NewRunner(f.Store, 1, pipeline.DefaultStages(f.Store, blobs,
		pipeline.StageConfig{ChunkMaxTokens: 128, Projector: projector}), nil, nil, nil)

	ctx := context.Background()
	gateway := ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1}, nil, nil, nil)
	receipt, err := gateway.Accept(ctx, ingest.Request{
		Scope:          f.Primary.Scope(),
		Principal:      f.Primary.Principal.Ref(),
		SourceID:       f.Primary.Source.ID,
		MediaType:      normalize.MediaTypePlain,
		Payload:        []byte("Acme supplies fasteners."),
		IdempotencyKey: "no-embedder-1",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := runner.Process(ctx, f.Primary.Workspace.ID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	counts, err := f.Store.CountProjected(ctx, f.Primary.Workspace.ID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if counts["lexical"] == 0 {
		t.Fatal("lexical projection must work without an embedder")
	}
	if counts["vector"] != 0 {
		t.Fatalf("no embedder means no vectors, got %d", counts["vector"])
	}

	hits, err := f.Store.SearchLexical(ctx, domain.LexicalQuery{
		Scope: f.Primary.Scope(), Text: "fasteners",
	})
	if err != nil || len(hits) == 0 {
		t.Fatalf("lexical retrieval should still work: %v", err)
	}
}

func TestIntegrationEmbeddingDimensionsAreEnforced(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()

	// A model of the wrong width must be caught rather than writing vectors the schema
	// cannot index.
	wrong := embeddingmock.New().WithDimensions(64)
	projector := projection.New(f.Store, wrong, projection.Options{}, nil, nil)

	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	gateway := ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1}, nil, nil, nil)
	runner := pipeline.NewRunner(f.Store, 1, pipeline.DefaultStages(f.Store, blobs,
		pipeline.StageConfig{ChunkMaxTokens: 128}), nil, nil, nil)

	receipt, err := gateway.Accept(ctx, ingest.Request{
		Scope:          f.Primary.Scope(),
		Principal:      f.Primary.Principal.Ref(),
		SourceID:       f.Primary.Source.ID,
		MediaType:      normalize.MediaTypePlain,
		Payload:        []byte("Acme supplies fasteners."),
		IdempotencyKey: "dims-1",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := runner.Process(ctx, f.Primary.Workspace.ID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	if _, err := projector.ProjectEvent(ctx, f.Primary.Scope(), receipt.SourceEventID); err == nil {
		t.Fatal("a dimension mismatch must fail rather than corrupt the projection")
	}
	if err := (embedding.Vector(make([]float32, 64))).Validate(embedding.Dimensions); err == nil {
		t.Fatal("dimension validation should reject the wrong width")
	}
}

// Helpers for comparing retrieval results across a rebuild.

func (h *harness) lexicalResults(t *testing.T, text string) []string {
	t.Helper()

	hits, err := h.fixture.Store.SearchLexical(context.Background(), domain.LexicalQuery{
		Scope: h.scope(), Text: text,
	})
	if err != nil {
		t.Fatalf("lexical: %v", err)
	}
	return recordIDs(hits)
}

func (h *harness) vectorResults(t *testing.T, text string) []string {
	t.Helper()

	hits, err := h.fixture.Store.SearchVectors(context.Background(), domain.VectorQuery{
		Scope: h.scope(), Embedding: h.embedQuery(t, text),
	})
	if err != nil {
		t.Fatalf("vector: %v", err)
	}
	return recordIDs(hits)
}

func (h *harness) graphResults(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()

	entities, err := h.fixture.Store.ListEntities(ctx, h.scope(), "", 100)
	if err != nil {
		t.Fatalf("list entities: %v", err)
	}
	var roots []domain.EntityID
	for _, entity := range entities {
		roots = append(roots, entity.ID)
	}
	if len(roots) == 0 {
		return nil
	}

	hits, err := h.fixture.Store.ExpandGraph(ctx, domain.GraphExpandQuery{
		Scope: h.scope(), Roots: roots, Depth: 2,
	})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, string(hit.EntityID))
	}
	sort.Strings(out)
	return out
}

func recordIDs(hits []domain.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, string(hit.Surface)+":"+hit.RecordID)
	}
	sort.Strings(out)
	return out
}

func equalResults(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func namesOf(hits []domain.GraphHit) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.Name)
	}
	return out
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
