package ontology_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding/hashing"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/ontology"
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
	ontology  *ontology.Service
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

	embedder := hashing.New()
	projector := projection.New(f.Store, embedder, projection.Options{}, nil, nil)
	stages := pipeline.DefaultStages(f.Store, blobs, pipeline.StageConfig{
		ChunkMaxTokens: 256, ChunkOverlapTokens: 16,
		Tokenizer: normalize.DefaultTokenizer, Projector: projector,
	})

	return &harness{
		fixture:   f,
		gateway:   ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1}, nil, nil, nil),
		runner:    pipeline.NewRunner(f.Store, 1, stages, nil, nil, nil),
		service:   knowledge.New(f.Store, knowledge.Options{}, nil, nil),
		ontology:  ontology.New(f.Store, nil),
		projector: projector,
		retriever: retrieval.New(f.Store, embedder, retrieval.Options{}, nil, nil),
	}
}

func (h *harness) scope() domain.Scope { return h.fixture.Primary.Scope() }

func (h *harness) ingest(t *testing.T, scope domain.Scope, text, key string) domain.SourceEventID {
	t.Helper()
	ctx := context.Background()

	receipt, err := h.gateway.Accept(ctx, ingest.Request{
		Scope:          scope,
		Principal:      h.fixture.Primary.Principal.Ref(),
		SourceID:       h.fixture.Primary.Source.ID,
		MediaType:      normalize.MediaTypePlain,
		Payload:        []byte(text),
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", key, err)
	}
	if _, err := h.runner.Process(ctx, scope.WorkspaceID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process %s: %v", key, err)
	}
	return receipt.SourceEventID
}

// supplyChainSchema is a small, realistic ontology.
func supplyChainSchema() ontology.DefineRequest {
	return ontology.DefineRequest{
		Name: "supply chain v1",
		EntityTypes: []domain.EntityTypeDef{
			{Name: "organization", Aliases: []string{"company", "org"}},
			{Name: "facility"},
		},
		Predicates: []domain.PredicateConstraint{
			{
				Name:         "SUPPLIES_TO",
				SubjectTypes: []string{"organization"},
				ObjectTypes:  []string{"facility"},
				ObjectKinds:  []domain.ObjectKind{domain.ObjectEntity},
			},
			{
				Name:          "TIER",
				SubjectTypes:  []string{"organization"},
				ObjectKinds:   []domain.ObjectKind{domain.ObjectSymbol},
				AllowedValues: []string{"PREMIUM", "STANDARD"},
			},
		},
		Activate:           true,
		RegisterPredicates: true,
	}
}

// TestIntegrationSameSourceInOpenAndGuidedMode is phase 9's first acceptance criterion.
//
// One document, two graph spaces, two modes. Open mode records what the source said; guided
// mode records what the schema recognizes and holds the rest. Neither mode loses the claim,
// and the difference is visible rather than implicit.
func TestIntegrationSameSourceInOpenAndGuidedMode(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	open := h.scope()
	guided := h.fixture.NewGraphSpace(t, "guided").Scope()

	version, err := h.ontology.Define(ctx, withScope(supplyChainSchema(), open, h.fixture.Primary.Principal.Ref()))
	if err != nil {
		t.Fatalf("define: %v", err)
	}
	if err := h.ontology.Bind(ctx, guided, domain.OntologyGuided, &version.ID); err != nil {
		t.Fatalf("bind: %v", err)
	}

	const document = "Acme Corporation supplies the Portland plant and is rumored to be for sale."

	// The same claims either way: one the schema describes, one it does not.
	claims := func() []knowledge.Claim {
		return []knowledge.Claim{
			{
				Subject:      knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
				Predicate:    "SUPPLIES_TO",
				ObjectEntity: &knowledge.EntityRef{Name: "Portland Plant", Type: "facility"},
			},
			{
				Subject:   knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
				Predicate: "RUMORED_FOR_SALE",
				Object:    domain.ObjectOfBool(true),
			},
		}
	}

	openEvent := h.ingest(t, open, document, "mode-open")
	openResult, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: open, Principal: h.fixture.Primary.Principal.Ref(),
		SourceEventID: openEvent, Claims: claims(),
	})
	if err != nil {
		t.Fatalf("open mode refused a claim: %v", err)
	}
	if len(openResult.Assertions) != 2 {
		t.Fatalf("open mode should record both claims, got %d", len(openResult.Assertions))
	}
	if len(openResult.Quarantined) != 0 {
		t.Fatal("open mode has nothing to validate against and must quarantine nothing")
	}

	guidedEvent := h.ingest(t, guided, document, "mode-guided")
	guidedResult, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: guided, Principal: h.fixture.Primary.Principal.Ref(),
		SourceEventID: guidedEvent, Claims: claims(),
		OnViolation: domain.DispositionQuarantine,
	})
	if err != nil {
		t.Fatalf("guided mode: %v", err)
	}
	if len(guidedResult.Assertions) != 2 {
		t.Fatalf("guided mode should still record both claims, got %d", len(guidedResult.Assertions))
	}
	if len(guidedResult.Quarantined) != 1 {
		t.Fatalf("exactly the unrecognized claim should be held, got %d", len(guidedResult.Quarantined))
	}

	// The conforming claim went through, and it names the schema that checked it.
	var conforming, held domain.Assertion
	for _, assertion := range guidedResult.Assertions {
		if assertion.Status == domain.AssertionQuarantined {
			held = assertion
		} else {
			conforming = assertion
		}
	}
	if conforming.Predicate.Name != "SUPPLIES_TO" {
		t.Fatalf("the schema-conforming claim should be active, got %q", conforming.Predicate.Name)
	}
	if conforming.OntologyVersionID == nil || *conforming.OntologyVersionID != version.ID {
		t.Fatal("a validated claim must record the version that validated it")
	}
	if held.Predicate.Name != "RUMORED_FOR_SALE" {
		t.Fatalf("the unrecognized claim should be the held one, got %q", held.Predicate.Name)
	}
	if !strings.Contains(held.QuarantineReason, string(domain.ViolationUnknownPredicate)) {
		t.Fatalf("a held claim must say why: %q", held.QuarantineReason)
	}
}

// TestIntegrationInvalidCandidatesAreNeverSilentlyCommitted is the second acceptance
// criterion, in both dispositions.
func TestIntegrationInvalidCandidatesAreNeverSilentlyCommitted(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	guided := h.fixture.NewGraphSpace(t, "strict").Scope()
	version, err := h.ontology.Define(ctx,
		withScope(supplyChainSchema(), guided, h.fixture.Primary.Principal.Ref()))
	if err != nil {
		t.Fatalf("define: %v", err)
	}
	if err := h.ontology.Bind(ctx, guided, domain.OntologyGuided, &version.ID); err != nil {
		t.Fatalf("bind: %v", err)
	}

	event := h.ingest(t, guided, "Acme Corporation is on the platinum tier.", "strict-1")
	badValue := []knowledge.Claim{{
		Subject:   knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
		Predicate: "TIER",
		Object:    domain.ObjectOfSymbol("PLATINUM"),
	}}

	// A caller stating a claim gets it back with the reason, because a caller can fix it.
	_, err = h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: guided, Principal: h.fixture.Primary.Principal.Ref(),
		SourceEventID: event, Claims: badValue,
	})
	if err == nil {
		t.Fatal("a value outside the closed vocabulary must not be committed silently")
	}
	if domain.CodeOf(err) != domain.CodeOntologyViolation {
		t.Fatalf("expected ontology_violation, got %s: %v", domain.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "PLATINUM") {
		t.Fatalf("the error must name the offending value: %v", err)
	}

	// A model proposing the same claim has no such loop, so it is held instead.
	result, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: guided, Principal: h.fixture.Primary.Principal.Ref(),
		SourceEventID: event, Claims: badValue,
		OnViolation: domain.DispositionQuarantine,
	})
	if err != nil {
		t.Fatalf("quarantine disposition should commit and hold: %v", err)
	}
	if len(result.Quarantined) != 1 {
		t.Fatalf("expected one held claim, got %d", len(result.Quarantined))
	}

	// Held is not the same as believed. It must not reach retrieval.
	if _, err := h.projector.ProjectEvent(ctx, guided, event); err != nil {
		t.Fatalf("project: %v", err)
	}
	found, err := h.retriever.Query(ctx, domain.QueryRequest{
		Scope: guided, Query: "platinum tier", Limit: 10,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, item := range found.Items {
		if item.Surface == domain.SurfaceAssertion &&
			item.RecordID == string(result.Quarantined[0].AssertionID) {
			t.Fatal("a quarantined claim was retrievable, which is indistinguishable from believed")
		}
	}
}

// TestIntegrationValidateReportsWithoutChangingAnything covers the migration tool: the
// useful question about a schema change is what it would refuse, answered before it is bound.
func TestIntegrationValidateReportsWithoutChangingAnything(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	open := h.scope()
	event := h.ingest(t, open, "Acme Corporation supplies the Portland plant.", "validate-1")

	// Committed in open mode, before any schema exists.
	before, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: open, Principal: h.fixture.Primary.Principal.Ref(), SourceEventID: event,
		Claims: []knowledge.Claim{
			{
				Subject:      knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
				Predicate:    "SUPPLIES_TO",
				ObjectEntity: &knowledge.EntityRef{Name: "Portland Plant", Type: "facility"},
			},
			{
				Subject:   knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
				Predicate: "MOOD",
				Object:    domain.ObjectOfString("optimistic"),
			},
		},
	})
	if err != nil {
		t.Fatalf("assert: %v", err)
	}

	version, err := h.ontology.Define(ctx, withScope(supplyChainSchema(), open, h.fixture.Primary.Principal.Ref()))
	if err != nil {
		t.Fatalf("define: %v", err)
	}

	report, err := h.ontology.Validate(ctx, open, version.ID, 0)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if report.Checked < 2 {
		t.Fatalf("expected both claims checked, got %d", report.Checked)
	}
	if report.Conforming != 1 || len(report.Violations) != 1 {
		t.Fatalf("expected one conforming and one violating, got %d and %d",
			report.Conforming, len(report.Violations))
	}
	if report.ByCode[domain.ViolationUnknownPredicate] != 1 {
		t.Fatalf("the unknown predicate should be counted: %v", report.ByCode)
	}

	// Nothing moved. A report that quietly quarantined things would be a trap.
	for _, assertion := range before.Assertions {
		current, err := h.fixture.Store.GetAssertion(ctx, open.WorkspaceID, assertion.ID)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if current.Status != assertion.Status {
			t.Fatalf("validation changed a claim's status from %s to %s",
				assertion.Status, current.Status)
		}
		if current.OntologyVersionID != nil {
			t.Fatal("validation retroactively stamped a claim with a version it never saw")
		}
	}
}

// TestIntegrationOntologyVersionsAreImmutableAndSequenced holds the property assertions
// depend on: the version a claim names must still describe what it was checked against.
func TestIntegrationOntologyVersionsAreImmutableAndSequenced(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	scope := h.scope()
	first, err := h.ontology.Define(ctx, withScope(supplyChainSchema(), scope, h.fixture.Primary.Principal.Ref()))
	if err != nil {
		t.Fatalf("define: %v", err)
	}

	second := supplyChainSchema()
	second.Name = "supply chain v2"
	second.Predicates = append(second.Predicates, domain.PredicateConstraint{Name: "MOOD"})
	created, err := h.ontology.Define(ctx, withScope(second, scope, h.fixture.Primary.Principal.Ref()))
	if err != nil {
		t.Fatalf("define second: %v", err)
	}

	if created.Version != first.Version+1 {
		t.Fatalf("versions must be sequential: %d then %d", first.Version, created.Version)
	}

	// The first version still says what it always said.
	reloaded, err := h.ontology.Get(ctx, scope.WorkspaceID, first.ID)
	if err != nil {
		t.Fatalf("reload first: %v", err)
	}
	if len(reloaded.Predicates) != len(first.Predicates) {
		t.Fatal("an earlier version changed when a later one was defined")
	}
	if reloaded.Status != domain.OntologySuperseded {
		t.Fatalf("activating v2 should supersede v1, got %s", reloaded.Status)
	}
}

// TestIntegrationDraftVersionsCannotBeBound guards against putting claims under a schema
// nobody has committed to.
func TestIntegrationDraftVersionsCannotBeBound(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	scope := h.scope()
	draft := supplyChainSchema()
	draft.Activate = false

	version, err := h.ontology.Define(ctx, withScope(draft, scope, h.fixture.Primary.Principal.Ref()))
	if err != nil {
		t.Fatalf("define: %v", err)
	}
	if version.Status != domain.OntologyDraft {
		t.Fatalf("expected a draft, got %s", version.Status)
	}

	if err := h.ontology.Bind(ctx, scope, domain.OntologyGuided, &version.ID); err == nil {
		t.Fatal("a draft must not be bindable")
	}
	if err := h.ontology.Bind(ctx, scope, domain.OntologyGuided, nil); err == nil {
		// Guided with no version validates against nothing and accepts everything, which
		// is the failure that looks most like success.
		t.Fatal("guided mode without a version must be refused")
	}
}

// TestIntegrationTypeConstrainedRetrieval covers the last deliverable: a query can ask for
// one kind of thing (AGENTS.md section 19.2).
func TestIntegrationTypeConstrainedRetrieval(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	scope := h.scope()
	event := h.ingest(t, scope, "Portland is both a plant and a person's name here.", "types-1")

	if _, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: scope, Principal: h.fixture.Primary.Principal.Ref(), SourceEventID: event,
		Claims: []knowledge.Claim{
			{
				Subject:   knowledge.EntityRef{Name: "Portland Plant", Type: "facility"},
				Predicate: "NOTES",
				Object:    domain.ObjectOfString("assembles fasteners"),
			},
			{
				Subject:   knowledge.EntityRef{Name: "Portland Reed", Type: "person"},
				Predicate: "NOTES",
				Object:    domain.ObjectOfString("assembles fasteners"),
			},
		},
	}); err != nil {
		t.Fatalf("assert: %v", err)
	}
	if _, err := h.projector.ProjectEvent(ctx, scope, event); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := h.projector.ProjectEntities(ctx, scope); err != nil {
		t.Fatalf("project entities: %v", err)
	}

	unfiltered, err := h.retriever.Query(ctx, domain.QueryRequest{
		Scope: scope, Query: "assembles fasteners", Limit: 20,
		Filters: domain.QueryFilters{Surfaces: []domain.Surface{domain.SurfaceAssertion}},
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(unfiltered.Items) < 2 {
		t.Fatalf("both claims should be findable without a type filter, got %d", len(unfiltered.Items))
	}

	filtered, err := h.retriever.Query(ctx, domain.QueryRequest{
		Scope: scope, Query: "assembles fasteners", Limit: 20,
		Filters: domain.QueryFilters{
			Surfaces:    []domain.Surface{domain.SurfaceAssertion},
			EntityTypes: []string{"Facility"},
		},
	})
	if err != nil {
		t.Fatalf("filtered query: %v", err)
	}
	if len(filtered.Items) == 0 {
		t.Fatal("the type filter removed everything")
	}
	if len(filtered.Items) >= len(unfiltered.Items) {
		t.Fatalf("the type filter changed nothing: %d of %d",
			len(filtered.Items), len(unfiltered.Items))
	}
}

func withScope(req ontology.DefineRequest, scope domain.Scope, principal domain.PrincipalRef) ontology.DefineRequest {
	req.Scope = scope
	req.Principal = principal
	return req
}
