package policy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/contextblock"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding/hashing"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/pipeline"
	"github.com/gimantha/strata/internal/policy"
	"github.com/gimantha/strata/internal/projection"
	"github.com/gimantha/strata/internal/retrieval"
	"github.com/gimantha/strata/internal/store/blob"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// tenant is one workspace with everything needed to put knowledge in and get it out.
type tenant struct {
	pgtest.Tenant
	secret string
}

type harness struct {
	fixture   *pgtest.Fixture
	acme      tenant
	globex    tenant
	gateway   *ingest.Gateway
	runner    *pipeline.Runner
	service   *knowledge.Service
	projector *projection.Projector
	retriever *retrieval.Retriever
	assembler *contextblock.Assembler
	policy    *policy.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	f := pgtest.NewFixture(t)
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	embedder := hashing.New()
	projector := projection.New(f.Store, embedder, projection.Options{}, nil, nil)
	service := knowledge.New(f.Store, knowledge.Options{}, nil, nil)
	retriever := retrieval.New(f.Store, embedder, retrieval.Options{Traces: f.Store}, nil, nil)

	stages := pipeline.DefaultStages(f.Store, blobs, pipeline.StageConfig{
		ChunkMaxTokens: 256, ChunkOverlapTokens: 16,
		Tokenizer: normalize.DefaultTokenizer, Projector: projector,
	})

	return &harness{
		fixture:   f,
		acme:      tenant{Tenant: f.Primary, secret: "acme"},
		globex:    tenant{Tenant: f.NewTenant(t, "globex"), secret: "globex"},
		gateway:   ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1}, nil, nil, nil),
		runner:    pipeline.NewRunner(f.Store, 1, stages, nil, nil, nil),
		service:   service,
		projector: projector,
		retriever: retriever,
		assembler: contextblock.New(retriever, f.Store, contextblock.Options{}, nil, nil),
		policy:    policy.New(f.Store, policy.NewLedgerAuditor(f.Store), policy.Options{}, nil),
	}
}

// seed puts one distinctive secret into a tenant, through the whole pipeline.
func (h *harness) seed(t *testing.T, owner tenant, text, marker string) domain.AssertionID {
	t.Helper()
	ctx := context.Background()

	receipt, err := h.gateway.Accept(ctx, ingest.Request{
		Scope:          owner.Scope(),
		Principal:      owner.Principal.Ref(),
		SourceID:       owner.Source.ID,
		MediaType:      normalize.MediaTypePlain,
		Payload:        []byte(text),
		IdempotencyKey: "seed-" + marker,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := h.runner.Process(ctx, owner.Workspace.ID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	episodes, err := h.fixture.Store.ListEpisodes(ctx, owner.Workspace.ID, receipt.SourceEventID)
	if err != nil || len(episodes) == 0 {
		t.Fatalf("episodes: %v", err)
	}

	result, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: owner.Scope(), Principal: owner.Principal.Ref(),
		SourceEventID: receipt.SourceEventID,
		Claims: []knowledge.Claim{{
			Subject:      knowledge.EntityRef{Name: marker + " Holdings", Type: "organization"},
			Predicate:    "OPERATES",
			ObjectEntity: &knowledge.EntityRef{Name: marker + " Plant", Type: "facility"},
			Evidence:     []knowledge.EvidenceInput{{EpisodeID: episodes[0].ID, ExtractedText: text}},
		}},
	})
	if err != nil {
		t.Fatalf("assert: %v", err)
	}
	if _, err := h.projector.ProjectEvent(ctx, owner.Scope(), receipt.SourceEventID); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := h.projector.ProjectEntities(ctx, owner.Scope()); err != nil {
		t.Fatalf("project entities: %v", err)
	}
	return result.Assertions[0].ID
}

// TestIntegrationOneWorkspaceCannotReachAnothersKnowledge is phase 11's acceptance criterion.
//
// Every read path, one test: lexical, vector, graph, entity, context assembly, canonical
// reads, provenance, export, and traces. The two tenants hold deliberately distinctive words
// so a leak is unmistakable rather than a judgement call, and each path is asked directly for
// the other tenant's material rather than being spot-checked.
func TestIntegrationOneWorkspaceCannotReachAnothersKnowledge(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	acmeClaim := h.seed(t, h.acme, "Zarathine Holdings operates the Kelvinbridge plant.", "Zarathine")
	globexClaim := h.seed(t, h.globex, "Marrowgate Holdings operates the Ferrisport plant.", "Marrowgate")

	// Sanity: each tenant can find its own. Without this the isolation assertions below
	// would pass on an empty database.
	if !h.canFind(t, h.acme, "Zarathine") {
		t.Fatal("a tenant cannot find its own knowledge, so this test proves nothing")
	}
	if !h.canFind(t, h.globex, "Marrowgate") {
		t.Fatal("a tenant cannot find its own knowledge, so this test proves nothing")
	}

	t.Run("lexical", func(t *testing.T) {
		h.mustNotFind(t, h.acme, "Marrowgate", domain.ModeLexical)
		h.mustNotFind(t, h.globex, "Zarathine", domain.ModeLexical)
	})

	t.Run("exact", func(t *testing.T) {
		h.mustNotFind(t, h.acme, "Marrowgate", domain.ModeExact)
	})

	t.Run("vector", func(t *testing.T) {
		h.mustNotFind(t, h.acme, "Marrowgate Holdings operates a plant", domain.ModeVector)
	})

	t.Run("entity and graph", func(t *testing.T) {
		h.mustNotFind(t, h.acme, "Marrowgate Holdings", domain.ModeEntity)
		h.mustNotFind(t, h.acme, "Marrowgate Holdings", domain.ModeGraph)

		// Graph expansion asked directly with the other tenant's entity as a root: the
		// traversal itself must refuse, not merely fail to be seeded.
		others, err := h.fixture.Store.FindEntitiesByName(ctx, h.globex.Scope(), "Marrowgate Holdings")
		if err != nil || len(others) == 0 {
			t.Fatalf("expected the other tenant's entity to exist: %v", err)
		}
		hits, err := h.fixture.Store.ExpandGraph(ctx, domain.GraphExpandQuery{
			Scope: h.acme.Scope(), Roots: []domain.EntityID{others[0].ID}, Depth: 3, Limit: 50,
		})
		if err != nil {
			t.Fatalf("expand: %v", err)
		}
		for _, hit := range hits {
			if strings.Contains(hit.Name, "Marrowgate") || strings.Contains(hit.Name, "Ferrisport") {
				t.Fatalf("graph expansion crossed a workspace: %s", hit.Name)
			}
		}
	})

	t.Run("context assembly", func(t *testing.T) {
		block, err := h.assembler.Assemble(ctx, domain.ContextRequest{
			Scope: h.acme.Scope(), Query: "Marrowgate Holdings Ferrisport", TokenBudget: 1500,
		})
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}

		// The selected items, not the whole rendering: the header echoes the caller's own
		// question back, and a caller quoting a word is not the system disclosing one.
		for _, item := range block.Items {
			if strings.Contains(item.Text, "Marrowgate") || strings.Contains(item.Text, "Ferrisport") {
				t.Fatalf("a context block leaked another workspace: %s", item.Text)
			}
		}
		for _, citation := range block.Citations {
			if strings.Contains(citation.Quote, "Marrowgate") ||
				strings.Contains(citation.Quote, "Ferrisport") {
				t.Fatalf("a citation leaked another workspace: %s", citation.Quote)
			}
		}
	})

	t.Run("canonical reads", func(t *testing.T) {
		// By id, which is the path that skips every search filter.
		if _, err := h.fixture.Store.GetAssertion(ctx, h.acme.Workspace.ID, globexClaim); err == nil {
			t.Fatal("an assertion was readable from another workspace")
		}
		if _, err := h.fixture.Store.ProvenanceChain(ctx, h.acme.Workspace.ID, globexClaim); err == nil {
			t.Fatal("provenance was walkable from another workspace")
		}
		if _, err := h.fixture.Store.GetAssertion(ctx, h.globex.Workspace.ID, acmeClaim); err == nil {
			t.Fatal("isolation is not symmetric")
		}
	})

	t.Run("export", func(t *testing.T) {
		rows, err := h.fixture.Store.QueryAssertions(ctx, domain.AssertionQuery{
			Scope: h.acme.Scope(), IncludeSuperseded: true, Limit: domain.MaxAssertionLimit,
		})
		if err != nil {
			t.Fatalf("export query: %v", err)
		}
		if len(rows) == 0 {
			t.Fatal("the export path returned nothing, so it proves nothing")
		}
		for _, row := range rows {
			if row.ID == globexClaim {
				t.Fatal("an export included another workspace's claim")
			}
			if strings.Contains(row.Object.Display(), "Ferrisport") {
				t.Fatalf("an export leaked another workspace's content: %s", row.Object.Display())
			}
		}
	})

	t.Run("traces", func(t *testing.T) {
		// Retrieval records a trace naming the records it returned, so a trace served
		// across tenants is a leak with extra steps.
		if _, err := h.retriever.Query(ctx, domain.QueryRequest{
			Scope: h.globex.Scope(), Query: "Marrowgate", Limit: 5,
		}); err != nil {
			t.Fatalf("query: %v", err)
		}

		traces, err := h.fixture.Store.ListTraces(ctx, h.globex.Scope(), 10)
		if err != nil {
			t.Fatalf("list traces: %v", err)
		}
		if len(traces) == 0 {
			t.Fatal("no trace was recorded, so this proves nothing")
		}

		if _, err := h.fixture.Store.GetTrace(ctx, h.acme.Workspace.ID, traces[0].ID); err == nil {
			t.Fatal("a trace was readable from another workspace")
		}
		// A listing is scoped too: acme must not see traces recorded for globex. Checked by
		// identity rather than by content, because acme's own traces legitimately contain
		// whatever acme typed — including another tenant's name, which discloses nothing.
		mine, err := h.fixture.Store.ListTraces(ctx, h.acme.Scope(), 50)
		if err != nil {
			t.Fatalf("list traces: %v", err)
		}
		theirs := map[domain.TraceID]struct{}{}
		for _, trace := range traces {
			theirs[trace.ID] = struct{}{}
		}
		for _, trace := range mine {
			if _, leaked := theirs[trace.ID]; leaked {
				t.Fatal("another workspace's trace appeared in this workspace's listing")
			}
			if trace.GraphSpaceID != h.acme.GraphSpace.ID {
				t.Fatalf("a trace from graph space %s appeared in acme's listing",
					trace.GraphSpaceID)
			}
			for _, ref := range trace.SelectedRefs {
				if ref.RecordID == string(globexClaim) {
					t.Fatal("a trace named another workspace's record")
				}
			}
		}
	})
}

// canFind reports whether a tenant's own retrieval returns a marker.
func (h *harness) canFind(t *testing.T, who tenant, term string) bool {
	t.Helper()

	result, err := h.retriever.Query(context.Background(), domain.QueryRequest{
		Scope: who.Scope(), Query: term, Limit: 20,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, item := range result.Items {
		if strings.Contains(item.Content, term) {
			return true
		}
	}
	return false
}

// mustNotFind fails when one retriever returns another tenant's material.
func (h *harness) mustNotFind(t *testing.T, who tenant, term string, mode domain.RetrievalMode) {
	t.Helper()

	result, err := h.retriever.Query(context.Background(), domain.QueryRequest{
		Scope: who.Scope(), Query: term, Modes: []domain.RetrievalMode{mode}, Limit: 50,
	})
	if err != nil {
		t.Fatalf("%s query: %v", mode, err)
	}
	for _, item := range result.Items {
		for _, leak := range []string{"Marrowgate", "Ferrisport", "Zarathine", "Kelvinbridge"} {
			if !strings.Contains(item.Content, leak) {
				continue
			}
			// The tenant's own words are fine; the other tenant's are not.
			if (who.Slug() == "acme" && (leak == "Marrowgate" || leak == "Ferrisport")) ||
				(who.Slug() == "globex" && (leak == "Zarathine" || leak == "Kelvinbridge")) {
				t.Fatalf("%s retrieval leaked %q into %s: %s", mode, leak, who.Slug(), item.Content)
			}
		}
	}
}
