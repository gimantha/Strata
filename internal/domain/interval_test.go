package domain

import (
	"testing"
	"time"
)

func at(day int) *time.Time {
	t := time.Date(2026, 3, day, 0, 0, 0, 0, time.UTC)
	return &t
}

func TestIntervalOverlapUsesHalfOpenSemantics(t *testing.T) {
	// Consecutive intervals that meet exactly do not overlap. This is what stops a clean
	// handover - one role ending the day the next begins - from being reported as a
	// contradiction.
	first := Interval{Start: at(1), End: at(10)}
	second := Interval{Start: at(10), End: at(20)}
	if first.Overlaps(second) || second.Overlaps(first) {
		t.Fatal("intervals that merely meet must not overlap")
	}

	overlapping := Interval{Start: at(9), End: at(20)}
	if !first.Overlaps(overlapping) || !overlapping.Overlaps(first) {
		t.Fatal("intervals sharing time must overlap, in both directions")
	}
}

func TestIntervalOverlapWithUnboundedEnds(t *testing.T) {
	// An open-ended claim reaches into the future, so anything after its start overlaps.
	openEnded := Interval{Start: at(1)}
	later := Interval{Start: at(100), End: at(200)}
	if !openEnded.Overlaps(later) {
		t.Fatal("an open-ended interval must overlap a later one")
	}

	openStart := Interval{End: at(10)}
	earlier := Interval{Start: at(1), End: at(5)}
	if !openStart.Overlaps(earlier) {
		t.Fatal("an interval open at the start must overlap an earlier one")
	}

	if !(Interval{}).Overlaps(Interval{Start: at(5), End: at(6)}) {
		t.Fatal("a fully unbounded interval overlaps everything")
	}

	// An interval that ends before another begins still must not overlap it.
	if (Interval{End: at(5)}).Overlaps(Interval{Start: at(5)}) {
		t.Fatal("meeting at a boundary is not an overlap even when unbounded elsewhere")
	}
}

func TestIntervalEmptyNeverOverlaps(t *testing.T) {
	empty := Interval{Start: at(10), End: at(10)}
	if !empty.Empty() {
		t.Fatal("a zero-width half-open interval is empty")
	}
	if empty.Overlaps(Interval{Start: at(1), End: at(20)}) {
		t.Fatal("an empty interval covers no time and cannot overlap")
	}
}

func TestIntervalContains(t *testing.T) {
	iv := Interval{Start: at(10), End: at(20)}
	instant := func(day int) time.Time { return *at(day) }

	if !iv.Contains(instant(10)) {
		t.Fatal("the start instant is inside a half-open interval")
	}
	if iv.Contains(instant(20)) {
		t.Fatal("the end instant is outside a half-open interval")
	}
	if iv.Contains(instant(9)) || iv.Contains(instant(21)) {
		t.Fatal("instants outside the range must not be contained")
	}
	if !(Interval{}).Contains(instant(5)) {
		t.Fatal("an unbounded interval contains every instant")
	}
}

func TestIntervalRelations(t *testing.T) {
	cases := []struct {
		name string
		a, b Interval
		want IntervalRelation
	}{
		{"before", Interval{at(1), at(5)}, Interval{at(10), at(20)}, RelationBefore},
		{"after", Interval{at(10), at(20)}, Interval{at(1), at(5)}, RelationAfter},
		{"meets", Interval{at(1), at(10)}, Interval{at(10), at(20)}, RelationMeets},
		{"met by", Interval{at(10), at(20)}, Interval{at(1), at(10)}, RelationMetBy},
		{"overlaps", Interval{at(1), at(15)}, Interval{at(10), at(20)}, RelationOverlaps},
		{"overlapped by", Interval{at(10), at(20)}, Interval{at(1), at(15)}, RelationOverlappedBy},
		{"during", Interval{at(12), at(15)}, Interval{at(10), at(20)}, RelationDuring},
		{"contains", Interval{at(10), at(20)}, Interval{at(12), at(15)}, RelationContains},
		{"starts", Interval{at(10), at(15)}, Interval{at(10), at(20)}, RelationStarts},
		{"started by", Interval{at(10), at(20)}, Interval{at(10), at(15)}, RelationStartedBy},
		{"finishes", Interval{at(15), at(20)}, Interval{at(10), at(20)}, RelationFinishes},
		{"finished by", Interval{at(10), at(20)}, Interval{at(15), at(20)}, RelationFinishedBy},
		{"equals", Interval{at(10), at(20)}, Interval{at(10), at(20)}, RelationEquals},
		{"both unbounded", Interval{}, Interval{}, RelationEquals},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Relate(tc.b); got != tc.want {
				t.Fatalf("Relate() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestValidityOfReadsTheWorldInterval(t *testing.T) {
	// Validity comes from world time, never from knowledge time: when the system learned
	// something is a different question from when it was true.
	tc := TemporalCoordinates{
		ValidFrom:  at(10),
		ValidTo:    at(20),
		ObservedAt: time.Now(),
		RecordedAt: time.Now(),
	}
	iv := ValidityOf(tc)
	if iv.Start != tc.ValidFrom || iv.End != tc.ValidTo {
		t.Fatal("validity must be taken from the world-time bounds")
	}
}
