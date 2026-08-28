package neo4j

import (
	"github.com/gimantha/strata/internal/domain"
)

// edgeConditions renders every filter that must hold at each hop.
//
// This function is the port claim for this backend, and the graph port's filters differ from
// the other two in a way that matters: they apply to the walk rather than to its results. An
// edge that fails one of these is not merely hidden — everything reachable only through it
// becomes unreachable, and something reachable another way may be reported deeper than it
// would otherwise have been. So every condition below sits inside the `all(edge IN rels ...)`
// predicate, which is Cypher's way of saying what the reference says by putting the clause
// inside the recursive term.
//
// The third return reports that the query can match nothing at all.
func edgeConditions(q domain.GraphExpandQuery) (conditions []string, params map[string]any,
	impossible bool) {
	params = map[string]any{}
	add := func(condition string, name string, value any) {
		conditions = append(conditions, condition)
		if name != "" {
			params[name] = value
		}
	}

	// Scoped by the edge's own workspace, not by its endpoints'. That is what makes a root
	// from another tenant safe: its edges are elsewhere, so it reaches nothing.
	add("edge.workspace = $workspace", "", nil)
	if !domain.IsZero(q.Scope.GraphSpaceID) {
		add("edge.graphSpace = $graphSpace", "graphSpace", string(q.Scope.GraphSpaceID))
	}
	// Scope.CollectionID is deliberately not a filter, matching the reference: only a
	// policy rule about collections narrows this leg.

	// The one filter with no off switch. Active and disputed by default — a contested
	// relationship is still believed — and IncludeSuperseded adds exactly one more status,
	// never proposed, retracted or quarantined.
	statuses := []string{string(domain.AssertionActive), string(domain.AssertionDisputed)}
	if q.IncludeSuperseded {
		statuses = append(statuses, string(domain.AssertionSuperseded))
	}
	add("edge.status IN $statuses", "statuses", statuses)

	if len(q.Predicates) > 0 {
		// Normalized by the port, as the reference normalizes them: a caller writing
		// "worksAt" and one writing "WORKS_AT" mean the same edge.
		normalized := make([]string, 0, len(q.Predicates))
		for _, predicate := range q.Predicates {
			normalized = append(normalized, domain.NormalizePredicateName(predicate))
		}
		add("edge.predicate IN $predicates", "predicates", normalized)
	}

	// Half-open, and an absent bound is unbounded. Cypher compares against null the way
	// SQL does, so these are the reference's clauses almost verbatim.
	if q.ValidAt != nil {
		at := q.ValidAt.UTC().UnixMicro()
		add("(edge.validFrom IS NULL OR edge.validFrom <= $validAt)", "validAt", at)
		add("(edge.validTo IS NULL OR edge.validTo > $validAt)", "", nil)
	}
	// Two bounds, not three. An edge has no activeFrom — there is no such thing as a
	// not-yet-active relationship — so copying the record-level helper would exclude edges
	// that should be walked.
	if q.ActiveAt != nil {
		at := q.ActiveAt.UTC().UnixMicro()
		add("(edge.activeUntil IS NULL OR edge.activeUntil > $activeAt)", "activeAt", at)
		add("(edge.expiresAt IS NULL OR edge.expiresAt > $activeAt)", "", nil)
	}

	policy, policyParams, impossible := policyConditions(q.Policy)
	if impossible {
		return nil, nil, true
	}
	for name, value := range policyParams {
		params[name] = value
	}
	return append(conditions, policy...), params, false
}

// policyConditions renders the narrowing an access decision requires.
func policyConditions(filters domain.PolicyFilters) (conditions []string,
	params map[string]any, impossible bool) {
	params = map[string]any{}
	if !filters.Restrictive() {
		return nil, params, false
	}

	permitted := enumStrings(filters.PermittedClassifications())
	if len(permitted) == 0 {
		return nil, nil, true
	}
	// Installed by any restrictive policy, even one naming only collections. An edge whose
	// classification is unrecognised is therefore invisible under every policy and visible
	// under none, which is the reference's behaviour rather than an accident of SQL.
	conditions = append(conditions, "edge.classification IN $permittedClassifications")
	params["permittedClassifications"] = permitted

	// Sources and collections look alike and are not symmetric. An empty string is how an
	// absent reference is stored, so an allow-list on sources excludes it and one on
	// collections admits it — matching the reference, where the asymmetry is deliberate.
	if len(filters.AllowedSources) > 0 {
		conditions = append(conditions, "edge.source IN $allowedSources")
		params["allowedSources"] = idStrings(filters.AllowedSources)
	}
	if len(filters.DeniedSources) > 0 {
		conditions = append(conditions, "NOT edge.source IN $deniedSources")
		params["deniedSources"] = idStrings(filters.DeniedSources)
	}
	if len(filters.AllowedCollections) > 0 {
		conditions = append(conditions,
			"(edge.collection = '' OR edge.collection IN $allowedCollections)")
		params["allowedCollections"] = idStrings(filters.AllowedCollections)
	}
	if len(filters.DeniedCollections) > 0 {
		conditions = append(conditions,
			"(edge.collection = '' OR NOT edge.collection IN $deniedCollections)")
		params["deniedCollections"] = idStrings(filters.DeniedCollections)
	}

	if len(filters.AllowedMemoryKinds) > 0 {
		conditions = append(conditions, "edge.memoryKind IN $allowedMemoryKinds")
		params["allowedMemoryKinds"] = enumStrings(filters.AllowedMemoryKinds)
	}
	if len(filters.DeniedMemoryKinds) > 0 {
		conditions = append(conditions, "NOT edge.memoryKind IN $deniedMemoryKinds")
		params["deniedMemoryKinds"] = enumStrings(filters.DeniedMemoryKinds)
	}

	// No empty-string escape hatch on predicates here, unlike the record-level filter: a
	// passage without a predicate is normal and an edge without one is meaningless.
	if len(filters.AllowedPredicates) > 0 {
		conditions = append(conditions, "edge.predicate IN $allowedPredicates")
		params["allowedPredicates"] = filters.AllowedPredicates
	}
	if len(filters.DeniedPredicates) > 0 {
		conditions = append(conditions, "NOT edge.predicate IN $deniedPredicates")
		params["deniedPredicates"] = filters.DeniedPredicates
	}

	// Entity types are not applied here and cannot be: an edge carries no entity type,
	// because a type belongs to the entity. The reference has the same gap and closes it
	// where the disclosure happens — the retriever checks the type when it resolves a hit
	// into a name (ADR 0021).
	return conditions, params, false
}

func enumStrings[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

func idStrings[T ~string](values []T) []string { return enumStrings(values) }
