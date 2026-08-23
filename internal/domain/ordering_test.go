package domain

import (
	"testing"
	"time"
)

func TestCompareSourcePositionPrefersStrongerEvidence(t *testing.T) {
	commit := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	later := commit.Add(time.Hour)

	// A sequence is the source's own counter and settles the question outright, even when
	// a weaker signal disagrees.
	a := SourcePosition{Sequence: "102", CommitTime: &commit}
	b := SourcePosition{Sequence: "101", CommitTime: &later}
	if got := CompareSourcePosition(a, b); got != OrderAfter {
		t.Fatalf("a sequence must outrank a commit time, got %v", got)
	}

	// Without sequences, a version is next.
	if got := CompareSourcePosition(
		SourcePosition{Version: "3"}, SourcePosition{Version: "10"}); got != OrderBefore {
		t.Fatalf("versions must compare numerically, got %v", got)
	}

	// Then commit time, then source time.
	if got := CompareSourcePosition(
		SourcePosition{CommitTime: &commit}, SourcePosition{CommitTime: &later}); got != OrderBefore {
		t.Fatalf("commit times must order, got %v", got)
	}
	if got := CompareSourcePosition(
		SourcePosition{SourceTime: &later}, SourcePosition{SourceTime: &commit}); got != OrderAfter {
		t.Fatalf("source times must order, got %v", got)
	}
}

func TestCompareSourcePositionComparesSequencesNumerically(t *testing.T) {
	// String ordering would put "9" after "10", which is wrong for every source that emits
	// a counter, and would silently invert the order of two updates.
	if got := CompareSourcePosition(
		SourcePosition{Sequence: "9"}, SourcePosition{Sequence: "10"}); got != OrderBefore {
		t.Fatalf("9 precedes 10, got %v", got)
	}

	// A 64-bit counter must not overflow into a wrong answer.
	if got := CompareSourcePosition(
		SourcePosition{Sequence: "9223372036854775807"},
		SourcePosition{Sequence: "9223372036854775808"}); got != OrderBefore {
		t.Fatalf("large counters must compare correctly, got %v", got)
	}

	// Non-numeric markers such as an LSN fall back to lexicographic order, which is what
	// those formats are designed for.
	if got := CompareSourcePosition(
		SourcePosition{Sequence: "0/16B3748"}, SourcePosition{Sequence: "0/16B3749"}); got != OrderBefore {
		t.Fatalf("LSNs must compare lexicographically, got %v", got)
	}
}

func TestCompareSourcePositionRefusesToGuess(t *testing.T) {
	// Nothing to compare means concurrent, not equal and not ordered. Assuming an order the
	// source never stated is how a late event overwrites a newer one.
	if got := CompareSourcePosition(SourcePosition{}, SourcePosition{}); got != OrderUnknown {
		t.Fatalf("expected unknown ordering, got %v", got)
	}
	if got := CompareSourcePosition(
		SourcePosition{Sequence: "5"}, SourcePosition{}); got != OrderUnknown {
		t.Fatalf("one-sided evidence orders nothing, got %v", got)
	}

	// Sequences from different sources are not on one timeline.
	if got := CompareSourcePosition(
		SourcePosition{SourceID: "a", Sequence: "10"},
		SourcePosition{SourceID: "b", Sequence: "5"}); got != OrderUnknown {
		t.Fatalf("different sources cannot be ordered against each other, got %v", got)
	}
}

func TestSourcePositionComparable(t *testing.T) {
	if (SourcePosition{}).Comparable() {
		t.Fatal("an empty position offers nothing to compare")
	}
	now := time.Now()
	for _, position := range []SourcePosition{
		{Sequence: "1"}, {Version: "v2"}, {CommitTime: &now}, {SourceTime: &now},
	} {
		if !position.Comparable() {
			t.Fatalf("%+v should be comparable", position)
		}
	}
}

func TestTrustLevelOrdering(t *testing.T) {
	levels := []TrustLevel{TrustUntrusted, TrustLow, TrustStandard, TrustHigh, TrustAuthoritative}
	for i := 1; i < len(levels); i++ {
		if !levels[i].MoreAuthoritativeThan(levels[i-1]) {
			t.Fatalf("%s should outrank %s", levels[i], levels[i-1])
		}
	}
	if TrustLow.MoreAuthoritativeThan(TrustHigh) {
		t.Fatal("low trust must not outrank high trust")
	}
	if TrustStandard.MoreAuthoritativeThan(TrustStandard) {
		t.Fatal("equal trust settles nothing")
	}
	// A misconfigured source must never win an authority contest.
	if TrustLevel("nonsense").MoreAuthoritativeThan(TrustUntrusted) {
		t.Fatal("an unknown trust level must outrank nothing")
	}
}

func TestConfidenceFactorsDescendWithTrust(t *testing.T) {
	levels := []TrustLevel{TrustAuthoritative, TrustHigh, TrustStandard, TrustLow, TrustUntrusted}
	for i := 1; i < len(levels); i++ {
		if levels[i].ConfidenceFactor() >= levels[i-1].ConfidenceFactor() {
			t.Fatalf("%s should discount more than %s", levels[i], levels[i-1])
		}
	}
	// Even an untrusted source's claim keeps some confidence: it is recorded and findable,
	// just ranked below better-supported knowledge.
	if TrustUntrusted.ConfidenceFactor() <= 0 {
		t.Fatal("an untrusted claim is still a claim")
	}
}

func TestComputeConfidence(t *testing.T) {
	score := func(v float64) *float64 { return &v }

	if got := ComputeConfidence(ConfidenceBreakdown{}); got != 1 {
		t.Fatalf("no signals means nothing to doubt, got %v", got)
	}

	// Each component can only reduce confidence.
	weak := ComputeConfidence(ConfidenceBreakdown{SourceTrust: score(0.4), Extraction: score(0.5)})
	if weak >= 0.5 {
		t.Fatalf("weak signals must compound downwards, got %v", weak)
	}

	// Corroboration recovers some ground without ever exceeding full confidence.
	corroborated := ComputeConfidence(ConfidenceBreakdown{
		SourceTrust: score(0.4), Extraction: score(0.5), Corroboration: score(1),
	})
	if corroborated <= weak {
		t.Fatalf("corroboration should raise confidence: %v vs %v", corroborated, weak)
	}
	if corroborated > 1 {
		t.Fatalf("confidence must stay within range, got %v", corroborated)
	}

	// A contradiction is the strongest reason to doubt, and applies last.
	contradicted := ComputeConfidence(ConfidenceBreakdown{
		SourceTrust: score(0.95), ContradictionPenalty: score(0.8),
	})
	if contradicted > 0.2 {
		t.Fatalf("a contradiction must dominate, got %v", contradicted)
	}

	// Human confirmation overrides the arithmetic: someone looked.
	confirmed := ComputeConfidence(ConfidenceBreakdown{
		SourceTrust: score(0.4), ContradictionPenalty: score(0.5), HumanConfirmation: score(1),
	})
	if confirmed != 1 {
		t.Fatalf("human confirmation should win, got %v", confirmed)
	}

	// Out-of-range inputs are clamped rather than producing nonsense.
	if got := ComputeConfidence(ConfidenceBreakdown{Extraction: score(5)}); got != 1 {
		t.Fatalf("expected clamping, got %v", got)
	}
	if got := ComputeConfidence(ConfidenceBreakdown{Extraction: score(-3)}); got != 0 {
		t.Fatalf("expected clamping, got %v", got)
	}
}
