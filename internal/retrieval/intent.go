package retrieval

import (
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gimantha/strata/internal/domain"
)

// Intent is the deterministic part of understanding a query. It deliberately handles
// only syntax that can be recognized without guessing; a planning model may add semantic
// rewrites, but identifiers and explicit timestamps never need a model.
type Intent struct {
	FocusTerms   []string
	EntityTerms  []string
	Temporal     domain.TemporalQuery
	Requirements []string
}

var (
	rfc3339InText = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})\b`)
	dateInText    = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	quotedInText  = regexp.MustCompile(`["“]([^"”]{2,100})["”]`)
)

// Interpret recovers exact anchors, explicit time coordinates, and broad answer
// requirements. It is provider-independent and safe to run on every query.
func Interpret(query string) Intent {
	intent := Intent{FocusTerms: ExtractIdentifiers(query), EntityTerms: ExtractEntityTerms(query)}
	for _, match := range quotedInText.FindAllStringSubmatch(query, 4) {
		if len(match) == 2 {
			intent.FocusTerms = appendUniqueFold(intent.FocusTerms, strings.TrimSpace(match[1]))
			intent.EntityTerms = appendUniqueFold(intent.EntityTerms, strings.TrimSpace(match[1]))
		}
	}

	lower := strings.ToLower(query)
	instants := explicitTimes(query)
	previousEnd := 0
	for _, found := range instants {
		start := found.start - 48
		if start < previousEnd {
			start = previousEnd
		}
		cue := lower[start:found.start]
		instant := found.value
		switch {
		case containsAny(cue, "known at", "known on", "known by", "believed at", "believe on",
			"recorded by", "learned by", "received by", "information received"):
			if found.dateOnly && containsAny(cue, " by", "received", "known", "believ") {
				instant = instant.Add(24*time.Hour - time.Nanosecond)
			}
			intent.Temporal.KnownAt = ptrTime(instant)
		case containsAny(cue, "active at", "active on", "context at", "memory at"):
			intent.Temporal.ActiveAt = ptrTime(instant)
		default:
			intent.Temporal.ValidAt = ptrTime(instant)
		}
		previousEnd = found.end
	}
	// A single unlabelled time in an explicitly epistemic question is a knowledge cutoff.
	if len(instants) == 1 && intent.Temporal.ValidAt != nil && containsAny(lower,
		"what did we know", "what was known", "using only information", "information received") {
		intent.Temporal.KnownAt, intent.Temporal.ValidAt = intent.Temporal.ValidAt, nil
	}

	if containsAny(lower, "why", "reason", "rationale", "because", "decision") {
		intent.Requirements = append(intent.Requirements, "decision_trace")
	}
	if containsAny(lower, "source", "provenance", "event id", "evidence", "citation") {
		intent.Requirements = append(intent.Requirements, "provenance")
	}
	if containsAny(lower, "difference", "delta", "change", "compare", "increase", "decrease") {
		intent.Requirements = append(intent.Requirements, "comparison_inputs")
	}
	if containsAny(lower, "unknown", "none", "no successor", "expired", "expiry") {
		intent.Requirements = append(intent.Requirements, "absence_proof")
	}
	if containsAny(lower, "was ", "historical", "previous", "before", "at the time") {
		intent.Requirements = append(intent.Requirements, "history")
	}
	intent.Requirements = uniqueSorted(intent.Requirements)
	return intent
}

// ExtractEntityTerms finds proper-name phrases inside a question so canonical entity
// lookup receives "Alice Chen" rather than "why did Alice Chen change roles". It is a
// retrieval hint only; entity resolution still decides whether the name exists.
func ExtractEntityTerms(query string) []string {
	clean := strings.NewReplacer("?", " ", "!", " ", ",", " ", ".", " ", "(", " ", ")", " ").Replace(query)
	words := strings.Fields(clean)
	var out []string
	for i := 0; i < len(words); {
		word := strings.Trim(words[i], "\"'“”")
		if !properNameWord(word) {
			i++
			continue
		}
		start := i
		phrase := []string{word}
		i++
		for i < len(words) && len(phrase) < 4 {
			next := strings.Trim(words[i], "\"'“”")
			if !properNameWord(next) {
				break
			}
			phrase = append(phrase, next)
			i++
		}
		candidate := strings.Join(phrase, " ")
		if start == 0 && questionWord(strings.ToLower(candidate)) {
			continue
		}
		out = appendUniqueFold(out, candidate)
	}
	return out
}

func properNameWord(word string) bool {
	if word == "" || identifierToken(word) || isTimeToken(word) || functionWord(word) {
		return false
	}
	runes := []rune(word)
	if !unicode.IsUpper(runes[0]) {
		return false
	}
	upper := 0
	for _, r := range runes {
		if unicode.IsUpper(r) {
			upper++
		}
	}
	return upper < len(runes) || len(runes) > 4
}

func questionWord(value string) bool {
	switch value {
	case "who", "what", "when", "where", "why", "how", "which", "is", "did", "does", "show", "find", "list":
		return true
	default:
		return false
	}
}

// ExtractIdentifiers returns identifier-like tokens wherever they occur in a sentence.
// Dates are excluded because they are temporal coordinates, not entity anchors.
func ExtractIdentifiers(query string) []string {
	var out []string
	for _, raw := range strings.Fields(query) {
		token := strings.Trim(raw, "\t\r\n ,;!?()[]{}\"'“”")
		if token == "" || isTimeToken(token) || !identifierToken(token) {
			continue
		}
		out = appendUniqueFold(out, token)
	}
	return out
}

// MergeTemporal applies inferred coordinates only where the caller did not provide one.
func MergeTemporal(explicit, inferred domain.TemporalQuery) domain.TemporalQuery {
	if explicit.ValidAt == nil {
		explicit.ValidAt = inferred.ValidAt
	}
	if explicit.KnownAt == nil {
		explicit.KnownAt = inferred.KnownAt
	}
	if explicit.ActiveAt == nil {
		explicit.ActiveAt = inferred.ActiveAt
	}
	return explicit
}

type explicitTime struct {
	value      time.Time
	start, end int
	dateOnly   bool
}

func explicitTimes(query string) []explicitTime {
	var out []explicitTime
	occupied := map[int]bool{}
	for _, bounds := range rfc3339InText.FindAllStringIndex(query, -1) {
		value := query[bounds[0]:bounds[1]]
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			out = append(out, explicitTime{value: parsed.UTC(), start: bounds[0], end: bounds[1]})
			for i := bounds[0]; i < bounds[1]; i++ {
				occupied[i] = true
			}
		}
	}
	for _, bounds := range dateInText.FindAllStringIndex(query, -1) {
		if occupied[bounds[0]] {
			continue
		}
		value := query[bounds[0]:bounds[1]]
		if parsed, err := time.Parse(domain.DateLayout, value); err == nil {
			out = append(out, explicitTime{value: parsed.UTC(), start: bounds[0], end: bounds[1], dateOnly: true})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

func identifierToken(token string) bool {
	var letters, digits int
	var separator, upper bool
	for _, r := range token {
		switch {
		case unicode.IsDigit(r):
			digits++
		case unicode.IsLetter(r):
			letters++
			upper = upper || unicode.IsUpper(r)
		case r == '-' || r == '_' || r == '.' || r == '/' || r == ':':
			separator = true
		default:
			return false
		}
	}
	return (digits > 0 && (letters > 0 || separator)) ||
		(digits >= 4 && letters == 0) || (upper && separator)
}

func isTimeToken(token string) bool {
	return rfc3339InText.MatchString(token) || dateInText.MatchString(token) || calendarWord(token)
}

// calendarWord reports whether a token is a month or weekday name. These are capitalized in
// English and so look like proper names, but "January" is a coordinate, not an identity, and
// a sub-query for it would retrieve every record that mentions the month.
func calendarWord(token string) bool {
	_, ok := monthNumbers[strings.ToLower(strings.TrimSuffix(token, "."))]
	if ok {
		return true
	}
	switch strings.ToLower(strings.TrimSuffix(token, ".")) {
	case "monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday",
		"mon", "tue", "tues", "wed", "thu", "thur", "thurs", "fri", "sat", "sun":
		return true
	}
	return false
}

// monthNumbers maps English month names and their common abbreviations to month numbers.
var monthNumbers = map[string]time.Month{
	"january": time.January, "jan": time.January,
	"february": time.February, "feb": time.February,
	"march": time.March, "mar": time.March,
	"april": time.April, "apr": time.April,
	"may":  time.May,
	"june": time.June, "jun": time.June,
	"july": time.July, "jul": time.July,
	"august": time.August, "aug": time.August,
	"september": time.September, "sep": time.September, "sept": time.September,
	"october": time.October, "oct": time.October,
	"november": time.November, "nov": time.November,
	"december": time.December, "dec": time.December,
}

// functionWord reports whether a capitalized word is grammar or instruction rather than a
// name. A question that opens "For ..." or ends "... Return the reason code" capitalizes
// words that name nothing, and a sub-query built from one retrieves whatever happens to
// contain a common word. The list is deliberately confined to articles, prepositions,
// conjunctions, auxiliaries, pronouns, and the verbs questions are phrased with; adjectives
// that begin real names ("New", "North", "Great") are left alone.
func functionWord(word string) bool {
	switch strings.ToLower(word) {
	case "a", "an", "the", "and", "or", "but", "nor", "so", "yet", "if", "then", "than", "that",
		"this", "these", "those", "as", "at", "by", "for", "from", "in", "into", "of", "off", "on",
		"onto", "to", "up", "with", "without", "within", "about", "above", "across", "after",
		"against", "along", "among", "around", "before", "behind", "below", "beneath", "beside",
		"between", "beyond", "during", "except", "inside", "near", "outside", "over", "past",
		"per", "since", "through", "throughout", "toward", "towards", "under", "until", "upon",
		"via", "versus", "vs",
		"i", "me", "my", "we", "us", "our", "you", "your", "he", "him", "his", "she", "her", "it",
		"its", "they", "them", "their", "who", "whom", "whose", "which", "what", "when", "where",
		"why", "how", "whether",
		"am", "is", "are", "was", "were", "be", "been", "being", "do", "does", "did", "done",
		"has", "have", "had", "having", "can", "could", "may", "might", "must", "shall", "should",
		"will", "would",
		"not", "no", "yes", "all", "any", "both", "each", "either", "every", "neither", "none",
		"only", "also", "even", "just", "still", "already", "again", "please",
		"return", "give", "tell", "show", "list", "find", "explain", "describe", "state", "cite",
		"answer", "report", "provide", "identify", "determine", "summarize", "summarise",
		"compare", "include", "including", "use", "using", "used", "based", "according",
		"given", "consider", "considering", "assume", "assuming", "note":
		return true
	}
	return false
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func appendUniqueFold(values []string, value string) []string {
	for _, existing := range values {
		if strings.EqualFold(existing, value) {
			return values
		}
	}
	return append(values, value)
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := values[:0]
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func ptrTime(value time.Time) *time.Time { return &value }
