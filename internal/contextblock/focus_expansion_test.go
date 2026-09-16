package contextblock

import (
	"strings"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

func TestRequirementExpansionFollowsTheQuestionsSubject(t *testing.T) {
	c := newCorpus()
	wanted := c.entity("CASE-42")
	other := c.entity("CASE-99")
	c.claim(wanted, "BUDGET", "125", claimOptions{score: .20})
	c.claim(wanted, "BUDGET", "100", claimOptions{score: .01, status: domain.AssertionSuperseded})
	c.claim(other, "BUDGET", "900", claimOptions{score: .18})
	c.claim(other, "BUDGET", "800", claimOptions{score: .01, status: domain.AssertionSuperseded})

	req := c.request(1500)
	req.Query = "What was the budget of CASE-42 before the change?"
	req.Explain = true
	block, err := c.assembler(Options{}).Assemble(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block.Text, "125") || !strings.Contains(block.Text, "100") {
		t.Fatalf("the subject's current and historical values belong in the block:\n%s", block.Text)
	}
	// The other case's history used to arrive as a required premise and bypass the focus
	// gate, so a question about one record stated another record's budget as fact.
	if strings.Contains(block.Text, "900") || strings.Contains(block.Text, "800") || strings.Contains(block.Text, "CASE-99") {
		t.Fatalf("history of an unrelated record leaked into the block:\n%s", block.Text)
	}
}

func TestAsOfQuestionShowsTheClaimThatEndedTheInterval(t *testing.T) {
	c := newCorpus()
	case42 := c.entity("CASE-42")
	jan1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	jan12 := time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC)
	c.claim(case42, "OWNER", "Mara", claimOptions{score: .20, validFrom: &jan1, validTo: &jan12})
	c.claim(case42, "OWNER", "Theo", claimOptions{score: .01, validFrom: &jan12})
	// Retrieval found only the claim in force at the instant asked about.
	c.retriever.items = c.retriever.items[:1]

	req := c.request(1500)
	req.Query = "Who was the owner of CASE-42 on 2026-01-09?"
	req.Explain = true
	block, err := c.assembler(Options{}).Assemble(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	facts := strings.Index(block.Text, "## KNOWN FACTS")
	history := strings.Index(block.Text, "## HISTORICAL")
	mara := strings.Index(block.Text, "owner Mara")
	theo := strings.Index(block.Text, "owner Theo")
	if facts < 0 || history < 0 || mara < 0 || theo < 0 {
		t.Fatalf("expected both owners with the successor under history:\n%s", block.Text)
	}
	if !(facts < mara && mara < history && history < theo) {
		t.Fatalf("the claim in force belongs under facts and the one that ended it under history:\n%s", block.Text)
	}
	if !strings.Contains(block.Text, "since 2026-01-12") {
		t.Fatalf("the successor must carry its start so it cannot be read as the state on the 9th:\n%s", block.Text)
	}
}

func TestRequiredPremiseBringsItsSourceExcerpt(t *testing.T) {
	c := newCorpus()
	caseID := c.entity("CASE-42")
	supportID := c.claim(caseID, "RISK_SCORE", "91", claimOptions{score: .01})
	decisionID := c.claim(caseID, "DECISION", "manual review", claimOptions{score: .20})

	passage := "Audit of CASE-42: the risk score was 91 after the second review, above the threshold of 80."
	support := c.store.chains[supportID]
	support.Links[0].Chunk = &domain.Chunk{ID: domain.ChunkID("chunk-support"), Content: passage,
		EpisodeID: support.Links[0].Episode.ID}
	support.Links[0].SourceEvent = domain.SourceEvent{ID: domain.SourceEventID("event-support"), IdempotencyKey: "AUDIT-7"}
	c.store.chains[supportID] = support
	c.store.chains[decisionID] = domain.ProvenanceChain{
		Assertion: c.store.assertions[decisionID], Subject: c.store.entities[caseID],
		Links:    c.store.chains[decisionID].Links,
		Supports: []domain.Assertion{c.store.assertions[supportID]},
	}
	// Retrieval found only the decision.
	c.retriever.items = c.retriever.items[1:]

	req := c.request(1500)
	req.Query = "Why was the decision taken for CASE-42?"
	req.Explain = true
	block, err := c.assembler(Options{}).Assemble(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block.Text, passage) {
		t.Fatalf("the premise's source passage must be quotable from the block:\n%s", block.Text)
	}
	found := false
	for _, citation := range block.Citations {
		found = found || (citation.Surface == domain.SurfaceChunk && citation.IdempotencyKey == "AUDIT-7")
	}
	if !found {
		t.Fatal("the companion excerpt must be cited back to its source event")
	}
}

func TestComparisonProofAcceptsUnitBearingAmounts(t *testing.T) {
	c := newCorpus()
	project := c.entity("Project-001")
	oldID := c.claim(project, "APPROVED_BUDGET", "USD 12,100", claimOptions{score: .08, status: domain.AssertionSuperseded})
	newID := c.claim(project, "APPROVED_BUDGET", "USD 14200", claimOptions{score: .09})
	old := c.store.assertions[oldID]
	old.Temporal.RecordedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := c.store.assertions[newID]
	newer.Temporal.RecordedAt = time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	c.store.assertions[oldID], c.store.assertions[newID] = old, newer
	c.store.chains[oldID] = domain.ProvenanceChain{Assertion: old, Subject: c.store.entities[project], Links: c.store.chains[oldID].Links}
	c.store.chains[newID] = domain.ProvenanceChain{Assertion: newer, Subject: c.store.entities[project], Links: c.store.chains[newID].Links}

	req := c.request(1500)
	req.Query = "By how much did the approved budget of Project-001 increase?"
	req.Explain = true
	block, err := c.assembler(Options{}).Assemble(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block.Text, "exact delta 2100 USD") {
		t.Fatalf("a delta between two same-unit amounts must be derived:\n%s", block.Text)
	}
}

func TestQuantityOfReadsUnitsAndRejectsNonsense(t *testing.T) {
	for _, tc := range []struct {
		text  string
		want  string
		unit  string
		valid bool
	}{
		{"USD 12,100", "12100", "USD", true},
		{"$99.50", "199/2", "$", true},
		{"45 %", "45", "%", true},
		{"14200 EUR", "14200", "EUR", true},
		{"14200", "14200", "", true},
		{"USD 100 EUR", "", "", false},
		{"approved", "", "", false},
	} {
		value, unit, ok := quantityOf(tc.text)
		if ok != tc.valid || (ok && (value.RatString() != tc.want || unit != tc.unit)) {
			t.Fatalf("quantityOf(%q) = %v %q %v", tc.text, value, unit, ok)
		}
	}
}
