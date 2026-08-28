// Package neo4j stores the graph projection in a dedicated graph database
// (AGENTS.md phase 15).
//
// The last of the three index ports to get a second implementation, and the one where the
// two engines are least alike. A recursive CTE and a Cypher variable-length pattern are not
// dialects of one idea; they are different ways of thinking about reachability, which is
// what makes this the port most worth proving.
//
// Two things shape the adapter.
//
// The port writes edges and never entities. A graph database wants nodes to hang
// relationships from, so they are created as a side effect of writing an edge and carry
// nothing but an identifier and a workspace. Names, types and everything else stay
// canonical: traversal reports identifiers and the retriever resolves them, which is exactly
// the seam ADR 0021 opened so that a backend like this one could exist.
//
// The reference reports each entity once, at the shallowest depth it was reached, with the
// edge that produced that depth. Cypher gives every path rather than the best one, so the
// query orders by depth and keeps the first — and takes the via triple from the same row, so
// depth and provenance cannot disagree.
package neo4j

import (
	"context"
	"fmt"
	"strings"
	"time"

	driver "github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/store/index"
)

// Name identifies this backend on a startup log and in the recovery report.
const Name = "neo4j"

// Options configure the backend.
type Options struct {
	// URI is the Bolt endpoint, such as bolt://127.0.0.1:7687.
	URI      string
	Username string
	Password string
	// Database selects a database within the server. Empty uses the server's default,
	// which is what a community edition offers.
	Database string
}

func (o Options) withDefaults() Options {
	if o.URI == "" {
		o.URI = "bolt://127.0.0.1:7687"
	}
	return o
}

// Store is the Neo4j-backed graph index.
type Store struct {
	driver   driver.DriverWithContext
	database string
}

// Open connects and provisions the constraints the traversal depends on.
func Open(ctx context.Context, opts Options) (*Store, error) {
	const op = "neo4j.Open"

	opts = opts.withDefaults()
	conn, err := driver.NewDriverWithContext(opts.URI,
		driver.BasicAuth(opts.Username, opts.Password, ""))
	if err != nil {
		return nil, domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot connect to Neo4j")
	}
	if err := conn.VerifyConnectivity(ctx); err != nil {
		_ = conn.Close(ctx)
		return nil, domain.Wrap(err, domain.CodeProviderUnavailable, op, "Neo4j is not reachable")
	}

	store := &Store{driver: conn, database: opts.Database}
	if err := store.ensureConstraints(ctx); err != nil {
		_ = conn.Close(ctx)
		return nil, err
	}
	return store, nil
}

// Close releases the connection pool.
func (s *Store) Close(ctx context.Context) error { return s.driver.Close(ctx) }

// Name identifies the backend.
func (s *Store) Name() string { return Name }

// ensureConstraints creates what upsert and traversal depend on.
//
// The edge key is a uniqueness constraint rather than a convention, because the reference's
// ON CONFLICT (assertion_id) is enforced by the database and a backend relying on a MERGE
// alone would diverge the moment two writers raced.
func (s *Store) ensureConstraints(ctx context.Context) error {
	const op = "neo4j.ensureConstraints"

	statements := []string{
		`CREATE CONSTRAINT strata_entity_key IF NOT EXISTS
		 FOR (e:Entity) REQUIRE (e.workspace, e.id) IS UNIQUE`,
		`CREATE CONSTRAINT strata_edge_key IF NOT EXISTS
		 FOR ()-[r:EDGE]-() REQUIRE r.assertion IS UNIQUE`,
		`CREATE INDEX strata_edge_workspace IF NOT EXISTS
		 FOR ()-[r:EDGE]-() ON (r.workspace)`,
	}
	for _, statement := range statements {
		if _, err := s.run(ctx, statement, nil); err != nil {
			// A relationship uniqueness constraint needs enterprise edition. The adapter
			// still works without it — MERGE on the assertion gives convergence for a
			// single writer — so this is a note rather than a failure, and the community
			// edition is what most deployments will meet.
			if strings.Contains(err.Error(), "constraint") &&
				strings.Contains(strings.ToLower(err.Error()), "enterprise") {
				continue
			}
			return domain.Wrap(err, domain.CodeProviderUnavailable, op,
				"cannot provision the graph schema")
		}
	}
	return nil
}

// run executes one statement and returns its records.
func (s *Store) run(ctx context.Context, cypher string,
	params map[string]any) ([]*driver.Record, error) {
	options := []driver.ExecuteQueryConfigurationOption{}
	if s.database != "" {
		options = append(options, driver.ExecuteQueryWithDatabase(s.database))
	}
	result, err := driver.ExecuteQuery(ctx, s.driver, cypher, params,
		driver.EagerResultTransformer, options...)
	if err != nil {
		return nil, err
	}
	return result.Records, nil
}

// stamp renders a timestamp as epoch microseconds, or nil for an absent bound.
//
// Nulls rather than sentinels, unlike the Qdrant adapter: Cypher compares against null the
// way SQL does, so "(r.valid_from IS NULL OR r.valid_from <= $at)" is the reference clause
// almost verbatim.
func stamp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().UnixMicro()
}

// UpsertEdges writes edges, creating the nodes they connect.
func (s *Store) UpsertEdges(ctx context.Context, edges []domain.GraphEdge) error {
	const op = "neo4j.UpsertEdges"

	if len(edges) == 0 {
		return nil
	}

	rows := make([]map[string]any, 0, len(edges))
	for _, e := range edges {
		rows = append(rows, map[string]any{
			"assertion":      string(e.AssertionID),
			"workspace":      string(e.WorkspaceID),
			"graphSpace":     string(e.GraphSpaceID),
			"subject":        string(e.SubjectID),
			"object":         string(e.ObjectEntityID),
			"predicate":      e.Predicate,
			"status":         string(e.Status),
			"classification": string(e.Classification),
			"source":         string(e.SourceID),
			"collection":     string(e.CollectionID),
			"memoryKind":     string(e.MemoryKind),
			"validFrom":      stamp(e.ValidFrom),
			"validTo":        stamp(e.ValidTo),
			"activeUntil":    stamp(e.ActiveUntil),
			"expiresAt":      stamp(e.ExpiresAt),
		})
	}

	// Keyed on the assertion alone, matching the reference's ON CONFLICT (assertion_id):
	// one edge per claim, so replay converges. Endpoints are merged rather than created,
	// since a node is shared by every edge that touches it.
	//
	// The graph space is set on every write rather than only on create, because a
	// projection reflects the ledger — the same reason the reference's SET list carries it.
	const cypher = `
		UNWIND $rows AS row
		MERGE (subject:Entity {workspace: row.workspace, id: row.subject})
		MERGE (object:Entity {workspace: row.workspace, id: row.object})
		MERGE (subject)-[edge:EDGE {assertion: row.assertion}]->(object)
		SET edge.workspace = row.workspace,
		    edge.graphSpace = row.graphSpace,
		    edge.predicate = row.predicate,
		    edge.status = row.status,
		    edge.classification = row.classification,
		    edge.source = row.source,
		    edge.collection = row.collection,
		    edge.memoryKind = row.memoryKind,
		    edge.validFrom = row.validFrom,
		    edge.validTo = row.validTo,
		    edge.activeUntil = row.activeUntil,
		    edge.expiresAt = row.expiresAt`

	if _, err := s.run(ctx, cypher, map[string]any{"rows": rows}); err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot write edges")
	}
	return nil
}

// Expand walks outward from the roots.
func (s *Store) Expand(ctx context.Context, q domain.GraphExpandQuery) ([]domain.GraphHit, error) {
	const op = "neo4j.Expand"

	q = q.Normalize()
	if domain.IsZero(q.Scope.WorkspaceID) {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op, "workspace scope is required")
	}
	if len(q.Roots) == 0 {
		return nil, nil
	}

	conditions, params, impossible := edgeConditions(q)
	if impossible {
		return nil, nil
	}

	roots := make([]string, 0, len(q.Roots))
	for _, root := range q.Roots {
		roots = append(roots, string(root))
	}
	params["roots"] = roots
	params["workspace"] = string(q.Scope.WorkspaceID)
	params["limit"] = q.Limit

	// The depth bound is interpolated because Cypher will not take a parameter in a
	// variable-length pattern. It is safe to do so: Normalize has already clamped it to
	// the range the port allows, so the value can only be an integer between one and the
	// ceiling.
	cypher := fmt.Sprintf(`
		MATCH (root:Entity {workspace: $workspace})
		WHERE root.id IN $roots
		MATCH path = (root)-[rels:EDGE*1..%d]-(reached:Entity)
		WHERE all(edge IN rels WHERE %s)
		WITH reached, length(path) AS depth, last(rels) AS via,
		     nodes(path)[length(path) - 1] AS steppedFrom
		ORDER BY reached.id, depth
		WITH reached.id AS entityId,
		     collect({depth: depth, via: via, from: steppedFrom})[0] AS best
		// Roots are dropped after the limit rather than before, because the reference's
		// limit counts them. Moving it would quietly widen every traversal.
		ORDER BY entityId
		LIMIT $limit
		WITH entityId, best WHERE NOT entityId IN $roots
		RETURN entityId, best.depth AS depth, best.via.assertion AS viaAssertion,
		       best.via.predicate AS viaPredicate, best.from.id AS fromEntity`,
		q.Depth, strings.Join(conditions, " AND "))

	records, err := s.run(ctx, cypher, params)
	if err != nil {
		return nil, domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot expand graph")
	}

	out := make([]domain.GraphHit, 0, len(records))
	for _, record := range records {
		hit := domain.GraphHit{
			EntityID:     domain.EntityID(stringAt(record, "entityId")),
			Depth:        intAt(record, "depth"),
			ViaAssertion: domain.AssertionID(stringAt(record, "viaAssertion")),
			ViaPredicate: stringAt(record, "viaPredicate"),
			FromEntityID: domain.EntityID(stringAt(record, "fromEntity")),
		}
		out = append(out, hit)
	}
	return out, nil
}

// Purge removes every edge in a workspace, and the nodes left with none.
func (s *Store) Purge(ctx context.Context, ws domain.WorkspaceID) error {
	const op = "neo4j.Purge"

	// Edges first, then the nodes nothing connects. A node is only ever created to hang an
	// edge from, so one with no edges is debris rather than data — and leaving it would
	// make a later count of entities wrong even though the port counts edges.
	if _, err := s.run(ctx, `
		MATCH ()-[edge:EDGE {workspace: $workspace}]-() DELETE edge`,
		map[string]any{"workspace": string(ws)}); err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot purge edges")
	}
	if _, err := s.run(ctx, `
		MATCH (entity:Entity {workspace: $workspace}) WHERE NOT (entity)--() DELETE entity`,
		map[string]any{"workspace": string(ws)}); err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot purge orphan nodes")
	}
	return nil
}

// Count reports how many edges a workspace holds.
func (s *Store) Count(ctx context.Context, ws domain.WorkspaceID) (int, error) {
	const op = "neo4j.Count"

	// Directed, so an edge is counted once. An undirected match would see each edge from
	// both ends and report double.
	records, err := s.run(ctx, `
		MATCH ()-[edge:EDGE {workspace: $workspace}]->() RETURN count(edge) AS total`,
		map[string]any{"workspace": string(ws)})
	if err != nil {
		return 0, domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot count edges")
	}
	if len(records) == 0 {
		return 0, nil
	}
	return intAt(records[0], "total"), nil
}

func stringAt(record *driver.Record, key string) string {
	value, ok := record.Get(key)
	if !ok || value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func intAt(record *driver.Record, key string) int {
	value, ok := record.Get(key)
	if !ok || value == nil {
		return 0
	}
	if number, ok := value.(int64); ok {
		return int(number)
	}
	return 0
}

// Ensure the adapter satisfies the port.
var _ index.Graph = (*Store)(nil)
