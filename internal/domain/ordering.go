package domain

import (
	"math/big"
	"strings"
	"time"
)

// Ordering is the result of comparing two claims' positions in a source's own sequence.
type Ordering int

const (
	// OrderBefore means the first claim describes an earlier state of the record.
	OrderBefore Ordering = -1
	// OrderSame means the two claims are at the same source position.
	OrderSame Ordering = 0
	// OrderAfter means the first claim describes a later state of the record.
	OrderAfter Ordering = 1
	// OrderUnknown means the source gave nothing to order these by. They are treated as
	// concurrent, which is honest: assuming an order the source did not state is how a
	// late event silently overwrites a newer one.
	OrderUnknown Ordering = 2
)

// SourcePosition is where a claim sits in its originating system's own ordering.
//
// Arrival order is never used for this. A CDC stream can deliver update 102 before update
// 101, a webhook can retry an hour late, and a backfill can replay a year of history in
// minutes - in each case the order things were recorded says nothing about the order they
// happened (AGENTS.md section 11.4).
type SourcePosition struct {
	SourceID   SourceID
	Sequence   string
	Version    string
	CommitTime *time.Time
	SourceTime *time.Time
}

// SourcePositionOf reads the ordering evidence off a claim.
func SourcePositionOf(a Assertion) SourcePosition {
	return SourcePosition{
		Sequence:   a.Temporal.SourceSequence,
		Version:    a.Temporal.SourceVersion,
		CommitTime: a.Temporal.SourceCommitTime,
		SourceTime: a.Temporal.SourceTime,
	}
}

// Comparable reports whether the source supplied anything to order by.
func (p SourcePosition) Comparable() bool {
	return p.Sequence != "" || p.Version != "" || p.CommitTime != nil || p.SourceTime != nil
}

// CompareSourcePosition orders two claims by what their source said, in order of how
// reliably each field expresses sequence.
//
// A sequence or LSN is the source's own monotonic counter and is definitive. A version is
// next. Commit time follows, then source time, which is the weakest because clocks skew and
// several records can share a timestamp. When nothing is comparable the answer is
// OrderUnknown rather than a guess.
func CompareSourcePosition(a, b SourcePosition) Ordering {
	// Positions from different sources are not on one timeline, so their sequences cannot
	// be compared at all.
	if a.SourceID != "" && b.SourceID != "" && a.SourceID != b.SourceID {
		return OrderUnknown
	}

	if a.Sequence != "" && b.Sequence != "" {
		if order := compareSequenceStrings(a.Sequence, b.Sequence); order != OrderSame {
			return order
		}
		return OrderSame
	}
	if a.Version != "" && b.Version != "" {
		if order := compareSequenceStrings(a.Version, b.Version); order != OrderSame {
			return order
		}
		return OrderSame
	}
	if a.CommitTime != nil && b.CommitTime != nil {
		return compareTimes(*a.CommitTime, *b.CommitTime)
	}
	if a.SourceTime != nil && b.SourceTime != nil {
		return compareTimes(*a.SourceTime, *b.SourceTime)
	}
	return OrderUnknown
}

// compareSequenceStrings compares two sequence markers.
//
// Numeric comparison is used when both are integers, because "9" precedes "10" in every
// source that emits counters and follows it in string order. Non-numeric markers - LSNs
// such as "0/16B3748", ULIDs, opaque tokens - fall back to lexicographic comparison, which
// is correct for the formats that are designed to sort that way.
func compareSequenceStrings(a, b string) Ordering {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return OrderSame
	}

	if left, right, ok := parseBigInts(a, b); ok {
		switch left.Cmp(right) {
		case -1:
			return OrderBefore
		case 1:
			return OrderAfter
		default:
			return OrderSame
		}
	}

	if a < b {
		return OrderBefore
	}
	return OrderAfter
}

// parseBigInts parses both markers as arbitrary-precision integers, so a 64-bit LSN or a
// long counter does not overflow into a wrong answer.
func parseBigInts(a, b string) (*big.Int, *big.Int, bool) {
	left, okLeft := new(big.Int).SetString(a, 10)
	if !okLeft {
		return nil, nil, false
	}
	right, okRight := new(big.Int).SetString(b, 10)
	if !okRight {
		return nil, nil, false
	}
	return left, right, true
}

func compareTimes(a, b time.Time) Ordering {
	switch {
	case a.Before(b):
		return OrderBefore
	case a.After(b):
		return OrderAfter
	default:
		return OrderSame
	}
}

// Rank orders trust levels from least to most authoritative. It is used to decide which of
// two conflicting sources to believe, so the ordering is deliberately explicit rather than
// derived from declaration order.
func (t TrustLevel) Rank() int {
	switch t {
	case TrustUntrusted:
		return 1
	case TrustLow:
		return 2
	case TrustStandard:
		return 3
	case TrustHigh:
		return 4
	case TrustAuthoritative:
		return 5
	default:
		return 0
	}
}

// MoreAuthoritativeThan reports whether this level outranks another. An unknown level
// outranks nothing, so a misconfigured source can never win an authority contest.
func (t TrustLevel) MoreAuthoritativeThan(other TrustLevel) bool {
	return t.Rank() > 0 && t.Rank() > other.Rank()
}

// ConfidenceFactor turns a trust level into a multiplier on a claim's confidence.
//
// The values express how much a claim is discounted for coming from a given source, and
// they are deliberately gentle: an untrusted source's claim is still recorded and still
// findable, just ranked below better-supported knowledge.
func (t TrustLevel) ConfidenceFactor() float64 {
	switch t {
	case TrustAuthoritative:
		return 1.0
	case TrustHigh:
		return 0.95
	case TrustStandard:
		return 0.85
	case TrustLow:
		return 0.65
	case TrustUntrusted:
		return 0.4
	default:
		return 0.5
	}
}

// ComputeConfidence combines the component signals into a single score
// (AGENTS.md section 14.4).
//
// The formula is deliberately simple and multiplicative: each signal can only reduce
// confidence from a starting point of full belief, except corroboration, which recovers a
// bounded amount. Anything more elaborate would be harder to explain than to justify, and
// the components are stored alongside the result precisely so the number can be argued with.
func ComputeConfidence(b ConfidenceBreakdown) float64 {
	score := 1.0

	for _, component := range []*float64{
		b.SourceTrust, b.Extraction, b.EntityResolution, b.TemporalParsing, b.OntologyValidation,
	} {
		if component != nil {
			score *= clamp01(*component)
		}
	}

	// Corroboration recovers some of what the other signals took away, without ever
	// exceeding full confidence: several mediocre sources agreeing is worth something, but
	// not more than one authoritative source.
	if b.Corroboration != nil && *b.Corroboration > 0 {
		score += (1 - score) * clamp01(*b.Corroboration) * 0.5
	}

	// A contradiction is the strongest reason to doubt a claim, so it is applied last and
	// directly.
	if b.ContradictionPenalty != nil {
		score *= 1 - clamp01(*b.ContradictionPenalty)
	}

	// Human confirmation overrides the arithmetic: someone looked.
	if b.HumanConfirmation != nil && *b.HumanConfirmation > score {
		score = clamp01(*b.HumanConfirmation)
	}

	return clamp01(score)
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}
