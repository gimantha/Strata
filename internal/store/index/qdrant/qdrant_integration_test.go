package qdrant_test

import (
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding/hashing"
	"github.com/gimantha/strata/internal/store/index"
	"github.com/gimantha/strata/internal/store/index/qdrant"
	"github.com/gimantha/strata/internal/testsupport/qdranttest"
)

// open connects to a collection of this test's own, provisioned and dropped with it.
func open(t *testing.T) *qdrant.Store {
	t.Helper()

	host, port := qdranttest.Address(t)
	store, err := qdrant.Open(t.Context(), qdrant.Options{
		Host: host, Port: port, Collection: qdranttest.Collection(t),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// fixture supplies the suites with scopes and references.
//
// Identifiers are invented rather than created, unlike the PostgreSQL fixture: Qdrant has no
// foreign keys, so a source or collection here is a value in a payload and nothing has to
// exist for it to be filtered on. That difference is itself worth noticing — the reference
// enforces referential integrity and this backend does not, so a deployment moving its
// vectors here loses a check it may not know it had.
func fixture(t *testing.T) index.Fixture {
	t.Helper()

	embedder := hashing.New()
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
		SourceA:     "01a00000-0000-7000-8000-0000000000c1",
		SourceB:     "01a00000-0000-7000-8000-0000000000c2",
		CollectionA: "01a00000-0000-7000-8000-0000000000d1",
		CollectionB: "01a00000-0000-7000-8000-0000000000d2",
	}
}

// TestIntegrationQdrantMeetsTheVectorContract is what the port was for.
//
// A backend sharing no code, no query language and no storage engine with the reference,
// held to the same behaviour by the same suite. ADR 0021 could describe this as the goal and
// not demonstrate it; passing here is the demonstration.
func TestIntegrationQdrantMeetsTheVectorContract(t *testing.T) {
	index.RunVectorConformance(t, "qdrant", open(t), fixture(t))
}

// TestIntegrationQdrantMeetsTheVectorFilterContract is the half that actually distinguishes
// two engines.
//
// The shape suite would pass for a backend that ignored every policy filter. This one holds
// the Qdrant filter translation to the same twenty-six expectations PostgreSQL meets,
// including the asymmetries — an allow-list on sources dropping source-less records while
// one on collections keeps collection-less ones, the query-level entity filter excluding
// passages where the policy-level one admits them.
func TestIntegrationQdrantMeetsTheVectorFilterContract(t *testing.T) {
	index.RunVectorFilterConformance(t, "qdrant", open(t), fixture(t))
}
