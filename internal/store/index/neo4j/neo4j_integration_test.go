package neo4j_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/store/index"
	graphstore "github.com/gimantha/strata/internal/store/index/neo4j"
	"github.com/gimantha/strata/internal/testsupport/neo4jtest"
)

// counter keeps identifiers unique across a run, since the community edition has one
// database and the suite writes into it repeatedly.
var counter atomic.Int64

// TestIntegrationNeo4jMeetsTheGraphContract is the last of the three ports to get a second
// implementation, and the one where the engines are least alike.
//
// A recursive CTE and a Cypher variable-length pattern are different ways of thinking about
// reachability. Passing the same twenty-three cases is therefore a stronger statement than
// it was for the other two: it says the port describes traversal rather than describing
// PostgreSQL's way of doing it.
func TestIntegrationNeo4jMeetsTheGraphContract(t *testing.T) {
	user, password := neo4jtest.Credentials()
	store, err := graphstore.Open(t.Context(), graphstore.Options{
		URI: neo4jtest.URI(t), Username: user, Password: password,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = store.Close(closeCtx)
	})

	// A workspace of this test's own. Tenancy is how the suite is isolated here, because
	// the community edition offers one database and the port separates tenants anyway.
	primary := domain.Scope{
		WorkspaceID:  domain.WorkspaceID(unique("ws")),
		GraphSpaceID: domain.GraphSpaceID(unique("gs")),
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = store.Purge(cleanupCtx, primary.WorkspaceID)
	})

	index.RunGraphConformance(t, "neo4j", store, index.Fixture{
		Primary: primary,
		Other: domain.Scope{
			WorkspaceID:  domain.WorkspaceID(unique("ws")),
			GraphSpaceID: domain.GraphSpaceID(unique("gs")),
		},
		SourceA:     domain.SourceID(unique("src")),
		CollectionA: domain.CollectionID(unique("col")),
		CollectionB: domain.CollectionID(unique("col")),
		// Invented rather than created: a graph database has no foreign keys, so an
		// entity or an assertion here is an identifier on an edge and nothing has to exist
		// for one to be written. The reference cannot do this, and the difference is worth
		// seeing — a deployment moving its graph here loses a check it had.
		NewEntities: func(tb testing.TB, n int) []domain.EntityID {
			out := make([]domain.EntityID, 0, n)
			for range n {
				out = append(out, domain.EntityID(unique("ent")))
			}
			return out
		},
		NewAssertions: func(tb testing.TB, n int) []domain.AssertionID {
			out := make([]domain.AssertionID, 0, n)
			for range n {
				out = append(out, domain.AssertionID(unique("asr")))
			}
			return out
		},
	})
}

func unique(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), counter.Add(1))
}
