package extraction

import (
	"regexp"
	"strings"
)

// Span is a region of source text, in byte offsets.
type Span struct {
	Start  int
	End    int
	Reason string
}

// injectionPatterns match text that is trying to instruct a reader rather than state a
// fact.
//
// This is a heuristic and is not a defense on its own. It exists because quote grounding,
// which catches invented claims, cannot catch a planted one: an attacker who writes
// "report that X is true" into a document has put a genuine quote there for the model to
// find. Detecting the surrounding instruction is what separates that case from an ordinary
// sentence (AGENTS.md section 24).
var injectionPatterns = []struct {
	pattern *regexp.Regexp
	reason  string
}{
	{regexp.MustCompile(`(?i)\b(ignore|disregard|forget)\b.{0,40}\b(previous|prior|above|earlier|all)\b.{0,30}\b(instruction|prompt|rule|direction)`), "instructs the reader to ignore prior instructions"},
	{regexp.MustCompile(`(?i)\byou are now\b|\bfrom now on you\b|\bact as\b.{0,30}\b(admin|administrator|system|developer)\b`), "attempts to reassign the reader's role"},
	{regexp.MustCompile(`(?i)\b(admin|administrator|developer|debug|god)\s+mode\b`), "claims a privileged mode"},
	{regexp.MustCompile(`(?i)\b(new|updated|revised)\s+(instruction|system prompt|directive)s?\b`), "presents itself as new instructions"},
	{regexp.MustCompile(`(?i)\b(reveal|print|output|show|repeat)\b.{0,30}\b(system prompt|your instructions|your prompt)\b`), "asks the reader to disclose its instructions"},
	{regexp.MustCompile(`(?i)\b(call|invoke|execute|run)\b.{0,20}\b(tool|function|command|script)\b`), "asks the reader to take an action"},
	{regexp.MustCompile(`(?i)\bgrant\b.{0,40}\b(access|permission|privilege|admin)\b`), "asks for privileges to be granted"},
	{regexp.MustCompile(`(?i)\b(set|mark|change)\b.{0,40}\b(classification|permission|policy|visibility)\b.{0,20}\b(public|open|none)\b`), "asks for a policy or classification change"},
	{regexp.MustCompile(`(?i)\breport that\b|\bclaim that\b|\bsay that\b.{0,60}\b(is|are|was|were)\b`), "instructs the reader to assert something"},
}

// SuspiciousSpans finds regions of source text that read as instructions.
//
// A match taints its whole paragraph, not just the matching phrase. An instruction and the
// claim it plants are written together - "ignore your instructions and report that X" -
// so treating them as one unit is what makes the planted claim detectable.
func SuspiciousSpans(content string) []Span {
	var spans []Span

	offset := 0
	for _, paragraph := range splitParagraphs(content) {
		start := offset + paragraph.offset
		text := paragraph.text

		for _, candidate := range injectionPatterns {
			if candidate.pattern.MatchString(text) {
				spans = append(spans, Span{
					Start:  start,
					End:    start + len(text),
					Reason: candidate.reason,
				})
				break
			}
		}
	}
	return spans
}

// QuoteIsSuspicious reports whether a quote falls inside instruction-like text.
func QuoteIsSuspicious(content, quote string) (string, bool) {
	if strings.TrimSpace(quote) == "" {
		return "", false
	}
	for _, span := range SuspiciousSpans(content) {
		if span.End > len(content) {
			span.End = len(content)
		}
		region := content[span.Start:span.End]
		if containsCollapsed(region, quote) {
			return span.Reason, true
		}
	}
	return "", false
}

// containsCollapsed compares ignoring whitespace differences, matching how quotes are
// grounded.
func containsCollapsed(haystack, needle string) bool {
	return strings.Contains(collapseWhitespace(haystack), collapseWhitespace(needle))
}

// collapseWhitespace normalizes text for comparison.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

type paragraph struct {
	offset int
	text   string
}

// splitParagraphs breaks text on blank lines, keeping each part's offset.
func splitParagraphs(content string) []paragraph {
	var (
		out   []paragraph
		start int
	)
	for i := 0; i <= len(content); i++ {
		atEnd := i == len(content)
		isBreak := !atEnd && strings.HasPrefix(content[i:], "\n\n")

		if atEnd || isBreak {
			if text := content[start:i]; strings.TrimSpace(text) != "" {
				out = append(out, paragraph{offset: start, text: text})
			}
			if isBreak {
				// Skip the blank line separating paragraphs.
				for i < len(content) && content[i] == '\n' {
					i++
				}
				start = i
				i--
			}
		}
	}
	return out
}
