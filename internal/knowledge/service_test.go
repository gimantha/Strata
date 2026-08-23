package knowledge_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
)

func TestIntegrationAssertIsIdempotentOnReplay(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	eventID, episodeID, _ := h.ingestEpisode(t, "Acme is headquartered in Berlin.", "hq-1")
	claim := knowledge.Claim{
		Subject:      knowledge.EntityRef{Name: "Acme", Type: "organization"},
		Predicate:    "HEADQUARTERED_IN",
		ObjectEntity: &knowledge.EntityRef{Name: "Berlin", Type: "place"},
		Evidence:     []knowledge.EvidenceInput{{EpisodeID: episodeID}},
	}
	req := knowledge.AssertRequest{
		Scope: h.scope(), Principal: h.principal(), SourceEventID: eventID,
		Claims: []knowledge.Claim{claim},
	}

	first, err := h.service.Assert(ctx, req)
	if err != nil {
		t.Fatalf("first assert: %v", err)
	}
	if first.Duplicates != 0 {
		t.Fatal("the first commit is not a duplicate")
	}

	// Reprocessing the same event must not create a second copy of the same knowledge.
	second, err := h.service.Assert(ctx, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if second.Duplicates != 1 {
		t.Fatalf("a replay must be recognized, got %d duplicates", second.Duplicates)
	}
	if second.Assertions[0].ID != first.Assertions[0].ID {
		t.Fatal("a replay must resolve to the original assertion")
	}

	believed := h.query(t, domain.AssertionQuery{Predicates: []string{"HEADQUARTERED_IN"}})
	if len(believed) != 1 {
		t.Fatalf("expected exactly one claim after replay, got %d", len(believed))
	}
	evidence, err := h.fixture.Store.ListEvidence(ctx, h.scope().WorkspaceID, first.Assertions[0].ID)
	if err != nil {
		t.Fatalf("list evidence: %v", err)
	}
	if len(evidence) != 1 {
		t.Fatalf("a replay must not duplicate evidence either, got %d records", len(evidence))
	}

	// The same statement from a different source event is separate knowledge, because
	// independent corroboration is real information.
	otherEvent, otherEpisode, _ := h.ingestEpisode(t, "Acme's head office is in Berlin.", "hq-2")
	corroborating := claim
	corroborating.Evidence = []knowledge.EvidenceInput{{EpisodeID: otherEpisode}}
	third, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: h.scope(), Principal: h.principal(), SourceEventID: otherEvent,
		Claims: []knowledge.Claim{corroborating},
	})
	if err != nil {
		t.Fatalf("corroborating assert: %v", err)
	}
	if third.Duplicates != 0 || third.Assertions[0].ID == first.Assertions[0].ID {
		t.Fatal("the same statement from another event must be its own claim")
	}
}

func TestIntegrationTypedObjectsRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	eventID, episodeID, _ := h.ingestEpisode(t, "Assorted facts about Acme.", "typed-1")

	cases := []struct {
		predicate string
		object    domain.AssertionObject
	}{
		{"LEGAL_NAME", domain.ObjectOfString("Acme Corporation GmbH")},
		{"EMPLOYEE_COUNT", domain.ObjectOfInteger(1234)},
		// A decimal a float cannot represent exactly.
		{"ANNUAL_REVENUE", domain.ObjectOfDecimal("12345678.90")},
		{"IS_PUBLICLY_TRADED", domain.ObjectOfBool(false)},
		{"FOUNDED_ON", domain.ObjectOfDate(date(1998, time.July, 14))},
		{"LAST_AUDITED_AT", domain.ObjectOfTimestamp(time.Date(2026, 2, 3, 14, 30, 0, 0, time.UTC))},
		{"SUPPORT_SLA", domain.ObjectOfDuration(4 * time.Hour)},
		{"HQ_COORDINATES", domain.ObjectOfGeo(52.520008, 13.404954)},
		{"WEBSITE", domain.ObjectOfURI("https://acme.example/about")},
		{"TIER", domain.ObjectOfSymbol("ENTERPRISE")},
		{"RAW_PROFILE", domain.ObjectOfJSON(json.RawMessage(`{"b":2,"a":[1,2,3]}`))},
	}

	for _, tc := range cases {
		t.Run(tc.predicate, func(t *testing.T) {
			committed := h.assertOne(t, eventID, knowledge.Claim{
				Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
				Predicate: tc.predicate,
				Object:    tc.object,
				Evidence:  []knowledge.EvidenceInput{{EpisodeID: episodeID}},
			})

			reloaded, err := h.service.Get(ctx, h.scope().WorkspaceID, committed.ID)
			if err != nil {
				t.Fatalf("reload: %v", err)
			}
			if reloaded.Object.Kind != tc.object.Kind {
				t.Fatalf("object kind changed: %s became %s", tc.object.Kind, reloaded.Object.Kind)
			}
			// Values keep their type and precision through the database round trip.
			if !reloaded.Object.Equal(tc.object) {
				t.Fatalf("value changed in storage: %q became %q",
					tc.object.Display(), reloaded.Object.Display())
			}
		})
	}

	// Every value is queryable by its canonical key.
	found := h.query(t, domain.AssertionQuery{
		Predicates: []string{"ANNUAL_REVENUE"},
		ObjectKey:  domain.ObjectOfDecimal("12345678.90").Key(),
	})
	if len(found) != 1 {
		t.Fatalf("expected to find the decimal claim by value, got %d", len(found))
	}
	// Equal values written differently must match, since 12345678.9 is the same number.
	sameValue := h.query(t, domain.AssertionQuery{
		Predicates: []string{"ANNUAL_REVENUE"},
		ObjectKey:  domain.ObjectOfDecimal("12345678.9000").Key(),
	})
	if len(sameValue) != 1 {
		t.Fatal("equal decimals written differently must compare equal")
	}
}

func TestIntegrationRetractionIsAKnowledgeTimeEvent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	recorded := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	retracted := time.Date(2026, 5, 20, 10, 0, 0, 0, time.UTC)
	h.clock.Set(recorded)

	eventID, episodeID, _ := h.ingestEpisode(t, "Acme uses the Fastener API.", "retract-1")
	claim := h.assertOne(t, eventID, knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
		Predicate: "USES_PRODUCT",
		Object:    domain.ObjectOfSymbol("FASTENER_API"),
		Evidence:  []knowledge.EvidenceInput{{EpisodeID: episodeID}},
	})

	h.clock.Set(retracted)
	withdrawn, err := h.service.Retract(ctx, h.scope().WorkspaceID, claim.ID,
		"reported in error by the source", h.fixture.Primary.Principal.ID)
	if err != nil {
		t.Fatalf("retract: %v", err)
	}
	if withdrawn.Status != domain.AssertionRetracted {
		t.Fatalf("expected retracted status, got %s", withdrawn.Status)
	}
	if withdrawn.RetractionReason == "" {
		t.Fatal("a retraction must record why")
	}

	// Current belief no longer includes it.
	if current := h.query(t, domain.AssertionQuery{Predicates: []string{"USES_PRODUCT"}}); len(current) != 0 {
		t.Fatalf("a retracted claim must not be current belief, got %d", len(current))
	}

	// But the system did believe it before the retraction, and still says so.
	between := time.Date(2026, 5, 10, 10, 0, 0, 0, time.UTC)
	historical := h.query(t, domain.AssertionQuery{
		Predicates: []string{"USES_PRODUCT"},
		KnownAt:    &between,
	})
	if len(historical) != 1 {
		t.Fatalf("the claim was believed on May 10 and must still be reported as such, got %d",
			len(historical))
	}

	// Retracting twice is refused rather than silently moving the instant belief changed.
	if _, err := h.service.Retract(ctx, h.scope().WorkspaceID, claim.ID, "again",
		h.fixture.Primary.Principal.ID); err == nil {
		t.Fatal("a second retraction must be refused")
	}
}

func TestIntegrationTemporalFilters(t *testing.T) {
	h := newHarness(t)

	eventID, episodeID, _ := h.ingestEpisode(t, "Acme office moves.", "moves-1")

	// Two consecutive tenancies that meet exactly: one ends when the next begins.
	h.assertOne(t, eventID, knowledge.Claim{
		Subject:      knowledge.EntityRef{Name: "Acme", Type: "organization"},
		Predicate:    "OFFICE_AT",
		ObjectEntity: &knowledge.EntityRef{Name: "Berlin", Type: "place"},
		ValidFrom:    ptr(date(2024, time.January, 1)),
		ValidTo:      ptr(date(2026, time.January, 1)),
		Evidence:     []knowledge.EvidenceInput{{EpisodeID: episodeID}},
	})
	h.assertOne(t, eventID, knowledge.Claim{
		Subject:      knowledge.EntityRef{Name: "Acme", Type: "organization"},
		Predicate:    "OFFICE_AT",
		ObjectEntity: &knowledge.EntityRef{Name: "Munich", Type: "place"},
		ValidFrom:    ptr(date(2026, time.January, 1)),
		Evidence:     []knowledge.EvidenceInput{{EpisodeID: episodeID}},
	})

	// A clean handover is not a contradiction, so both claims stand.
	all := h.query(t, domain.AssertionQuery{Predicates: []string{"OFFICE_AT"}})
	if len(all) != 2 {
		t.Fatalf("consecutive tenancies must coexist, got %d", len(all))
	}

	during2025 := h.query(t, domain.AssertionQuery{
		Predicates: []string{"OFFICE_AT"},
		ValidAt:    ptr(date(2025, time.June, 1)),
	})
	if len(during2025) != 1 {
		t.Fatalf("expected one office in mid-2025, got %d", len(during2025))
	}

	// The boundary instant belongs to the new interval, not the old one: intervals are
	// half-open.
	atBoundary := h.query(t, domain.AssertionQuery{
		Predicates: []string{"OFFICE_AT"},
		ValidAt:    ptr(date(2026, time.January, 1)),
	})
	if len(atBoundary) != 1 {
		t.Fatalf("exactly one claim must hold at the handover instant, got %d", len(atBoundary))
	}

	overlapping := h.query(t, domain.AssertionQuery{
		Predicates: []string{"OFFICE_AT"},
		ValidBetween: &domain.TimeRange{
			Start: date(2025, time.December, 1),
			End:   date(2026, time.February, 1),
		},
	})
	if len(overlapping) != 2 {
		t.Fatalf("a range spanning the handover must return both, got %d", len(overlapping))
	}

	// Nothing was true before the first tenancy began.
	before := h.query(t, domain.AssertionQuery{
		Predicates: []string{"OFFICE_AT"},
		ValidAt:    ptr(date(2020, time.January, 1)),
	})
	if len(before) != 0 {
		t.Fatalf("no claim covers 2020, got %d", len(before))
	}
}

func TestIntegrationContextLifecycleIsSeparateFromTruth(t *testing.T) {
	h := newHarness(t)

	// Scenario E in miniature: ephemeral context stops being active without ceasing to
	// be historically true (AGENTS.md section 37).
	eventID, episodeID, _ := h.ingestEpisode(t, "The user is staying at the Hilton tonight.", "stay-1")

	tonight := date(2026, time.June, 1)
	tomorrow := date(2026, time.June, 2)
	h.assertOne(t, eventID, knowledge.Claim{
		Subject:      knowledge.EntityRef{Name: "The user", Type: "person"},
		Predicate:    "STAYING_AT",
		ObjectEntity: &knowledge.EntityRef{Name: "Hilton", Type: "place"},
		MemoryKind:   domain.MemoryWorking,
		ValidFrom:    &tonight,
		ValidTo:      &tomorrow,
		ActiveFrom:   &tonight,
		ExpiresAt:    &tomorrow,
		Evidence:     []knowledge.EvidenceInput{{EpisodeID: episodeID}},
	})

	activeTonight := h.query(t, domain.AssertionQuery{
		Predicates: []string{"STAYING_AT"},
		ActiveAt:   &tonight,
	})
	if len(activeTonight) != 1 {
		t.Fatalf("the stay should be active context tonight, got %d", len(activeTonight))
	}

	activeTomorrow := h.query(t, domain.AssertionQuery{
		Predicates: []string{"STAYING_AT"},
		ActiveAt:   ptr(tomorrow.Add(time.Hour)),
	})
	if len(activeTomorrow) != 0 {
		t.Fatal("the stay must stop being active context after it expires")
	}

	// It remains historically true, and remains evidence.
	historical := h.query(t, domain.AssertionQuery{
		Predicates: []string{"STAYING_AT"},
		ValidAt:    ptr(tonight.Add(12 * time.Hour)),
	})
	if len(historical) != 1 {
		t.Fatal("expiry must not delete history: the stay still happened")
	}
}

func TestIntegrationAmbiguousEntityNameIsRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Two distinct people share a name. Resolution is phase 4's job; until then the
	// service must refuse to guess rather than silently merge them.
	for i := 0; i < 2; i++ {
		if _, err := h.fixture.Store.CreateEntity(ctx, domain.Entity{
			WorkspaceID:   h.scope().WorkspaceID,
			GraphSpaceID:  h.scope().GraphSpaceID,
			CanonicalName: "Alex Kim",
			EntityType:    "person",
		}); err != nil {
			t.Fatalf("create entity: %v", err)
		}
	}

	eventID, episodeID, _ := h.ingestEpisode(t, "Alex Kim approved the order.", "ambiguous-1")
	_, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: h.scope(), Principal: h.principal(), SourceEventID: eventID,
		Claims: []knowledge.Claim{{
			Subject:   knowledge.EntityRef{Name: "Alex Kim", Type: "person"},
			Predicate: "APPROVED",
			Object:    domain.ObjectOfSymbol("ORDER"),
			Evidence:  []knowledge.EvidenceInput{{EpisodeID: episodeID}},
		}},
	})
	if err == nil {
		t.Fatal("an ambiguous name must not be resolved by guessing")
	}
	if !domain.IsCode(err, domain.CodeConflict) {
		t.Fatalf("expected conflict, got %s: %v", domain.CodeOf(err), err)
	}
}

func TestIntegrationKnowledgeIsWorkspaceScoped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	eventID, episodeID, _ := h.ingestEpisode(t, "Acme is a supplier.", "iso-1")
	claim := h.assertOne(t, eventID, knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
		Predicate: "CLASSIFIED_AS",
		Object:    domain.ObjectOfSymbol("SUPPLIER"),
		Evidence:  []knowledge.EvidenceInput{{EpisodeID: episodeID}},
	})

	other := h.fixture.NewTenant(t, "globex")

	// A valid identifier from another tenant must not resolve, through any read path.
	if _, err := h.service.Get(ctx, other.Workspace.ID, claim.ID); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("cross-tenant assertion read must fail, got %s", domain.CodeOf(err))
	}
	if _, err := h.service.Provenance(ctx, other.Workspace.ID, claim.ID); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("cross-tenant provenance walk must fail, got %s", domain.CodeOf(err))
	}
	found, err := h.service.Query(ctx, domain.AssertionQuery{Scope: other.Scope()})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("another tenant must see none of this knowledge, got %d", len(found))
	}

	// Attaching a claim to another tenant's source event must fail too, or provenance
	// chains could be forged across the boundary.
	if _, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: other.Scope(), Principal: other.Principal.Ref(), SourceEventID: eventID,
		Claims: []knowledge.Claim{{
			Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
			Predicate: "CLASSIFIED_AS",
			Object:    domain.ObjectOfSymbol("SUPPLIER"),
		}},
	}); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("expected not_found for a cross-tenant source event, got %s", domain.CodeOf(err))
	}
}

func TestIntegrationClassificationPropagatesToClaims(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	confidential, err := h.fixture.Store.CreateSource(ctx, domain.Source{
		WorkspaceID:    h.scope().WorkspaceID,
		Kind:           domain.SourceKindDatabase,
		Name:           "hr-system",
		TrustLevel:     domain.TrustHigh,
		Classification: domain.ClassificationConfidential,
	}, h.fixture.Primary.Principal.ID)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	receipt, err := h.gateway.Accept(ctx, ingestRequestFor(h, confidential.ID, "Salary band: senior."))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := h.runner.Process(ctx, h.scope().WorkspaceID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	episodes, err := h.fixture.Store.ListEpisodes(ctx, h.scope().WorkspaceID, receipt.SourceEventID)
	if err != nil || len(episodes) == 0 {
		t.Fatalf("expected episodes: %v", err)
	}

	claim := h.assertOne(t, receipt.SourceEventID, knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Employee 42", Type: "person"},
		Predicate: "SALARY_BAND",
		Object:    domain.ObjectOfSymbol("SENIOR"),
		// A claim asking for a weaker label must not lower what the source demands.
		Classification: domain.ClassificationPublic,
		Evidence:       []knowledge.EvidenceInput{{EpisodeID: episodes[0].ID}},
	})

	if claim.Classification != domain.ClassificationConfidential {
		t.Fatalf("sensitivity must propagate from the source event, got %q", claim.Classification)
	}
}

// ingestRequestFor builds an ingest request against a specific source.
func ingestRequestFor(h *harness, sourceID domain.SourceID, content string) ingest.Request {
	return ingest.Request{
		Scope:          h.scope(),
		Principal:      h.principal(),
		SourceID:       sourceID,
		MediaType:      "text/plain",
		Payload:        []byte(content),
		IdempotencyKey: "classification-" + content[:8],
	}
}
