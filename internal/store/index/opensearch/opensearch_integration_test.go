package opensearch_test

import (
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/store/index"
	"github.com/gimantha/strata/internal/store/index/opensearch"
	"github.com/gimantha/strata/internal/testsupport/opensearchtest"
)

// open connects to an index of this test's own.
func open(t *testing.T) *opensearch.Store {
	t.Helper()

	store, err := opensearch.Open(t.Context(), opensearch.Options{
		Addresses: []string{opensearchtest.URL(t)},
		Index:     opensearchtest.Index(t),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return store
}

// fixture supplies the suites with scopes and references.
//
// Identifiers are invented rather than created, as with Qdrant: OpenSearch has no foreign
// keys, so a source or collection is a value in a document and nothing has to exist for it
// to be filtered on. Worth noticing rather than glossing — a deployment moving its lexical
// projection here loses a check the reference enforces.
func fixture(t *testing.T) index.Fixture {
	t.Helper()

	return index.Fixture{
		Primary: domain.Scope{
			WorkspaceID:  "01a00000-0000-7000-8000-0000000000a1",
			GraphSpaceID: "01a00000-0000-7000-8000-0000000000b1",
		},
		PrimaryAltSpace: domain.Scope{
			WorkspaceID:  "01a00000-0000-7000-8000-0000000000a1",
			GraphSpaceID: "01a00000-0000-7000-8000-0000000000b2",
		},
		Other: domain.Scope{
			WorkspaceID:  "01a00000-0000-7000-8000-0000000000a2",
			GraphSpaceID: "01a00000-0000-7000-8000-0000000000b3",
		},
		SourceA:     "01a00000-0000-7000-8000-0000000000c1",
		SourceB:     "01a00000-0000-7000-8000-0000000000c2",
		CollectionA: "01a00000-0000-7000-8000-0000000000d1",
		CollectionB: "01a00000-0000-7000-8000-0000000000d2",
	}
}

// TestIntegrationOpenSearchMeetsTheLexicalContract runs the shape suite.
func TestIntegrationOpenSearchMeetsTheLexicalContract(t *testing.T) {
	index.RunLexicalConformance(t, "opensearch", open(t), fixture(t))
}

// TestIntegrationOpenSearchMeetsTheLexicalFilterContract is the half that distinguishes two
// engines.
//
// The shape suite would pass for a backend that ignored every policy filter. This holds the
// OpenSearch translation to the same twenty-five expectations PostgreSQL meets — the
// asymmetries between sources and collections, the escape hatch that exists at policy level
// and not at query level, and the half-open temporal bounds where an absent value means
// unbounded rather than missing.
func TestIntegrationOpenSearchMeetsTheLexicalFilterContract(t *testing.T) {
	index.RunLexicalFilterConformance(t, "opensearch", open(t), fixture(t))
}
