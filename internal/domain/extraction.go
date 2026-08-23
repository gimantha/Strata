package domain

import (
	"strings"
	"time"
)

// ExtractionResult is what a model returns for one unit of source material
// (AGENTS.md section 13.1). It is a set of *candidates*: nothing here is knowledge until
// it has been validated, resolved, and committed.
type ExtractionResult struct {
	Entities   []EntityCandidate
	Assertions []AssertionCandidate
	Temporal   []TemporalHint
}

// EntityCandidate is a proposed identity mentioned in the source.
type EntityCandidate struct {
	Name       string
	EntityType string
	Aliases    []string
	// MentionText is the exact span in the source that named this entity. It must appear
	// in the material the model was shown; see GroundedIn.
	MentionText string
	Confidence  float64
}

func (c EntityCandidate) Validate() error {
	const op = "domain.EntityCandidate.Validate"

	if strings.TrimSpace(c.Name) == "" {
		return Errorf(CodeInvalidArgument, op, "entity candidate needs a name")
	}
	if len(c.Name) > MaxCandidateNameLength {
		return Errorf(CodeInvalidArgument, op,
			"entity name is longer than %d characters", MaxCandidateNameLength)
	}
	if c.Confidence < 0 || c.Confidence > 1 {
		return Errorf(CodeInvalidArgument, op, "confidence must be between 0 and 1")
	}
	return nil
}

// AssertionCandidate is a proposed claim.
//
// Note what it cannot express: no workspace, no graph space, no classification downgrade,
// no status. A model's output describes what the source says; it never decides where the
// knowledge lands or how sensitive it is (AGENTS.md sections 13.3, 24).
type AssertionCandidate struct {
	SubjectName string
	SubjectType string
	Predicate   string

	// ObjectEntityName makes this a relation between identities; otherwise the literal
	// object below is used.
	ObjectEntityName string
	ObjectEntityType string
	ObjectKind       ObjectKind
	ObjectValue      string

	ScopeKey  string
	ValidFrom *time.Time
	ValidTo   *time.Time
	EventTime *time.Time

	// EvidenceQuote must be a verbatim span of the source material. It is the anchor that
	// makes an extracted claim checkable rather than merely plausible.
	EvidenceQuote string
	Confidence    float64

	// Quarantine marks a candidate whose supporting quote sits inside instruction-like
	// text. Such a claim is recorded rather than dropped - the document really does say
	// it - but it is held out of current belief until a human looks
	// (AGENTS.md section 24).
	Quarantine       bool
	QuarantineReason string
}

// Candidate size limits. A model can emit arbitrarily long strings, and unbounded values
// would flow straight into the ledger.
const (
	MaxCandidateNameLength  = 500
	MaxCandidateValueLength = 4000
	MaxCandidateQuoteLength = MaxEvidenceExcerpt
	MaxCandidatesPerRequest = 200
)

func (c AssertionCandidate) Validate() error {
	const op = "domain.AssertionCandidate.Validate"

	if strings.TrimSpace(c.SubjectName) == "" {
		return Errorf(CodeInvalidArgument, op, "assertion candidate needs a subject")
	}
	if strings.TrimSpace(c.Predicate) == "" {
		return Errorf(CodeInvalidArgument, op, "assertion candidate needs a predicate")
	}
	if NormalizePredicateName(c.Predicate) == "" {
		return Errorf(CodeInvalidArgument, op,
			"predicate %q normalizes to nothing usable", c.Predicate)
	}
	if len(c.SubjectName) > MaxCandidateNameLength || len(c.ObjectEntityName) > MaxCandidateNameLength {
		return Errorf(CodeInvalidArgument, op,
			"entity names are limited to %d characters", MaxCandidateNameLength)
	}
	if len(c.ObjectValue) > MaxCandidateValueLength {
		return Errorf(CodeInvalidArgument, op,
			"object values are limited to %d characters", MaxCandidateValueLength)
	}
	if len(c.EvidenceQuote) > MaxCandidateQuoteLength {
		return Errorf(CodeInvalidArgument, op,
			"evidence quotes are limited to %d characters", MaxCandidateQuoteLength)
	}

	hasObject := c.ObjectEntityName != "" || c.ObjectValue != "" || c.ObjectKind == ObjectBoolean
	if !hasObject {
		return Errorf(CodeInvalidArgument, op, "assertion candidate needs an object")
	}
	if c.ObjectEntityName == "" {
		if _, err := ParseObjectKind(string(c.ObjectKind)); err != nil {
			return err
		}
		if c.ObjectKind == ObjectEntity {
			return Errorf(CodeInvalidArgument, op,
				"an entity object must be given as object_entity_name")
		}
	}
	if c.Confidence < 0 || c.Confidence > 1 {
		return Errorf(CodeInvalidArgument, op, "confidence must be between 0 and 1")
	}
	if c.ValidFrom != nil && c.ValidTo != nil && c.ValidTo.Before(*c.ValidFrom) {
		return Errorf(CodeInvalidArgument, op, "validity interval ends before it starts")
	}
	return nil
}

// GroundedIn reports whether the candidate's evidence quote actually appears in the source
// material it was extracted from.
//
// This is the strongest cheap defense available against both hallucination and prompt
// injection: a model that invents a fact, or that obeys an instruction hidden in the
// source, cannot produce a verbatim quote supporting the result. Comparison ignores
// whitespace differences, since models routinely re-wrap text they quote
// (AGENTS.md sections 13.3, 24).
func (c AssertionCandidate) GroundedIn(source string) bool {
	if c.EvidenceQuote == "" {
		return false
	}
	return strings.Contains(collapseSpace(source), collapseSpace(c.EvidenceQuote))
}

// collapseSpace normalizes whitespace for quote comparison.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// TemporalHint is a time expression the model found, kept separate from the claims so a
// parsing failure never silently changes a claim's validity.
type TemporalHint struct {
	Text       string
	Kind       string // valid_from, valid_to, event_time, mentioned
	Resolved   *time.Time
	Confidence float64
}

// ModelRunStatus is the outcome of one model interaction.
type ModelRunStatus string

const (
	// ModelRunSucceeded means the provider answered and the output validated.
	ModelRunSucceeded ModelRunStatus = "succeeded"
	// ModelRunInvalid means the provider answered but the output failed validation. The
	// run is recorded precisely so bad output is visible rather than lost.
	ModelRunInvalid ModelRunStatus = "invalid"
	// ModelRunFailed means the provider could not be reached or refused.
	ModelRunFailed ModelRunStatus = "failed"
)

var modelRunStatuses = []ModelRunStatus{ModelRunSucceeded, ModelRunInvalid, ModelRunFailed}

func ParseModelRunStatus(s string) (ModelRunStatus, error) {
	return parseEnum("model run status", s, modelRunStatuses)
}

// ModelRun records one interaction with a model (AGENTS.md section 13.2).
//
// It stores hashes of the request and response rather than their content: the prompt
// embeds source material, and duplicating that here would scatter copies of sensitive text
// outside the archive. Credentials are never stored.
type ModelRun struct {
	ID           ModelRunID
	WorkspaceID  WorkspaceID
	GraphSpaceID GraphSpaceID

	Provider string
	Model    string
	// PromptTemplate and PromptVersion identify what was asked, so a change in prompting
	// can be correlated with a change in extraction quality.
	PromptTemplate string
	PromptVersion  int

	RequestHash  string
	ResponseHash string

	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostMicros       int64

	Latency time.Duration
	Status  ModelRunStatus
	// ValidationError explains why structured output was rejected, if it was.
	ValidationError string
	// ResponseExcerpt keeps a bounded sample of rejected output for debugging. It is only
	// populated for invalid runs, where seeing the shape of the failure is the point.
	ResponseExcerpt string

	SourceEventID SourceEventID
	CreatedAt     time.Time
}

// MaxResponseExcerpt bounds what is kept from a rejected response.
const MaxResponseExcerpt = 2000

func (r ModelRun) Validate() error {
	const op = "domain.ModelRun.Validate"

	if IsZero(r.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id is required")
	}
	if r.Provider == "" || r.Model == "" {
		return Errorf(CodeInvalidArgument, op, "provider and model are required")
	}
	if r.RequestHash == "" {
		return Errorf(CodeInvalidArgument, op, "request_hash is required")
	}
	if _, err := ParseModelRunStatus(string(r.Status)); err != nil {
		return err
	}
	if len(r.ResponseExcerpt) > MaxResponseExcerpt {
		return Errorf(CodeInvalidArgument, op,
			"response excerpt is limited to %d characters", MaxResponseExcerpt)
	}
	return nil
}
