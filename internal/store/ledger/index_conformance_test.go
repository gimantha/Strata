package ledger_test

import (
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding/hashing"
	"github.com/gimantha/strata/internal/store/index"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

// TestIntegrationPostgresMeetsTheIndexContracts runs the conformance suites against the
// reference implementation.
//
// PostgreSQL runs them too, not only a future substitute. A suite written after the fact and
// run only by the newcomer encodes whatever its author assumed the incumbent did; running it
// against the incumbent is what makes it a contract rather than a description.
func TestIntegrationPostgresMeetsTheIndexContracts(t *testing.T) {
	f := pgtest.NewFixture(t)
	other := f.NewTenant(t, "conformance-other")

	embedder := hashing.New()
	fixture := index.Fixture{
		Primary: f.Primary.Scope(),
		Other:   other.Scope(),
		Model:   embedder.Model(),
		Version: embedder.Version(),
		Embed: func(tb testing.TB, text string) []float32 {
			tb.Helper()
			vectors, err := embedder.Embed(tb.Context(), []string{text})
			if err != nil {
				tb.Fatalf("embed: %v", err)
			}
			return vectors[0]
		},
	}

	indexes := f.Store.Indexes()
	index.RunVectorConformance(t, "postgres", indexes.Vectors, fixture)
	index.RunLexicalConformance(t, "postgres", indexes.Lexical, fixture)
}

// TestIntegrationPostgresMeetsTheVectorFilterContract runs the filter suite against the
// reference.
//
// Separate from the shape suite because it needs more fixture: real sources and collections
// to narrow by, which PostgreSQL's foreign keys require to exist. Run against the incumbent
// for the same reason as everything else here — a suite only the newcomer runs describes
// what its author assumed rather than what the system does.
func TestIntegrationPostgresMeetsTheVectorFilterContract(t *testing.T) {
	f := pgtest.NewFixture(t)
	other := f.NewTenant(t, "filter-other")
	ctx := t.Context()

	source := func(name string) domain.SourceID {
		t.Helper()
		created, err := f.Store.CreateSource(ctx, domain.Source{
			WorkspaceID: f.Primary.Workspace.ID, Kind: domain.SourceKindDocument,
			Name: name, TrustLevel: domain.TrustStandard,
		}, f.Primary.Principal.ID)
		if err != nil {
			t.Fatalf("create source %s: %v", name, err)
		}
		return created.ID
	}
	collection := func(slug string) domain.CollectionID {
		t.Helper()
		created, err := f.Store.CreateCollection(ctx, domain.Collection{
			WorkspaceID:  f.Primary.Workspace.ID,
			GraphSpaceID: f.Primary.GraphSpace.ID,
			Slug:         slug, Name: slug,
		}, f.Primary.Principal.ID)
		if err != nil {
			t.Fatalf("create collection %s: %v", slug, err)
		}
		return created.ID
	}

	embedder := hashing.New()
	index.RunVectorFilterConformance(t, "postgres", f.Store.Indexes().Vectors, index.Fixture{
		Primary: f.Primary.Scope(),
		Other:   other.Scope(),
		Model:   embedder.Model(),
		Version: embedder.Version(),
		Embed: func(tb testing.TB, text string) []float32 {
			tb.Helper()
			vectors, err := embedder.Embed(tb.Context(), []string{text})
			if err != nil {
				tb.Fatalf("embed: %v", err)
			}
			return vectors[0]
		},
		SourceA:     source("filter-source-a"),
		SourceB:     source("filter-source-b"),
		CollectionA: collection("filter-collection-a"),
		CollectionB: collection("filter-collection-b"),
	})
}

// TestIntegrationIndexSetReportsItsBackends checks the reporting a recovery drill and a
// startup log depend on.
func TestIntegrationIndexSetReportsItsBackends(t *testing.T) {
	f := pgtest.NewFixture(t)
	indexes := f.Store.Indexes()

	names := indexes.Names()
	for _, projection := range domain.RetrievalProjections() {
		if names[projection] == "" {
			t.Errorf("the %s projection does not report a backend", projection)
		}
	}

	counts, err := indexes.Counts(t.Context(), f.Primary.Workspace.ID)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	for _, projection := range domain.RetrievalProjections() {
		if _, ok := counts[projection]; !ok {
			t.Errorf("the %s projection is missing from the counts", projection)
		}
	}
}

// A partially configured set must degrade rather than panic, the way a nil embedder already
// does: a deployment moving one projection elsewhere should not lose the other two while it
// does so.
func TestIndexSetToleratesMissingBackends(t *testing.T) {
	var empty index.Set

	if names := empty.Names(); len(names) != 0 {
		t.Fatalf("an unconfigured set named %d backends", len(names))
	}
	counts, err := empty.Counts(t.Context(), "01a00000-0000-7000-8000-000000000001")
	if err != nil {
		t.Fatalf("counts on an unconfigured set: %v", err)
	}
	if len(counts) != 0 {
		t.Fatalf("an unconfigured set counted %d projections", len(counts))
	}
	if err := empty.Purge(t.Context(), "01a00000-0000-7000-8000-000000000001"); err != nil {
		t.Fatalf("purging an unconfigured set: %v", err)
	}
}
