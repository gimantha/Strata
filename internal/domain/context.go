package domain

import "time"

// ContextSection names one part of an assembled block (AGENTS.md section 20.1).
//
// The sections exist to keep kinds of content separable in the prompt. A model that cannot
// tell a claim the system believes from a passage a document happens to contain cannot be
// asked to weigh them differently, and an instruction hidden in the second one is then
// indistinguishable from policy (AGENTS.md section 20.3).
type ContextSection string

const (
	// SectionFacts holds current, high-confidence assertions.
	SectionFacts ContextSection = "facts"
	// SectionHistory holds assertions that no longer hold but are temporally relevant:
	// superseded values, and claims whose validity ended before the queried instant.
	SectionHistory ContextSection = "history"
	// SectionGraph holds claims reached by traversal rather than by matching.
	SectionGraph ContextSection = "graph"
	// SectionExcerpts holds quoted source content. Untrusted by construction.
	SectionExcerpts ContextSection = "excerpts"
	// SectionConflicts holds contradictions the ledger recorded rather than resolved.
	SectionConflicts ContextSection = "conflicts"
)

var contextSections = []ContextSection{
	SectionFacts, SectionHistory, SectionGraph, SectionExcerpts, SectionConflicts,
}

func ParseContextSection(s string) (ContextSection, error) {
	return parseEnum("context section", s, contextSections)
}

// ContextSections returns every section, in the order they are rendered.
func ContextSections() []ContextSection {
	return append([]ContextSection(nil), contextSections...)
}

// Trusted reports whether a section's content originates inside the system.
//
// Excerpts are the only untrusted section: they are source bytes, reproduced. Everything
// else is a claim the ledger holds, with provenance behind it.
func (s ContextSection) Trusted() bool { return s != SectionExcerpts }

// Budget defaults. A budget is in estimated tokens, not characters or items.
const (
	DefaultTokenBudget = 2000
	MinTokenBudget     = 100
	MaxTokenBudget     = 200000
)

// ContextRequest asks for a prompt-ready block rather than a result list
// (AGENTS.md sections 20 and 25.2).
type ContextRequest struct {
	Scope     Scope
	Query     string
	Principal PrincipalRef

	Temporal TemporalQuery
	Filters  QueryFilters

	// TokenBudget is the ceiling for the rendered block, measured by the configured
	// estimator. It is a hard ceiling: assembly drops content rather than exceeding it.
	TokenBudget int
	// Sections restricts what may appear. Empty means every section.
	Sections []ContextSection
	// MaxItems bounds the number of selected items regardless of budget.
	MaxItems int

	// Explain reports the retrieval plan, the per-item selection arithmetic, and what was
	// dropped and why.
	Explain bool
}

// DefaultMaxContextItems bounds selection when a caller does not.
const DefaultMaxContextItems = 40

// Normalize applies defaults and bounds.
func (r ContextRequest) Normalize() ContextRequest {
	if r.TokenBudget <= 0 {
		r.TokenBudget = DefaultTokenBudget
	}
	if r.TokenBudget > MaxTokenBudget {
		r.TokenBudget = MaxTokenBudget
	}
	if r.MaxItems <= 0 {
		r.MaxItems = DefaultMaxContextItems
	}
	for i, predicate := range r.Filters.Predicates {
		r.Filters.Predicates[i] = NormalizePredicateName(predicate)
	}
	return r
}

func (r ContextRequest) Validate() error {
	const op = "domain.ContextRequest.Validate"

	if IsZero(r.Scope.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace scope is required")
	}
	if r.Query == "" {
		return Errorf(CodeInvalidArgument, op, "a query is required")
	}
	if len(r.Query) > MaxQueryLength {
		return Errorf(CodeInvalidArgument, op,
			"query is longer than %d characters", MaxQueryLength)
	}
	if r.TokenBudget != 0 && r.TokenBudget < MinTokenBudget {
		// A budget too small to hold the scaffolding would return an empty block and look
		// like a retrieval failure.
		return Errorf(CodeInvalidArgument, op,
			"token_budget must be at least %d", MinTokenBudget)
	}
	for _, section := range r.Sections {
		if _, err := ParseContextSection(string(section)); err != nil {
			return err
		}
	}
	if r.Filters.MinConfidence < 0 || r.Filters.MinConfidence > 1 {
		return Errorf(CodeInvalidArgument, op, "min_confidence must be between 0 and 1")
	}
	return nil
}

// Wants reports whether a section may appear in the result.
func (r ContextRequest) Wants(section ContextSection) bool {
	if len(r.Sections) == 0 {
		return true
	}
	for _, want := range r.Sections {
		if want == section {
			return true
		}
	}
	return false
}

// ContextBlock is prompt-ready text plus everything needed to audit it.
//
// Text and Items are two views of the same content: Text is what goes to a model, Items is
// what a reviewer inspects. They are produced together so they cannot disagree.
type ContextBlock struct {
	Text  string
	Items []ContextItem

	// Citations are indexed by the markers appearing in Text.
	Citations []Citation

	Budget BudgetReport
	Plan   *RetrievalPlan
	// Dropped records what was considered and left out, with the reason. Absence is the
	// hardest thing to debug in an assembled prompt.
	Dropped []DroppedItem
}

// ContextItem is one selected piece of content.
type ContextItem struct {
	Section ContextSection
	// Marker is the citation number rendered next to this item, starting at 1.
	Marker int
	Text   string

	Surface  Surface
	RecordID string

	// Relevance is the fused retrieval score; Selection is what selection actually
	// maximized, after confidence, evidence, coverage, and redundancy are applied.
	Relevance float64
	Selection float64
	// Redundancy is the similarity to the most similar already-selected item, in [0,1].
	Redundancy float64
	Tokens     int

	// Signals records the selection arithmetic when explain is on.
	Signals map[string]float64

	Conflict *ConflictNote
}

// ConflictNote annotates a claim the ledger holds as disputed (AGENTS.md section 20.2).
//
// A contradiction is reported, never silently resolved by picking a side, which is the same
// rule the reconciler follows.
type ConflictNote struct {
	ConflictSetID ConflictSetID
	Reason        string
	// Others renders the competing claims, so the disagreement is legible in the prompt
	// rather than only in the citation.
	Others []string
}

// Citation resolves a marker to canonical records (AGENTS.md sections 13 and 20.1).
type Citation struct {
	Marker  int
	Surface Surface

	AssertionID *AssertionID
	EvidenceIDs []EvidenceID
	ChunkID     *ChunkID
	EpisodeID   *EpisodeID

	SourceEventID SourceEventID
	SourceID      SourceID
	SourceName    string

	// Quote is the evidence text an assertion rests on, or the excerpt itself.
	Quote   string
	Locator string

	ValidFrom  *time.Time
	ValidTo    *time.Time
	KnownAt    *time.Time
	Confidence float64
	Status     AssertionStatus
}

// Factual reports whether a citation must reach an assertion.
//
// Excerpts are quoted source, cited to their chunk and episode; everything else is a claim
// and must name the assertion and the evidence under it. Phase 8's acceptance criterion is
// exactly this distinction.
func (c Citation) Factual() bool { return c.Surface != SurfaceChunk }

// BudgetReport accounts for every token in the block.
type BudgetReport struct {
	Limit int
	Used  int
	// Scaffolding is what headers, section titles, and the reference list cost, as
	// distinct from content. A budget spent mostly on scaffolding means the budget is
	// too small, which is worth being able to see.
	Scaffolding int
	BySection   map[ContextSection]int
	// Tolerance is the estimator's declared error against a real tokenizer, as a
	// fraction. Used stays under Limit exactly; a downstream tokenizer may differ by
	// this much.
	Tolerance float64
	Estimator string
}

// DropReason explains an omission.
type DropReason string

const (
	// DropRedundant means the content repeated something already selected.
	DropRedundant DropReason = "redundant"
	// DropBudget means it did not fit.
	DropBudget DropReason = "budget"
	// DropNoEvidence means a factual item could not be cited, so it was not rendered.
	DropNoEvidence DropReason = "no_evidence"
	// DropSectionExcluded means the caller asked for other sections.
	DropSectionExcluded DropReason = "section_excluded"
	// DropItemLimit means MaxItems was reached.
	DropItemLimit DropReason = "item_limit"
)

// DroppedItem records one omission.
type DroppedItem struct {
	Surface    Surface
	RecordID   string
	Section    ContextSection
	Reason     DropReason
	Detail     string
	Relevance  float64
	Redundancy float64
}
