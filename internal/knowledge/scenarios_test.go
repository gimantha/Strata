package knowledge_test

import (
	"context"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/knowledge"
)

// Scenario A from AGENTS.md section 37: a fact is learned, later corrected, and queried
// at several combinations of world time and knowledge time.
//
// This is the scenario the whole temporal model exists for. A single-timestamp model
// cannot answer the middle question at all, and a bitemporal one that overwrites on
// correction answers it wrongly.
func TestIntegrationScenarioATemporalCorrection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	var (
		april2  = time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
		april10 = time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC)
		april20 = time.Date(2026, 4, 20, 9, 0, 0, 0, time.UTC)
		april25 = time.Date(2026, 4, 25, 9, 0, 0, 0, time.UTC)
		march25 = date(2026, time.March, 25)
	)

	// April 2: the source says Alice was CEO through March 31.
	h.clock.Set(april2)
	firstEvent, firstEpisode, firstChunk := h.ingestEpisode(t,
		"Alice Chen served as CEO of Acme through March 31st.", "ceo-1")

	original := h.assertOne(t, firstEvent, knowledge.Claim{
		Subject:      knowledge.EntityRef{Name: "Alice Chen", Type: "person"},
		Predicate:    "ROLE_AT",
		ObjectEntity: &knowledge.EntityRef{Name: "Acme", Type: "organization"},
		ScopeKey:     "CEO",
		ValidFrom:    ptr(date(2025, time.January, 1)),
		ValidTo:      ptr(date(2026, time.April, 1)), // through March 31, half-open
		Evidence: []knowledge.EvidenceInput{{
			EpisodeID:     firstEpisode,
			ChunkID:       &firstChunk,
			ExtractedText: "Alice Chen served as CEO of Acme through March 31st.",
		}},
	})

	// April 20: a correction says she actually ceased being CEO on March 20.
	h.clock.Set(april20)
	secondEvent, secondEpisode, secondChunk := h.ingestEpisode(t,
		"Correction: Alice Chen stepped down as CEO of Acme on March 20th.", "ceo-2")

	corrected := h.assertOne(t, secondEvent, knowledge.Claim{
		Subject:      knowledge.EntityRef{Name: "Alice Chen", Type: "person"},
		Predicate:    "ROLE_AT",
		ObjectEntity: &knowledge.EntityRef{Name: "Acme", Type: "organization"},
		ScopeKey:     "CEO",
		ValidFrom:    ptr(date(2025, time.January, 1)),
		ValidTo:      ptr(date(2026, time.March, 20)),
		Supersedes:   []domain.AssertionID{original.ID},
		Evidence: []knowledge.EvidenceInput{{
			EpisodeID:     secondEpisode,
			ChunkID:       &secondChunk,
			ExtractedText: "Alice Chen stepped down as CEO of Acme on March 20th.",
		}},
	})

	// The original is not edited or deleted. It is marked superseded, which changes what
	// the system believes without rewriting what it once believed.
	reloaded, err := h.service.Get(ctx, h.scope().WorkspaceID, original.ID)
	if err != nil {
		t.Fatalf("reload original: %v", err)
	}
	if reloaded.Status != domain.AssertionSuperseded {
		t.Fatalf("the corrected claim must be superseded, got %s", reloaded.Status)
	}
	if reloaded.Temporal.SupersededAt == nil || !reloaded.Temporal.SupersededAt.Equal(april20) {
		t.Fatalf("supersession must be stamped with the knowledge time it happened, got %v",
			reloaded.Temporal.SupersededAt)
	}
	if reloaded.Temporal.ValidTo == nil || !reloaded.Temporal.ValidTo.Equal(date(2026, time.April, 1)) {
		t.Fatal("supersession must not rewrite the world validity the original claimed")
	}
	if corrected.SupersedesID == nil || *corrected.SupersedesID != original.ID {
		t.Fatal("the correction must record what it replaced")
	}

	// Question 1: what do we currently believe about March 25?
	// Answer: she was not CEO. The correction ended her tenure on March 20.
	h.clock.Set(april25)
	current := h.query(t, domain.AssertionQuery{
		Predicates: []string{"ROLE_AT"},
		ValidAt:    &march25,
	})
	if len(current) != 0 {
		t.Fatalf("current belief about March 25 should be that she was not CEO, got %v",
			objectsOf(current))
	}

	// Question 2: what did we believe on April 10 about March 25?
	// Answer: she was CEO. The correction had not arrived yet.
	asOfApril10 := h.query(t, domain.AssertionQuery{
		Predicates: []string{"ROLE_AT"},
		ValidAt:    &march25,
		KnownAt:    &april10,
	})
	if len(asOfApril10) != 1 {
		t.Fatalf("as of April 10 we believed she was CEO on March 25, got %d claims", len(asOfApril10))
	}
	if asOfApril10[0].ID != original.ID {
		t.Fatal("the belief held on April 10 was the original claim, not the correction")
	}

	// Question 3: what did we believe on April 25 about March 25?
	// Answer: she was not CEO. The correction had arrived by then.
	asOfApril25 := h.query(t, domain.AssertionQuery{
		Predicates: []string{"ROLE_AT"},
		ValidAt:    &march25,
		KnownAt:    &april25,
	})
	if len(asOfApril25) != 0 {
		t.Fatalf("as of April 25 we believed she was not CEO on March 25, got %v",
			objectsOf(asOfApril25))
	}

	// The corrected claim is still the current belief about a date inside its interval.
	march15 := date(2026, time.March, 15)
	stillCEO := h.query(t, domain.AssertionQuery{
		Predicates: []string{"ROLE_AT"},
		ValidAt:    &march15,
	})
	if len(stillCEO) != 1 || stillCEO[0].ID != corrected.ID {
		t.Fatalf("she was still CEO on March 15 under current belief, got %d claims", len(stillCEO))
	}

	// Both claims keep their evidence: a correction does not erase why the earlier claim
	// was made.
	for _, id := range []domain.AssertionID{original.ID, corrected.ID} {
		evidence, err := h.fixture.Store.ListEvidence(ctx, h.scope().WorkspaceID, id)
		if err != nil {
			t.Fatalf("list evidence: %v", err)
		}
		if len(evidence) != 1 {
			t.Fatalf("assertion %s lost its evidence", id)
		}
	}
}

// Scenario C: a non-functional predicate must let several values coexist. Liking tea does
// not stop someone liking coffee, and treating it as a contradiction would destroy one of
// the two facts.
func TestIntegrationScenarioCNonConflictingMultiValue(t *testing.T) {
	h := newHarness(t)

	eventID, episodeID, _ := h.ingestEpisode(t, "Alice likes tea and coffee.", "likes-1")

	for _, drink := range []string{"Tea", "Coffee"} {
		h.assertOne(t, eventID, knowledge.Claim{
			Subject:      knowledge.EntityRef{Name: "Alice", Type: "person"},
			Predicate:    "LIKES",
			ObjectEntity: &knowledge.EntityRef{Name: drink, Type: "thing"},
			Evidence:     []knowledge.EvidenceInput{{EpisodeID: episodeID}},
		})
	}

	believed := h.query(t, domain.AssertionQuery{Predicates: []string{"LIKES"}})
	if len(believed) != 2 {
		t.Fatalf("both preferences must coexist, got %d: %v", len(believed), objectsOf(believed))
	}
	for _, a := range believed {
		if a.Status != domain.AssertionActive {
			t.Fatalf("neither preference should be disputed or superseded, got %s", a.Status)
		}
		if a.ConflictSetID != nil {
			t.Fatal("a multi-valued predicate must not produce a conflict set")
		}
	}

	conflicts, err := h.fixture.Store.ListConflictSets(context.Background(), h.scope(), true, 10)
	if err != nil {
		t.Fatalf("list conflicts: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("expected no conflicts, got %d", len(conflicts))
	}
}

// Scenario D: two overlapping claims for a functional predicate must produce a resolvable
// conflict, never an arbitrary deletion.
func TestIntegrationScenarioDConflictingFunctionalFact(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// CURRENT_PLAN is functional: a customer is on one plan at a time.
	if _, err := h.fixture.Store.DefinePredicate(ctx, domain.PredicateDefinition{
		WorkspaceID:    h.scope().WorkspaceID,
		Name:           "CURRENT_PLAN",
		Functional:     true,
		ConflictPolicy: domain.ConflictPolicyManual,
		TemporalPolicy: domain.TemporalPolicyStateful,
	}, h.fixture.Primary.Principal.ID); err != nil {
		t.Fatalf("define predicate: %v", err)
	}

	firstEvent, firstEpisode, _ := h.ingestEpisode(t, "Acme is on the enterprise plan.", "plan-1")
	secondEvent, secondEpisode, _ := h.ingestEpisode(t, "Acme is on the standard plan.", "plan-2")

	from := date(2026, time.January, 1)
	first := h.assertOne(t, firstEvent, knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
		Predicate: "CURRENT_PLAN",
		Object:    domain.ObjectOfSymbol("ENTERPRISE"),
		ValidFrom: &from,
		Evidence:  []knowledge.EvidenceInput{{EpisodeID: firstEpisode}},
	})

	result, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope:         h.scope(),
		Principal:     h.principal(),
		SourceEventID: secondEvent,
		Claims: []knowledge.Claim{{
			Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
			Predicate: "CURRENT_PLAN",
			Object:    domain.ObjectOfSymbol("STANDARD"),
			ValidFrom: &from,
			Evidence:  []knowledge.EvidenceInput{{EpisodeID: secondEpisode}},
		}},
	})
	if err != nil {
		t.Fatalf("assert conflicting claim: %v", err)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("overlapping values for a functional predicate must produce a conflict, got %d",
			len(result.Conflicts))
	}
	second := result.Assertions[0]

	// Neither claim is deleted. Both are marked disputed and joined to the conflict set,
	// so the disagreement is visible rather than silently resolved.
	for _, id := range []domain.AssertionID{first.ID, second.ID} {
		claim, err := h.service.Get(ctx, h.scope().WorkspaceID, id)
		if err != nil {
			t.Fatalf("reload claim: %v", err)
		}
		if claim.Status != domain.AssertionDisputed {
			t.Fatalf("claim %s should be disputed, got %s", id, claim.Status)
		}
		if claim.ConflictSetID == nil || *claim.ConflictSetID != result.Conflicts[0].ID {
			t.Fatalf("claim %s must be attached to the conflict set", id)
		}
	}

	// A disputed claim is still believed: retrieval must be able to see the disagreement
	// rather than have it hidden.
	believed := h.query(t, domain.AssertionQuery{Predicates: []string{"CURRENT_PLAN"}})
	if len(believed) != 2 {
		t.Fatalf("both sides of the conflict must remain queryable, got %d", len(believed))
	}

	// The conflict is resolvable, and resolution returns the survivors to active.
	if err := h.fixture.Store.ResolveConflictSet(ctx, h.scope().WorkspaceID, result.Conflicts[0].ID,
		domain.ConflictResolvedByHuman, h.clock.Now(), h.fixture.Primary.Principal.ID); err != nil {
		t.Fatalf("resolve conflict: %v", err)
	}
	open, err := h.fixture.Store.ListConflictSets(ctx, h.scope(), true, 10)
	if err != nil {
		t.Fatalf("list conflicts: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("the conflict should be closed, %d still open", len(open))
	}
}

// Scenario G: for an answer fact, the API can walk fact to evidence to chunk to episode
// to artifact to source event to source.
func TestIntegrationScenarioGProvenanceWalk(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	const statement = "Acme Corporation is a supplier of industrial fasteners."
	eventID, episodeID, chunkID := h.ingestEpisode(t, statement, "supplier-1")

	assertion := h.assertOne(t, eventID, knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Acme Corporation", Type: "organization"},
		Predicate: "classified_as",
		Object:    domain.ObjectOfSymbol("SUPPLIER"),
		Evidence: []knowledge.EvidenceInput{{
			EpisodeID:     episodeID,
			ChunkID:       &chunkID,
			ExtractedText: statement,
			Confidence:    0.9,
		}},
	})

	chain, err := h.service.Provenance(ctx, h.scope().WorkspaceID, assertion.ID)
	if err != nil {
		t.Fatalf("walk provenance: %v", err)
	}

	if chain.Assertion.ID != assertion.ID {
		t.Fatal("the chain must start at the claim")
	}
	if chain.Subject.CanonicalName != "Acme Corporation" {
		t.Fatalf("the chain must name its subject, got %q", chain.Subject.CanonicalName)
	}
	if len(chain.Links) != 1 {
		t.Fatalf("expected one evidence link, got %d", len(chain.Links))
	}

	link := chain.Links[0]
	switch {
	case link.Evidence.ExtractedText != statement:
		t.Fatalf("evidence lost its excerpt: %q", link.Evidence.ExtractedText)
	case link.Chunk == nil || link.Chunk.ID != chunkID:
		t.Fatal("the chain must reach the chunk the claim came from")
	case link.Episode.ID != episodeID:
		t.Fatal("the chain must reach the episode")
	case link.Episode.Content != statement:
		t.Fatalf("the episode must still hold the source text: %q", link.Episode.Content)
	case link.Artifact.ContentHash != domain.ContentHash([]byte(statement)):
		t.Fatal("the chain must reach the archived artifact, addressed by its content")
	case link.SourceEvent.ID != eventID:
		t.Fatal("the chain must reach the source event")
	case link.Source.ID != h.fixture.Primary.Source.ID:
		t.Fatal("the chain must reach the source")
	case link.Source.TrustLevel == "":
		t.Fatal("the chain must expose how much the source is trusted")
	}

	// The quoted text must be reproducible from the archived bytes, not merely stored
	// alongside them.
	if link.Chunk.Content != statement {
		t.Fatalf("the cited chunk must reproduce the source text, got %q", link.Chunk.Content)
	}
}

// A derived claim must name what produced it and what it reasoned from. Inference is
// never presented as direct observation (AGENTS.md section 6.11).
func TestIntegrationDerivedClaimExplainsItself(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	eventID, episodeID, _ := h.ingestEpisode(t,
		"Acme ordered fasteners in January, February, and March.", "orders-1")

	observed := h.assertOne(t, eventID, knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Acme", Type: "organization"},
		Predicate: "ORDER_COUNT",
		Object:    domain.ObjectOfInteger(3),
		Evidence:  []knowledge.EvidenceInput{{EpisodeID: episodeID}},
	})

	// An inferred claim without a derivation must be rejected outright.
	_, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope:         h.scope(),
		Principal:     h.principal(),
		SourceEventID: eventID,
		Claims: []knowledge.Claim{{
			Subject:        knowledge.EntityRef{Name: "Acme", Type: "organization"},
			Predicate:      "IS_RECURRING_CUSTOMER",
			Object:         domain.ObjectOfBool(true),
			ProvenanceMode: domain.ProvenanceInferred,
		}},
	})
	if err == nil {
		t.Fatal("an inferred claim with no derivation must be rejected")
	}
	if !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("expected invalid_argument, got %s", domain.CodeOf(err))
	}

	// With a derivation naming its inputs, it is accepted and explainable.
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
		t.Fatalf("assert derived claim: %v", err)
	}
	derived := result.Assertions[0]
	if derived.ProvenanceMode != domain.ProvenanceInferred {
		t.Fatalf("the claim must stay marked as inferred, got %s", derived.ProvenanceMode)
	}

	chain, err := h.service.Provenance(ctx, h.scope().WorkspaceID, derived.ID)
	if err != nil {
		t.Fatalf("walk provenance: %v", err)
	}
	if chain.Derivation == nil {
		t.Fatal("a derived claim must expose what produced it")
	}
	if chain.Derivation.RuleName != "recurring_customer" {
		t.Fatalf("unexpected rule: %q", chain.Derivation.RuleName)
	}
	if len(chain.Supports) != 1 || chain.Supports[0].ID != observed.ID {
		t.Fatal("a derived claim must expose the claims it reasoned from")
	}
}
