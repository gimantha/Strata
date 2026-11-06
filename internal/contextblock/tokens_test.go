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

func TestEstimatorStaysAboveARealTokenizerWithoutWastingTheBudget(t *testing.T) {
	// Counts from OpenAI's o200k_base tokenizer. The estimate must not fall below them,
	// because then a block could overflow a model's window; and over a whole block it
	// must not run far above them either, because every extra estimated token is budget
	// left empty. Version 1 of the estimator came out 1.4 to 1.7 times these counts.
	samples := []struct {
		text  string
		o200k int
	}{
		{"Caroline: I went to a LGBTQ support group yesterday and it was so powerful.", 17},
		{"Melanie: Woah, Caroline, it sounds like you're doing some impressive work. It's inspiring to see your dedication to helping others. What motivated you to pursue counseling?", 34},
		{"Acme Corporation supplies industrial fasteners to Globex Industries under a long term agreement.", 17},
		{"[12] (2023-07-20 Thu 20:56) Caroline: Hey Melanie! Just wanted to say hi!\n[12] benchmark: conv-26/session_10, Caroline message 12\n", 44},
		{"func (s *selector) best(remaining []candidate, overlaps []float64) (candidate, int, float64) {\n\tbestIndex, bestValue := -1, 0.0\n", 40},
		{`SKU-4471 shipped 2026-03-14, invoice INV-88213, net 30. {"order": 88213, "status": "shipped"}`, 37},
	}
	estimator := NewHeuristicEstimator()
	estimated, real := 0, 0
	for _, s := range samples {
		got := estimator.Estimate(s.text)
		if got < s.o200k {
			t.Errorf("estimate %d is below the real count %d for %q", got, s.o200k, s.text)
		}
		estimated += got
		real += s.o200k
	}
	if ratio := float64(estimated) / float64(real); ratio > 1.4 {
		t.Fatalf("estimates total %.2f times the real count; budget would be left empty", ratio)
	}
}
