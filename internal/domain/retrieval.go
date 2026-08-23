package domain

import "time"

// RetrievalMode names a candidate generator (AGENTS.md section 19.2).
type RetrievalMode string

const (
	// ModeLexical is stemmed full-text search over projected content.
	ModeLexical RetrievalMode = "lexical"
	// ModeExact is substring matching, for identifiers and codes that stemming mangles.
	ModeExact RetrievalMode = "exact"
	// ModeVector is nearest-neighbour search over embeddings.
	ModeVector RetrievalMode = "vector"
	// ModeEntity resolves names directly to identities.
	ModeEntity RetrievalMode = "entity"
	// ModeGraph expands outwards from entities the other modes found, which is what lets a
	// query reach a fact no single passage states (AGENTS.md section 19.5).
	ModeGraph RetrievalMode = "graph"
)

var retrievalModes = []RetrievalMode{ModeLexical, ModeExact, ModeVector, ModeEntity, ModeGraph}

func ParseRetrievalMode(s string) (RetrievalMode, error) {
	return parseEnum("retrieval mode", s, retrievalModes)
}

// RetrievalModes returns every mode.
func RetrievalModes() []RetrievalMode { return append([]RetrievalMode(nil), retrievalModes...) }

// TemporalQuery carries the time filters from AGENTS.md section 25.3.
type TemporalQuery struct {
	ValidAt  *time.Time
	KnownAt  *time.Time
	ActiveAt *time.Time
}

// Any reports whether any temporal filter is set.
func (t TemporalQuery) Any() bool {
	return t.ValidAt != nil || t.KnownAt != nil || t.ActiveAt != nil
}

// QueryFilters narrow candidates before ranking.
type QueryFilters struct {
	Surfaces        []Surface
	Classifications []Classification
	MemoryKinds     []MemoryKind
	Predicates      []string
	Statuses        []AssertionStatus
	MinConfidence   float64
}

// QueryRequest is one retrieval (AGENTS.md section 19.1).
type QueryRequest struct {
	Scope     Scope
	Query     string
	Principal PrincipalRef

	Temporal TemporalQuery
	Filters  QueryFilters
	// Modes restricts which candidate generators run. Empty lets the planner decide.
	Modes []RetrievalMode

	Limit      int
	GraphDepth int
	// Explain returns the plan and per-signal scores, so a result can be argued with
	// rather than merely accepted.
	Explain bool
}

// DefaultQueryLimit bounds an unspecified query.
const DefaultQueryLimit = 20

// MaxQueryLimit caps what a caller may request.
const MaxQueryLimit = 200

// Normalize applies defaults and bounds.
func (q QueryRequest) Normalize() QueryRequest {
	if q.Limit <= 0 {
		q.Limit = DefaultQueryLimit
	}
	if q.Limit > MaxQueryLimit {
		q.Limit = MaxQueryLimit
	}
	if q.GraphDepth <= 0 {
		q.GraphDepth = DefaultGraphDepth
	}
	if q.GraphDepth > MaxGraphDepth {
		q.GraphDepth = MaxGraphDepth
	}
	for i, predicate := range q.Filters.Predicates {
		q.Filters.Predicates[i] = NormalizePredicateName(predicate)
	}
	return q
}

func (q QueryRequest) Validate() error {
	const op = "domain.QueryRequest.Validate"

	if IsZero(q.Scope.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace scope is required")
	}
	if q.Query == "" {
		return Errorf(CodeInvalidArgument, op, "a query is required")
	}
	if len(q.Query) > MaxQueryLength {
		return Errorf(CodeInvalidArgument, op,
			"query is longer than %d characters", MaxQueryLength)
	}
	if q.Filters.MinConfidence < 0 || q.Filters.MinConfidence > 1 {
		return Errorf(CodeInvalidArgument, op, "min_confidence must be between 0 and 1")
	}
	for _, mode := range q.Modes {
		if _, err := ParseRetrievalMode(string(mode)); err != nil {
			return err
		}
	}
	return nil
}

// MaxQueryLength bounds a query string.
const MaxQueryLength = 4000

// RetrievedItem is one fused result.
type RetrievedItem struct {
	Surface  Surface
	RecordID string
	Content  string

	// Score is the fused rank score.
	Score float64
	// Signals records what each contributing factor scored, so ranking is inspectable
	// rather than a single opaque number (AGENTS.md section 19.3).
	Signals map[string]float64
	// FoundBy names the retrievers that surfaced this item, and Ranks their positions.
	FoundBy []RetrievalMode
	Ranks   map[RetrievalMode]int

	// Path explains how graph expansion reached this item, when it did.
	Path *GraphPath
}

// GraphPath explains a graph-derived result.
type GraphPath struct {
	FromEntityID EntityID
	ViaPredicate string
	ViaAssertion AssertionID
	Depth        int
}

// RetrievalPlan records what the planner decided and why, which is the first thing anyone
// debugging a disappointing result needs.
type RetrievalPlan struct {
	Modes      []RetrievalMode
	Reasons    map[RetrievalMode]string
	Skipped    map[RetrievalMode]string
	Candidates map[RetrievalMode]int
	Elapsed    map[RetrievalMode]time.Duration
}

// QueryResult is what retrieval returns.
type QueryResult struct {
	Items []RetrievedItem
	Plan  *RetrievalPlan
	// Total is how many distinct candidates were considered before truncation.
	Total int
}
