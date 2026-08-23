package resolution_test

import (
	"context"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/resolution"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

type harness struct {
	fixture  *pgtest.Fixture
	resolver *resolution.Resolver
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	f := pgtest.NewFixture(t)
	return &harness{
		fixture:  f,
		resolver: resolution.New(f.Store, resolution.DefaultOptions(), nil),
	}
}

func (h *harness) scope() domain.Scope { return h.fixture.Primary.Scope() }

func (h *harness) resolve(t *testing.T, mention domain.Mention) resolution.Result {
	t.Helper()

	result, err := h.resolver.Resolve(context.Background(), h.scope(), mention)
	if err != nil {
		t.Fatalf("resolve %q: %v", mention.Name, err)
	}
	return result
}

// Acceptance criterion: repeated mentions resolve consistently.
func TestIntegrationRepeatedMentionsResolveConsistently(t *testing.T) {
	h := newHarness(t)

	first := h.resolve(t, domain.Mention{Name: "Acme Corporation", EntityType: "organization"})
	if !first.Created || first.Method != domain.MethodCreated {
		t.Fatalf("the first mention should create an identity, got %s", first.Method)
	}

	// The same name again resolves to the same identity, by exact alias.
	for i := 0; i < 5; i++ {
		again := h.resolve(t, domain.Mention{Name: "Acme Corporation", EntityType: "organization"})
		if again.EntityID != first.EntityID {
			t.Fatalf("mention %d resolved to a different identity", i)
		}
		if again.Created {
			t.Fatal("a repeated mention must not create a new identity")
		}
		if again.Method != domain.MethodExactAlias {
			t.Fatalf("expected an exact alias match, got %s", again.Method)
		}
	}

	// Case and spacing are insignificant in a name.
	variant := h.resolve(t, domain.Mention{Name: "  acme   CORPORATION ", EntityType: "organization"})
	if variant.EntityID != first.EntityID {
		t.Fatal("case and spacing must not create a second identity")
	}
}

func TestIntegrationStableIdentifiersOutrankNames(t *testing.T) {
	h := newHarness(t)
	sourceID := h.fixture.Primary.Source.ID

	// A record arrives with the upstream system's own key.
	first := h.resolve(t, domain.Mention{
		Name: "Acme Corporation", EntityType: "organization",
		SourceID: &sourceID, ExternalID: "cust-001",
	})

	// The same record later appears under a different name. The key settles it: this is
	// the case a name-only resolver gets wrong.
	renamed := h.resolve(t, domain.Mention{
		Name: "Acme Holdings GmbH", EntityType: "organization",
		SourceID: &sourceID, ExternalID: "cust-001",
	})
	if renamed.EntityID != first.EntityID {
		t.Fatal("an upstream key must settle identity even when the name changed")
	}
	if renamed.Method != domain.MethodSourceIdentifier {
		t.Fatalf("expected the source identifier rung, got %s", renamed.Method)
	}
	if renamed.Confidence != 1.0 {
		t.Fatalf("an upstream key is the strongest evidence available, got %v", renamed.Confidence)
	}

	// And a different record that happens to share the original name is not confused with
	// it, because it carries its own key.
	other := h.resolve(t, domain.Mention{
		Name: "Acme Corporation", EntityType: "organization",
		SourceID: &sourceID, ExternalID: "cust-002",
	})
	if other.EntityID == first.EntityID {
		t.Fatal("distinct upstream keys must be distinct identities, whatever they are called")
	}

	// The new name became searchable, so later name-only mentions find the record too.
	byNewName := h.resolve(t, domain.Mention{Name: "Acme Holdings GmbH", EntityType: "organization"})
	if byNewName.EntityID != first.EntityID {
		t.Fatal("a name learned from a resolved mention must be matchable later")
	}
}

func TestIntegrationDomainKeysResolveAcrossSources(t *testing.T) {
	h := newHarness(t)

	first := h.resolve(t, domain.Mention{
		Name: "Alice Chen", EntityType: "person",
		DomainKeys: []domain.DomainKey{{Namespace: "email", Value: "alice@acme.example"}},
	})

	// A different source, a different spelling of the name, the same business key.
	same := h.resolve(t, domain.Mention{
		Name: "A. Chen", EntityType: "person",
		DomainKeys: []domain.DomainKey{{Namespace: "email", Value: "ALICE@ACME.EXAMPLE"}},
	})
	if same.EntityID != first.EntityID {
		t.Fatal("a domain key must resolve across sources and spellings")
	}
	if same.Method != domain.MethodDomainKey {
		t.Fatalf("expected the domain key rung, got %s", same.Method)
	}

	// A different key is a different identity, even with an identical name.
	other := h.resolve(t, domain.Mention{
		Name: "Alice Chen", EntityType: "person",
		DomainKeys: []domain.DomainKey{{Namespace: "email", Value: "alice.chen@globex.example"}},
	})
	if other.EntityID == first.EntityID {
		t.Fatal("distinct business keys must not be merged on the strength of a shared name")
	}
}

// Acceptance criterion: ambiguous cases can remain separate.
func TestIntegrationAmbiguityKeepsIdentitiesSeparate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Two people genuinely share a name.
	var known []domain.EntityID
	for i := 0; i < 2; i++ {
		entity, err := h.fixture.Store.CreateEntity(ctx, domain.Entity{
			WorkspaceID:   h.scope().WorkspaceID,
			GraphSpaceID:  h.scope().GraphSpaceID,
			CanonicalName: "Alex Kim",
			EntityType:    "person",
		})
		if err != nil {
			t.Fatalf("create entity: %v", err)
		}
		known = append(known, entity.ID)
	}

	result := h.resolve(t, domain.Mention{Name: "Alex Kim", EntityType: "person"})

	if result.Method != domain.MethodAmbiguous {
		t.Fatalf("expected the ambiguity to be recognized, got %s", result.Method)
	}
	if !result.Created {
		t.Fatal("an ambiguous mention must get its own identity rather than joining one")
	}
	for _, prior := range known {
		if result.EntityID == prior {
			t.Fatal("the resolver picked one of two equally plausible identities")
		}
	}
	if len(result.Candidates) != 2 {
		t.Fatalf("both candidates must be recorded for review, got %d", len(result.Candidates))
	}
	if result.Confidence >= 0.9 {
		t.Fatalf("an ambiguous resolution must not claim high confidence, got %v", result.Confidence)
	}
}

func TestIntegrationFuzzyMatchingOnlyGeneratesCandidates(t *testing.T) {
	h := newHarness(t)

	original := h.resolve(t, domain.Mention{Name: "Alice Chen", EntityType: "person"})

	// Similarity is not evidence of identity. Measured on PostgreSQL, a transposition typo
	// of a company name scores 0.619 while two entirely different people, "Alice Chen" and
	// "Alice Chan", score 0.571. No threshold separates those, so a close name is recorded
	// as a candidate and never acted on.
	similar := h.resolve(t, domain.Mention{Name: "Alice Chan", EntityType: "person"})
	if similar.EntityID == original.EntityID {
		t.Fatal("a similar name must not resolve to an existing identity")
	}
	if similar.Method != domain.MethodAmbiguous {
		t.Fatalf("a near miss should be kept separate for review, got %s", similar.Method)
	}
	if len(similar.Candidates) == 0 {
		t.Fatal("the near miss must be recorded so a human can merge later")
	}

	// A typo behaves identically: kept separate, recorded, mergeable by a human. That is
	// the deliberate cost of never merging two different people by accident.
	typo := h.resolve(t, domain.Mention{Name: "Alice Chenn", EntityType: "person"})
	if typo.EntityID == original.EntityID {
		t.Fatal("even a likely typo must not auto-resolve")
	}
	if len(typo.Candidates) == 0 {
		t.Fatal("the typo must surface the identity it probably meant")
	}

	// An unrelated name generates no candidates at all, so review queues stay meaningful.
	unrelated := h.resolve(t, domain.Mention{Name: "Zbigniew Kowalski", EntityType: "person"})
	if unrelated.Method != domain.MethodCreated {
		t.Fatalf("an unrelated name is simply new, got %s", unrelated.Method)
	}
	if len(unrelated.Candidates) != 0 {
		t.Fatalf("an unrelated name must not produce candidates, got %d", len(unrelated.Candidates))
	}
}

func TestIntegrationTypesKeepDistinctThingsApart(t *testing.T) {
	h := newHarness(t)

	person := h.resolve(t, domain.Mention{Name: "Washington", EntityType: "person"})
	place := h.resolve(t, domain.Mention{Name: "Washington", EntityType: "place"})

	if person.EntityID == place.EntityID {
		t.Fatal("a person and a place sharing a name are not the same thing")
	}
	if place.Method == domain.MethodExactAlias {
		t.Fatal("an exact name match across types must not resolve")
	}
}

// Acceptance criterion: a mistaken merge can be reversed without losing provenance.
func TestIntegrationMergeIsReversible(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ws := h.scope().WorkspaceID

	left := h.resolve(t, domain.Mention{Name: "Acme Corp", EntityType: "organization"})
	right := h.resolve(t, domain.Mention{Name: "Acme Corporation", EntityType: "organization"})
	if left.EntityID == right.EntityID {
		t.Fatal("these should start as separate identities")
	}

	// An operator decides they are the same thing.
	merge, err := h.fixture.Store.MergeEntities(ctx, ws, left.EntityID, right.EntityID,
		domain.ResolutionDecision{
			WorkspaceID:  ws,
			GraphSpaceID: h.scope().GraphSpaceID,
			Confidence:   1,
			ActorID:      h.fixture.Primary.Principal.ID,
			Reason:       "same company, different spelling",
		})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merge.Method != domain.MethodHumanMerge || !merge.HumanOverride {
		t.Fatalf("a merge must be recorded as a human decision: %+v", merge)
	}

	// The merged identity now redirects, and both identifiers reach one cluster.
	canonical, err := h.fixture.Store.CanonicalEntityID(ctx, ws, left.EntityID)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	if canonical != right.EntityID {
		t.Fatalf("the merged identity should redirect to the survivor, got %s", canonical)
	}
	cluster, err := h.fixture.Store.IdentityCluster(ctx, ws, left.EntityID)
	if err != nil {
		t.Fatalf("cluster: %v", err)
	}
	if len(cluster) != 2 {
		t.Fatalf("the cluster should contain both identities, got %d", len(cluster))
	}

	// Nothing was destroyed: the merged entity's row, name, and aliases all survive. That
	// is what makes the merge reversible at all (AGENTS.md section 12.3).
	merged, err := h.fixture.Store.GetEntity(ctx, ws, left.EntityID)
	if err != nil {
		t.Fatalf("the merged identity must still exist: %v", err)
	}
	if merged.CanonicalName != "Acme Corp" {
		t.Fatal("a merge must not rewrite the identity it redirected")
	}
	aliases, err := h.fixture.Store.ListAliases(ctx, ws, left.EntityID)
	if err != nil || len(aliases) == 0 {
		t.Fatalf("the merged identity must keep its names: %v", err)
	}

	// The merge was wrong. Undo it.
	split, err := h.fixture.Store.SplitEntity(ctx, ws, left.EntityID, domain.ResolutionDecision{
		WorkspaceID:  ws,
		GraphSpaceID: h.scope().GraphSpaceID,
		Confidence:   1,
		ActorID:      h.fixture.Primary.Principal.ID,
		Reason:       "they are different subsidiaries after all",
	})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if split.Method != domain.MethodHumanSplit {
		t.Fatalf("expected a split decision, got %s", split.Method)
	}

	// The identity stands on its own again.
	canonical, err = h.fixture.Store.CanonicalEntityID(ctx, ws, left.EntityID)
	if err != nil {
		t.Fatalf("canonical after split: %v", err)
	}
	if canonical != left.EntityID {
		t.Fatal("after a split the identity must be its own canonical form")
	}

	// The history of both decisions survives: the merge is marked reverted rather than
	// deleted, so the record of what happened is intact.
	decisions, err := h.fixture.Store.ListResolutionDecisions(ctx, h.scope(), false, 50)
	if err != nil {
		t.Fatalf("list decisions: %v", err)
	}
	var sawMerge, sawSplit bool
	for _, decision := range decisions {
		switch decision.Method {
		case domain.MethodHumanMerge:
			sawMerge = true
			if decision.RevertedAt == nil {
				t.Fatal("the reversed merge must be marked as reverted")
			}
			if decision.Reason == "" {
				t.Fatal("a merge must record why it was made")
			}
		case domain.MethodHumanSplit:
			sawSplit = true
		}
	}
	if !sawMerge || !sawSplit {
		t.Fatal("both the merge and the split must remain in the decision ledger")
	}
}

func TestIntegrationResolvingThroughAMergeFollowsTheRedirect(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ws := h.scope().WorkspaceID

	left := h.resolve(t, domain.Mention{Name: "Initech", EntityType: "organization"})
	right := h.resolve(t, domain.Mention{Name: "Initech Limited", EntityType: "organization"})

	if _, err := h.fixture.Store.MergeEntities(ctx, ws, left.EntityID, right.EntityID,
		domain.ResolutionDecision{
			WorkspaceID: ws, GraphSpaceID: h.scope().GraphSpaceID, Confidence: 1,
			ActorID: h.fixture.Primary.Principal.ID, Reason: "same company",
		}); err != nil {
		t.Fatalf("merge: %v", err)
	}

	// A later mention of the merged name resolves to the survivor, not to a dangling
	// redirect.
	again := h.resolve(t, domain.Mention{Name: "Initech", EntityType: "organization"})
	if again.EntityID != right.EntityID {
		t.Fatalf("a mention of a merged name must resolve to the surviving identity, got %s",
			again.EntityID)
	}
}

func TestIntegrationMergeRejectsCyclesAndSelfMerges(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ws := h.scope().WorkspaceID

	a := h.resolve(t, domain.Mention{Name: "Alpha", EntityType: "organization"})
	b := h.resolve(t, domain.Mention{Name: "Beta", EntityType: "organization"})

	decision := domain.ResolutionDecision{
		WorkspaceID: ws, GraphSpaceID: h.scope().GraphSpaceID, Confidence: 1,
		ActorID: h.fixture.Primary.Principal.ID, Reason: "test",
	}

	if _, err := h.fixture.Store.MergeEntities(ctx, ws, a.EntityID, a.EntityID, decision); err == nil {
		t.Fatal("an entity must not be mergeable into itself")
	}
	if _, err := h.fixture.Store.MergeEntities(ctx, ws, a.EntityID, b.EntityID, decision); err != nil {
		t.Fatalf("merge: %v", err)
	}
	// A cycle would make canonical resolution loop forever.
	if _, err := h.fixture.Store.MergeEntities(ctx, ws, b.EntityID, a.EntityID, decision); err == nil {
		t.Fatal("a merge that would create a cycle must be refused")
	}
	// Merging something already merged is a conflict, not a silent rebind.
	if _, err := h.fixture.Store.MergeEntities(ctx, ws, a.EntityID, b.EntityID, decision); err == nil {
		t.Fatal("an already-merged entity must not be merged again")
	}
}

func TestIntegrationMergeChainsCollapseToOneHop(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ws := h.scope().WorkspaceID

	a := h.resolve(t, domain.Mention{Name: "One", EntityType: "thing"})
	b := h.resolve(t, domain.Mention{Name: "Two", EntityType: "thing"})
	c := h.resolve(t, domain.Mention{Name: "Three", EntityType: "thing"})

	decision := domain.ResolutionDecision{
		WorkspaceID: ws, GraphSpaceID: h.scope().GraphSpaceID, Confidence: 1,
		ActorID: h.fixture.Primary.Principal.ID, Reason: "test",
	}
	if _, err := h.fixture.Store.MergeEntities(ctx, ws, a.EntityID, b.EntityID, decision); err != nil {
		t.Fatalf("merge a into b: %v", err)
	}
	// Merging b into c must not leave a pointing at a now-merged b.
	if _, err := h.fixture.Store.MergeEntities(ctx, ws, b.EntityID, c.EntityID, decision); err != nil {
		t.Fatalf("merge b into c: %v", err)
	}

	for _, member := range []domain.EntityID{a.EntityID, b.EntityID} {
		canonical, err := h.fixture.Store.CanonicalEntityID(ctx, ws, member)
		if err != nil {
			t.Fatalf("canonical: %v", err)
		}
		if canonical != c.EntityID {
			t.Fatalf("every member of the chain must resolve to the survivor, got %s", canonical)
		}
	}

	cluster, err := h.fixture.Store.IdentityCluster(ctx, ws, a.EntityID)
	if err != nil {
		t.Fatalf("cluster: %v", err)
	}
	if len(cluster) != 3 {
		t.Fatalf("the whole chain belongs to one cluster, got %d", len(cluster))
	}
}

func TestIntegrationIdentifierConflictsAreReportedNotRebound(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sourceID := h.fixture.Primary.Source.ID

	first := h.resolve(t, domain.Mention{
		Name: "Record A", EntityType: "thing", SourceID: &sourceID, ExternalID: "row-1",
	})
	other, err := h.fixture.Store.CreateEntity(ctx, domain.Entity{
		WorkspaceID:   h.scope().WorkspaceID,
		GraphSpaceID:  h.scope().GraphSpaceID,
		CanonicalName: "Record B",
		EntityType:    "thing",
	})
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	// Two identities claiming one upstream key is a real problem upstream. Quietly
	// rebinding would hide it.
	_, err = h.fixture.Store.UpsertIdentifier(ctx, domain.EntityIdentifier{
		WorkspaceID:  h.scope().WorkspaceID,
		GraphSpaceID: h.scope().GraphSpaceID,
		EntityID:     other.ID,
		Kind:         domain.IdentifierSource,
		Namespace:    string(sourceID),
		Value:        "row-1",
	})
	if !domain.IsCode(err, domain.CodeConflict) {
		t.Fatalf("expected a conflict, got %s", domain.CodeOf(err))
	}

	// The original binding is untouched.
	bound, err := h.fixture.Store.FindByIdentifier(ctx, h.scope(),
		domain.IdentifierSource, string(sourceID), "row-1")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if bound.EntityID != first.EntityID {
		t.Fatal("a conflicting claim must not steal an existing binding")
	}
}

func TestIntegrationEveryResolutionIsRecorded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.resolve(t, domain.Mention{Name: "Recorded Co", EntityType: "organization"})
	h.resolve(t, domain.Mention{Name: "Recorded Co", EntityType: "organization"})

	decisions, err := h.fixture.Store.ListResolutionDecisions(ctx, h.scope(), false, 50)
	if err != nil {
		t.Fatalf("list decisions: %v", err)
	}
	if len(decisions) != 2 {
		t.Fatalf("every resolution must be recorded, got %d", len(decisions))
	}

	// Each records enough to review it: what was matched, how, how confident, and which
	// version of the resolver decided (AGENTS.md section 12.2).
	for _, decision := range decisions {
		switch {
		case decision.MentionText != "Recorded Co":
			t.Fatalf("the mention must be recorded: %q", decision.MentionText)
		case decision.ResolverVersion != resolution.Version:
			t.Fatalf("the resolver version must be recorded, got %d", decision.ResolverVersion)
		case domain.IsZero(decision.ChosenEntityID):
			t.Fatal("the chosen identity must be recorded")
		case !decision.Method.Automatic():
			t.Fatal("these were automatic decisions")
		}
	}
}

func TestIntegrationResolutionIsWorkspaceScoped(t *testing.T) {
	h := newHarness(t)

	mine := h.resolve(t, domain.Mention{Name: "Shared Name", EntityType: "organization"})

	other := h.fixture.NewTenant(t, "globex")
	theirResolver := resolution.New(h.fixture.Store, resolution.DefaultOptions(), nil)
	theirs, err := theirResolver.Resolve(context.Background(), other.Scope(),
		domain.Mention{Name: "Shared Name", EntityType: "organization"})
	if err != nil {
		t.Fatalf("resolve in the other tenant: %v", err)
	}

	// An identical name in another tenant is a different thing entirely.
	if theirs.EntityID == mine.EntityID {
		t.Fatal("identities must never resolve across workspaces")
	}
	if !theirs.Created {
		t.Fatal("the other tenant should have created its own identity")
	}
}
