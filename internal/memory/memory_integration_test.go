package memory_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding/hashing"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/memory"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/pipeline"
	"github.com/gimantha/strata/internal/projection"
	"github.com/gimantha/strata/internal/retrieval"
	"github.com/gimantha/strata/internal/store/blob"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

type harness struct {
	fixture   *pgtest.Fixture
	gateway   *ingest.Gateway
	runner    *pipeline.Runner
	service   *knowledge.Service
	projector *projection.Projector
	retriever *retrieval.Retriever
	memory    *memory.Service
	now       time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	f := pgtest.NewFixture(t)
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	embedder := hashing.New()
	projector := projection.New(f.Store, embedder, projection.Options{}, nil, nil)
	service := knowledge.New(f.Store, knowledge.Options{}, nil, nil)

	stages := pipeline.DefaultStages(f.Store, blobs, pipeline.StageConfig{
		ChunkMaxTokens: 256, ChunkOverlapTokens: 16,
		Tokenizer: normalize.DefaultTokenizer, Projector: projector,
	})

	return &harness{
		fixture:   f,
		gateway:   ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1}, nil, nil, nil),
		runner:    pipeline.NewRunner(f.Store, 1, stages, nil, nil, nil),
		service:   service,
		projector: projector,
		retriever: retrieval.New(f.Store, embedder, retrieval.Options{}, nil, nil),
		memory: memory.New(f.Store, service, projector, memory.Options{
			Clock: func() time.Time { return now },
		}, nil, nil),
		now: now,
	}
}

func (h *harness) scope() domain.Scope { return h.fixture.Primary.Scope() }

// observe ingests a document and records one claim from it, returning the claim.
func (h *harness) observe(t *testing.T, text string, key string, claim knowledge.Claim) domain.Assertion {
	t.Helper()
	ctx := context.Background()

	receipt, err := h.gateway.Accept(ctx, ingest.Request{
		Scope:          h.scope(),
		Principal:      h.fixture.Primary.Principal.Ref(),
		SourceID:       h.fixture.Primary.Source.ID,
		MediaType:      normalize.MediaTypePlain,
		Payload:        []byte(text),
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", key, err)
	}
	if _, err := h.runner.Process(ctx, h.scope().WorkspaceID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process %s: %v", key, err)
	}

	episodes, err := h.fixture.Store.ListEpisodes(ctx, h.scope().WorkspaceID, receipt.SourceEventID)
	if err != nil || len(episodes) == 0 {
		t.Fatalf("episodes: %v", err)
	}
	if len(claim.Evidence) == 0 {
		claim.Evidence = []knowledge.EvidenceInput{{EpisodeID: episodes[0].ID, ExtractedText: text}}
	}

	result, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: h.scope(), Principal: h.fixture.Primary.Principal.Ref(),
		SourceEventID: receipt.SourceEventID, Claims: []knowledge.Claim{claim},
	})
	if err != nil {
		t.Fatalf("assert %s: %v", key, err)
	}
	if _, err := h.projector.ProjectEvent(ctx, h.scope(), receipt.SourceEventID); err != nil {
		t.Fatalf("project %s: %v", key, err)
	}
	return result.Assertions[0]
}

// TestIntegrationTransientMemoryStopsBeingActiveWithoutLosingItsEpisode is phase 12's first
// acceptance criterion, stated as three separate observations because "stops being active"
// and "is still there" are easy to satisfy one at a time and easy to break together.
func TestIntegrationTransientMemoryStopsBeingActiveWithoutLosingItsEpisode(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	tonight := h.now.Add(12 * time.Hour)
	claim := h.observe(t, "Priya Raman is staying at the Kelvinbridge Hotel tonight.", "transient-1",
		knowledge.Claim{
			Subject:   knowledge.EntityRef{Name: "Priya Raman", Type: "person"},
			Predicate: "STAYING_AT",
			Object:    domain.ObjectOfString("Kelvinbridge Hotel"),
			// Working memory with an explicit expiry: true forever, useful for a day
			// (AGENTS.md section 21.3).
			MemoryKind:  domain.MemoryWorking,
			ActiveUntil: &tonight,
			ExpiresAt:   &tonight,
		})

	// While it is still tonight, the memory is in scope.
	if !h.retrievable(t, "Kelvinbridge Hotel", &h.now) {
		t.Fatal("the memory should be active before it expires")
	}

	tomorrow := h.now.Add(36 * time.Hour)

	// One: it is no longer active context.
	if h.retrievable(t, "Kelvinbridge Hotel", &tomorrow) {
		t.Fatal("expired working memory is still being surfaced as current")
	}

	// Two: the claim itself is untouched. Not retracted, not superseded, not deleted.
	stored, err := h.fixture.Store.GetAssertion(ctx, h.scope().WorkspaceID, claim.ID)
	if err != nil {
		t.Fatalf("the claim should still exist: %v", err)
	}
	if stored.Status != domain.AssertionActive {
		t.Fatalf("expiry must not change status, got %s", stored.Status)
	}
	if stored.RetractedAt != nil {
		t.Fatal("expiry is not retraction")
	}

	// Three: the episode and its evidence survive, so the fact remains explicable.
	evidence, err := h.fixture.Store.ListEvidence(ctx, h.scope().WorkspaceID, claim.ID)
	if err != nil || len(evidence) == 0 {
		t.Fatalf("the supporting evidence was lost: %v", err)
	}
	chain, err := h.fixture.Store.ProvenanceChain(ctx, h.scope().WorkspaceID, claim.ID)
	if err != nil {
		t.Fatalf("provenance should still walk: %v", err)
	}
	if len(chain.Links) == 0 {
		t.Fatal("the historical episode was lost")
	}
	if !strings.Contains(chain.Links[0].Episode.Content, "Kelvinbridge") {
		t.Fatalf("the episode's text changed: %q", chain.Links[0].Episode.Content)
	}

	// And it is still answerable as of when it held, which is the difference between
	// expiring and forgetting.
	asOf, err := h.fixture.Store.QueryAssertions(ctx, domain.AssertionQuery{
		Scope: h.scope(), ActiveAt: &h.now, Predicates: []string{"STAYING_AT"},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("as-of query: %v", err)
	}
	if len(asOf) == 0 {
		t.Fatal("the memory should still be answerable as of when it was active")
	}
}

// TestIntegrationDerivedFactLinksToItsSupportingAssertions is the second acceptance criterion.
func TestIntegrationDerivedFactLinksToItsSupportingAssertions(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	// Three separate episodes saying the same thing. One occasion is an event; three is a
	// pattern, which is the distinction consolidation exists to draw.
	var observations []domain.Assertion
	for i, text := range []string{
		"Priya Raman took the Tuesday review again this week.",
		"Priya Raman ran the Tuesday review.",
		"The Tuesday review was led by Priya Raman.",
	} {
		observations = append(observations, h.observe(t, text, "obs-"+itoa(i), knowledge.Claim{
			Subject:    knowledge.EntityRef{Name: "Priya Raman", Type: "person"},
			Predicate:  "LEADS",
			Object:     domain.ObjectOfString("Tuesday review"),
			MemoryKind: domain.MemoryEpisodic,
			Confidence: 0.8,
		}))
	}

	result, err := h.memory.Consolidate(ctx, memory.ConsolidateRequest{
		Scope: h.scope(), Principal: h.fixture.Primary.Principal.Ref(),
	})
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	if len(result.Derived) != 1 {
		t.Fatalf("expected one derived fact, got %d (examined %d, groups %d)",
			len(result.Derived), result.Examined, result.Groups)
	}

	derived := result.Derived[0]
	if derived.MemoryKind != domain.MemorySemantic {
		t.Fatalf("a consolidated fact is semantic, got %s", derived.MemoryKind)
	}
	if derived.ProvenanceMode != domain.ProvenanceDerived {
		t.Fatalf("expected derived provenance, got %s", derived.ProvenanceMode)
	}
	if derived.DerivationID == nil {
		t.Fatal("a derived fact must name what produced it")
	}

	// The link the criterion is about: every supporting observation, by id.
	derivation, err := h.fixture.Store.GetDerivation(ctx, h.scope().WorkspaceID, *derived.DerivationID)
	if err != nil {
		t.Fatalf("load derivation: %v", err)
	}
	if len(derivation.InputAssertionIDs) != len(observations) {
		t.Fatalf("expected %d supporting assertions, got %d",
			len(observations), len(derivation.InputAssertionIDs))
	}

	supporting := map[domain.AssertionID]bool{}
	for _, id := range derivation.InputAssertionIDs {
		supporting[id] = true
	}
	for _, observation := range observations {
		if !supporting[observation.ID] {
			t.Fatalf("observation %s is missing from the derivation", observation.ID)
		}
		// The observations are untouched: consolidation adds a conclusion, it does not
		// consume the evidence for it.
		stored, err := h.fixture.Store.GetAssertion(ctx, h.scope().WorkspaceID, observation.ID)
		if err != nil {
			t.Fatalf("observation %s was lost: %v", observation.ID, err)
		}
		if stored.Status != domain.AssertionActive {
			t.Fatalf("observation %s changed status to %s", observation.ID, stored.Status)
		}
	}

	// A conclusion drawn from observations is never more certain than an observation.
	if derived.Confidence >= 1 {
		t.Fatalf("repetition manufactured certainty: %v", derived.Confidence)
	}
	if derived.Confidence <= 0 {
		t.Fatalf("a corroborated fact should carry confidence, got %v", derived.Confidence)
	}
}

// TestIntegrationConsolidationIsIdempotentAndRuleGated holds the two properties a background
// job needs: running it twice changes nothing, and it does not fire on thin evidence.
func TestIntegrationConsolidationIsIdempotentAndRuleGated(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	for i, text := range []string{
		"Acme shipped late in March.",
		"Acme shipped late again in April.",
	} {
		h.observe(t, text, "thin-"+itoa(i), knowledge.Claim{
			Subject:    knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
			Predicate:  "SHIPS_LATE",
			Object:     domain.ObjectOfBool(true),
			MemoryKind: domain.MemoryEpisodic,
			Confidence: 0.8,
		})
	}

	// Two observations is not a pattern under the default rule.
	thin, err := h.memory.Consolidate(ctx, memory.ConsolidateRequest{
		Scope: h.scope(), Principal: h.fixture.Primary.Principal.Ref(),
	})
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	if len(thin.Derived) != 0 {
		t.Fatalf("two observations should not consolidate, got %d", len(thin.Derived))
	}

	h.observe(t, "Acme shipped late in May as well.", "thin-2", knowledge.Claim{
		Subject:    knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
		Predicate:  "SHIPS_LATE",
		Object:     domain.ObjectOfBool(true),
		MemoryKind: domain.MemoryEpisodic,
		Confidence: 0.8,
	})

	first, err := h.memory.Consolidate(ctx, memory.ConsolidateRequest{
		Scope: h.scope(), Principal: h.fixture.Primary.Principal.Ref(),
	})
	if err != nil {
		t.Fatalf("consolidate: %v", err)
	}
	if len(first.Derived) != 1 {
		t.Fatalf("three observations should consolidate, got %d", len(first.Derived))
	}

	// Running the job again must not produce a second copy of the same conclusion. A
	// background job that accumulates duplicates every time it runs is worse than one that
	// never runs.
	second, err := h.memory.Consolidate(ctx, memory.ConsolidateRequest{
		Scope: h.scope(), Principal: h.fixture.Primary.Principal.Ref(),
	})
	if err != nil {
		t.Fatalf("consolidate again: %v", err)
	}
	if len(second.Derived) != 0 {
		t.Fatalf("a second pass derived %d new facts", len(second.Derived))
	}
	if second.Existing == 0 {
		t.Fatal("the second pass should recognize the conclusion it already drew")
	}

	// A dry run reports without writing, which is how a rule change is evaluated.
	dry, err := h.memory.Consolidate(ctx, memory.ConsolidateRequest{
		Scope: h.scope(), Principal: h.fixture.Primary.Principal.Ref(),
		Rule: domain.ConsolidationRule{MinObservations: 2}, DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(dry.Qualified) == 0 {
		t.Fatal("a looser rule should qualify something")
	}
	if len(dry.Derived) != 0 {
		t.Fatal("a dry run wrote something")
	}
}

// TestIntegrationDeactivationIsNotRetraction covers AGENTS.md section 21.4: the four ways of
// forgetting are different operations, and the two that survive are reversible.
func TestIntegrationDeactivationIsNotRetraction(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	claim := h.observe(t, "Acme prefers Thursday deliveries.", "forget-1", knowledge.Claim{
		Subject:    knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
		Predicate:  "PREFERS",
		Object:     domain.ObjectOfString("Thursday deliveries"),
		MemoryKind: domain.MemoryPreference,
	})

	actor := h.fixture.Primary.Principal.Ref()
	deactivated, err := h.memory.Forget(ctx, memory.ForgetRequest{
		Scope: h.scope(), Actor: actor, AssertionID: claim.ID,
		Kind: domain.ForgetDeactivate, Reason: "the customer asked us to stop using this",
	})
	if err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	// Still asserted, still true. Only the context clock moved.
	if deactivated.Status != domain.AssertionActive {
		t.Fatalf("deactivation must not change status, got %s", deactivated.Status)
	}
	if deactivated.RetractedAt != nil {
		t.Fatal("deactivation is not retraction")
	}
	if deactivated.DeactivatedAt == nil || deactivated.DeactivationReason == "" {
		t.Fatal("a deactivation must record when and why")
	}

	later := h.now.Add(time.Hour)
	if h.retrievable(t, "Thursday deliveries", &later) {
		t.Fatal("a deactivated claim is still being surfaced as current")
	}

	// Reversible, which is what makes it safe to use for transient material.
	restored, err := h.memory.Reactivate(ctx, h.scope(), actor, claim.ID)
	if err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if restored.DeactivatedAt != nil {
		t.Fatal("reactivation did not clear the deactivation")
	}
	if !h.retrievable(t, "Thursday deliveries", &later) {
		t.Fatal("a reactivated claim should be findable again")
	}
}

// TestIntegrationDestructiveForgettingIsRefusedHere holds the boundary section 21.4 draws:
// the destructive workflows are not a flag on the reversible one.
func TestIntegrationDestructiveForgettingIsRefusedHere(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	claim := h.observe(t, "Acme prefers Friday deliveries.", "forget-2", knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
		Predicate: "PREFERS",
		Object:    domain.ObjectOfString("Friday deliveries"),
	})

	actor := h.fixture.Primary.Principal.Ref()
	for _, kind := range []domain.ForgetKind{domain.ForgetRetention, domain.ForgetErasure} {
		if _, err := h.memory.Forget(ctx, memory.ForgetRequest{
			Scope: h.scope(), Actor: actor, AssertionID: claim.ID,
			Kind: kind, Reason: "policy",
		}); err == nil {
			t.Fatalf("%s should not be available through the soft-forget path", kind)
		}
	}

	// Retraction is a knowledge-time correction and belongs to the knowledge service.
	if _, err := h.memory.Forget(ctx, memory.ForgetRequest{
		Scope: h.scope(), Actor: actor, AssertionID: claim.ID,
		Kind: domain.ForgetRetract, Reason: "wrong",
	}); err == nil {
		t.Fatal("retraction should not be routed through deactivation")
	}

	// And a deactivation with no reason is refused: someone will want to know why.
	if _, err := h.memory.Forget(ctx, memory.ForgetRequest{
		Scope: h.scope(), Actor: actor, AssertionID: claim.ID,
		Kind: domain.ForgetDeactivate,
	}); err == nil {
		t.Fatal("a reasonless deactivation was accepted")
	}

	// Nothing above changed the claim.
	stored, err := h.fixture.Store.GetAssertion(ctx, h.scope().WorkspaceID, claim.ID)
	if err != nil {
		t.Fatalf("the claim should be untouched: %v", err)
	}
	if stored.DeactivatedAt != nil || stored.Status != domain.AssertionActive {
		t.Fatal("a refused operation changed the claim anyway")
	}
}

// TestIntegrationDecayRanksDownWithoutRemoving covers AGENTS.md section 21.2: decay affects
// relevance, never truth.
func TestIntegrationDecayRanksDownWithoutRemoving(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	old := h.now.Add(-180 * 24 * time.Hour)
	h.observe(t, "Meridian Freight handles the northern corridor.", "decay-old",
		knowledge.Claim{
			Subject:       knowledge.EntityRef{Name: "Meridian Freight", Type: "organization"},
			Predicate:     "HANDLES",
			Object:        domain.ObjectOfString("northern corridor"),
			DecayStartsAt: &old,
		})
	h.observe(t, "Meridian Freight handles the southern corridor.", "decay-new",
		knowledge.Claim{
			Subject:   knowledge.EntityRef{Name: "Meridian Freight", Type: "organization"},
			Predicate: "HANDLES",
			Object:    domain.ObjectOfString("southern corridor"),
		})

	result, err := h.retriever.Query(ctx, domain.QueryRequest{
		Scope: h.scope(), Query: "Meridian Freight corridor", Limit: 20, Explain: true,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	var decayed, fresh *domain.RetrievedItem
	for i := range result.Items {
		switch {
		case strings.Contains(result.Items[i].Content, "northern"):
			decayed = &result.Items[i]
		case strings.Contains(result.Items[i].Content, "southern"):
			fresh = &result.Items[i]
		}
	}

	// Still there. A decayed memory that vanished would be deletion wearing a ranking
	// function's clothes.
	if decayed == nil {
		t.Fatal("a decayed memory was removed from results rather than ranked down")
	}
	if fresh == nil {
		t.Fatal("the fresh memory should be findable")
	}
	if decayed.Signals["decay"] == 0 {
		t.Fatalf("the decay multiplier should be reported: %v", decayed.Signals)
	}
	if decayed.Signals["decay"] >= 1 {
		t.Fatalf("a six-month-old memory should have decayed, got %v", decayed.Signals["decay"])
	}
	if decayed.Signals["decay"] < domain.MinDecayWeight {
		t.Fatalf("decay should stop at the floor, got %v", decayed.Signals["decay"])
	}
}

// retrievable reports whether a *claim* about a term is in scope at an instant.
//
// Restricted to the assertion surface on purpose. Lifecycle belongs to a claim, not to the
// document it was drawn from: the source passage still says the guest is at the hotel, and it
// will say that forever, because that is what the document says. Expiring the claim must not
// rewrite the record of what was written. Checking chunks here would test the wrong thing and
// would push the design towards editing source material, which is the one thing the ledger
// exists to prevent.
func (h *harness) retrievable(t *testing.T, term string, activeAt *time.Time) bool {
	t.Helper()

	result, err := h.retriever.Query(context.Background(), domain.QueryRequest{
		Scope:    h.scope(),
		Query:    term,
		Temporal: domain.TemporalQuery{ActiveAt: activeAt},
		Filters:  domain.QueryFilters{Surfaces: []domain.Surface{domain.SurfaceAssertion}},
		Limit:    20,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, item := range result.Items {
		if strings.Contains(item.Content, term) {
			return true
		}
	}
	return false
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := make([]byte, 0, 4)
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
