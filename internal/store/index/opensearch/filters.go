package opensearch

import (
	"strings"

	"github.com/gimantha/strata/internal/domain"
)

// matchClause renders the text half of a query.
//
// Two modes, one method, because they are one index: stemmed full text for prose, and
// literal substring for the identifiers stemming destroys. The second return reports that
// the query can match nothing, which is not the same as having no text.
func matchClause(q domain.LexicalQuery) (clause []any, empty bool) {
	if q.Exact {
		return []any{map[string]any{
			"wildcard": map[string]any{
				"content.exact": map[string]any{
					"value": wildcardPattern(q.Text),
					// The reference's ILIKE is case-insensitive; without this the two
					// backends would disagree on every capitalised identifier.
					"case_insensitive": true,
				},
			},
		}}, false
	}

	return []any{map[string]any{
		"match": map[string]any{
			"content": map[string]any{
				"query": q.Text,
				// Every term must appear, matching websearch_to_tsquery's default of
				// conjunction. The OpenSearch default is OR, which would return documents
				// sharing one common word with the query and make the two engines disagree
				// about almost every multi-word search.
				"operator": "and",
			},
		},
	}}, false
}

// wildcardPattern renders a substring search that means what it says.
//
// The same problem the reference had with LIKE, in a different alphabet: * and ? are
// wildcards here and \ escapes them, and all three occur in the identifiers exact mode
// exists to find. Escaping them makes the search literal. Note that % and _ are ordinary
// characters to OpenSearch and special to PostgreSQL — which is why the conformance suite
// checks both alphabets rather than one engine's.
func wildcardPattern(text string) string {
	escaped := strings.NewReplacer(
		"\\", "\\\\",
		"*", "\\*",
		"?", "\\?",
	).Replace(text)
	return "*" + escaped + "*"
}

// filters renders every narrowing the query and its policy require.
//
// This function is the port claim for this backend. Everything else is plumbing; if one
// clause disagrees with the reference then the two answer differently and the interface was
// decoration. index.RunLexicalFilterConformance holds it to the same twenty-five
// expectations PostgreSQL meets.
//
// The second return reports that the query can match nothing at all.
func (s *Store) filters(q domain.LexicalQuery) (clauses []any, impossible bool) {
	add := func(clause any) { clauses = append(clauses, clause) }
	term := func(field, value string) any {
		return map[string]any{"term": map[string]any{field: value}}
	}
	terms := func(field string, values []string) any {
		return map[string]any{"terms": map[string]any{field: values}}
	}

	add(term("workspace_id", string(q.Scope.WorkspaceID)))
	if !domain.IsZero(q.Scope.GraphSpaceID) {
		add(term("graph_space_id", string(q.Scope.GraphSpaceID)))
	}
	// Scope.CollectionID is deliberately not a filter, matching the reference: a query's
	// collection narrows nothing on this leg, and only a policy rule about collections does.
	if len(q.Surfaces) > 0 {
		add(terms("surface", surfaceStrings(q.Surfaces)))
	}
	if len(q.Statuses) > 0 {
		add(terms("status", q.Statuses))
	}
	if len(q.Classification) > 0 {
		add(terms("classification", enumStrings(q.Classification)))
	}
	if len(q.MemoryKinds) > 0 {
		add(terms("memory_kind", enumStrings(q.MemoryKinds)))
	}
	if len(q.EntityTypes) > 0 {
		// No empty-string escape hatch here, unlike the policy filter below. A query for
		// organizations is asking for entities, so passages are excluded.
		add(terms("entity_type", q.EntityTypes))
	}

	// Validity is half-open, and an absent bound is unbounded. Expressed with exists
	// rather than a sentinel, which is the shape the reference's SQL has: a missing field
	// is the unbounded case in both.
	if q.ValidAt != nil {
		at := q.ValidAt.UTC().Format(stampLayout)
		add(unboundedOr("valid_from", map[string]any{"lte": at}))
		add(unboundedOr("valid_to", map[string]any{"gt": at}))
	}
	// Three bounds for records, where an edge has two.
	if q.ActiveAt != nil {
		at := q.ActiveAt.UTC().Format(stampLayout)
		add(unboundedOr("active_from", map[string]any{"lte": at}))
		add(unboundedOr("active_until", map[string]any{"gt": at}))
		add(unboundedOr("expires_at", map[string]any{"gt": at}))
	}

	policy, impossible := policyFilters(q.Policy)
	if impossible {
		return nil, true
	}
	return append(clauses, policy...), false
}

// unboundedOr matches a document whose field satisfies the range, or does not have it.
//
// The direct translation of "(field IS NULL OR field <op> $1)". A should with two branches
// and one required, which in a filter context contributes no score.
func unboundedOr(field string, comparison map[string]any) any {
	return map[string]any{
		"bool": map[string]any{
			"should": []any{
				map[string]any{"bool": map[string]any{
					"must_not": []any{map[string]any{"exists": map[string]any{"field": field}}},
				}},
				map[string]any{"range": map[string]any{field: comparison}},
			},
			"minimum_should_match": 1,
		},
	}
}

// policyFilters renders the narrowing an access decision requires.
func policyFilters(filters domain.PolicyFilters) (clauses []any, impossible bool) {
	// The gate the reference uses: a policy that narrows anything at all also always
	// installs the permitted-classification set, defaulting the ceiling when none was
	// named. So a rule mentioning only collections still constrains classification, and a
	// record whose classification is unrecognised is invisible under any policy.
	if !filters.Restrictive() {
		return nil, false
	}

	permitted := enumStrings(filters.PermittedClassifications())
	if len(permitted) == 0 {
		// An unparseable ceiling permits nothing, which PostgreSQL says with an empty
		// ANY. An empty terms clause here would match nothing too, but saying so
		// explicitly is clearer than relying on that.
		return nil, true
	}

	add := func(clause any) { clauses = append(clauses, clause) }
	terms := func(field string, values []string) any {
		return map[string]any{"terms": map[string]any{field: values}}
	}
	notTerms := func(field string, values []string) any {
		return map[string]any{"bool": map[string]any{
			"must_not": []any{terms(field, values)},
		}}
	}

	add(terms("classification", permitted))

	// Collections and sources look alike and are not symmetric. An allow-list on
	// collections keeps records in no collection — material outside every collection is
	// not what a collection rule is about — while an allow-list on sources drops records
	// with no source. Both mirror the reference, where the asymmetry is deliberate.
	if len(filters.AllowedCollections) > 0 {
		add(terms("collection_id", append(idStrings(filters.AllowedCollections), "")))
	}
	if len(filters.DeniedCollections) > 0 {
		add(notTerms("collection_id", idStrings(filters.DeniedCollections)))
	}
	if len(filters.AllowedSources) > 0 {
		add(terms("source_id", idStrings(filters.AllowedSources)))
	}
	if len(filters.DeniedSources) > 0 {
		add(notTerms("source_id", idStrings(filters.DeniedSources)))
	}

	if len(filters.AllowedMemoryKinds) > 0 {
		// No escape hatch: memory_kind is always set, so an allow-list means what it says.
		add(terms("memory_kind", enumStrings(filters.AllowedMemoryKinds)))
	}
	if len(filters.DeniedMemoryKinds) > 0 {
		add(notTerms("memory_kind", enumStrings(filters.DeniedMemoryKinds)))
	}

	// Entity types and predicates carry the escape hatch on the allow side, because a
	// passage has neither and a rule about entity types must not hide every passage.
	if len(filters.AllowedEntityTypes) > 0 {
		add(terms("entity_type", append(clone(filters.AllowedEntityTypes), "")))
	}
	if len(filters.DeniedEntityTypes) > 0 {
		add(notTerms("entity_type", filters.DeniedEntityTypes))
	}
	if len(filters.AllowedPredicates) > 0 {
		add(terms("predicate", append(clone(filters.AllowedPredicates), "")))
	}
	if len(filters.DeniedPredicates) > 0 {
		add(notTerms("predicate", filters.DeniedPredicates))
	}

	return clauses, false
}

// stampLayout matches the mapping's date format.
const stampLayout = "2006-01-02T15:04:05.999999999Z07:00"

func surfaceStrings(surfaces []domain.Surface) []string {
	out := make([]string, 0, len(surfaces))
	for _, surface := range surfaces {
		out = append(out, string(surface))
	}
	return out
}

func enumStrings[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

func idStrings[T ~string](values []T) []string { return enumStrings(values) }

func clone(values []string) []string {
	out := make([]string, len(values))
	copy(out, values)
	return out
}
