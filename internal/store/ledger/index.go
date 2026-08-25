// PostgreSQL implementations of the three index ports (AGENTS.md phase 15).
//
// Thin adapters over the SQL that was already here. They exist so the projector and the
// retriever depend on what an index does rather than on which database happens to hold it,
// and so a deployment can replace one projection without reimplementing the ledger.

package ledger

import (
	"context"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/store/index"
)

// Name identifies these backends on a startup log and in the recovery report.
//
// One name for all three, because they are one database: an operator reading
// "vector: postgres, lexical: postgres" learns something an operator reading
// "vector: pgvector, lexical: tsvector" does not, which is that a single restore covers
// both.
const indexBackendName = "postgres"

// Indexes returns the three projections backed by this store.
func (s *Store) Indexes() index.Set {
	return index.Set{
		Vectors: vectorIndex{store: s},
		Lexical: lexicalIndex{store: s},
		Graph:   graphIndex{store: s},
	}
}

type vectorIndex struct{ store *Store }

func (v vectorIndex) Upsert(ctx context.Context, records []domain.VectorRecord) error {
	return v.store.UpsertVectors(ctx, records)
}

func (v vectorIndex) RefreshMetadata(ctx context.Context, model string, version int,
	records []domain.ProjectedRecord) error {
	return v.store.RefreshVectorMetadata(ctx, model, version, records)
}

func (v vectorIndex) ExistingHashes(ctx context.Context, ws domain.WorkspaceID, model string,
	version int, surface domain.Surface, recordIDs []string) (map[string]string, error) {
	return v.store.ExistingVectorHashes(ctx, ws, model, version, surface, recordIDs)
}

func (v vectorIndex) Search(ctx context.Context, q domain.VectorQuery) ([]domain.Hit, error) {
	return v.store.SearchVectors(ctx, q)
}

func (v vectorIndex) Purge(ctx context.Context, ws domain.WorkspaceID) error {
	return v.store.purgeTable(ctx, "ledger.vectorIndex.Purge", "vector_records", ws)
}

func (v vectorIndex) Count(ctx context.Context, ws domain.WorkspaceID) (int, error) {
	return v.store.countTable(ctx, "ledger.vectorIndex.Count", "vector_records", ws)
}

func (v vectorIndex) Name() string { return indexBackendName }

type lexicalIndex struct{ store *Store }

func (l lexicalIndex) Upsert(ctx context.Context, records []domain.ProjectedRecord) error {
	return l.store.UpsertLexical(ctx, records)
}

func (l lexicalIndex) Search(ctx context.Context, q domain.LexicalQuery) ([]domain.Hit, error) {
	return l.store.SearchLexical(ctx, q)
}

func (l lexicalIndex) Purge(ctx context.Context, ws domain.WorkspaceID) error {
	return l.store.purgeTable(ctx, "ledger.lexicalIndex.Purge", "lexical_records", ws)
}

func (l lexicalIndex) Count(ctx context.Context, ws domain.WorkspaceID) (int, error) {
	return l.store.countTable(ctx, "ledger.lexicalIndex.Count", "lexical_records", ws)
}

func (l lexicalIndex) Name() string { return indexBackendName }

type graphIndex struct{ store *Store }

func (g graphIndex) UpsertEdges(ctx context.Context, edges []domain.GraphEdge) error {
	return g.store.UpsertGraphEdges(ctx, edges)
}

func (g graphIndex) Expand(ctx context.Context, q domain.GraphExpandQuery) ([]domain.GraphHit, error) {
	return g.store.ExpandGraph(ctx, q)
}

func (g graphIndex) Purge(ctx context.Context, ws domain.WorkspaceID) error {
	return g.store.purgeTable(ctx, "ledger.graphIndex.Purge", "graph_edges", ws)
}

func (g graphIndex) Count(ctx context.Context, ws domain.WorkspaceID) (int, error) {
	return g.store.countTable(ctx, "ledger.graphIndex.Count", "graph_edges", ws)
}

func (g graphIndex) Name() string { return indexBackendName }

// purgeTable empties one projection table for a workspace.
//
// Per table rather than the four-table transaction this replaces. That transaction was not
// buying atomicity anybody relied on — the three writes were never atomic with each other —
// and keeping it would have meant a port that only a single database could implement.
func (s *Store) purgeTable(ctx context.Context, op, table string, ws domain.WorkspaceID) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM `+table+` WHERE workspace_id = $1`, ws); err != nil {
		return mapError(err, op, "cannot purge the projection")
	}
	return nil
}

func (s *Store) countTable(ctx context.Context, op, table string, ws domain.WorkspaceID) (int, error) {
	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM `+table+` WHERE workspace_id = $1`, ws).Scan(&count); err != nil {
		return 0, mapError(err, op, "cannot count projected records")
	}
	return count, nil
}

// DeleteCheckpoints clears the positions of the named projections.
//
// Named rather than "all for this workspace", because projection_checkpoints is shared: the
// consolidation job keeps its cursor there, and a rebuild that cleared the table would reset
// a component it has nothing to do with.
func (s *Store) DeleteCheckpoints(ctx context.Context, ws domain.WorkspaceID, names []string) error {
	const op = "ledger.DeleteCheckpoints"

	if len(names) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM projection_checkpoints
		 WHERE workspace_id = $1 AND projection = ANY($2::text[])`, ws, names); err != nil {
		return mapError(err, op, "cannot delete projection checkpoints")
	}
	return nil
}

// The adapters satisfy the ports. Asserted rather than left implicit: with several
// implementations and an out-of-tree backend as the point of the exercise, "it happens to
// satisfy it" stops being self-evident and starts being someone else's compile error.
var (
	_ index.Vectors = vectorIndex{}
	_ index.Lexical = lexicalIndex{}
	_ index.Graph   = graphIndex{}
)
