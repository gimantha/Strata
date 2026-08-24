package domain

import (
	"testing"
	"time"
)

var noon = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// after returns an instant relative to noon. Named to avoid colliding with the interval
// tests' own helper, which counts hours rather than durations.
func after(offset time.Duration) *time.Time {
	t := noon.Add(offset)
	return &t
}

func TestActiveWindowIsHalfOpen(t *testing.T) {
	// Every interval in this system is half-open. Something active until noon is not
	// active at noon, so a handover at a boundary is clean rather than double-counted.
	window := Lifecycle{ActiveFrom: after(-time.Hour), ActiveUntil: after(0)}

	if !window.Active(noon.Add(-time.Minute)) {
		t.Fatal("should be active a minute before the end")
	}
	if window.Active(noon) {
		t.Fatal("active until noon means not active at noon")
	}
	if window.Active(noon.Add(-2 * time.Hour)) {
		t.Fatal("should not be active before it starts")
	}
}

func TestExpiryOverridesTheActiveWindow(t *testing.T) {
	// Expiry is the deployment saying "stop using this", not a schedule. It wins.
	expired := Lifecycle{ActiveUntil: after(24 * time.Hour), ExpiresAt: after(-time.Hour)}

	if expired.Active(noon) {
		t.Fatal("an expired memory is out of scope whatever its window said")
	}
	if !expired.Expired(noon) {
		t.Fatal("Expired should agree")
	}
}

func TestNoLifecycleMeansAlwaysActive(t *testing.T) {
	// Nearly all knowledge has no context clock at all, and it must not be filtered by one.
	if !(Lifecycle{}).Active(noon) {
		t.Fatal("knowledge with no lifecycle is always in scope")
	}
	if (Lifecycle{}).Expired(noon) {
		t.Fatal("knowledge with no expiry never expires")
	}
	if got := (Lifecycle{}).DecayWeight(noon, DecayHalfLife); got != 1 {
		t.Fatalf("knowledge with no decay carries full weight, got %v", got)
	}
}

func TestDecayHalvesAndThenStops(t *testing.T) {
	decaying := Lifecycle{DecayStartsAt: after(0)}

	if got := decaying.DecayWeight(noon.Add(-time.Hour), DecayHalfLife); got != 1 {
		t.Fatalf("before decay starts, weight is full: %v", got)
	}
	if got := decaying.DecayWeight(noon, DecayHalfLife); got != 1 {
		t.Fatalf("at the instant decay starts, weight is full: %v", got)
	}

	half := decaying.DecayWeight(noon.Add(DecayHalfLife), DecayHalfLife)
	if half < 0.49 || half > 0.51 {
		t.Fatalf("one half-life should halve the weight, got %v", half)
	}

	// The floor is the whole point: decay affects ranking, not truth. A weight of zero
	// would make an old fact unfindable, which is deletion wearing a ranking function's
	// clothes (AGENTS.md section 21.2).
	ancient := decaying.DecayWeight(noon.Add(100*DecayHalfLife), DecayHalfLife)
	if ancient != MinDecayWeight {
		t.Fatalf("decay should settle at the floor, got %v", ancient)
	}
	if MinDecayWeight <= 0 {
		t.Fatal("the floor must be above zero or decay becomes deletion")
	}
}

func TestWorkingMemoryRanksBelowSemantic(t *testing.T) {
	// Scaffolding for a task in progress should not compete with established facts.
	if MemoryPriority(MemoryWorking) >= MemoryPriority(MemorySemantic) {
		t.Fatal("working memory should not outrank semantic knowledge")
	}
	if MemoryPriority(MemorySemantic) != 1 {
		t.Fatalf("semantic knowledge is the baseline, got %v", MemoryPriority(MemorySemantic))
	}
	if MemoryPriority("something-new") <= 0 {
		t.Fatal("an unknown kind should still carry weight rather than vanishing")
	}
}

func TestTheFourWaysOfForgettingStayDistinct(t *testing.T) {
	// AGENTS.md section 21.4: these must not share one ambiguous delete flag.
	if !ForgetRetention.Destructive() || !ForgetErasure.Destructive() {
		t.Fatal("retention deletion and privacy erasure destroy records")
	}
	if ForgetDeactivate.Destructive() || ForgetRetract.Destructive() {
		t.Fatal("deactivation and retraction preserve the record")
	}

	for _, name := range []string{"deactivate", "retract", "retention", "erasure"} {
		if _, err := ParseForgetKind(name); err != nil {
			t.Fatalf("%s should be a recognized kind: %v", name, err)
		}
	}
	if _, err := ParseForgetKind("delete"); err == nil {
		// "delete" is exactly the ambiguous verb this enumeration exists to refuse.
		t.Fatal("an unqualified delete must not be a valid kind")
	}
}

func groupOf(members int, sources int, confidence float64) ObservationGroup {
	group := ObservationGroup{Sources: map[SourceID]struct{}{}}
	for i := range members {
		group.Members = append(group.Members, Assertion{
			Confidence: confidence,
			Temporal:   TemporalCoordinates{RecordedAt: noon.Add(time.Duration(i) * time.Hour)},
		})
	}
	for i := range sources {
		group.Sources[SourceID(string(rune('a'+i)))] = struct{}{}
	}
	return group
}

func TestConsolidationNeedsRepetitionAndCorroboration(t *testing.T) {
	rule := DefaultConsolidationRule()

	if groupOf(2, 1, 0.9).Qualifies(rule) {
		t.Fatal("two observations is not a pattern")
	}
	if !groupOf(3, 1, 0.9).Qualifies(rule) {
		t.Fatal("three observations should qualify under the default rule")
	}

	strict := ConsolidationRule{MinObservations: 3, MinDistinctSources: 2}
	if groupOf(5, 1, 0.9).Qualifies(strict) {
		t.Fatal("one source repeating itself is not corroboration")
	}
	if !groupOf(3, 2, 0.9).Qualifies(strict) {
		t.Fatal("three observations across two sources should qualify")
	}
}

func TestConfidenceRisesWithEvidenceButNeverReachesCertainty(t *testing.T) {
	// A conclusion drawn from observations is never more certain than an observation,
	// however many times it was seen. The cap is what stops repetition from manufacturing
	// confidence out of one unreliable source saying the same thing all day.
	few := groupOf(3, 1, 0.8).Confidence()
	many := groupOf(20, 1, 0.8).Confidence()
	corroborated := groupOf(3, 3, 0.8).Confidence()

	if many <= few {
		t.Fatalf("more observations should raise confidence: %v then %v", few, many)
	}
	if corroborated <= few {
		t.Fatalf("independent sources should raise confidence: %v then %v", few, corroborated)
	}
	if many >= 0.96 {
		t.Fatalf("repetition should not manufacture certainty, got %v", many)
	}
	if weak := groupOf(10, 3, 0.2).Confidence(); weak >= 0.8 {
		t.Fatalf("weak observations should stay weak, got %v", weak)
	}
	if empty := (ObservationGroup{}).Confidence(); empty != 0 {
		t.Fatalf("no observations means no confidence, got %v", empty)
	}
}

func TestConsolidatedValidityStartsWhenTheEarliestObservationDid(t *testing.T) {
	group := ObservationGroup{Sources: map[SourceID]struct{}{}}
	early, late := noon.Add(-72*time.Hour), noon.Add(-time.Hour)
	group.Members = []Assertion{
		{Temporal: TemporalCoordinates{ValidFrom: &late}},
		{Temporal: TemporalCoordinates{ValidFrom: &early}},
		{Temporal: TemporalCoordinates{}},
	}

	got := group.EarliestValidFrom()
	if got == nil || !got.Equal(early) {
		t.Fatalf("expected the earliest stated start, got %v", got)
	}
}

func TestObservationGroupKeyDistinguishesSlots(t *testing.T) {
	base := ObservationGroup{SubjectID: "e1", Predicate: "TIER", ScopeKey: "", ObjectKey: "PREMIUM"}
	sameSlotDifferentValue := base
	sameSlotDifferentValue.ObjectKey = "STANDARD"
	differentScope := base
	differentScope.ScopeKey = "eu"

	if base.Key() == sameSlotDifferentValue.Key() {
		t.Fatal("different values must not consolidate together")
	}
	if base.Key() == differentScope.Key() {
		t.Fatal("different scopes are different slots")
	}
	if base.Key() != (ObservationGroup{
		SubjectID: "e1", Predicate: "TIER", ObjectKey: "PREMIUM",
	}).Key() {
		t.Fatal("the same slot must produce the same key")
	}
}
