package contextblock

import (
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
// It counts one token per six letters of a word, one per three digits of a number, one per
// punctuation character and one per line break. Letters of scripts written without spaces
// (Chinese, Japanese and the like) count one token each. That is roughly how today's
// byte-pair tokenizers behave: most English words are one token, long and rare ones split,
// numbers split into groups of three digits, and punctuation mostly stands alone. The
// result is then scaled up by a safety factor.
//
// Deliberately biased to over-count. Under-counting produces a prompt that overflows the
// model's window, which fails at the worst possible moment; over-counting produces a
// slightly smaller prompt, which nobody notices. When the estimate must be exact, supply a
// real tokenizer.
//
// Version 1 counted one token per four letters with a 1.15 safety factor. Measured against
// OpenAI's o200k_base and cl100k_base tokenizers it counted 1.4 to 1.7 times the real
// number on chat messages, documentation, Go code and rendered context blocks, so a block
// asked to fit 4,000 tokens came out near 2,700. Version 2 lands 1.2 to 1.3 times the real
// number on the same text: still above it, with room for tokenizers that split English more
// finely than OpenAI's, and no longer a third of the budget left empty.
type HeuristicEstimator struct {
	// Safety multiplies the raw estimate. 1.0 disables the margin.
	Safety float64
}

// NewHeuristicEstimator returns the default estimator.
func NewHeuristicEstimator() HeuristicEstimator { return HeuristicEstimator{Safety: 1.05} }

func (h HeuristicEstimator) Name() string { return "heuristic-v2" }

// Tolerance reports the error this estimator admits to.
//
// The estimate is above the real count of the common byte-pair tokenizers on ordinary text.
// What remains is the chance of still under-counting unusual text — long identifier soup,
// base64, scripts this heuristic does not know — and tokenizers that split far more finely.
func (h HeuristicEstimator) Tolerance() float64 { return 0.10 }

// Letters per token in a word, and digits per token in a number.
const (
	lettersPerToken = 6
	digitsPerToken  = 3
)

func (h HeuristicEstimator) Estimate(text string) int {
	if text == "" {
		return 0
	}
	safety := h.Safety
	if safety <= 0 {
		safety = 1
	}

	raw := 0
	letters, digits := 0, 0
	flush := func() {
		raw += (letters+lettersPerToken-1)/lettersPerToken + (digits+digitsPerToken-1)/digitsPerToken
		letters, digits = 0, 0
	}
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) && r > unicode.MaxASCII && !unicode.In(r, unicode.Latin, unicode.Greek, unicode.Cyrillic):
			flush()
			raw++
		case unicode.IsDigit(r):
			if letters > 0 {
				flush()
			}
			digits++
		case unicode.IsLetter(r):
			if digits > 0 {
				flush()
			}
			letters++
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
