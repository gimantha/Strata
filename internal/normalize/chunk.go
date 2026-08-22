package normalize

import (
	"strings"
	"unicode/utf8"

	"github.com/gimantha/strata/internal/domain"
)

// Tokenizer estimates token counts. It is an interface because the real estimator
// depends on the model in use, and context assembly in phase 8 needs a budget that
// matches the target model rather than a guess baked into the chunker.
type Tokenizer interface {
	CountTokens(text string) int
}

// HeuristicTokenizer approximates tokens from character count. It is deliberately
// simple and provider-independent: no domain package may depend on a model's
// tokenizer (AGENTS.md section 2.11).
type HeuristicTokenizer struct {
	// CharsPerToken defaults to 4, close to typical English subword ratios.
	CharsPerToken float64
}

// DefaultTokenizer is the estimator used unless a caller supplies another.
var DefaultTokenizer Tokenizer = HeuristicTokenizer{}

func (h HeuristicTokenizer) charsPerToken() float64 {
	if h.CharsPerToken <= 0 {
		return 4
	}
	return h.CharsPerToken
}

func (h HeuristicTokenizer) CountTokens(text string) int {
	if text == "" {
		return 0
	}
	runes := utf8.RuneCountInString(text)
	tokens := int(float64(runes)/h.charsPerToken() + 0.5)
	if tokens < 1 {
		return 1
	}
	return tokens
}

// ChunkOptions controls chunking.
type ChunkOptions struct {
	MaxTokens     int
	OverlapTokens int
	Tokenizer     Tokenizer
}

func (o ChunkOptions) withDefaults() ChunkOptions {
	if o.MaxTokens <= 0 {
		o.MaxTokens = 320
	}
	if o.OverlapTokens < 0 || o.OverlapTokens >= o.MaxTokens {
		o.OverlapTokens = o.MaxTokens / 8
	}
	if o.Tokenizer == nil {
		o.Tokenizer = DefaultTokenizer
	}
	return o
}

// ChunkSpec is one chunk with its offsets into the text it came from.
//
// The offsets are a hard contract: Content must always equal text[ByteStart:ByteEnd].
// Provenance that cannot reproduce the exact quoted bytes is not provenance.
type ChunkSpec struct {
	Sequence   int64
	Content    string
	TokenCount int
	CharStart  int
	CharEnd    int
	ByteStart  int
	ByteEnd    int
}

// Chunk splits text into overlapping chunks that respect natural boundaries.
//
// Boundaries are preferred in order: paragraph break, line break, sentence end, word
// break. A hard cut is the last resort, because a chunk that ends mid-word retrieves
// badly. Overlap carries context across boundaries so a fact split across two chunks is
// still findable in one of them.
func Chunk(text string, opts ChunkOptions) []ChunkSpec {
	opts = opts.withDefaults()
	if text == "" {
		return nil
	}

	charsPerToken := 4.0
	if h, ok := opts.Tokenizer.(HeuristicTokenizer); ok {
		charsPerToken = h.charsPerToken()
	}
	targetChars := int(float64(opts.MaxTokens) * charsPerToken)
	if targetChars < 16 {
		targetChars = 16
	}
	overlapChars := int(float64(opts.OverlapTokens) * charsPerToken)
	if overlapChars >= targetChars {
		overlapChars = targetChars / 8
	}

	var (
		out       []ChunkSpec
		byteStart int
		charStart int
		sequence  int64
	)

	for byteStart < len(text) {
		remaining := text[byteStart:]

		byteEnd := byteStart + len(remaining)
		if utf8.RuneCountInString(remaining) > targetChars {
			cut := findBreak(remaining, targetChars)
			byteEnd = byteStart + cut
		}

		content := text[byteStart:byteEnd]
		trimmedContent := strings.TrimSpace(content)
		if trimmedContent != "" {
			out = append(out, ChunkSpec{
				Sequence:   sequence,
				Content:    content,
				TokenCount: opts.Tokenizer.CountTokens(content),
				CharStart:  charStart,
				CharEnd:    charStart + utf8.RuneCountInString(content),
				ByteStart:  byteStart,
				ByteEnd:    byteEnd,
			})
			sequence++
		}

		if byteEnd >= len(text) {
			break
		}

		// Step back by the overlap, but always make forward progress: without this
		// guard a pathological boundary could loop forever.
		nextByteStart := byteEnd
		if overlapChars > 0 {
			nextByteStart = stepBack(text, byteStart, byteEnd, overlapChars)
		}
		if nextByteStart <= byteStart {
			nextByteStart = byteEnd
		}

		charStart += utf8.RuneCountInString(text[byteStart:nextByteStart])
		byteStart = nextByteStart
	}

	return out
}

// findBreak returns a byte offset at or before maxRunes runes, preferring a natural
// boundary in the last quarter of the window.
func findBreak(s string, maxRunes int) int {
	hardCut := runeOffsetToByte(s, maxRunes)
	if hardCut >= len(s) {
		return len(s)
	}

	window := s[:hardCut]
	// Only accept a boundary that keeps the chunk reasonably full; otherwise a single
	// early newline would produce a stream of tiny chunks.
	minAccept := runeOffsetToByte(s, maxRunes*3/4)

	for _, sep := range []string{"\n\n", "\n", ". ", "! ", "? ", "; ", ", ", " "} {
		if idx := strings.LastIndex(window, sep); idx >= minAccept {
			return idx + len(sep)
		}
	}
	return hardCut
}

// stepBack computes the next chunk start, overlapping the previous chunk by about
// overlapRunes runes without ever moving backwards past the previous start.
func stepBack(text string, prevStart, end, overlapRunes int) int {
	chunk := text[prevStart:end]
	chunkRunes := utf8.RuneCountInString(chunk)
	if overlapRunes >= chunkRunes {
		return end
	}

	keepFrom := runeOffsetToByte(chunk, chunkRunes-overlapRunes)
	candidate := prevStart + keepFrom

	// Prefer resuming at a word boundary so the overlap reads naturally.
	if idx := strings.IndexAny(text[candidate:end], " \n"); idx >= 0 && idx < 32 {
		candidate += idx + 1
	}
	return candidate
}

// runeOffsetToByte converts a rune offset into a byte offset, clamped to the string.
func runeOffsetToByte(s string, runes int) int {
	if runes <= 0 {
		return 0
	}
	count := 0
	for i := range s {
		if count == runes {
			return i
		}
		count++
	}
	return len(s)
}

// LocatorForChunk derives a chunk's locator from its episode's locator, translating
// offsets into artifact coordinates.
func LocatorForChunk(episode domain.Locator, spec ChunkSpec) domain.Locator {
	out := episode
	out.ArtifactCharStart = episode.ArtifactCharStart + spec.CharStart
	out.ArtifactCharEnd = episode.ArtifactCharStart + spec.CharEnd
	// Copy the extra map so chunks of one episode never share mutable state.
	if episode.Extra != nil {
		extra := make(map[string]any, len(episode.Extra))
		for k, v := range episode.Extra {
			extra[k] = v
		}
		out.Extra = extra
	}
	if episode.HeadingPath != nil {
		out.HeadingPath = append([]string(nil), episode.HeadingPath...)
	}
	return out
}
