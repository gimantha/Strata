package contextblock_test

import (
	"strings"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/contextblock"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/knowledge"
)

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %s: %v", value, err)
	}
	return parsed
}

// TestIntegrationEveryCitationResolvesToRealRecords is the acceptance criterion checked
// against the ledger rather than against a fixture: each marker in the assembled text is
// followed back through the citation to an assertion and an evidence row that exist.
func TestIntegrationEveryCitationResolvesToRealRecords(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	event, episode := h.ingest(t,
		"Acme Corporation supplies industrial fasteners to the Portland plant. "+
			"Deliveries run weekly and the account has been active since 2019.", "ctx-1")
	h.claim(t, event, episode, knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
		Predicate: "supplies",
		Object:    domain.ObjectOfString("industrial fasteners"),
		Evidence: []knowledge.EvidenceInput{{
			EpisodeID:     episode,
			ExtractedText: "Acme Corporation supplies industrial fasteners",
		}},
	})

	block, err := h.assembler.Assemble(ctx, domain.ContextRequest{
		Scope: h.scope(), Query: "who supplies industrial fasteners", TokenBudget: 1200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(block.Items) == 0 {
		t.Fatalf("expected content:\n%s", block.Text)
	}

	for _, citation := range block.Citations {
		if !strings.Contains(block.Text, "["+itoa(citation.Marker)+"]") {
			t.Fatalf("citation %d is not referenced in the text", citation.Marker)
		}

		if citation.Factual() {
			if citation.AssertionID == nil {
				t.Fatalf("factual citation %d names no assertion", citation.Marker)
			}
			if _, err := h.fixture.Store.GetAssertion(ctx, h.scope().WorkspaceID, *citation.AssertionID); err != nil {
				t.Fatalf("citation %d points at assertion %s, which does not exist: %v",
					citation.Marker, *citation.AssertionID, err)
			}

			evidence, err := h.fixture.Store.ListEvidence(ctx, h.scope().WorkspaceID, *citation.AssertionID)
			if err != nil {
				t.Fatalf("list evidence: %v", err)
			}
			if len(evidence) == 0 {
				t.Fatalf("citation %d claims evidence the ledger does not have", citation.Marker)
			}
			continue
		}

		if citation.ChunkID == nil {
			t.Fatalf("excerpt citation %d names no chunk", citation.Marker)
		}
		found, err := h.fixture.Store.ChunkProvenance(ctx, h.scope().WorkspaceID,
			[]domain.ChunkID{*citation.ChunkID})
		if err != nil || len(found) == 0 {
			t.Fatalf("citation %d points at chunk %s, which does not resolve: %v",
				citation.Marker, *citation.ChunkID, err)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}

// TestIntegrationBudgetHoldsAgainstRealProjections checks the ceiling where the content is
// whatever chunking actually produced, rather than strings a test chose.
func TestIntegrationBudgetHoldsAgainstRealProjections(t *testing.T) {
	h := newHarness(t)

	for i, text := range []string{
		"Acme Corporation supplies industrial fasteners to the Portland plant, and has done so since 2019 under a framework agreement renewed each January.",
		"The Portland plant runs a night shift on Thursdays. Throughput on those shifts is roughly forty percent of a weekday.",
		"Globex Industries took over the Salem distribution route in March. Their fleet is smaller but their coverage is denser.",
		"Fastener stock is counted weekly. Discrepancies above two percent trigger a manual recount and a note to the supplier.",
	} {
		h.ingest(t, text, "budget-"+itoa(i))
	}

	estimator := contextblock.NewHeuristicEstimator()
	sawContent := false
	for _, budget := range []int{120, 200, 400, 900, 2000} {
		block, err := h.assembler.Assemble(t.Context(), domain.ContextRequest{
			Scope: h.scope(), Query: "what happens at the Portland plant", TokenBudget: budget,
		})
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		if actual := estimator.Estimate(block.Text); actual > budget {
			t.Fatalf("budget %d exceeded: %d tokens\n%s", budget, actual, block.Text)
		}
		sawContent = sawContent || len(block.Items) > 0
	}
	if !sawContent {
		// A block that is always empty satisfies any budget. The ceiling is only
		// interesting while there is content pressing against it.
		t.Fatal("no budget produced any content, so the ceiling was never tested")
	}
}

// TestIntegrationContextIsGraphSpaceScoped holds the invariant one more layer out. Assembly
// reads projections through retrieval and canonical records through hydration, and either
// path is a place tenant scope could be dropped.
func TestIntegrationContextIsGraphSpaceScoped(t *testing.T) {
	h := newHarness(t)

	event, episode := h.ingest(t, "Acme Corporation supplies industrial fasteners.", "scope-1")
	h.claim(t, event, episode, knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
		Predicate: "supplies",
		Object:    domain.ObjectOfString("industrial fasteners"),
	})

	other := h.fixture.NewTenant(t, "globex")
	block, err := h.assembler.Assemble(t.Context(), domain.ContextRequest{
		Scope: other.Scope(), Query: "who supplies industrial fasteners", TokenBudget: 1200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(block.Items) != 0 {
		t.Fatalf("another tenant's content leaked into the block:\n%s", block.Text)
	}
	if strings.Contains(block.Text, "Acme") {
		t.Fatalf("another tenant's content leaked into the text:\n%s", block.Text)
	}
}

// TestIntegrationTemporalFiltersReachAssembly checks that asking as of a past instant
// changes what is presented as current, not merely what is retrieved.
func TestIntegrationTemporalFiltersReachAssembly(t *testing.T) {
	h := newHarness(t)

	event, episode := h.ingest(t, "Acme Corporation was on the legacy tier, then moved to premium.", "temporal-1")

	switchover := mustTime(t, "2026-03-01T00:00:00Z")
	h.claim(t, event, episode, knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
		Predicate: "tier",
		Object:    domain.ObjectOfSymbol("LEGACY"),
		ValidTo:   &switchover,
	})
	h.claim(t, event, episode, knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
		Predicate: "tier",
		Object:    domain.ObjectOfSymbol("PREMIUM"),
		ValidFrom: &switchover,
	})

	before := mustTime(t, "2026-02-01T00:00:00Z")
	block, err := h.assembler.Assemble(t.Context(), domain.ContextRequest{
		Scope:       h.scope(),
		Query:       "what tier is Acme Corporation on",
		TokenBudget: 1200,
		Temporal:    domain.TemporalQuery{ValidAt: &before},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(block.Items) == 0 {
		t.Fatalf("the as-of query returned nothing, so the assertion below proves nothing:\n%s", block.Text)
	}
	if !strings.Contains(block.Text, "LEGACY") {
		t.Fatalf("the value that held at that instant is missing:\n%s", block.Text)
	}
	for _, item := range block.Items {
		if item.Section == domain.SectionFacts && strings.Contains(item.Text, "PREMIUM") {
			t.Fatalf("a claim that had not started yet was presented as current:\n%s", block.Text)
		}
	}
	if !strings.Contains(block.Text, "as of: 2026-02-01") {
		t.Fatalf("the block should state the instant it describes:\n%s", block.Text)
	}
}
