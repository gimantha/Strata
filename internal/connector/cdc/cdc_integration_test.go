package cdc_test

import (
	"context"
	"testing"

	"github.com/gimantha/strata/internal/domain"
)

// TestIntegrationRowUpdateDoesNotRebuildTheSubgraph is phase 10's first acceptance
// criterion, measured by assertion identity rather than by counting rows.
//
// An update touches one column. The claims that column produced must move, and every other
// claim about the subject must be the same assertion it was before — same id, still active,
// never superseded. Counting assertions would pass even if the whole subgraph were deleted
// and rewritten, which is precisely what this forbids.
func TestIntegrationRowUpdateDoesNotRebuildTheSubgraph(t *testing.T) {
	h := newHarness(t)
	h.registerStream(t, "public.customers", customerMapping())

	insert := map[string]any{
		"id": 42, "name": "Acme Corporation", "tier": "STANDARD",
		"region": "west", "credit_limit": 50000,
	}
	h.consume(t, "public.customers", false,
		row(domain.ChangeInsert, "0/1000", "1", at(t, "2026-01-01T00:00:00Z"), insert))

	before := h.ids(t)
	if len(before) != 4 {
		t.Fatalf("expected four claims from four mapped columns, got %d: %v", len(before), before)
	}

	// One column moves. Everything else is byte-identical.
	updated := map[string]any{
		"id": 42, "name": "Acme Corporation", "tier": "PREMIUM",
		"region": "west", "credit_limit": 50000,
	}
	event := row(domain.ChangeUpdate, "0/2000", "2", at(t, "2026-02-01T00:00:00Z"), updated)
	event.Before = insert
	h.consume(t, "public.customers", false, event)

	after := h.ids(t)
	if got := h.current(t)["TIER"]; got != "PREMIUM" {
		t.Fatalf("the changed column should be current, got %q", got)
	}

	for _, predicate := range []string{"LEGAL_NAME", "REGION", "CREDIT_LIMIT"} {
		if before[predicate] == "" {
			t.Fatalf("%s was missing before the update", predicate)
		}
		if after[predicate] != before[predicate] {
			t.Fatalf("%s was rebuilt: assertion %s became %s",
				predicate, before[predicate], after[predicate])
		}
	}
	if after["TIER"] == before["TIER"] {
		t.Fatal("the changed column should be a new assertion, not an edited one")
	}

	// The old value is superseded, not deleted: last quarter's tier is still answerable.
	old, err := h.fixture.Store.GetAssertion(context.Background(), h.scope().WorkspaceID, before["TIER"])
	if err != nil {
		t.Fatalf("the superseded claim should still exist: %v", err)
	}
	if old.Status != domain.AssertionSuperseded {
		t.Fatalf("expected superseded, got %s", old.Status)
	}
}

// TestIntegrationReplayFromCheckpointIsIdempotent is the second acceptance criterion.
//
// The same change log, replayed twice, must leave the same state. Once because a connector
// restart replays whatever came after the last checkpoint, and once more because replaying
// an archive is how a workspace is rebuilt.
func TestIntegrationReplayFromCheckpointIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.registerStream(t, "public.customers", customerMapping())

	log := []domain.ChangeEvent{
		row(domain.ChangeInsert, "0/1000", "1", at(t, "2026-01-01T00:00:00Z"), map[string]any{
			"id": 42, "name": "Acme Corporation", "tier": "STANDARD", "credit_limit": 50000,
		}),
		row(domain.ChangeUpdate, "0/2000", "2", at(t, "2026-02-01T00:00:00Z"), map[string]any{
			"id": 42, "name": "Acme Corporation", "tier": "PREMIUM", "credit_limit": 50000,
		}),
		row(domain.ChangeUpdate, "0/3000", "3", at(t, "2026-03-01T00:00:00Z"), map[string]any{
			"id": 42, "name": "Acme Corporation", "tier": "PREMIUM", "credit_limit": 75000,
		}),
	}

	first := h.consume(t, "public.customers", true, log...)
	if first.Accepted != 3 {
		t.Fatalf("expected three new events, got %d", first.Accepted)
	}

	stateAfterFirst := h.current(t)
	idsAfterFirst := h.ids(t)

	// Replay the whole log again, resuming from the checkpoint. Nothing should move.
	second := h.consume(t, "public.customers", true, log...)
	if second.Accepted != 0 {
		t.Fatalf("a resumed replay should accept nothing new, got %d", second.Accepted)
	}
	if second.Skipped != 3 {
		t.Fatalf("expected three events skipped by the checkpoint, got %d", second.Skipped)
	}

	// And again ignoring the checkpoint entirely, which is what rebuilding from an
	// archive does. Idempotency has to come from the keys, not from the bookmark.
	third := h.consume(t, "public.customers", false, log...)
	if third.Accepted != 0 {
		t.Fatalf("re-ingesting the same changes should produce no new events, got %d", third.Accepted)
	}
	if third.Duplicates != 3 {
		t.Fatalf("expected three recognized duplicates, got %d", third.Duplicates)
	}

	if got := h.current(t); !sameState(got, stateAfterFirst) {
		t.Fatalf("replay changed the state:\n before %v\n after  %v", stateAfterFirst, got)
	}
	if got := h.ids(t); !sameIDs(got, idsAfterFirst) {
		t.Fatal("replay rewrote assertions that should have been left alone")
	}
}

func sameState(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func sameIDs(a, b map[string]domain.AssertionID) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// TestIntegrationCheckpointResumesWhereItStopped covers the restart path directly: consume
// half a log, then hand the connector the whole thing again.
func TestIntegrationCheckpointResumesWhereItStopped(t *testing.T) {
	h := newHarness(t)
	h.registerStream(t, "public.customers", customerMapping())

	early := row(domain.ChangeInsert, "0/1000", "1", at(t, "2026-01-01T00:00:00Z"), map[string]any{
		"id": 42, "name": "Acme Corporation", "tier": "STANDARD",
	})
	late := row(domain.ChangeUpdate, "0/2000", "2", at(t, "2026-02-01T00:00:00Z"), map[string]any{
		"id": 42, "name": "Acme Corporation", "tier": "PREMIUM",
	})

	h.consume(t, "public.customers", true, early)

	stream, err := h.fixture.Store.GetCDCStream(context.Background(), h.scope().WorkspaceID,
		h.fixture.Primary.Source.ID, "public.customers")
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if stream.Checkpoint.LastOffset != "0/1000" {
		t.Fatalf("checkpoint should be at the last accepted offset, got %q",
			stream.Checkpoint.LastOffset)
	}
	if stream.Checkpoint.EventsConsumed != 1 {
		t.Fatalf("expected one event consumed, got %d", stream.Checkpoint.EventsConsumed)
	}

	result := h.consume(t, "public.customers", true, early, late)
	if result.Skipped != 1 {
		t.Fatalf("the checkpointed event should be skipped, got %d skipped", result.Skipped)
	}
	if result.Accepted != 1 {
		t.Fatalf("the new event should be accepted, got %d", result.Accepted)
	}
	if got := h.current(t)["TIER"]; got != "PREMIUM" {
		t.Fatalf("the resumed event should have taken effect, got %q", got)
	}
}

// TestIntegrationOutOfOrderChangesConverge is scenario B from AGENTS.md section 37: sequence
// 102 arrives before 101, and the system must end up where source order says it should.
func TestIntegrationOutOfOrderChangesConverge(t *testing.T) {
	h := newHarness(t)
	h.registerStream(t, "public.customers", customerMapping())

	// Sequence 102 first: the newer state, arriving early.
	later := row(domain.ChangeUpdate, "0/3000", "102", at(t, "2026-03-01T00:00:00Z"), map[string]any{
		"id": 42, "name": "Acme Corporation", "tier": "PREMIUM",
	})
	// Sequence 101 second: the older state, arriving late.
	earlier := row(domain.ChangeUpdate, "0/2000", "101", at(t, "2026-02-01T00:00:00Z"), map[string]any{
		"id": 42, "name": "Acme Corporation", "tier": "STANDARD",
	})

	h.consume(t, "public.customers", false, later)
	if got := h.current(t)["TIER"]; got != "PREMIUM" {
		t.Fatalf("the first arrival should be current, got %q", got)
	}

	h.consume(t, "public.customers", false, earlier)

	// The late arrival is recorded — it is what the source said — but it never becomes
	// current belief, because the source already told us about a later state.
	if got := h.current(t)["TIER"]; got != "PREMIUM" {
		t.Fatalf("a late arrival must not overwrite newer knowledge, got %q", got)
	}

	all, err := h.fixture.Store.QueryAssertions(context.Background(), domain.AssertionQuery{
		Scope:             h.scope(),
		Predicates:        []string{"TIER"},
		IncludeSuperseded: true,
		Limit:             domain.MaxAssertionLimit,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("both arrivals should be recorded, got %d", len(all))
	}

	var sawStandard bool
	for _, assertion := range all {
		if assertion.Object.Display() == "STANDARD" {
			sawStandard = true
			if assertion.Status == domain.AssertionActive {
				t.Fatal("the out-of-order arrival became current belief")
			}
		}
	}
	if !sawStandard {
		t.Fatal("the late arrival was dropped rather than recorded")
	}
}

// TestIntegrationTombstoneRetractsWithoutErasing covers AGENTS.md section 11.3: a deleted
// row means the source stopped claiming the record, not that it was never true.
func TestIntegrationTombstoneRetractsWithoutErasing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.registerStream(t, "public.customers", customerMapping())

	image := map[string]any{
		"id": 42, "name": "Acme Corporation", "tier": "PREMIUM", "credit_limit": 50000,
	}
	h.consume(t, "public.customers", false,
		row(domain.ChangeInsert, "0/1000", "1", at(t, "2026-01-01T00:00:00Z"), image))

	before := h.ids(t)
	if len(before) == 0 {
		t.Fatal("nothing was asserted, so the tombstone would prove nothing")
	}

	h.consume(t, "public.customers", false,
		row(domain.ChangeDelete, "0/2000", "2", at(t, "2026-04-01T00:00:00Z"), image))

	if remaining := h.current(t); len(remaining) != 0 {
		t.Fatalf("the source stopped claiming this record, so nothing should be current: %v", remaining)
	}

	for predicate, id := range before {
		assertion, err := h.fixture.Store.GetAssertion(ctx, h.scope().WorkspaceID, id)
		if err != nil {
			t.Fatalf("%s was erased rather than retracted: %v", predicate, err)
		}
		if assertion.Status != domain.AssertionRetracted {
			t.Fatalf("%s should be retracted, got %s", predicate, assertion.Status)
		}
		if assertion.RetractionReason == "" {
			t.Fatalf("%s was retracted without saying why", predicate)
		}

		// The evidence survives. A retracted claim must still be explicable.
		evidence, err := h.fixture.Store.ListEvidence(ctx, h.scope().WorkspaceID, id)
		if err != nil || len(evidence) == 0 {
			t.Fatalf("%s lost its evidence: %v", predicate, err)
		}
	}
}

// TestIntegrationUnchangedRowProducesNoNewKnowledge holds the property that makes CDC cheap:
// a row rewritten with identical values costs nothing.
func TestIntegrationUnchangedRowProducesNoNewKnowledge(t *testing.T) {
	h := newHarness(t)
	h.registerStream(t, "public.customers", customerMapping())

	image := map[string]any{"id": 42, "name": "Acme Corporation", "tier": "PREMIUM"}
	h.consume(t, "public.customers", false,
		row(domain.ChangeInsert, "0/1000", "1", at(t, "2026-01-01T00:00:00Z"), image))
	before := h.ids(t)

	// A no-op update: some upstreams emit one for every touched row, whether or not
	// anything moved.
	same := row(domain.ChangeUpdate, "0/2000", "2", at(t, "2026-02-01T00:00:00Z"), image)
	same.Before = image
	h.consume(t, "public.customers", false, same)

	if got := h.ids(t); !sameIDs(got, before) {
		t.Fatal("a row rewritten with identical values produced new assertions")
	}
	if changed := same.ChangedColumns(); len(changed) != 0 {
		t.Fatalf("nothing changed, but the event reports %v", changed)
	}
}

// TestIntegrationUnmappedStreamIsRecordedNotLost checks the ordering people actually hit:
// changes arrive before anyone has written the mapping.
func TestIntegrationUnmappedStreamIsRecordedNotLost(t *testing.T) {
	h := newHarness(t)

	// No stream registered yet.
	result := h.consume(t, "public.customers", false,
		row(domain.ChangeInsert, "0/1000", "1", at(t, "2026-01-01T00:00:00Z"), map[string]any{
			"id": 42, "name": "Acme Corporation", "tier": "PREMIUM",
		}))
	if result.Accepted != 1 {
		t.Fatalf("the change should still be ingested, got %d accepted", result.Accepted)
	}
	if len(h.current(t)) != 0 {
		t.Fatal("nothing can be interpreted without a mapping")
	}

	// Register the mapping and re-run the stage. The archived change is still there.
	h.registerStream(t, "public.customers", customerMapping())
	if _, err := h.process.Process(context.Background(), h.scope().WorkspaceID,
		result.Events[0], true); err != nil {
		t.Fatalf("reprocess: %v", err)
	}

	if got := h.current(t)["TIER"]; got != "PREMIUM" {
		t.Fatalf("a mapping registered later should pick up archived changes, got %q", got)
	}
}
