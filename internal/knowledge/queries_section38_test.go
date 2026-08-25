package knowledge_test

import (
	"context"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/knowledge"
)

// The three queries from AGENTS.md section 38 that the data model could record but not
// answer: everything needed was already in the ledger, and none of it was reachable
// through a query. Section 38 makes that a defect rather than a gap to fill later —
// "if the data model cannot naturally answer these, change the implementation before
// adding more features".

// "Which assertions were inferred rather than directly observed?"
//
// The ledger has always refused to disguise inference as observation: provenance_mode is
// written on every claim and inferred claims must name a derivation. Until now the
// distinction could only be read one claim at a time.
func TestIntegrationQueryWhichAssertionsWereInferred(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	eventID, episodeID, _ := h.ingestEpisode(t,
		"Acme ordered fasteners in January, February, and March.", "inferred-1")

	observed := h.assertOne(t, eventID, knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
		Predicate: "ORDER_COUNT",
		Object:    domain.ObjectOfInteger(3),
		Evidence:  []knowledge.EvidenceInput{{EpisodeID: episodeID}},
	})

	result, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope:         h.scope(),
		Principal:     h.principal(),
		SourceEventID: eventID,
		Derivation: &knowledge.DerivationInput{
			Method:            "rule_inference",
			RuleName:          "recurring_customer",
			RuleVersion:       "1",
			InputAssertionIDs: []domain.AssertionID{observed.ID},
		},
		Claims: []knowledge.Claim{{
			Subject:        knowledge.EntityRef{Name: "Acme", Type: "organization"},
			Predicate:      "IS_RECURRING_CUSTOMER",
			Object:         domain.ObjectOfBool(true),
			ProvenanceMode: domain.ProvenanceInferred,
		}},
	})
	if err != nil {
		t.Fatalf("assert the inferred claim: %v", err)
	}
	inferred := result.Assertions[0]

	found := h.query(t, domain.AssertionQuery{
		ProvenanceModes: []domain.ProvenanceMode{domain.ProvenanceInferred},
	})
	if len(found) != 1 {
		t.Fatalf("expected exactly the inferred claim, got %d: %v", len(found), objectsOf(found))
	}
	if found[0].ID != inferred.ID {
		t.Fatalf("expected %s, got %s", inferred.ID, found[0].ID)
	}

	// And the complement, because a filter that returned everything would also pass the
	// assertion above if the inferred claim happened to sort first.
	observedOnly := h.query(t, domain.AssertionQuery{
		ProvenanceModes: []domain.ProvenanceMode{domain.ProvenanceExtracted},
	})
	for _, a := range observedOnly {
		if a.ID == inferred.ID {
			t.Fatal("an inferred claim was returned as directly observed")
		}
	}
	if len(observedOnly) != 1 || observedOnly[0].ID != observed.ID {
		t.Fatalf("expected exactly the observed claim, got %v", objectsOf(observedOnly))
	}
}

// "Find facts about this entity only from audited sources."
//
// Trust is a property of the registered source, not of the claim, so the filter narrows
// through the event that produced it. An authority floor rather than an exact set: the
// question is "at least this trustworthy", and enumerating levels at the call site would
// put the ranking in two places.
func TestIntegrationQueryFactsFromAuditedSourcesOnly(t *testing.T) {
	h := newHarness(t)

	audited := h.registerSource(t, "system-of-record", domain.TrustAuthoritative)
	rumour := h.registerSource(t, "public-forum", domain.TrustUntrusted)

	auditedEvent := h.ingestFrom(t, audited,
		"Acme's registered address is 14 Kelvin Way, Glasgow.", "audited-1", "acme", "1")
	rumourEvent := h.ingestFrom(t, rumour,
		"Someone said Acme moved to Edinburgh.", "rumour-1", "acme", "1")

	trusted := h.assertOne(t, auditedEvent, knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
		Predicate: "REGISTERED_CITY",
		Object:    domain.ObjectOfString("Glasgow"),
		ScopeKey:  "registered",
		Evidence:  []knowledge.EvidenceInput{{EpisodeID: h.episodeOf(t, auditedEvent)}},
	})
	hearsay := h.assertOne(t, rumourEvent, knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
		Predicate: "REGISTERED_CITY",
		Object:    domain.ObjectOfString("Edinburgh"),
		ScopeKey:  "rumoured",
		Evidence:  []knowledge.EvidenceInput{{EpisodeID: h.episodeOf(t, rumourEvent)}},
	})

	// Without the filter both claims are believed: an untrusted source is recorded and
	// findable, just ranked lower. That is the behaviour this filter exists to override.
	all := h.query(t, domain.AssertionQuery{Predicates: []string{"REGISTERED_CITY"}})
	if len(all) != 2 {
		t.Fatalf("expected both claims to be recorded, got %v", objectsOf(all))
	}

	found := h.query(t, domain.AssertionQuery{
		Predicates:    []string{"REGISTERED_CITY"},
		MinTrustLevel: domain.TrustHigh,
	})
	if len(found) != 1 {
		t.Fatalf("expected only the audited claim, got %v", objectsOf(found))
	}
	if found[0].ID != trusted.ID {
		t.Fatalf("expected the claim from the authoritative source, got %s", found[0].ID)
	}
	if found[0].ID == hearsay.ID {
		t.Fatal("an untrusted source's claim passed an authority floor")
	}

	// A floor below both returns both, so the filter is a threshold rather than a
	// synonym for "the most trusted source".
	loose := h.query(t, domain.AssertionQuery{
		Predicates:    []string{"REGISTERED_CITY"},
		MinTrustLevel: domain.TrustUntrusted,
	})
	if len(loose) != 2 {
		t.Fatalf("a floor of untrusted must exclude nothing, got %v", objectsOf(loose))
	}
}

// "What changed since source version 1837?"
//
// A downstream consumer asking what it has not seen yet. The cursor follows the source's
// own ordering rather than arrival, so a backfill replaying a year of history in minutes
// does not read as a year of change (ADR 0010).
func TestIntegrationQueryWhatChangedSinceASourceVersion(t *testing.T) {
	h := newHarness(t)

	crm := h.registerSource(t, "crm", domain.TrustHigh)

	// Versions either side of the cursor, and one far below it that must not sneak in
	// through string comparison: "999" is lexicographically after "1837".
	versions := []string{"999", "1837", "1838", "2100"}
	byVersion := map[string]domain.AssertionID{}
	for _, version := range versions {
		event := h.ingestFrom(t, crm,
			"Acme's plan at version "+version+".", "crm-"+version, "acme-plan", version)
		claim := h.assertOne(t, event, knowledge.Claim{
			Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
			Predicate: "CURRENT_PLAN",
			Object:    domain.ObjectOfString("plan-" + version),
			ScopeKey:  version,
			Evidence:  []knowledge.EvidenceInput{{EpisodeID: h.episodeOf(t, event)}},
		})
		byVersion[version] = claim.ID
	}

	found := h.query(t, domain.AssertionQuery{
		SourceIDs:    []domain.SourceID{crm},
		ChangedSince: &domain.SourcePosition{Version: "1837"},
		Statuses: []domain.AssertionStatus{
			domain.AssertionActive, domain.AssertionDisputed, domain.AssertionSuperseded,
		},
	})

	got := map[domain.AssertionID]bool{}
	for _, a := range found {
		got[a.ID] = true
	}
	for _, version := range []string{"1838", "2100"} {
		if !got[byVersion[version]] {
			t.Fatalf("version %s is after the cursor and should have been returned", version)
		}
	}
	for _, version := range []string{"999", "1837"} {
		if got[byVersion[version]] {
			t.Fatalf("version %s is not after the cursor; %s", version,
				map[bool]string{true: "the comparison is lexicographic, not numeric",
					false: "the cursor is inclusive"}[version == "999"])
		}
	}
}

// A cursor without a source is refused rather than answered, because a sequence from one
// system says nothing about a sequence from another. Answering would invent an order
// neither source stated — the failure ADR 0010 exists to prevent.
func TestChangedSinceRequiresExactlyOneSource(t *testing.T) {
	base := domain.AssertionQuery{
		Scope:        domain.Scope{WorkspaceID: "01a00000-0000-7000-8000-000000000001"},
		ChangedSince: &domain.SourcePosition{Version: "1837"},
	}

	if err := base.Validate(); err == nil {
		t.Fatal("a cursor with no source must be refused")
	} else if !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("expected invalid_argument, got %s", domain.CodeOf(err))
	}

	twoSources := base
	twoSources.SourceIDs = []domain.SourceID{"a", "b"}
	if err := twoSources.Validate(); err == nil {
		t.Fatal("a cursor spanning two sources must be refused")
	}

	empty := base
	empty.SourceIDs = []domain.SourceID{"a"}
	empty.ChangedSince = &domain.SourcePosition{}
	if err := empty.Validate(); err == nil {
		t.Fatal("a cursor with nothing to compare must be refused")
	}

	valid := base
	valid.SourceIDs = []domain.SourceID{"a"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a single-source cursor is valid: %v", err)
	}
}

// TrustLevelsAtLeast is the ranking the store turns into set membership, so it has to
// agree with the ordering conflict resolution already uses.
func TestTrustLevelsAtLeastMatchesTheAuthorityRanking(t *testing.T) {
	cases := []struct {
		min  domain.TrustLevel
		want []domain.TrustLevel
	}{
		{domain.TrustAuthoritative, []domain.TrustLevel{domain.TrustAuthoritative}},
		{domain.TrustHigh, []domain.TrustLevel{domain.TrustHigh, domain.TrustAuthoritative}},
		{domain.TrustUntrusted, []domain.TrustLevel{
			domain.TrustUntrusted, domain.TrustLow, domain.TrustStandard,
			domain.TrustHigh, domain.TrustAuthoritative,
		}},
		// No floor means no narrowing, not "nothing qualifies".
		{"", nil},
	}

	for _, tc := range cases {
		got := domain.TrustLevelsAtLeast(tc.min)
		if len(got) != len(tc.want) {
			t.Fatalf("floor %q: expected %v, got %v", tc.min, tc.want, got)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("floor %q: expected %v, got %v", tc.min, tc.want, got)
			}
		}
	}
}
