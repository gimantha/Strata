package contextblock

import (
	"strings"
	"testing"
)

func TestEstimatorNeverCountsFewerTokensThanWords(t *testing.T) {
	// A tokenizer that returned fewer tokens than there are words would let a block
	// overflow a model's window while reporting that it fits, which is the one failure
	// mode the budget machinery exists to prevent.
	estimator := NewHeuristicEstimator()

	samples := []string{
		"Acme Corporation supplies industrial fasteners to the Portland plant.",
		"SKU-4471 shipped 2026-03-14, invoice INV-88213, net 30.",
		"one two three four five six seven eight nine ten",
		"supercalifragilisticexpialidocious antidisestablishmentarianism",
		"日本語のテキストは空白で区切られていない",
	}
	for _, sample := range samples {
		words := len(strings.Fields(sample))
		if got := estimator.Estimate(sample); got < words {
			t.Fatalf("estimate %d is below the %d words in %q", got, words, sample)
		}
	}
}

func TestEstimatorIsSuperadditiveAcrossLines(t *testing.T) {
	// The renderer bills each fragment as it is written and enforces the budget on the
	// running total. That is only safe if the sum of the parts is at least the estimate
	// of the whole; otherwise the finished block could exceed a budget the renderer
	// believed it was under.
	estimator := NewHeuristicEstimator()

	parts := []string{
		"CONTEXT BLOCK\nquestion: who supplies fasteners\n",
		"\n## KNOWN FACTS\n",
		"[1] Acme Corporation supplies industrial fasteners [since 2026-01-01]\n",
		"[2] Portland Plant operates a night shift (Thursdays)\n",
		"\n## REFERENCES\n",
		"[1] assertion 01a0-aaaa, evidence 01a0-bbbb, source support-chat\n",
	}

	sum := 0
	for _, part := range parts {
		sum += estimator.Estimate(part)
	}
	whole := estimator.Estimate(strings.Join(parts, ""))
	if sum < whole {
		t.Fatalf("parts estimate %d is below the whole-text estimate %d, so the renderer "+
			"could exceed a budget it thought it was under", sum, whole)
	}
}

func TestEstimatorGrowsWithText(t *testing.T) {
	estimator := NewHeuristicEstimator()

	if estimator.Estimate("") != 0 {
		t.Fatal("empty text should cost nothing")
	}
	short := estimator.Estimate("a short line of text")
	long := estimator.Estimate("a short line of text " + strings.Repeat("and more words ", 20))
	if long <= short {
		t.Fatalf("longer text should cost more: %d vs %d", long, short)
	}
}

func TestEstimatorSafetyFactorRaisesTheCount(t *testing.T) {
	const sample = "Acme Corporation supplies industrial fasteners to the Portland plant."

	raw := HeuristicEstimator{Safety: 1}.Estimate(sample)
	padded := NewHeuristicEstimator().Estimate(sample)
	if padded <= raw {
		t.Fatalf("the default estimator should over-count on purpose: %d vs raw %d", padded, raw)
	}
}
