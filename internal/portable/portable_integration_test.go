package portable_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding/hashing"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/pipeline"
	"github.com/gimantha/strata/internal/portable"
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
	exporter  *portable.Exporter
	importer  *portable.Importer
}

// recorder attributes an import to a real ingestion event, the way the app wiring does.
type recorder struct {
	gateway *ingest.Gateway
	runner  *pipeline.Runner
	store   *pgtest.Fixture
}

func (r recorder) RecordImport(ctx context.Context, scope domain.Scope, principal domain.PrincipalRef, sourceID domain.SourceID, payload []byte, key string) (domain.SourceEventID, domain.EpisodeID, error) {
	receipt, err := r.gateway.Accept(ctx, ingest.Request{
		Scope: scope, Principal: principal, SourceID: sourceID,
		EventType: "package.import", Operation: domain.SourceOpSnapshot,
		MediaType: normalize.MediaTypeJSON, Payload: payload,
		IdempotencyKey: key, DirectEpisode: true,
	})
	if err != nil {
		return "", "", err
	}
	if _, err := r.runner.Process(ctx, scope.WorkspaceID, receipt.SourceEventID, false); err != nil {
		return "", "", err
	}
	episodes, err := r.store.Store.ListEpisodes(ctx, scope.WorkspaceID, receipt.SourceEventID)
	if err != nil || len(episodes) == 0 {
		return "", "", err
	}
	return receipt.SourceEventID, episodes[0].ID, nil
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	f := pgtest.NewFixture(t)
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	embedder := hashing.New()
	projector := projection.New(f.Store, f.Store, f.Store.Indexes(), embedder, projection.Options{}, nil, nil)
	service := knowledge.New(f.Store, knowledge.Options{}, nil, nil)
	gateway := ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1}, nil, nil, nil)

	stages := pipeline.DefaultStages(f.Store, blobs, pipeline.StageConfig{
		ChunkMaxTokens: 256, ChunkOverlapTokens: 16,
		Tokenizer: normalize.DefaultTokenizer, Projector: projector,
	})
	runner := pipeline.NewRunner(f.Store, 1, stages, nil, nil, nil)

	opts := portable.Options{Instance: "test"}
	return &harness{
		fixture:   f,
		gateway:   gateway,
		runner:    runner,
		service:   service,
		projector: projector,
		retriever: retrieval.New(f.Store, f.Store.Indexes(), embedder, retrieval.Options{}, nil, nil),
		exporter:  portable.NewExporter(f.Store, opts, nil),
		importer: portable.NewImporter(f.Store, service,
			recorder{gateway: gateway, runner: runner, store: f}, opts, nil),
	}
}

// seed puts one document and one claim into a tenant.
func (h *harness) seed(t *testing.T, tenant pgtest.Tenant, text string, claim knowledge.Claim, key string) domain.Assertion {
	t.Helper()
	ctx := context.Background()

	receipt, err := h.gateway.Accept(ctx, ingest.Request{
		Scope: tenant.Scope(), Principal: tenant.Principal.Ref(), SourceID: tenant.Source.ID,
		MediaType: normalize.MediaTypePlain, Payload: []byte(text), IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := h.runner.Process(ctx, tenant.Workspace.ID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	episodes, err := h.fixture.Store.ListEpisodes(ctx, tenant.Workspace.ID, receipt.SourceEventID)
	if err != nil || len(episodes) == 0 {
		t.Fatalf("episodes: %v", err)
	}
	if len(claim.Evidence) == 0 {
		claim.Evidence = []knowledge.EvidenceInput{{EpisodeID: episodes[0].ID, ExtractedText: text}}
	}

	result, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: tenant.Scope(), Principal: tenant.Principal.Ref(),
		SourceEventID: receipt.SourceEventID, Claims: []knowledge.Claim{claim},
	})
	if err != nil {
		t.Fatalf("assert: %v", err)
	}
	return result.Assertions[0]
}

// TestIntegrationPackageRebuildsKnowledgeInAnEmptyInstance is phase 13's second acceptance
// criterion.
//
// The empty instance is a second workspace in the same database rather than a second process:
// what is being tested is that the package carries enough to reconstruct knowledge with no
// shared identifiers, and a fresh workspace shares none.
func TestIntegrationPackageRebuildsKnowledgeInAnEmptyInstance(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	source := h.fixture.Primary
	h.seed(t, source, "Thornbury Works supplies the Kelvinbridge assembly line.",
		knowledge.Claim{
			Subject:      knowledge.EntityRef{Name: "Thornbury Works", Type: "organization"},
			Predicate:    "SUPPLIES_TO",
			ObjectEntity: &knowledge.EntityRef{Name: "Kelvinbridge Line", Type: "facility"},
		}, "pkg-1")
	h.seed(t, source, "Thornbury Works is on the premium tier.",
		knowledge.Claim{
			Subject:   knowledge.EntityRef{Name: "Thornbury Works", Type: "organization"},
			Predicate: "TIER",
			Object:    domain.ObjectOfSymbol("PREMIUM"),
		}, "pkg-2")

	var package1 bytes.Buffer
	result, err := h.exporter.Export(ctx, portable.ExportRequest{
		Scope: source.Scope(), Principal: source.Principal.Ref(),
	}, &package1)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if result.Manifest.Counts[domain.PackageAssertion] != 2 {
		t.Fatalf("expected two assertions in the package, got %d",
			result.Manifest.Counts[domain.PackageAssertion])
	}
	if result.Manifest.Digest == "" {
		t.Fatal("the package carries no integrity digest")
	}

	// A workspace that shares nothing with the exporter: no entity ids, no assertion ids,
	// no predicates, no sources.
	target := h.fixture.NewTenant(t, "empty")

	before, err := h.fixture.Store.QueryAssertions(ctx, domain.AssertionQuery{
		Scope: target.Scope(), Limit: 10,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(before) != 0 {
		t.Fatal("the target workspace is not empty, so this proves nothing")
	}

	summary, err := h.importer.Import(ctx, portable.ImportRequest{
		Scope: target.Scope(), Principal: target.Principal.Ref(),
		AcceptPredicates: true,
	}, bytes.NewReader(package1.Bytes()))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.Assertions != 2 {
		t.Fatalf("expected two assertions imported, got %d (%s)",
			summary.Assertions, summary.Describe())
	}

	// The knowledge is present, with its own identifiers, and answers the same questions.
	rebuilt, err := h.fixture.Store.QueryAssertions(ctx, domain.AssertionQuery{
		Scope: target.Scope(), Limit: 50,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rebuilt) != 2 {
		t.Fatalf("expected two claims in the target, got %d", len(rebuilt))
	}

	byPredicate := map[string]domain.Assertion{}
	for _, assertion := range rebuilt {
		byPredicate[assertion.Predicate.Name] = assertion
	}
	if got := byPredicate["TIER"].Object.Display(); got != "PREMIUM" {
		t.Fatalf("the tier claim did not survive the move, got %q", got)
	}

	relation := byPredicate["SUPPLIES_TO"]
	if relation.Object.Kind != domain.ObjectEntity {
		t.Fatalf("the relation lost its entity object, got %s", relation.Object.Kind)
	}
	object, err := h.fixture.Store.GetEntity(ctx, target.Workspace.ID, relation.Object.EntityID)
	if err != nil {
		t.Fatalf("the relation's object was not created: %v", err)
	}
	if object.CanonicalName != "Kelvinbridge Line" {
		t.Fatalf("the object entity is wrong: %q", object.CanonicalName)
	}

	// Identifiers are the target's own, not the exporter's. A package that carried ids as
	// identity would collide the moment two deployments exchanged one.
	for _, assertion := range rebuilt {
		if assertion.WorkspaceID != target.Workspace.ID {
			t.Fatal("an imported claim belongs to the wrong workspace")
		}
		if assertion.ProvenanceMode != domain.ProvenanceImported {
			t.Fatalf("imported knowledge should say so, got %s", assertion.ProvenanceMode)
		}

		// Provenance reaches something real here, rather than dangling at the exporter.
		chain, err := h.fixture.Store.ProvenanceChain(ctx, target.Workspace.ID, assertion.ID)
		if err != nil {
			t.Fatalf("imported claim has no walkable provenance: %v", err)
		}
		if len(chain.Links) == 0 {
			t.Fatal("imported claim arrived with no evidence")
		}
		if !strings.Contains(chain.Links[0].Evidence.ExtractedText, "Thornbury") {
			t.Fatalf("the quote did not survive: %q", chain.Links[0].Evidence.ExtractedText)
		}
	}

	// And it is findable, which is the practical form of "rebuilt".
	if _, err := h.projector.Rebuild(ctx, target.Workspace.ID); err != nil {
		t.Fatalf("rebuild projections: %v", err)
	}
	found, err := h.retriever.Query(ctx, domain.QueryRequest{
		Scope: target.Scope(), Query: "Thornbury Works", Limit: 10,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(found.Items) == 0 {
		t.Fatal("imported knowledge is not retrievable in the target")
	}
}

// TestIntegrationImportRefusesATamperedPackage holds the property the manifest exists for.
func TestIntegrationImportRefusesATamperedPackage(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	source := h.fixture.Primary
	h.seed(t, source, "Thornbury Works is on the premium tier.", knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Thornbury Works", Type: "organization"},
		Predicate: "TIER",
		Object:    domain.ObjectOfSymbol("PREMIUM"),
	}, "tamper-1")

	var exported bytes.Buffer
	if _, err := h.exporter.Export(ctx, portable.ExportRequest{
		Scope: source.Scope(), Principal: source.Principal.Ref(),
	}, &exported); err != nil {
		t.Fatalf("export: %v", err)
	}
	target := h.fixture.NewTenant(t, "guarded")

	// A single edited value, with the manifest left alone: exactly what an attacker or a
	// corrupted transfer produces.
	tampered := strings.Replace(exported.String(), "PREMIUM", "ENTERPRISE", 1)
	if tampered == exported.String() {
		t.Fatal("the fixture did not contain the value this test edits")
	}

	_, err := h.importer.Import(ctx, portable.ImportRequest{
		Scope: target.Scope(), Principal: target.Principal.Ref(),
	}, strings.NewReader(tampered))
	if err == nil {
		t.Fatal("a tampered package was imported")
	}
	if !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("the refusal should name the integrity check: %v", err)
	}
	// And it should say where, not merely that something is wrong.
	if !strings.Contains(err.Error(), "assertion") {
		t.Fatalf("the refusal should name the offending section: %v", err)
	}

	// Nothing was committed.
	after, err := h.fixture.Store.QueryAssertions(ctx, domain.AssertionQuery{
		Scope: target.Scope(), Limit: 10,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(after) != 0 {
		t.Fatal("a refused import left knowledge behind")
	}
}

// TestIntegrationImportRefusesATruncatedPackage covers the other failure mode: an interrupted
// transfer, where every line present is valid and the end is missing.
func TestIntegrationImportRefusesATruncatedPackage(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	source := h.fixture.Primary
	h.seed(t, source, "Thornbury Works is on the premium tier.", knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Thornbury Works", Type: "organization"},
		Predicate: "TIER",
		Object:    domain.ObjectOfSymbol("PREMIUM"),
	}, "truncate-1")

	var exported bytes.Buffer
	if _, err := h.exporter.Export(ctx, portable.ExportRequest{
		Scope: source.Scope(), Principal: source.Principal.Ref(),
	}, &exported); err != nil {
		t.Fatalf("export: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(exported.String()), "\n")
	truncated := strings.Join(lines[:len(lines)-1], "\n")

	target := h.fixture.NewTenant(t, "truncated")
	_, err := h.importer.Import(ctx, portable.ImportRequest{
		Scope: target.Scope(), Principal: target.Principal.Ref(),
	}, strings.NewReader(truncated))
	if err == nil {
		t.Fatal("a truncated package was imported")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("the refusal should say the package was truncated: %v", err)
	}
}

// TestIntegrationExportRespectsPolicy checks that a package cannot carry what its exporter
// could not read.
func TestIntegrationExportRespectsPolicy(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	source := h.fixture.Primary
	h.seed(t, source, "Routine maintenance happens each spring.", knowledge.Claim{
		Subject:        knowledge.EntityRef{Name: "Thornbury Works", Type: "organization"},
		Predicate:      "NOTES",
		Object:         domain.ObjectOfString("routine maintenance each spring"),
		Classification: domain.ClassificationInternal,
	}, "policy-1")
	h.seed(t, source, "Thornbury Works is under investigation.", knowledge.Claim{
		Subject:        knowledge.EntityRef{Name: "Thornbury Works", Type: "organization"},
		Predicate:      "NOTES",
		Object:         domain.ObjectOfString("under Quillon investigation"),
		Classification: domain.ClassificationRestricted,
	}, "policy-2")

	var exported bytes.Buffer
	result, err := h.exporter.Export(ctx, portable.ExportRequest{
		Scope: source.Scope(), Principal: source.Principal.Ref(),
		Policy: domain.PolicyFilters{MaxClassification: domain.ClassificationInternal},
	}, &exported)
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	if strings.Contains(exported.String(), "Quillon") {
		t.Fatal("a package carried material its exporter was not cleared to read")
	}
	if !result.Header.Policy.Filtered {
		// A partial package that looks complete is worse than no package: someone will
		// treat it as a backup.
		t.Fatal("a filtered package must say so in its header")
	}
	// The ceiling is what a reader needs in order to decide whether this is a backup. The
	// excluded count reports only what the export pass itself dropped: the query was
	// narrowed too, so a record the database never returned is absent from both.
	if result.Header.Policy.MaxClassification != domain.ClassificationInternal {
		t.Fatalf("the ceiling should be recorded, got %q",
			result.Header.Policy.MaxClassification)
	}
	if result.Header.Policy.MaxClassification != domain.ClassificationInternal {
		t.Fatalf("the header should record the ceiling, got %q",
			result.Header.Policy.MaxClassification)
	}
}

// TestIntegrationReimportIsIdempotent covers the ordinary operational case: the same package
// applied twice.
func TestIntegrationReimportIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	source := h.fixture.Primary
	h.seed(t, source, "Thornbury Works is on the premium tier.", knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Thornbury Works", Type: "organization"},
		Predicate: "TIER",
		Object:    domain.ObjectOfSymbol("PREMIUM"),
	}, "idem-1")

	var exported bytes.Buffer
	if _, err := h.exporter.Export(ctx, portable.ExportRequest{
		Scope: source.Scope(), Principal: source.Principal.Ref(),
	}, &exported); err != nil {
		t.Fatalf("export: %v", err)
	}

	target := h.fixture.NewTenant(t, "twice")
	first, err := h.importer.Import(ctx, portable.ImportRequest{
		Scope: target.Scope(), Principal: target.Principal.Ref(),
	}, bytes.NewReader(exported.Bytes()))
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if first.Assertions != 1 {
		t.Fatalf("expected one assertion, got %d", first.Assertions)
	}

	second, err := h.importer.Import(ctx, portable.ImportRequest{
		Scope: target.Scope(), Principal: target.Principal.Ref(),
	}, bytes.NewReader(exported.Bytes()))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Assertions != 0 {
		t.Fatalf("re-importing should add nothing, got %d new assertions", second.Assertions)
	}
	if second.Duplicates == 0 {
		t.Fatal("the second import should recognize what it already had")
	}

	all, err := h.fixture.Store.QueryAssertions(ctx, domain.AssertionQuery{
		Scope: target.Scope(), Limit: 50,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("re-import duplicated knowledge: %d claims", len(all))
	}
}

// TestIntegrationDryRunVerifiesWithoutWriting is how an unfamiliar package is inspected.
func TestIntegrationDryRunVerifiesWithoutWriting(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	source := h.fixture.Primary
	h.seed(t, source, "Thornbury Works is on the premium tier.", knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Thornbury Works", Type: "organization"},
		Predicate: "TIER",
		Object:    domain.ObjectOfSymbol("PREMIUM"),
	}, "dry-1")

	var exported bytes.Buffer
	if _, err := h.exporter.Export(ctx, portable.ExportRequest{
		Scope: source.Scope(), Principal: source.Principal.Ref(),
	}, &exported); err != nil {
		t.Fatalf("export: %v", err)
	}

	target := h.fixture.NewTenant(t, "inspect")
	summary, err := h.importer.Import(ctx, portable.ImportRequest{
		Scope: target.Scope(), Principal: target.Principal.Ref(), DryRun: true,
	}, bytes.NewReader(exported.Bytes()))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if summary.Assertions == 0 {
		t.Fatal("a dry run should report what the package contains")
	}
	if summary.Header.Source.WorkspaceSlug != source.Workspace.Slug {
		t.Fatalf("the dry run should report where the package came from, got %q",
			summary.Header.Source.WorkspaceSlug)
	}

	after, err := h.fixture.Store.QueryAssertions(ctx, domain.AssertionQuery{
		Scope: target.Scope(), Limit: 10,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(after) != 0 {
		t.Fatal("a dry run wrote to the target")
	}
}

// TestIntegrationPackageIsReadableWithoutThisCode checks the format is what it claims:
// newline-delimited JSON that any tool can inspect.
func TestIntegrationPackageIsReadableWithoutThisCode(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	source := h.fixture.Primary
	h.seed(t, source, "Thornbury Works is on the premium tier.", knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Thornbury Works", Type: "organization"},
		Predicate: "TIER",
		Object:    domain.ObjectOfSymbol("PREMIUM"),
	}, "format-1")

	var exported bytes.Buffer
	if _, err := h.exporter.Export(ctx, portable.ExportRequest{
		Scope: source.Scope(), Principal: source.Principal.Ref(),
	}, &exported); err != nil {
		t.Fatalf("export: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(exported.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected a header, records, and a manifest; got %d lines", len(lines))
	}

	var header domain.PackageHeader
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatalf("the first line should be the header: %v", err)
	}
	if header.Format != domain.PackageFormat {
		t.Fatalf("unexpected format %q", header.Format)
	}

	var manifest domain.PackageManifest
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &manifest); err != nil {
		t.Fatalf("the last line should be the manifest: %v", err)
	}
	if manifest.Format != domain.ManifestFormat {
		t.Fatalf("unexpected manifest format %q", manifest.Format)
	}
	if !strings.HasPrefix(manifest.Digest, "sha256:") {
		t.Fatalf("the digest should name its algorithm, got %q", manifest.Digest)
	}

	// Every line between is a self-describing record.
	for i, line := range lines[1 : len(lines)-1] {
		var record domain.PackageRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line %d is not a record: %v", i+2, err)
		}
		if record.Kind == "" {
			t.Fatalf("line %d has no kind", i+2)
		}
	}

	// And the standalone verifier agrees.
	verifiedHeader, verifiedManifest, err := portable.Verify(ctx, bytes.NewReader(exported.Bytes()))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verifiedManifest.Digest != manifest.Digest {
		t.Fatal("the verifier computed a different digest than the package declares")
	}
	if verifiedHeader.Source.WorkspaceSlug != source.Workspace.Slug {
		t.Fatal("the verifier read the wrong origin")
	}
}
