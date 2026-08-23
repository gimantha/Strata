package domain

import "time"

// Surface names what a projected record represents.
//
// Several surfaces are indexed rather than only chunk text, because a question about an
// entity's name, a question about a fact, and a question about a passage are different
// searches (AGENTS.md section 17).
type Surface string

const (
	SurfaceChunk     Surface = "chunk"
	SurfaceEpisode   Surface = "episode"
	SurfaceEntity    Surface = "entity"
	SurfaceAssertion Surface = "assertion"
)

var surfaces = []Surface{SurfaceChunk, SurfaceEpisode, SurfaceEntity, SurfaceAssertion}

func ParseSurface(s string) (Surface, error) { return parseEnum("surface", s, surfaces) }

// Surfaces returns every projected surface.
func Surfaces() []Surface { return append([]Surface(nil), surfaces...) }

// ProjectionRef identifies one projected record.
type ProjectionRef struct {
	WorkspaceID WorkspaceID
	Surface     Surface
	RecordID    string
}

// ProjectedRecord is the shared shape of everything written to a projection.
//
// The filter fields are copied from the canonical record so retrieval can narrow before
// ranking. They are duplicated deliberately: joining back to the ledger on every candidate
// would defeat the point of having an index.
type ProjectedRecord struct {
	Scope    Scope
	Surface  Surface
	RecordID string

	// Content is the text this record is found by.
	Content string

	ValidFrom      *time.Time
	ValidTo        *time.Time
	Status         string
	Classification Classification
	MemoryKind     MemoryKind
	SourceEventID  SourceEventID
}

// VectorRecord is a projected record together with its embedding.
type VectorRecord struct {
	ProjectedRecord
	Model     string
	Version   int
	Embedding []float32
	// ContentHash lets a rebuild skip re-embedding text that has not changed, which is the
	// difference between a cheap incremental update and paying a provider twice.
	ContentHash string
}

// VectorQuery searches the vector projection.
type VectorQuery struct {
	Scope     Scope
	Embedding []float32
	Surfaces  []Surface
	Model     string
	Version   int

	// Filters applied before ranking.
	ValidAt        *time.Time
	Statuses       []string
	Classification []Classification
	MemoryKinds    []MemoryKind

	Limit int
	// MinScore drops weak matches. Vector search always returns its k nearest neighbours,
	// however far away they are, so without a floor an unrelated query still gets answers.
	MinScore float64
}

// LexicalQuery searches the lexical projection.
type LexicalQuery struct {
	Scope    Scope
	Text     string
	Surfaces []Surface

	ValidAt        *time.Time
	Statuses       []string
	Classification []Classification

	// Exact switches from stemmed full-text matching to substring matching, for
	// identifiers, error codes, and part numbers that stemming mangles.
	Exact bool
	Limit int
}

// Hit is one retrieval result. It carries the canonical reference rather than the content,
// so the caller reads authoritative state rather than a projection's copy of it.
type Hit struct {
	Surface  Surface
	RecordID string
	Score    float64
	Content  string
	// Detail explains the score, which retrieval traces need in later phases.
	Detail map[string]any
}

// GraphEdge is an entity-to-entity assertion projected as a traversable edge.
//
// Only entity-valued assertions become edges. A literal-valued claim is not an edge in any
// useful sense, and forcing one would either invent nodes for values or flatten the typed
// object the ledger works to preserve. The assertion id is carried so the full claim, with
// its temporal metadata and provenance, is one lookup away (AGENTS.md section 16).
type GraphEdge struct {
	WorkspaceID    WorkspaceID
	GraphSpaceID   GraphSpaceID
	SubjectID      EntityID
	Predicate      string
	ObjectEntityID EntityID
	AssertionID    AssertionID
	ValidFrom      *time.Time
	ValidTo        *time.Time
	Status         AssertionStatus
	Confidence     float64
	Classification Classification
}

// GraphExpandQuery walks the graph projection outwards from a set of entities.
type GraphExpandQuery struct {
	Scope Scope
	// Roots are the entities to expand from.
	Roots []EntityID
	// Depth bounds the walk. Unbounded traversal over a well-connected graph reaches
	// everything, which is never a useful answer (AGENTS.md sections 16, 39).
	Depth      int
	Predicates []string
	ValidAt    *time.Time
	// IncludeSuperseded walks historical edges as well as current ones.
	IncludeSuperseded bool
	Limit             int
}

// MaxGraphDepth is the hard ceiling on traversal, whatever a caller asks for.
const MaxGraphDepth = 5

// DefaultGraphDepth is used when a query does not say.
const DefaultGraphDepth = 2

// GraphHit is one entity reached by expansion.
type GraphHit struct {
	EntityID EntityID
	Name     string
	// Depth is how many hops from the nearest root.
	Depth int
	// ViaAssertion is the claim that produced the last edge walked, so a path can be
	// explained rather than merely reported.
	ViaAssertion AssertionID
	ViaPredicate string
	FromEntityID EntityID
}

// Normalize applies defaults and bounds.
func (q GraphExpandQuery) Normalize() GraphExpandQuery {
	if q.Depth <= 0 {
		q.Depth = DefaultGraphDepth
	}
	if q.Depth > MaxGraphDepth {
		q.Depth = MaxGraphDepth
	}
	if q.Limit <= 0 || q.Limit > MaxAssertionLimit {
		q.Limit = DefaultAssertionLimit
	}
	for i, predicate := range q.Predicates {
		q.Predicates[i] = NormalizePredicateName(predicate)
	}
	return q
}

// ProjectionCheckpoint records how far a projection has consumed the ledger.
type ProjectionCheckpoint struct {
	WorkspaceID      WorkspaceID
	Projection       string
	LastRecordedAt   *time.Time
	LastRecordID     string
	RecordsProjected int64
	LastError        string
	RebuiltAt        *time.Time
	UpdatedAt        time.Time
}
