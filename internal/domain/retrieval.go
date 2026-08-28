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
	// EntityTypes narrows to subjects of these types — ontology-constrained retrieval
	// (AGENTS.md section 19.2). Types are normalized, so "Organization" and "organisation"
	// filter the same way they are stored.
	EntityTypes []string
}

// QueryRequest is one retrieval (AGENTS.md section 19.1).
type QueryRequest struct {
	Scope     Scope
	Query     string
	Principal PrincipalRef

	Temporal TemporalQuery
	Filters  QueryFilters
	// Policy is the narrowing the caller's access decision requires. It is applied inside
	// every retriever's query rather than to their results (AGENTS.md section 22.4).
	Policy PolicyFilters
	// Purpose is the caller's stated reason for asking, when policy requires one.
	Purpose string
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
	for i, entityType := range q.Filters.EntityTypes {
		q.Filters.EntityTypes[i] = NormalizeEntityType(entityType)
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

	// Planner names what produced this plan, so a surprising result can be attributed to
	// the thing that chose the strategy rather than to the retrievers that executed it.
	Planner string
	// SubQueries are the searches actually issued. Always at least one: a plan that
	// searched for the question as asked records that rather than leaving it implied.
	//
	// More than one means the question was reshaped before retrieval, and a reader needs
	// to see what was actually asked of the index — a result that came back for a
	// rewritten question is not evidence about the original unless someone can compare
	// them (AGENTS.md section 19.4, reranker output must be traceable).
	SubQueries []SubQuery
	// PlannerNote records why a planner did something unusual, most often why an LLM
	// planner was not used after all.
	PlannerNote string
}

// SubQuery is one search issued on behalf of a request.
type SubQuery struct {
	Text   string
	Kind   SubQueryKind
	Reason string
}

// SubQueryKind says how a search relates to the question that prompted it.
type SubQueryKind string

const (
	// SubQueryOriginal is the question as the caller asked it. Always present: a rewrite
	// that loses the original loses the one phrasing known to be what someone meant.
	SubQueryOriginal SubQueryKind = "original"
	// SubQueryDecomposed is one part of a question that asked for several things.
	SubQueryDecomposed SubQueryKind = "decomposed"
	// SubQueryHypothetical is a sketch of what an answer might look like, searched for
	// instead of the question. A question and its answer are worded differently, so
	// embedding the answer's shape often lands closer to the passage that contains it.
	SubQueryHypothetical SubQueryKind = "hypothetical"
)

// MaxSubQueries bounds how far a question may be expanded.
//
// Each one is a full retrieval across every planned mode, so the cost is multiplied rather
// than added to. Four is enough for a decomposition plus a hypothetical answer and little
// enough that a planner cannot turn one question into a scan.
const MaxSubQueries = 4

// MaxSubQueryLength bounds a single rewritten search, so a planner cannot smuggle a
// document into the index as a query.
const MaxSubQueryLength = 512

// QueryResult is what retrieval returns.
type QueryResult struct {
	Items []RetrievedItem
	Plan  *RetrievalPlan
	// Total is how many distinct candidates were considered before truncation.
	Total int
	// TraceID names the persisted trace, when tracing is configured. It is what turns
	// "why did the agent say that" into a lookup rather than an investigation.
	TraceID TraceID
}

// ScoredRef is one record a trace names, with the score it had.
type ScoredRef struct {
	Surface  Surface `json:"surface"`
	RecordID string  `json:"record_id"`
	Score    float64 `json:"score,omitempty"`
}

// RetrievalTrace is query-time explainability made durable (AGENTS.md section 6.12).
//
// It answers "why did this agent see that" after the fact: what was asked, under which
// policy, what was considered, and what came back. Deferred from phase 8 on purpose — section
// 6.12 marks query text subject to redaction, and there was no policy to redact against.
type RetrievalTrace struct {
	ID           TraceID
	WorkspaceID  WorkspaceID
	GraphSpaceID GraphSpaceID

	QueryHash string
	QueryText string
	// Redacted records that the text was deliberately not kept, which is different from a
	// trace that happened to have no text.
	Redacted bool

	Principal PrincipalRef
	Purpose   string
	Action    PolicyAction

	PolicyVersion int
	PolicyRule    string
	PolicyFilters PolicyFilters

	Filters       QueryFilters
	CandidateRefs []ScoredRef
	SelectedRefs  []ScoredRef

	Latency   time.Duration
	QueryTime time.Time
}
