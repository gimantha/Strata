package retrieval

import (
	"strings"
	"unicode"

	"github.com/gimantha/strata/internal/domain"
)

// planner decides which candidate generators to run for a query.
//
// The decision is made from the query's shape rather than by asking a model, because
// retrieval must work without an LLM at query time (AGENTS.md section 19.4) and because a
// planner that costs a model call per query is a planner nobody can afford to run.
type planner struct {
	hasEmbedder bool
}

// plan returns the modes to run and why, so a disappointing result can be explained.
func (p planner) plan(req domain.QueryRequest) domain.RetrievalPlan {
	out := domain.RetrievalPlan{
		Reasons:    map[domain.RetrievalMode]string{},
		Skipped:    map[domain.RetrievalMode]string{},
		Candidates: map[domain.RetrievalMode]int{},
	}

	// An explicit mode list is honoured as given: a caller narrowing retrieval usually
	// knows something the planner does not, and silently adding modes would make measuring
	// one retriever impossible.
	if len(req.Modes) > 0 {
		for _, mode := range req.Modes {
			if mode == domain.ModeVector && !p.hasEmbedder {
				out.Skipped[mode] = "no embedding provider is configured"
				continue
			}
			out.Modes = append(out.Modes, mode)
			out.Reasons[mode] = "requested by the caller"
		}
		return out
	}

	// Lexical always runs. It is cheap, it never returns nonsense, and it is the only
	// retriever that reliably handles the words the user actually typed.
	out.Modes = append(out.Modes, domain.ModeLexical)
	out.Reasons[domain.ModeLexical] = "always runs: matches the words as written"

	if looksLikeIdentifier(req.Query) {
		out.Modes = append(out.Modes, domain.ModeExact)
		out.Reasons[domain.ModeExact] =
			"the query looks like an identifier, which stemming and embeddings both mangle"
	}

	if p.hasEmbedder {
		out.Modes = append(out.Modes, domain.ModeVector)
		out.Reasons[domain.ModeVector] = "finds wording the query did not use"
	} else {
		out.Skipped[domain.ModeVector] = "no embedding provider is configured"
	}

	// Entity lookup is worth running on anything short enough to be a name.
	if looksLikeName(req.Query) {
		out.Modes = append(out.Modes, domain.ModeEntity)
		out.Reasons[domain.ModeEntity] = "short query, so it may name an entity directly"
	}

	// Graph expansion runs last and only if something else found an entity to start from.
	// Expanding from nothing returns nothing, and expanding from everything returns the
	// whole graph (AGENTS.md section 19.5).
	out.Modes = append(out.Modes, domain.ModeGraph)
	out.Reasons[domain.ModeGraph] = "expands from entities the other retrievers found"

	return out
}

// looksLikeIdentifier reports whether a query resembles a code, key, or part number rather
// than prose.
//
// These are exactly the queries that stemming destroys and embeddings have no useful
// neighbourhood for, so recognizing them is what makes exact matching fire when it matters.
func looksLikeIdentifier(query string) bool {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" || len(trimmed) > 64 {
		return false
	}

	fields := strings.Fields(trimmed)
	if len(fields) > 3 {
		return false
	}

	for _, field := range fields {
		var hasDigit, hasLetter, hasSeparator, hasUpper bool
		digits := 0
		for _, r := range field {
			if unicode.IsDigit(r) {
				digits++
			}
			switch {
			case unicode.IsDigit(r):
				hasDigit = true
			case unicode.IsUpper(r):
				hasLetter, hasUpper = true, true
			case unicode.IsLetter(r):
				hasLetter = true
			case r == '-' || r == '_' || r == '.' || r == '/' || r == ':':
				hasSeparator = true
			}
		}
		// A token mixing letters with digits, or carrying an internal separator alongside
		// capitals, is far more likely a code than a word.
		if hasDigit && (hasLetter || hasSeparator) {
			return true
		}
		if hasUpper && hasSeparator {
			return true
		}
		// A bare run of digits is how invoice, ticket, and part numbers are usually typed.
		// Four is the threshold because shorter numbers are ordinary prose - "30 days",
		// "top 10" - while longer ones are almost always something being looked up. A
		// false positive here costs one extra query; a false negative means an identifier
		// gets stemmed and lost.
		if digits >= 4 && !hasLetter {
			return true
		}
	}
	return false
}

// looksLikeName reports whether a query is short enough to plausibly be an entity name.
func looksLikeName(query string) bool {
	fields := strings.Fields(strings.TrimSpace(query))
	return len(fields) > 0 && len(fields) <= 6
}
