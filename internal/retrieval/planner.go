package retrieval

import (
	"context"
	"strings"

	"github.com/gimantha/strata/internal/domain"
)

// Planner decides which candidate generators to run, and what to search for.
//
// An interface with two implementations, because the two answer different questions. The
// heuristic planner reads the query's shape; an LLM planner reads its meaning. The first is
// the default and, whatever else is configured, the fallback: AGENTS.md section 19.4
// requires retrieval to work without a model at query time.
type Planner interface {
	Plan(ctx context.Context, req domain.QueryRequest) domain.RetrievalPlan
}

// heuristicPlanner decides from the query's shape rather than by asking a model.
//
// Cheap, predictable, and always available. It cannot tell that "which support episodes" is
// a requirement rather than a topic — that is what an LLM planner is for — but it never
// costs a model call, never fails, and never sends the query anywhere.
type heuristicPlanner struct {
	hasEmbedder bool
}

// Plan returns the modes to run and why, so a disappointing result can be explained.
func (p heuristicPlanner) Plan(_ context.Context, req domain.QueryRequest) domain.RetrievalPlan {
	intent := Interpret(req.Query)
	out := domain.RetrievalPlan{
		Planner:    "heuristic",
		Reasons:    map[domain.RetrievalMode]string{},
		Skipped:    map[domain.RetrievalMode]string{},
		Candidates: map[domain.RetrievalMode]int{},
		// The question as asked. A heuristic planner never reshapes it, and recording
		// that explicitly means every plan says what was searched for rather than leaving
		// the single-query case implied.
		SubQueries: []domain.SubQuery{{
			Text: req.Query, Kind: domain.SubQueryOriginal,
			Reason: "the question as asked",
		}},
		FocusTerms:   append([]string(nil), intent.FocusTerms...),
		EntityTerms:  append([]string(nil), intent.EntityTerms...),
		Temporal:     MergeTemporal(req.Temporal, intent.Temporal),
		Requirements: append([]string(nil), intent.Requirements...),
	}
	for _, name := range intent.EntityTerms {
		if len(out.SubQueries) >= domain.MaxSubQueries {
			break
		}
		if containsSubQuery(out.SubQueries, name) {
			continue
		}
		out.SubQueries = append(out.SubQueries, domain.SubQuery{Text: name, Kind: domain.SubQueryDecomposed,
			Reason: "proper-name phrase recovered from the question"})
	}
	for _, focus := range intent.FocusTerms {
		if len(out.SubQueries) >= domain.MaxSubQueries {
			break
		}
		if strings.EqualFold(strings.TrimSpace(req.Query), focus) {
			continue
		}
		out.SubQueries = append(out.SubQueries, domain.SubQuery{
			Text: focus, Kind: domain.SubQueryDecomposed,
			Reason: "exact entity or record identifier recovered from the question",
		})
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

	if len(intent.FocusTerms) > 0 || looksLikeIdentifier(req.Query) {
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
	if len(intent.FocusTerms) > 0 || len(intent.EntityTerms) > 0 || looksLikeName(req.Query) {
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

func containsSubQuery(values []domain.SubQuery, text string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value.Text), strings.TrimSpace(text)) {
			return true
		}
	}
	return false
}

// looksLikeIdentifier reports whether a query resembles a code, key, or part number rather
// than prose.
//
// These are exactly the queries that stemming destroys and embeddings have no useful
// neighbourhood for, so recognizing them is what makes exact matching fire when it matters.
func looksLikeIdentifier(query string) bool {
	return len(ExtractIdentifiers(query)) > 0
}

// looksLikeName reports whether a query is short enough to plausibly be an entity name.
func looksLikeName(query string) bool {
	fields := strings.Fields(strings.TrimSpace(query))
	return len(fields) > 0 && len(fields) <= 6
}
