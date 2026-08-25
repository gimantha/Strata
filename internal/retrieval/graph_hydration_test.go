package retrieval_test

import (
	"context"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/retrieval"
	"github.com/gimantha/strata/internal/store/index"
)

// The first test in this package that needs no database.
//
// That is the split paying for itself: retrieval.Ledger has two methods and index.Graph has
// five, so the behaviour below can be driven with stubs. Before the ports were separated,
// exercising this would have meant implementing fifteen methods including QueryAssertions.

const (
	seedID     = domain.EntityID("01a00000-0000-7000-8000-000000000001")
	reachedID  = domain.EntityID("01a00000-0000-7000-8000-000000000002")
	danglingID = domain.EntityID("01a00000-0000-7000-8000-0000000000ff")
)

// stubLedger knows about two entities and nothing else.
type stubLedger struct{}

func (stubLedger) FindEntitiesByName(_ context.Context, scope domain.Scope,
	_ string) ([]domain.Entity, error) {
	return []domain.Entity{{
		ID: seedID, WorkspaceID: scope.WorkspaceID, GraphSpaceID: scope.GraphSpaceID,
		CanonicalName: "Acme Corporation", EntityType: "organization",
	}}, nil
}

func (stubLedger) GetEntities(_ context.Context, ws domain.WorkspaceID,
	ids []domain.EntityID) (map[domain.EntityID]domain.Entity, error) {
	known := map[domain.EntityID]string{
		seedID:    "Acme Corporation",
		reachedID: "Globex Industries",
		// danglingID is deliberately absent: this is an entity the graph still has an
		// edge to and the ledger no longer has.
	}
	out := map[domain.EntityID]domain.Entity{}
	for _, id := range ids {
		if name, ok := known[id]; ok {
			out[id] = domain.Entity{ID: id, WorkspaceID: ws, CanonicalName: name}
		}
	}
	return out, nil
}

// stubGraph returns one real hit and one whose entity no longer exists.
type stubGraph struct{}

func (stubGraph) UpsertEdges(context.Context, []domain.GraphEdge) error { return nil }

func (stubGraph) Expand(_ context.Context, q domain.GraphExpandQuery) ([]domain.GraphHit, error) {
	if len(q.Roots) == 0 {
		return nil, nil
	}
	return []domain.GraphHit{
		{EntityID: reachedID, Depth: 1, ViaPredicate: "SUPPLIES", FromEntityID: seedID},
		{EntityID: danglingID, Depth: 1, ViaPredicate: "SUPPLIES", FromEntityID: seedID},
	}, nil
}

func (stubGraph) Purge(context.Context, domain.WorkspaceID) error        { return nil }
func (stubGraph) Count(context.Context, domain.WorkspaceID) (int, error) { return 0, nil }
func (stubGraph) Name() string                                           { return "stub" }

var _ index.Graph = stubGraph{}

// TestGraphHitsWithNoEntityAreDropped covers the failure hydration introduces.
//
// Traversal reports identifiers now, and the retriever names them through the ledger. That
// raises a case the previous canonical join handled silently: an edge pointing at an entity
// the ledger does not have. PostgreSQL cannot produce one — graph_edges has foreign keys to
// entities with ON DELETE CASCADE — but a substituted backend has no such guarantee, and
// keeping stale edges is exactly what an eventually-consistent index does.
//
// The retriever drops the hit rather than returning an entity with an empty name, which
// would put a nameless result in front of a user and a citation pointing at nothing.
func TestGraphHitsWithNoEntityAreDropped(t *testing.T) {
	scope := domain.Scope{
		WorkspaceID:  "01a00000-0000-7000-8000-00000000000a",
		GraphSpaceID: "01a00000-0000-7000-8000-00000000000b",
	}

	// No vector or lexical index configured. The retriever skips those modes rather than
	// failing, which is the other half of what the ports made possible.
	retriever := retrieval.New(stubLedger{}, index.Set{Graph: stubGraph{}}, nil,
		retrieval.Options{}, nil, nil)

	result, err := retriever.Query(context.Background(), domain.QueryRequest{
		Scope: scope,
		Query: "Acme Corporation",
		Modes: []domain.RetrievalMode{domain.ModeEntity, domain.ModeGraph},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	var sawReached, sawDangling bool
	for _, item := range result.Items {
		switch domain.EntityID(item.RecordID) {
		case reachedID:
			sawReached = true
			if item.Content != "Globex Industries" {
				t.Errorf("a reached entity was not named: %q", item.Content)
			}
		case danglingID:
			sawDangling = true
		}
	}

	if !sawReached {
		t.Error("the entity the walk reached is missing from the results")
	}
	if sawDangling {
		t.Error("an entity the ledger does not have was returned by retrieval")
	}
}
