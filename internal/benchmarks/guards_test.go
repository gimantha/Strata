package benchmarks_test

import (
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/benchmarks"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/store/ledger"
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

// TestIntegrationScopedSearchPlanIsRecorded documents what the retriever's real query
// actually does today, so the known risk cannot quietly change without anyone noticing.
//
// Not an assertion about which plan is correct — at this corpus size scan-and-sort is
// genuinely cheapest and PostgreSQL is right to choose it. It fails only if the plan stops
// mentioning the table at all, which would mean this check had silently stopped checking.
func TestIntegrationScopedSearchPlanIsRecorded(t *testing.T) {
	d := newDeployment(t, guardCorpus())
	d.load(t)
	ctx := t.Context()

	// Fresh statistics, so the plan reflects the data rather than an empty-table estimate.
	if _, err := d.app.Ledger.Pool().Exec(ctx, "ANALYZE vector_records, lexical_records"); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	embedding, err := d.app.Embedder.Embed(ctx, []string{"delivery schedules across every region"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}

	vectorPlan, err := d.app.Ledger.ExplainVectorSearch(ctx, domain.VectorQuery{
		Scope: d.scope, Embedding: embedding[0], Limit: 20,
	}, ledger.PlanAsChosen)
	if err != nil {
		t.Fatalf("explain vector search: %v", err)
	}
	t.Logf("scoped vector search plan:\n%s", summarize(vectorPlan))
	if !strings.Contains(vectorPlan, "vector_records") {
		t.Fatalf("the plan does not mention vector_records; this check has stopped checking:\n%s",
			vectorPlan)
	}

	lexicalPlan, err := d.app.Ledger.ExplainLexicalSearch(ctx, domain.LexicalQuery{
		Scope: d.scope, Text: "delivery schedules capacity planning", Limit: 20,
	}, ledger.PlanAsChosen)
	if err != nil {
		t.Fatalf("explain lexical search: %v", err)
	}
	t.Logf("scoped lexical search plan:\n%s", summarize(lexicalPlan))
	if !strings.Contains(lexicalPlan, "lexical_records") {
		t.Fatalf("the plan does not mention lexical_records; this check has stopped checking:\n%s",
			lexicalPlan)
	}
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
