// Package index declares the retrieval projections as ports, one per index.
//
// The three projections are derived: drop them, replay the ledger, and nothing is lost
// (AGENTS.md sections 2.3, 15.2). That is what makes them replaceable, and phase 15 is where
// the replacement becomes possible rather than merely permitted.
//
// Three ports rather than one. A deployment putting its vectors in a dedicated store must
// not be made to implement full-text search and graph traversal to do it, and a combined
// interface would require exactly that. The three are independent in the ledger too: each
// write opens its own transaction, so nothing is being taken apart that was ever atomic.
//
// Read and write stay together in each port. Splitting them into a writer and a searcher
// would read tidily and make the contract unstateable: the fields a record is written with
// are the fields a query filters on, and no round-trip conformance test can be written
// across two interfaces that a backend might implement inconsistently.
//
// Following ADR 0020, which settled how this repository makes a port claim real: each port
// has an exported conformance suite that every implementation runs. Two implementations
// compiling against one interface prove nothing about behaviour.
package index

import (
	"context"

	"github.com/gimantha/strata/internal/domain"
)

// Vectors is the nearest-neighbour projection.
//
// Implementations must be safe for concurrent use, and every operation is scoped to a
// workspace: no query may reach another tenant's records, and that is the port's obligation
// rather than the caller's (AGENTS.md section 5).
type Vectors interface {
	// Upsert writes vectors idempotently, keyed by workspace, surface, record, model, and
	// version. Replay converges rather than duplicating — the whole rebuild story rests on
	// it, since a rebuild replays from the beginning every time.
	Upsert(ctx context.Context, records []domain.VectorRecord) error

	// RefreshMetadata updates a record's filter payload without touching its vector.
	//
	// Separate from Upsert because embedding is expensive and lifecycle is not. A claim
	// that has been deactivated, expired, or reclassified has identical text and different
	// standing, and a port without this operation forces a backend either to re-embed
	// unchanged text or to let the change go missing. It went missing here once.
	RefreshMetadata(ctx context.Context, model string, version int,
		records []domain.ProjectedRecord) error

	// ExistingHashes reports the content hash stored with each named record, so a caller
	// can skip re-embedding text that has not changed.
	//
	// The hash must be stored alongside the vector, never in a separate table: a backend
	// that kept it elsewhere could report a hash for a vector it no longer holds. Callers
	// write and then read hashes in sequence, so an implementation with asynchronous
	// indexing must make an upsert durable before the next read.
	ExistingHashes(ctx context.Context, ws domain.WorkspaceID, model string, version int,
		surface domain.Surface, recordIDs []string) (map[string]string, error)

	// Search returns the nearest records, with every filter on the query applied before
	// ranking rather than to the results. Applying them afterwards would silently shrink
	// the result set below the requested limit, and for policy filters it would mean
	// retrieving what the caller may not see (AGENTS.md section 22.4).
	Search(ctx context.Context, q domain.VectorQuery) ([]domain.Hit, error)

	// Purge removes every record in a workspace, for a rebuild.
	Purge(ctx context.Context, ws domain.WorkspaceID) error

	// Count reports how many records a workspace holds, so a recovery drill can show that
	// a rebuild restored what was dropped.
	Count(ctx context.Context, ws domain.WorkspaceID) (int, error)

	// Name identifies the backend in logs and in the recovery report.
	Name() string
}

// Lexical is the full-text and substring projection.
type Lexical interface {
	// Upsert writes records idempotently, keyed by workspace, surface, and record.
	Upsert(ctx context.Context, records []domain.ProjectedRecord) error

	// Search returns matching records. The query carries an Exact flag: exact is substring
	// matching for identifiers and codes that stemming mangles, and the two modes are one
	// method because they are one index.
	Search(ctx context.Context, q domain.LexicalQuery) ([]domain.Hit, error)

	Purge(ctx context.Context, ws domain.WorkspaceID) error
	Count(ctx context.Context, ws domain.WorkspaceID) (int, error)
	Name() string
}

// Graph is the entity-relationship projection.
//
// Narrower than it looks. Traversal here walks edges; it does not own entities. The roots a
// walk starts from and the names it reports are canonical, and the retriever supplies them —
// see the note on Expand.
type Graph interface {
	// UpsertEdges writes edges idempotently, keyed by the assertion that produced them.
	UpsertEdges(ctx context.Context, edges []domain.GraphEdge) error

	// Expand walks outward from the root entities named in the query, to the depth and
	// limit it sets, both of which are hard ceilings (AGENTS.md section 39).
	//
	// Returns identifiers, never names. Naming an entity is a canonical read, and a graph
	// backend that had to perform one would have to hold the entity table to be a graph
	// backend at all. The caller resolves names itself, in one batched lookup.
	//
	// Roots are not returned. Only entities reached by at least one edge appear, which
	// also means a walk seeded with an identifier from another workspace comes back empty
	// rather than echoing that identifier: edges are scoped, so a foreign root can reach
	// nothing. Tenancy is the port's obligation, enforced by the traversal rather than by
	// whoever reads its results.
	Expand(ctx context.Context, q domain.GraphExpandQuery) ([]domain.GraphHit, error)

	Purge(ctx context.Context, ws domain.WorkspaceID) error
	Count(ctx context.Context, ws domain.WorkspaceID) (int, error)
	Name() string
}

// Set is the three projections a deployment has configured.
//
// A struct rather than three parameters because they vary together: a deployment may run
// all three on PostgreSQL, or move one elsewhere, and a caller should be able to say which
// without counting positions. The projector already tolerates a nil embedder by keeping the
// other projections alive; the same reasoning applies to a nil index.
type Set struct {
	Vectors Vectors
	Lexical Lexical
	Graph   Graph
}

// Names reports which backend serves each projection, for a startup log or a recovery
// report. An operator debugging a retrieval problem should not have to infer this from
// configuration.
func (s Set) Names() map[string]string {
	out := map[string]string{}
	if s.Vectors != nil {
		out[domain.ProjectionVector] = s.Vectors.Name()
	}
	if s.Lexical != nil {
		out[domain.ProjectionLexical] = s.Lexical.Name()
	}
	if s.Graph != nil {
		out[domain.ProjectionGraph] = s.Graph.Name()
	}
	return out
}

// Counts reports how many records each configured projection holds.
func (s Set) Counts(ctx context.Context, ws domain.WorkspaceID) (map[string]int, error) {
	out := map[string]int{}
	if s.Vectors != nil {
		count, err := s.Vectors.Count(ctx, ws)
		if err != nil {
			return nil, err
		}
		out[domain.ProjectionVector] = count
	}
	if s.Lexical != nil {
		count, err := s.Lexical.Count(ctx, ws)
		if err != nil {
			return nil, err
		}
		out[domain.ProjectionLexical] = count
	}
	if s.Graph != nil {
		count, err := s.Graph.Count(ctx, ws)
		if err != nil {
			return nil, err
		}
		out[domain.ProjectionGraph] = count
	}
	return out, nil
}

// Purge empties every configured projection.
//
// Callers must clear the checkpoints first. A checkpoint says how far an index has consumed
// the ledger, so one left standing over an emptied index claims work that is no longer
// there; the reverse — a cleared checkpoint over a populated index — only causes a replay,
// and every write here is an idempotent upsert. The safe failure is the one that repeats
// work rather than the one that skips it.
func (s Set) Purge(ctx context.Context, ws domain.WorkspaceID) error {
	if s.Vectors != nil {
		if err := s.Vectors.Purge(ctx, ws); err != nil {
			return err
		}
	}
	if s.Lexical != nil {
		if err := s.Lexical.Purge(ctx, ws); err != nil {
			return err
		}
	}
	if s.Graph != nil {
		if err := s.Graph.Purge(ctx, ws); err != nil {
			return err
		}
	}
	return nil
}
