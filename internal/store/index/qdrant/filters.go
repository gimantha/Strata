package qdrant

import (
	client "github.com/qdrant/go-client/qdrant"

	"github.com/gimantha/strata/internal/domain"
)

// conditions translates a VectorQuery into a Qdrant filter.
//
// This function is the port claim. Everything else in the adapter is plumbing; if this
// disagrees with the reference by one clause then the two backends answer differently and
// the interface was decoration. index.RunVectorFilterConformance exists to hold it to that,
// case by case, against the same twenty-six expectations PostgreSQL meets.
//
// The whole query becomes a flat must/must-not of single-field conditions — no nesting, no
// should-clauses, no is-empty tests. That is a consequence of the sentinel encoding rather
// than luck: with NULL written as a value, every two-branch SQL clause collapses to one
// range comparison, and every condition stays individually indexable so Qdrant can plan the
// filtered traversal.
//
// The third return reports that the query can match nothing at all, which is not the same as
// having no filters.
func (s *Store) conditions(q domain.VectorQuery) (must, mustNot []*client.Condition, empty bool) {
	must = append(must, client.NewMatchKeyword("workspace_id", string(q.Scope.WorkspaceID)))
	if !domain.IsZero(q.Scope.GraphSpaceID) {
		must = append(must, client.NewMatchKeyword("graph_space_id", string(q.Scope.GraphSpaceID)))
	}
	if len(q.Surfaces) > 0 {
		must = append(must, client.NewMatchKeywords("surface", surfaceStrings(q.Surfaces)...))
	}
	// Model and version are coupled, exactly as the reference couples them: both clauses
	// live inside its `if Model != ""`. A query carrying a version and no model searches
	// every model, and filtering on the version alone would return strictly less.
	if q.Model != "" {
		must = append(must,
			client.NewMatchKeyword("embedding_model", q.Model),
			client.NewMatchInt("embedding_version", int64(q.Version)))
	}
	if len(q.Statuses) > 0 {
		must = append(must, client.NewMatchKeywords("status", q.Statuses...))
	}
	if len(q.Classification) > 0 {
		must = append(must, client.NewMatchKeywords("classification",
			enumStrings(q.Classification)...))
	}
	if len(q.MemoryKinds) > 0 {
		must = append(must, client.NewMatchKeywords("memory_kind", enumStrings(q.MemoryKinds)...))
	}
	if len(q.EntityTypes) > 0 {
		// No empty-string escape hatch here, unlike the policy filter below. A query
		// asking for organizations is asking for entities, so passages are excluded. The
		// two look alike and mean opposite things for records with no entity type.
		must = append(must, client.NewMatchKeywords("entity_type", q.EntityTypes...))
	}

	// Validity is half-open: inclusive at the lower bound, exclusive at the upper. Under
	// the sentinel encoding an absent bound is simply a value far outside the range, so
	// "NULL means unbounded" needs no branch.
	if q.ValidAt != nil {
		at := float64(q.ValidAt.UTC().UnixMicro())
		must = append(must,
			client.NewRange("valid_from", &client.Range{Lte: &at}),
			client.NewRange("valid_to", &client.Range{Gt: &at}))
	}
	// Lifecycle has three bounds, and expiry is the hard one. decay_starts_at is
	// deliberately not among them: decay reorders results, it never removes them.
	if q.ActiveAt != nil {
		at := float64(q.ActiveAt.UTC().UnixMicro())
		must = append(must,
			client.NewRange("active_from", &client.Range{Lte: &at}),
			client.NewRange("active_until", &client.Range{Gt: &at}),
			client.NewRange("expires_at", &client.Range{Gt: &at}))
	}

	policyMust, policyMustNot, policyEmpty := policyConditions(q.Policy)
	if policyEmpty {
		return nil, nil, true
	}
	return append(must, policyMust...), append(mustNot, policyMustNot...), false
}

// policyConditions translates the narrowing an access decision requires.
//
// Applied in the same filter as everything else rather than to the results, because section
// 22.4 forbids retrieving what a principal may not see and discarding it afterwards.
func policyConditions(filters domain.PolicyFilters) (must, mustNot []*client.Condition, empty bool) {
	// The gate the reference uses. When a policy narrows anything at all it also always
	// installs the permitted-classification set, defaulting the ceiling when none was
	// named — so a policy mentioning only collections still constrains classification.
	// That has a real consequence worth reproducing rather than smoothing over: a record
	// whose classification is unrecognised is invisible under any policy.
	if !filters.Restrictive() {
		return nil, nil, false
	}

	permitted := enumStrings(filters.PermittedClassifications())
	if len(permitted) == 0 {
		// An unparseable ceiling permits nothing. PostgreSQL says so with `= ANY('{}')`;
		// Qdrant's behaviour for an empty match is unspecified, so the caller short
		// circuits instead.
		return nil, nil, true
	}
	must = append(must, client.NewMatchKeywords("classification", permitted...))

	// Collections and sources share a shape, and it is not symmetric. An allow-list keeps
	// records that have no collection — material outside every collection is not what a
	// collection rule is about — while an allow-list on sources drops records with no
	// source. Both mirror the reference exactly; the asymmetry is deliberate there.
	if len(filters.AllowedCollections) > 0 {
		must = append(must, client.NewMatchKeywords("collection_id",
			append(idStrings(filters.AllowedCollections), "")...))
	}
	if len(filters.DeniedCollections) > 0 {
		mustNot = append(mustNot, client.NewMatchKeywords("collection_id",
			idStrings(filters.DeniedCollections)...))
	}
	if len(filters.AllowedSources) > 0 {
		must = append(must, client.NewMatchKeywords("source_id",
			idStrings(filters.AllowedSources)...))
	}
	if len(filters.DeniedSources) > 0 {
		// A source-less record survives: its source_id is the empty string, which the
		// denied list never contains.
		mustNot = append(mustNot, client.NewMatchKeywords("source_id",
			idStrings(filters.DeniedSources)...))
	}

	if len(filters.AllowedMemoryKinds) > 0 {
		// No escape hatch: memory_kind is always set, so an allow-list means what it says.
		must = append(must, client.NewMatchKeywords("memory_kind",
			enumStrings(filters.AllowedMemoryKinds)...))
	}
	if len(filters.DeniedMemoryKinds) > 0 {
		mustNot = append(mustNot, client.NewMatchKeywords("memory_kind",
			enumStrings(filters.DeniedMemoryKinds)...))
	}

	// Entity types and predicates both carry the escape hatch on the allow side: a chunk
	// has neither, and a rule about entity types must not silently hide every passage.
	if len(filters.AllowedEntityTypes) > 0 {
		must = append(must, client.NewMatchKeywords("entity_type",
			append(slicesClone(filters.AllowedEntityTypes), "")...))
	}
	if len(filters.DeniedEntityTypes) > 0 {
		mustNot = append(mustNot, client.NewMatchKeywords("entity_type",
			filters.DeniedEntityTypes...))
	}
	if len(filters.AllowedPredicates) > 0 {
		must = append(must, client.NewMatchKeywords("predicate",
			append(slicesClone(filters.AllowedPredicates), "")...))
	}
	if len(filters.DeniedPredicates) > 0 {
		mustNot = append(mustNot, client.NewMatchKeywords("predicate",
			filters.DeniedPredicates...))
	}

	return must, mustNot, false
}

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

func slicesClone(values []string) []string {
	out := make([]string, len(values))
	copy(out, values)
	return out
}
