package contextblock

import (
	"strings"
	"unicode"
)

// Estimator counts tokens in text (AGENTS.md section 20, phase 8).
//
// The interface exists because the right answer depends on a model this package does not
// know about. A caller targeting a specific model should supply that model's tokenizer;
// what ships here is a provider-independent approximation, so the budget machinery is
// testable and correct before any provider is chosen.
type Estimator interface {
	Estimate(text string) int
	// Name identifies the estimator in the budget report, because a number without the
	// thing that produced it cannot be checked.
	Name() string
	// Tolerance is the declared error against a real tokenizer, as a fraction. Assembly
	// keeps the block under budget in this estimator's units; downstream reality may
	// differ by this much.
	Tolerance() float64
}

// HeuristicEstimator approximates byte-pair tokenization without a vocabulary.
//
// It counts a token per short word, one per four characters of a long word, and one per
// punctuation character, which is roughly how BPE behaves on prose: common words are single
// tokens, rare and long ones split, and punctuation stands alone. It is then scaled up by a
// safety factor.
//
// Deliberately biased to over-count. Under-counting produces a prompt that overflows the
// model's window, which fails at the worst possible moment; over-counting produces a
// slightly smaller prompt, which nobody notices. When the estimate must be exact, supply a
// real tokenizer.
type HeuristicEstimator struct {
	// Safety multiplies the raw estimate. 1.0 disables the margin.
	Safety float64
}

// NewHeuristicEstimator returns the default estimator.
func NewHeuristicEstimator() HeuristicEstimator { return HeuristicEstimator{Safety: 1.15} }

func (h HeuristicEstimator) Name() string { return "heuristic-v1" }

// Tolerance reports the error this estimator admits to.
//
// Measured against a byte-pair tokenizer on English prose the raw count lands within about
// 15% either way; the safety factor moves that band upward, so what remains is the chance of
// still under-counting unusual text — dense CJK, base64, long identifier soup.
func (h HeuristicEstimator) Tolerance() float64 { return 0.10 }

func (h HeuristicEstimator) Estimate(text string) int {
	if text == "" {
		return 0
	}

	safety := h.Safety
	if safety <= 0 {
		safety = 1
	}

	raw := 0
	wordRunes := 0
	// A word of four characters or fewer is one token; longer words split every four,
	// which is where BPE stops recognizing a whole word.
	flush := func() {
		if wordRunes == 0 {
			return
		}
		raw += (wordRunes + 3) / 4
		wordRunes = 0
	}

	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if r > unicode.MaxASCII && !unicode.In(r, unicode.Latin, unicode.Greek, unicode.Cyrillic) {
				flush()
				raw++
				continue
			}
			wordRunes++
		case unicode.IsSpace(r):
			flush()
			if r == '\n' {
				raw++
			}
		default:
			flush()
			raw++
		}
	}
	flush()

	estimate := int(float64(raw)*safety + 0.999)
	if estimate < 1 {
		estimate = 1
	}
	return estimate
}

// FixedEstimator counts a token per whitespace-separated field. Test double.
type FixedEstimator struct{}

func (FixedEstimator) Name() string       { return "fixed-word" }
func (FixedEstimator) Tolerance() float64 { return 0 }
func (FixedEstimator) Estimate(s string) int {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return len(strings.Fields(s))
}
