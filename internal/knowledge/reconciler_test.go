package knowledge_test

import (
	"context"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/normalize"
)

// definePredicate registers a predicate with explicit semantics.
func (h *harness) definePredicate(t *testing.T, name string, functional bool, policy domain.ConflictPolicy) {
	t.Helper()

	if _, err := h.fixture.Store.DefinePredicate(context.Background(), domain.PredicateDefinition{
		WorkspaceID:    h.scope().WorkspaceID,
		Name:           name,
		Functional:     functional,
		ConflictPolicy: policy,
		TemporalPolicy: domain.TemporalPolicyStateful,
	}, h.fixture.Primary.Principal.ID); err != nil {
		t.Fatalf("define predicate %s: %v", name, err)
	}
}

// ingestSequenced records source material carrying a position in the source's own ordering.
func (h *harness) ingestSequenced(t *testing.T, content, key, sequence string, sourceID domain.SourceID) (domain.SourceEventID, domain.EpisodeID) {
	t.Helper()
	ctx := context.Background()

	receipt, err := h.gateway.Accept(ctx, ingest.Request{
		Scope:          h.scope(),
		Principal:      h.principal(),
		SourceID:       sourceID,
		MediaType:      normalize.MediaTypePlain,
		Payload:        []byte(content),
		IdempotencyKey: key,
		SourceSequence: sequence,
		ExternalID:     "row-1",
	})
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
	return receipt.SourceEventID, episodes[0].ID
}

// Scenario B from AGENTS.md section 37: source sequence 102 arrives before 101. The system
// must preserve source order semantics and converge correctly once 101 arrives.
func TestIntegrationScenarioBOutOfOrderEvents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sourceID := h.fixture.Primary.Source.ID

	h.definePredicate(t, "CURRENT_PLAN", true, domain.ConflictPolicyLatestWins)

	claim := func(eventID domain.SourceEventID, episodeID domain.EpisodeID, value string) knowledge.AssertResult {
		result, err := h.service.Assert(ctx, knowledge.AssertRequest{
			Scope: h.scope(), Principal: h.principal(), SourceEventID: eventID,
			Claims: []knowledge.Claim{{
				Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
				Predicate: "CURRENT_PLAN",
				Object:    domain.ObjectOfSymbol(value),
				ValidFrom: ptr(date(2026, time.January, 1)),
				Evidence:  []knowledge.EvidenceInput{{EpisodeID: episodeID}},
			}},
		})
		if err != nil {
			t.Fatalf("assert %s: %v", value, err)
		}
		return result
	}

	// Update 102 arrives first.
	event102, episode102 := h.ingestSequenced(t, "Acme moved to the enterprise plan.", "seq-102", "102", sourceID)
	later := claim(event102, episode102, "ENTERPRISE")
	if len(later.SupersededOnArrival) != 0 {
		t.Fatal("the first claim seen cannot be out of date")
	}

	current := h.query(t, domain.AssertionQuery{Predicates: []string{"CURRENT_PLAN"}})
	if len(current) != 1 || current[0].Object.Display() != "ENTERPRISE" {
		t.Fatalf("expected the enterprise plan, got %v", objectsOf(current))
	}

	// Update 101 arrives late. It describes an older state of the same record, so it must
	// not overwrite what 102 already told us - however it arrived.
	event101, episode101 := h.ingestSequenced(t, "Acme is on the standard plan.", "seq-101", "101", sourceID)
	earlier := claim(event101, episode101, "STANDARD")

	if len(earlier.SupersededOnArrival) != 1 {
		t.Fatalf("a late earlier state must be superseded on arrival, got %d",
			len(earlier.SupersededOnArrival))
	}
	if len(earlier.Conflicts) != 0 {
		t.Fatal("an ordered pair of updates is not a conflict; the source said which came first")
	}

	// Converged state: exactly what in-order delivery would have produced.
	current = h.query(t, domain.AssertionQuery{Predicates: []string{"CURRENT_PLAN"}})
	if len(current) != 1 {
		t.Fatalf("expected one current belief, got %d: %v", len(current), objectsOf(current))
	}
	if current[0].Object.Display() != "ENTERPRISE" {
		t.Fatalf("a late older update must not become current belief, got %s",
			current[0].Object.Display())
	}

	// The late claim is recorded, not discarded: it is what the source said, and its
	// evidence survives.
	stored, err := h.service.Get(ctx, h.scope().WorkspaceID, earlier.Assertions[0].ID)
	if err != nil {
		t.Fatalf("the late claim must still be readable: %v", err)
	}
	if stored.Status != domain.AssertionSuperseded {
		t.Fatalf("expected the late claim to be superseded, got %s", stored.Status)
	}
	if stored.Temporal.SupersededAt == nil {
		t.Fatal("supersession must be stamped with when it happened")
	}
}

func TestIntegrationInOrderDeliveryConvergesIdentically(t *testing.T) {
	// The same two updates delivered in order must reach the same state as Scenario B's
	// reversed delivery. If they do not, arrival order is leaking into the result.
	h := newHarness(t)
	ctx := context.Background()
	sourceID := h.fixture.Primary.Source.ID

	h.definePredicate(t, "CURRENT_PLAN", true, domain.ConflictPolicyLatestWins)

	for _, step := range []struct{ key, sequence, value string }{
		{"in-101", "101", "STANDARD"},
		{"in-102", "102", "ENTERPRISE"},
	} {
		eventID, episodeID := h.ingestSequenced(t, "Plan update "+step.sequence, step.key, step.sequence, sourceID)
		if _, err := h.service.Assert(ctx, knowledge.AssertRequest{
			Scope: h.scope(), Principal: h.principal(), SourceEventID: eventID,
			Claims: []knowledge.Claim{{
				Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
				Predicate: "CURRENT_PLAN",
				Object:    domain.ObjectOfSymbol(step.value),
				ValidFrom: ptr(date(2026, time.January, 1)),
				Evidence:  []knowledge.EvidenceInput{{EpisodeID: episodeID}},
			}},
		}); err != nil {
			t.Fatalf("assert %s: %v", step.value, err)
		}
	}

	current := h.query(t, domain.AssertionQuery{Predicates: []string{"CURRENT_PLAN"}})
	if len(current) != 1 || current[0].Object.Display() != "ENTERPRISE" {
		t.Fatalf("in-order delivery must converge to the same state, got %v", objectsOf(current))
	}
}

func TestIntegrationHighestAuthorityResolvesDisagreement(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.definePredicate(t, "LEGAL_NAME", true, domain.ConflictPolicyHighestAuthority)

	// Two sources of different standing: a registry and a chat log.
	registry, err := h.fixture.Store.CreateSource(ctx, domain.Source{
		WorkspaceID: h.scope().WorkspaceID, Kind: domain.SourceKindDatabase,
		Name: "company-registry", TrustLevel: domain.TrustAuthoritative,
	}, h.fixture.Primary.Principal.ID)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	chat, err := h.fixture.Store.CreateSource(ctx, domain.Source{
		WorkspaceID: h.scope().WorkspaceID, Kind: domain.SourceKindChat,
		Name: "hallway-chat", TrustLevel: domain.TrustLow,
	}, h.fixture.Primary.Principal.ID)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	assertFrom := func(source domain.SourceID, key, value string) knowledge.AssertResult {
		eventID, episodeID := h.ingestSequenced(t, "Name is "+value, key, "", source)
		result, err := h.service.Assert(ctx, knowledge.AssertRequest{
			Scope: h.scope(), Principal: h.principal(), SourceEventID: eventID,
			Claims: []knowledge.Claim{{
				Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
				Predicate: "LEGAL_NAME",
				Object:    domain.ObjectOfString(value),
				Evidence:  []knowledge.EvidenceInput{{EpisodeID: episodeID}},
			}},
		})
		if err != nil {
			t.Fatalf("assert: %v", err)
		}
		return result
	}

	// The weak source speaks first.
	assertFrom(chat.ID, "auth-1", "Acme Inc")

	// The authoritative source disagrees and wins, without anyone having to intervene.
	authoritative := assertFrom(registry.ID, "auth-2", "Acme Corporation GmbH")
	if len(authoritative.Superseded) != 1 {
		t.Fatalf("a more authoritative source should supersede a weaker one, got %d superseded",
			len(authoritative.Superseded))
	}
	if len(authoritative.Conflicts) != 0 {
		t.Fatal("clear authority is not a conflict")
	}

	current := h.query(t, domain.AssertionQuery{Predicates: []string{"LEGAL_NAME"}})
	if len(current) != 1 || current[0].Object.Display() != "Acme Corporation GmbH" {
		t.Fatalf("the authoritative name should stand, got %v", objectsOf(current))
	}

	// The weak source repeating itself does not overturn the registry.
	rebuttal := assertFrom(chat.ID, "auth-3", "Acme Inc")
	if len(rebuttal.Superseded) != 0 {
		t.Fatal("a less authoritative source must not supersede a more authoritative one")
	}
	if len(rebuttal.Conflicts) != 1 {
		t.Fatalf("the disagreement should be recorded rather than silently dropped, got %d",
			len(rebuttal.Conflicts))
	}
}

func TestIntegrationEqualAuthorityIsAConflictNotACoinFlip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.definePredicate(t, "HEADCOUNT", true, domain.ConflictPolicyHighestAuthority)

	var sources []domain.SourceID
	for _, name := range []string{"hr-system-a", "hr-system-b"} {
		source, err := h.fixture.Store.CreateSource(ctx, domain.Source{
			WorkspaceID: h.scope().WorkspaceID, Kind: domain.SourceKindDatabase,
			Name: name, TrustLevel: domain.TrustHigh,
		}, h.fixture.Primary.Principal.ID)
		if err != nil {
			t.Fatalf("create source: %v", err)
		}
		sources = append(sources, source.ID)
	}

	assertFrom := func(source domain.SourceID, key string, value int64) knowledge.AssertResult {
		eventID, episodeID := h.ingestSequenced(t, "headcount", key, "", source)
		result, err := h.service.Assert(ctx, knowledge.AssertRequest{
			Scope: h.scope(), Principal: h.principal(), SourceEventID: eventID,
			Claims: []knowledge.Claim{{
				Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
				Predicate: "HEADCOUNT",
				Object:    domain.ObjectOfInteger(value),
				Evidence:  []knowledge.EvidenceInput{{EpisodeID: episodeID}},
			}},
		})
		if err != nil {
			t.Fatalf("assert: %v", err)
		}
		return result
	}

	assertFrom(sources[0], "eq-1", 500)
	second := assertFrom(sources[1], "eq-2", 640)

	// Two equally trusted systems disagreeing is exactly the case where picking one would
	// be arbitrary. Both are kept and the disagreement is made visible.
	if len(second.Conflicts) != 1 {
		t.Fatalf("equal authority must produce a conflict, got %d", len(second.Conflicts))
	}
	believed := h.query(t, domain.AssertionQuery{Predicates: []string{"HEADCOUNT"}})
	if len(believed) != 2 {
		t.Fatalf("both figures must remain visible, got %d", len(believed))
	}
	for _, claim := range believed {
		if claim.Status != domain.AssertionDisputed {
			t.Fatalf("both sides should be marked disputed, got %s", claim.Status)
		}
	}
}

func TestIntegrationNonOverlappingIntervalsNeverConflict(t *testing.T) {
	h := newHarness(t)

	h.definePredicate(t, "OFFICE_AT", true, domain.ConflictPolicyManual)
	eventID, episodeID, _ := h.ingestEpisode(t, "Office history.", "intervals-1")

	// Consecutive tenancies that meet exactly. A functional predicate does not make these
	// contradictory: the company had one office at a time, and it moved.
	assert := func(place string, from, to *time.Time) knowledge.AssertResult {
		result, err := h.service.Assert(context.Background(), knowledge.AssertRequest{
			Scope: h.scope(), Principal: h.principal(), SourceEventID: eventID,
			Claims: []knowledge.Claim{{
				Subject:      knowledge.EntityRef{Name: "Acme", Type: "organization"},
				Predicate:    "OFFICE_AT",
				ObjectEntity: &knowledge.EntityRef{Name: place, Type: "place"},
				ValidFrom:    from,
				ValidTo:      to,
				Evidence:     []knowledge.EvidenceInput{{EpisodeID: episodeID}},
			}},
		})
		if err != nil {
			t.Fatalf("assert %s: %v", place, err)
		}
		return result
	}

	assert("Berlin", ptr(date(2024, time.January, 1)), ptr(date(2026, time.January, 1)))
	munich := assert("Munich", ptr(date(2026, time.January, 1)), nil)

	if len(munich.Conflicts) != 0 {
		t.Fatal("a clean handover is not a contradiction")
	}
	if len(munich.Superseded) != 0 {
		t.Fatal("a later tenancy must not supersede an earlier one that already ended")
	}

	believed := h.query(t, domain.AssertionQuery{Predicates: []string{"OFFICE_AT"}})
	if len(believed) != 2 {
		t.Fatalf("both tenancies stand, got %d", len(believed))
	}

	// An overlapping third claim is a genuine contradiction and is treated as one.
	overlapping := assert("Hamburg", ptr(date(2026, time.June, 1)), nil)
	if len(overlapping.Conflicts) != 1 {
		t.Fatalf("an overlapping tenancy must conflict, got %d", len(overlapping.Conflicts))
	}
}

// The deterministic fixture AGENTS.md section 36 asks for: a fact learned, corrected, and
// queried at several combinations of world time and knowledge time, with every answer fixed
// in advance.
func TestIntegrationTemporalFixtureIsDeterministic(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	var (
		learned   = time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
		corrected = time.Date(2026, 4, 20, 9, 0, 0, 0, time.UTC)
		queried   = time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	)

	h.clock.Set(learned)
	firstEvent, firstEpisode, _ := h.ingestEpisode(t, "Alice was CEO through March 31st.", "fixture-1")
	original := h.assertOne(t, firstEvent, knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Alice", Type: "person"},
		Predicate: "ROLE_AT",
		Object:    domain.ObjectOfSymbol("CEO"),
		ValidFrom: ptr(date(2025, time.January, 1)),
		ValidTo:   ptr(date(2026, time.April, 1)),
		Evidence:  []knowledge.EvidenceInput{{EpisodeID: firstEpisode}},
	})

	h.clock.Set(corrected)
	secondEvent, secondEpisode, _ := h.ingestEpisode(t, "Correction: she left on March 20th.", "fixture-2")
	h.assertOne(t, secondEvent, knowledge.Claim{
		Subject:    knowledge.EntityRef{Name: "Alice", Type: "person"},
		Predicate:  "ROLE_AT",
		Object:     domain.ObjectOfSymbol("CEO"),
		ValidFrom:  ptr(date(2025, time.January, 1)),
		ValidTo:    ptr(date(2026, time.March, 20)),
		Supersedes: []domain.AssertionID{original.ID},
		Evidence:   []knowledge.EvidenceInput{{EpisodeID: secondEpisode}},
	})
	h.clock.Set(queried)

	// Every combination of world time and knowledge time, with the expected answer stated
	// up front. These are the questions a single-timestamp model cannot even express.
	cases := []struct {
		name    string
		validAt time.Time
		knownAt *time.Time
		wantCEO bool
	}{
		{"believed now, about a date inside the corrected tenure", date(2026, time.March, 15), nil, true},
		{"believed now, about a date the correction excluded", date(2026, time.March, 25), nil, false},
		{"believed now, about a date after both", date(2026, time.April, 15), nil, false},
		{"believed before the correction, about the disputed date", date(2026, time.March, 25),
			ptr(time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)), true},
		{"believed before the correction, about a date inside both", date(2026, time.March, 15),
			ptr(time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)), true},
		{"believed after the correction, about the disputed date", date(2026, time.March, 25),
			ptr(time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)), false},
		{"believed before anything was learned", date(2026, time.March, 15),
			ptr(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found := h.query(t, domain.AssertionQuery{
				Predicates: []string{"ROLE_AT"},
				ValidAt:    &tc.validAt,
				KnownAt:    tc.knownAt,
			})
			gotCEO := len(found) > 0
			if gotCEO != tc.wantCEO {
				t.Fatalf("expected CEO=%t, got %t (%d claims)", tc.wantCEO, gotCEO, len(found))
			}
		})
	}

	// Determinism: the same questions asked again give the same answers. Nothing about the
	// wall clock, arrival order, or query order may influence a temporal answer.
	for range 3 {
		found := h.query(t, domain.AssertionQuery{
			Predicates: []string{"ROLE_AT"},
			ValidAt:    ptr(date(2026, time.March, 25)),
			KnownAt:    ptr(time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)),
		})
		if len(found) != 1 || found[0].ID != original.ID {
			t.Fatalf("the same temporal question must always give the same answer, got %d claims",
				len(found))
		}
	}

	_ = ctx
}
