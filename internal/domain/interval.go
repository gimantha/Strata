package domain

import "time"

// Interval is a half-open time range [Start, End). A nil bound is unbounded: a nil
// Start means "since forever", a nil End means "still holds as far as this claim says".
//
// Half-open matters for correctness: two claims where one ends exactly when the next
// begins do not overlap, so a clean handover is not mistaken for a contradiction.
type Interval struct {
	Start *time.Time
	End   *time.Time
}

// ValidityOf returns the world-validity interval of a claim.
func ValidityOf(t TemporalCoordinates) Interval {
	return Interval{Start: t.ValidFrom, End: t.ValidTo}
}

// IntervalRelation is an Allen-style relation between two intervals
// (AGENTS.md section 7.5). Callers use the intuitive helpers below; this exists for the
// cases where the precise relation matters.
type IntervalRelation string

const (
	RelationBefore       IntervalRelation = "before"
	RelationAfter        IntervalRelation = "after"
	RelationMeets        IntervalRelation = "meets"
	RelationMetBy        IntervalRelation = "met_by"
	RelationOverlaps     IntervalRelation = "overlaps"
	RelationOverlappedBy IntervalRelation = "overlapped_by"
	RelationDuring       IntervalRelation = "during"
	RelationContains     IntervalRelation = "contains"
	RelationStarts       IntervalRelation = "starts"
	RelationStartedBy    IntervalRelation = "started_by"
	RelationFinishes     IntervalRelation = "finishes"
	RelationFinishedBy   IntervalRelation = "finished_by"
	RelationEquals       IntervalRelation = "equals"
)

// Contains reports whether an instant falls inside the interval.
func (i Interval) Contains(t time.Time) bool {
	if i.Start != nil && i.Start.After(t) {
		return false
	}
	if i.End != nil && !i.End.After(t) {
		return false
	}
	return true
}

// Empty reports whether the interval covers no time at all.
func (i Interval) Empty() bool {
	return i.Start != nil && i.End != nil && !i.End.After(*i.Start)
}

// Overlaps reports whether the two intervals share any instant.
//
// This is the question conflict detection actually asks: two claims about the same
// subject and predicate can only contradict each other if they claim to hold at the
// same time.
func (i Interval) Overlaps(other Interval) bool {
	if i.Empty() || other.Empty() {
		return false
	}
	// They overlap unless one ends at or before the other begins.
	if endsAtOrBefore(i.End, other.Start) {
		return false
	}
	if endsAtOrBefore(other.End, i.Start) {
		return false
	}
	return true
}

// Relate returns the Allen relation of i to other.
func (i Interval) Relate(other Interval) IntervalRelation {
	startCmp := compareStarts(i.Start, other.Start)
	endCmp := compareEnds(i.End, other.End)

	switch {
	case startCmp == 0 && endCmp == 0:
		return RelationEquals
	case endsAtOrBefore(i.End, other.Start):
		if endsExactlyAt(i.End, other.Start) {
			return RelationMeets
		}
		return RelationBefore
	case endsAtOrBefore(other.End, i.Start):
		if endsExactlyAt(other.End, i.Start) {
			return RelationMetBy
		}
		return RelationAfter
	case startCmp == 0:
		if endCmp < 0 {
			return RelationStarts
		}
		return RelationStartedBy
	case endCmp == 0:
		if startCmp > 0 {
			return RelationFinishes
		}
		return RelationFinishedBy
	case startCmp < 0 && endCmp < 0:
		return RelationOverlaps
	case startCmp > 0 && endCmp > 0:
		return RelationOverlappedBy
	case startCmp > 0 && endCmp < 0:
		return RelationDuring
	default:
		return RelationContains
	}
}

// compareStarts orders two start bounds, treating nil as negative infinity.
func compareStarts(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	case a.Before(*b):
		return -1
	case a.After(*b):
		return 1
	default:
		return 0
	}
}

// compareEnds orders two end bounds, treating nil as positive infinity.
func compareEnds(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	case a.Before(*b):
		return -1
	case a.After(*b):
		return 1
	default:
		return 0
	}
}

// endsAtOrBefore reports whether an end bound is at or before a start bound.
func endsAtOrBefore(end, start *time.Time) bool {
	if end == nil || start == nil {
		return false // an unbounded side always reaches past the other
	}
	return !end.After(*start)
}

// endsExactlyAt reports whether one interval ends precisely where another begins.
func endsExactlyAt(end, start *time.Time) bool {
	return end != nil && start != nil && end.Equal(*start)
}
