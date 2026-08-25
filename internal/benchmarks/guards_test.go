package benchmarks_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/benchmarks"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/store/ledger"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

// Three of AGENTS.md section 39's items are not speeds. "Bounded graph traversal with hard
// limits", "no unbounded context generation", and "no full graph scan for ordinary semantic
// search" are invariants: they either hold or the system is wrong, and no amount of fast
// hardware fixes them.
//
// So they are ordinary tests that run in CI rather than benchmarks someone runs quarterly.
// The corpus is small, because these check shape rather than speed.

// guardCorpus is large enough to have a graph and small enough to load in seconds.
func guardCorpus() benchmarks.Corpus {
	return benchmarks.Corpus{
		Documents:           60,
		EntitiesPerDocument: 4,
		DistinctEntities:    24,
		WordsPerDocument:    60,
	}
}

// TestIntegrationGraphTraversalIsBounded covers "bounded graph traversal with hard limits".
//
// The corpus is deliberately built so entities recur across documents: a traversal over a
// graph with no cycles and no breadth proves nothing. The failure this excludes is a walk
// that follows a cycle forever or fans out across the whole graph, which is a hang rather
// than a slow query.
func TestIntegrationGraphTraversalIsBounded(t *testing.T) {
	d := newDeployment(t, guardCorpus())
	d.load(t)
	ctx := t.Context()

	entities, err := d.app.Ledger.ListEntities(ctx, d.scope, "", 10)
	if err != nil {
		t.Fatalf("list entities: %v", err)
	}
	if len(entities) == 0 {
		t.Fatal("the corpus produced no entities; the guard would pass vacuously")
	}
	roots := []domain.EntityID{entities[0].ID}

	// A request beyond the ceiling is clamped, not honoured. Depth is what bounds the
	// work, so a caller must not be able to raise it by asking.
	beyond := domain.GraphExpandQuery{
		Scope: d.scope, Roots: roots, Depth: domain.MaxGraphDepth + 50, Limit: 500,
	}
	normalized := beyond.Normalize()
	if normalized.Depth != domain.MaxGraphDepth {
		t.Fatalf("depth %d was requested and %d survived normalization; the ceiling is not a ceiling",
			beyond.Depth, normalized.Depth)
	}

	hits, err := d.app.Ledger.ExpandGraph(ctx, beyond)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	for _, hit := range hits {
		if hit.Depth > domain.MaxGraphDepth {
			t.Fatalf("traversal reached depth %d past a ceiling of %d",
				hit.Depth, domain.MaxGraphDepth)
		}
	}
	if len(hits) > beyond.Limit {
		t.Fatalf("traversal returned %d hits against a limit of %d", len(hits), beyond.Limit)
	}

	// And the result count is bounded by the limit even when the graph is larger.
	small := domain.GraphExpandQuery{Scope: d.scope, Roots: roots, Depth: 3, Limit: 5}
	limited, err := d.app.Ledger.ExpandGraph(ctx, small)
	if err != nil {
		t.Fatalf("expand with a limit: %v", err)
	}
	if len(limited) > 5 {
		t.Fatalf("a limit of 5 returned %d hits", len(limited))
	}
}

// TestIntegrationContextGenerationIsBounded covers "no unbounded context generation".
//
// The budget is a hard ceiling rather than a target: assembly drops content instead of
// exceeding it. Checked across a range of budgets, because a ceiling that holds at 2000
// tokens and not at 150 is a ceiling that fails exactly when it matters — a small budget
// is where the scaffolding alone can overrun.
func TestIntegrationContextGenerationIsBounded(t *testing.T) {
	d := newDeployment(t, guardCorpus())
	d.load(t)
	ctx := t.Context()

	for _, budget := range []int{domain.MinTokenBudget, 250, 800, 2000} {
		block, err := d.app.Assembler.Assemble(ctx, domain.ContextRequest{
			Scope:       d.scope,
			Query:       "quarterly review delivery schedules capacity planning",
			Principal:   d.principal,
			TokenBudget: budget,
		})
		if err != nil {
			t.Fatalf("assemble at %d tokens: %v", budget, err)
		}
		if block.Budget.Used > budget {
			t.Fatalf("a %d-token budget produced %d tokens; the ceiling is advisory",
				budget, block.Budget.Used)
		}
		if block.Budget.Limit != budget {
			t.Fatalf("the block reports a limit of %d against a request for %d",
				block.Budget.Limit, budget)
		}
	}

	// A budget beyond the ceiling is clamped rather than honoured, so a caller cannot ask
	// for an unbounded block by naming a large enough number.
	req := domain.ContextRequest{
		Scope: d.scope, Query: "delivery", Principal: d.principal,
		TokenBudget: domain.MaxTokenBudget * 10,
	}
	if got := req.Normalize().TokenBudget; got > domain.MaxTokenBudget {
		t.Fatalf("a budget of %d survived normalization against a ceiling of %d",
			got, domain.MaxTokenBudget)
	}
}

// TestIntegrationProjectionIndexesAreUsable covers "no full graph scan for ordinary
// semantic search" — as far as it can honestly be covered by a test.
//
// The naive version of this test asserts "no Seq Scan" against the retriever's real query
// and passes for the wrong reason: on a small fixture a sequential scan genuinely is the
// cheapest plan, so the test measures the size of the fixture. Running it against a
// production-sized corpus would fix that and make CI unusable.
//
// What is both durable and cheap to check is whether the indexes built for these searches
// can be chosen at all. The queries below are unscoped on purpose — they are testing the
// index and its operator class, not the retriever — and sequential scans are disabled so
// the planner has to reveal whether an index path exists. A changed distance operator, a
// dropped index, or a generated column that stops matching its operator class all fail
// here, and all of them would otherwise surface as a mysteriously slow deployment.
//
// What this does NOT prove is that the retriever's own scoped query uses these indexes. It
// currently does not, and that is a known, documented risk rather than an oversight — see
// docs/api/performance.md, "The scoped-search finding".
func TestIntegrationProjectionIndexesAreUsable(t *testing.T) {
	d := newDeployment(t, guardCorpus())
	d.load(t)
	ctx := t.Context()

	cases := []struct {
		name  string
		index string
		sql   string
	}{
		{
			name:  "vector nearest neighbour",
			index: "vector_records_embedding_idx",
			sql: `EXPLAIN SELECT record_id FROM vector_records
			      ORDER BY embedding <=> (SELECT embedding FROM vector_records LIMIT 1)
			      LIMIT 20`,
		},
		{
			name:  "lexical full text",
			index: "lexical_records_search_idx",
			sql: `EXPLAIN SELECT record_id FROM lexical_records
			      WHERE search_vector @@ websearch_to_tsquery('english', 'delivery schedules')
			      LIMIT 20`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := d.app.Ledger.Pool().Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()

			// SET LOCAL, so the flag dies with the transaction rather than following the
			// connection back into the pool and re-planning everything after it.
			if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
				t.Fatalf("disable sequential scans: %v", err)
			}

			rows, err := tx.Query(ctx, tc.sql)
			if err != nil {
				t.Fatalf("explain: %v", err)
			}
			defer rows.Close()

			var plan strings.Builder
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatalf("read plan: %v", err)
				}
				plan.WriteString(line)
				plan.WriteByte('\n')
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("read plan: %v", err)
			}

			if !strings.Contains(plan.String(), tc.index) {
				t.Fatalf("with sequential scans disabled the planner still will not use %s, "+
					"so that index cannot serve this search at any scale "+
					"(AGENTS.md section 39):\n%s", tc.index, plan.String())
			}
		})
	}
}

// TestIntegrationScopedSemanticSearchUsesItsIndex is AGENTS.md section 39's "no full graph
// scan for ordinary semantic search", asserted on the query the retriever actually runs.
//
// The earlier version only recorded the plan, because at fixture size a sequential scan
// genuinely is cheapest and asserting otherwise would have measured the fixture. Measuring
// settled what to assert instead, and the measurement is worth recording here because it is
// not what one would guess.
//
// Whether the planner *chooses* the index is not stable enough to assert. Over 2,000 rows it
// picked HNSW on one run and a sequential scan on another; at 20,000 it picked the scan. The
// cause is that pgvector stores vectors out of line, so the main table looks tiny — a
// 2,000-row scan is costed at about a hundred pages — and detoasting is not costed at all.
//
// What is stable at every size measured is whether the index *can* be used: with sequential
// scans disabled the planner reaches for HNSW every time. That is also exactly the property
// worth guarding, because the way this breaks is not the planner changing its mind. A
// deterministic tiebreak added to the ORDER BY once made the index unusable at every size —
// verified at two hundred thousand rows, still a sequential scan — and no test noticed,
// because every fixture in the suite is small enough for a scan to be the right answer.
func TestIntegrationScopedSemanticSearchUsesItsIndex(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := t.Context()

	// Past the crossover, with room to spare.
	const vectors = 2000
	if _, err := f.Store.Pool().Exec(ctx, `
		INSERT INTO vector_records (id, workspace_id, graph_space_id, surface, record_id,
		    embedding_model, embedding_version, embedding, status, classification,
		    memory_kind, content_hash)
		SELECT gen_random_uuid(), $1, $2, 'chunk', gen_random_uuid(),
		       'hashing-bow-v1', 1, v.emb, 'active', 'internal', 'semantic', ''
		FROM generate_series(1, $3) s
		CROSS JOIN LATERAL (
		    SELECT array_agg(random())::vector AS emb FROM generate_series(1, 1536)
		) v`, f.Primary.Workspace.ID, f.Primary.GraphSpace.ID, vectors); err != nil {
		t.Fatalf("seed vectors: %v", err)
	}
	// Without fresh statistics the planner estimates one row and any plan looks cheap.
	if _, err := f.Store.Pool().Exec(ctx, "ANALYZE vector_records"); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	var literal string
	if err := f.Store.Pool().QueryRow(ctx,
		`SELECT (SELECT array_agg(random())::vector FROM generate_series(1,1536))::text`).
		Scan(&literal); err != nil {
		t.Fatalf("build a query vector: %v", err)
	}

	plan, err := f.Store.ExplainVectorSearch(ctx, domain.VectorQuery{
		Scope:     f.Primary.Scope(),
		Embedding: parseVectorLiteral(t, literal),
		Model:     "hashing-bow-v1",
		Version:   1,
		Limit:     20,
	}, ledger.PlanPreferIndexes)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}

	if !strings.Contains(plan, "vector_records_embedding_idx") {
		t.Fatalf("a scoped semantic search over %d vectors cannot use the HNSW index even "+
			"with sequential scans disabled, so it cannot use it at any size "+
			"(AGENTS.md section 39). The usual cause is an ORDER BY the index cannot "+
			"satisfy: an HNSW scan matches the distance expression alone, so a second sort "+
			"key there forces a full scan and a sort. Sort deterministically outside the "+
			"limit instead.\n%s", vectors, summarize(plan))
	}
}

// parseVectorLiteral turns PostgreSQL's vector text form back into floats.
func parseVectorLiteral(t *testing.T, literal string) []float32 {
	t.Helper()

	parts := strings.Split(strings.Trim(literal, "[]"), ",")
	out := make([]float32, 0, len(parts))
	for _, part := range parts {
		var value float32
		if _, err := fmt.Sscan(part, &value); err != nil {
			t.Fatalf("parse %q: %v", part, err)
		}
		out = append(out, value)
	}
	return out
}

// summarize trims the embedding literal out of a plan, which is otherwise 1536 numbers of
// noise around the one line anybody reads.
func summarize(plan string) string {
	var out strings.Builder
	for line := range strings.SplitSeq(plan, "\n") {
		if i := strings.Index(line, "Sort Key"); i >= 0 {
			line = line[:i] + "Sort Key: <embedding distance>"
		}
		if i := strings.Index(line, "'["); i >= 0 {
			line = line[:i] + "<embedding>"
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}
