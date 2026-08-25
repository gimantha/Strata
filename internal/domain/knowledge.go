package domain

import (
	"encoding/json"
	"strings"
	"time"
)

// Entity is a stable identity, not a bag of mutable facts.
//
// Only identity-oriented values live here. Anything that changes - a title, an
// employer, an address - is an assertion, because overwriting a property destroys the
// history that makes this system useful (AGENTS.md section 6.7).
type Entity struct {
	ID            EntityID
	WorkspaceID   WorkspaceID
	GraphSpaceID  GraphSpaceID
	CanonicalName string
	EntityType    string
	Metadata      map[string]any
	CreatedAt     time.Time
	RetiredAt     *time.Time
}

func (e Entity) Validate() error {
	const op = "domain.Entity.Validate"

	if IsZero(e.WorkspaceID) || IsZero(e.GraphSpaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id and graph_space_id are required")
	}
	if strings.TrimSpace(e.CanonicalName) == "" {
		return Errorf(CodeInvalidArgument, op, "canonical_name is required")
	}
	if strings.TrimSpace(e.EntityType) == "" {
		return Errorf(CodeInvalidArgument, op, "entity_type is required")
	}
	return nil
}

// Active reports whether the entity is still in use.
func (e Entity) Active() bool { return e.RetiredAt == nil }

// EntityAlias is another name the same identity is known by. Aliases are how phase 4's
// resolution records what it matched, and they are reversible by design.
type EntityAlias struct {
	ID          string
	EntityID    EntityID
	WorkspaceID WorkspaceID
	Alias       string
	Normalized  string
	SourceID    *SourceID
	Confidence  float64
	CreatedAt   time.Time
}

// NormalizeAlias produces the matching form of a name: case-folded with collapsed
// whitespace. It is deliberately conservative, since aggressive normalization merges
// identities that should stay separate.
func NormalizeAlias(alias string) string {
	return strings.Join(strings.Fields(strings.ToLower(alias)), " ")
}

func (a EntityAlias) Validate() error {
	const op = "domain.EntityAlias.Validate"

	if IsZero(a.EntityID) || IsZero(a.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "entity_id and workspace_id are required")
	}
	if strings.TrimSpace(a.Alias) == "" {
		return Errorf(CodeInvalidArgument, op, "alias is required")
	}
	if a.Confidence < 0 || a.Confidence > 1 {
		return Errorf(CodeInvalidArgument, op, "confidence must be between 0 and 1")
	}
	return nil
}

// PredicateRef identifies the predicate a claim uses, at a specific registry version.
// Recording the version means a later change to predicate semantics cannot silently
// reinterpret assertions that were validated under the old definition.
type PredicateRef struct {
	ID      PredicateID
	Name    string
	Version int
}

// PredicateDefinition gives a predicate the semantics that make contradiction handling
// something other than guesswork (AGENTS.md section 8).
type PredicateDefinition struct {
	ID                PredicateID
	WorkspaceID       WorkspaceID
	Name              string
	Description       string
	SubjectTypes      []string
	ObjectTypes       []string
	ObjectKinds       []ObjectKind
	Functional        bool
	InverseFunctional bool
	Symmetric         bool
	Transitive        bool
	TemporalPolicy    TemporalPolicy
	ConflictPolicy    ConflictPolicy
	DefaultMemoryKind MemoryKind
	Sensitivity       Classification
	Status            PredicateStatus
	Version           int
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// NormalizePredicateName produces the canonical registry form: UPPER_SNAKE_CASE.
// Extraction in open mode invents names freely, and without one canonical form the
// registry would fill with near-duplicates such as "worksAt" and "works_at".
func NormalizePredicateName(name string) string {
	var b strings.Builder
	prevUnderscore := true // leading separators are dropped
	prevLower := false

	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32)
			prevUnderscore, prevLower = false, true
		case r >= 'A' && r <= 'Z':
			// camelCase boundary: worksAt becomes WORKS_AT.
			if prevLower && !prevUnderscore {
				b.WriteByte('_')
			}
			b.WriteRune(r)
			prevUnderscore, prevLower = false, false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUnderscore, prevLower = false, false
		default:
			if !prevUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

func (p PredicateDefinition) Validate() error {
	const op = "domain.PredicateDefinition.Validate"

	if IsZero(p.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id is required")
	}
	if p.Name == "" {
		return Errorf(CodeInvalidArgument, op, "name is required")
	}
	if p.Name != NormalizePredicateName(p.Name) {
		return Errorf(CodeInvalidArgument, op,
			"predicate name %q must be normalized to %q", p.Name, NormalizePredicateName(p.Name))
	}
	if p.Version < 1 {
		return Errorf(CodeInvalidArgument, op, "version must be at least 1")
	}
	if _, err := ParseTemporalPolicy(string(p.TemporalPolicy)); err != nil {
		return err
	}
	if _, err := ParseConflictPolicy(string(p.ConflictPolicy)); err != nil {
		return err
	}
	if _, err := ParsePredicateStatus(string(p.Status)); err != nil {
		return err
	}
	if _, err := ParseMemoryKind(string(p.DefaultMemoryKind)); err != nil {
		return err
	}
	if _, err := ParseClassification(string(p.Sensitivity)); err != nil {
		return err
	}
	return nil
}

// Ref returns the reference an assertion stores.
func (p PredicateDefinition) Ref() PredicateRef {
	return PredicateRef{ID: p.ID, Name: p.Name, Version: p.Version}
}

// AllowsMultipleValues reports whether two different objects may hold at once for the
// same subject and interval. A functional predicate is the case where they may not.
func (p PredicateDefinition) AllowsMultipleValues() bool {
	return !p.Functional && p.ConflictPolicy == ConflictPolicyCoexist
}

// ConfidenceBreakdown keeps a confidence score explainable by recording the signals
// behind it rather than only their product (AGENTS.md section 14.4). Phases 3 and 5
// populate more of these; an unset component simply does not contribute.
type ConfidenceBreakdown struct {
	SourceTrust          *float64 `json:"source_trust,omitempty"`
	Extraction           *float64 `json:"extraction,omitempty"`
	EntityResolution     *float64 `json:"entity_resolution,omitempty"`
	TemporalParsing      *float64 `json:"temporal_parsing,omitempty"`
	Corroboration        *float64 `json:"corroboration,omitempty"`
	OntologyValidation   *float64 `json:"ontology_validation,omitempty"`
	ContradictionPenalty *float64 `json:"contradiction_penalty,omitempty"`
	HumanConfirmation    *float64 `json:"human_confirmation,omitempty"`
}

// Assertion is the primary knowledge unit: one claim, immutable once committed
// (AGENTS.md section 6.9).
//
// Corrections never edit an assertion. They create a new one and mark this one
// superseded, which changes what the system believes without rewriting what it once
// believed or when a fact held in the world.
type Assertion struct {
	ID           AssertionID
	WorkspaceID  WorkspaceID
	GraphSpaceID GraphSpaceID

	SubjectID EntityID
	Predicate PredicateRef
	Object    AssertionObject

	MemoryKind MemoryKind
	// ScopeKey narrows a claim to a context in which it holds, such as a session or a
	// project, so the same predicate can hold different values in different scopes
	// without those values contradicting each other.
	ScopeKey string

	Temporal TemporalCoordinates

	Confidence          float64
	ConfidenceBreakdown *ConfidenceBreakdown
	Status              AssertionStatus
	SupersedesID        *AssertionID
	ConflictSetID       *ConflictSetID

	ProvenanceMode ProvenanceMode
	DerivationID   *DerivationID
	// SourceEventID ties the claim to the ingestion that produced it, which is the
	// first link of the provenance chain.
	SourceEventID SourceEventID

	// Fingerprint makes committing idempotent: reprocessing an event produces the same
	// fingerprint and collides instead of duplicating knowledge.
	Fingerprint string

	// RetractedAt withdraws a claim without replacing it. Like supersession it is a
	// knowledge-time event, so as-of queries before this instant still see the claim.
	RetractedAt      *time.Time
	RetractionReason string

	// DeactivatedAt records when this claim was taken out of active context, and
	// DeactivationReason why. Distinct from retraction: the claim is still true, still
	// cited, and still answerable as of any instant (AGENTS.md section 21.4).
	DeactivatedAt      *time.Time
	DeactivationReason string

	// OntologyVersionID names the schema this claim was validated against, when one was
	// bound (AGENTS.md section 8). Nil in open mode: recording a version the claim never
	// saw would be a lie that outlives the mistake.
	OntologyVersionID *OntologyVersionID
	// QuarantineReason says why a held claim is held. Quarantine has two causes now —
	// an instruction-like quote and a schema violation — and a status that cannot tell
	// them apart cannot be triaged.
	QuarantineReason string

	Classification Classification
	CreatedBy      PrincipalRef
	CreatedAt      time.Time

	// sourceID is derived from the source event rather than stored on the assertion. It
	// is unexported so it cannot be set independently of the event it must agree with.
	sourceID SourceID
}

func (a Assertion) Validate() error {
	const op = "domain.Assertion.Validate"

	if IsZero(a.WorkspaceID) || IsZero(a.GraphSpaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id and graph_space_id are required")
	}
	if IsZero(a.SubjectID) {
		return Errorf(CodeInvalidArgument, op, "subject_id is required")
	}
	if a.Predicate.Name == "" {
		return Errorf(CodeInvalidArgument, op, "predicate is required")
	}
	if a.Predicate.Version < 1 {
		return Errorf(CodeInvalidArgument, op, "predicate version is required")
	}
	if err := a.Object.Validate(); err != nil {
		return err
	}
	if a.Confidence < 0 || a.Confidence > 1 {
		return Errorf(CodeInvalidArgument, op, "confidence must be between 0 and 1")
	}
	if _, err := ParseMemoryKind(string(a.MemoryKind)); err != nil {
		return err
	}
	if _, err := ParseAssertionStatus(string(a.Status)); err != nil {
		return err
	}
	if _, err := ParseProvenanceMode(string(a.ProvenanceMode)); err != nil {
		return err
	}
	if a.ProvenanceMode.RequiresDerivation() && a.DerivationID == nil {
		return Errorf(CodeInvalidArgument, op,
			"%s assertions must reference a derivation: inference may not be presented as observation",
			a.ProvenanceMode)
	}
	if err := a.Temporal.Validate(); err != nil {
		return err
	}
	return nil
}

// ComputeFingerprint derives the idempotency key for a claim.
//
// It covers everything that makes this a distinct claim, including the world interval
// and the originating event. Two extractions of the same statement from the same event
// collide; the same statement from a different event does not, because independent
// corroboration is real information.
func (a Assertion) ComputeFingerprint() string {
	return DeriveKey("assertion",
		string(a.GraphSpaceID),
		string(a.SubjectID),
		a.Predicate.Name,
		a.Object.Key(),
		a.ScopeKey,
		string(a.MemoryKind),
		timeKey(a.Temporal.ValidFrom),
		timeKey(a.Temporal.ValidTo),
		string(a.SourceEventID),
		string(a.ProvenanceMode),
	)
}

// SourceID reports which source produced this claim, when that is known from its temporal
// coordinates. Sequences from different sources are not on one timeline, so ordering has to
// know whether two claims even came from the same place.
func (a Assertion) SourceID() SourceID { return a.sourceID }

// WithSourceID records the originating source. It is set when a claim is loaded, rather
// than stored twice, since the source event already names it.
func (a Assertion) WithSourceID(id SourceID) Assertion {
	a.sourceID = id
	return a
}

// BelievedAt reports whether the system held this claim at knowledge time k.
//
// This is what makes "what did we believe on April 10" answerable: a claim recorded
// before k, and neither superseded nor retracted until after k, was believed then -
// regardless of what replaced it since.
func (a Assertion) BelievedAt(k time.Time) bool {
	if a.Temporal.RecordedAt.After(k) {
		return false
	}
	if a.Temporal.SupersededAt != nil && !a.Temporal.SupersededAt.After(k) {
		return false
	}
	if a.RetractedAt != nil && !a.RetractedAt.After(k) {
		return false
	}
	return true
}

// ValidAt reports whether the claim describes the world as of world time t. An open
// interval end means "still true as far as this claim says".
func (a Assertion) ValidAt(t time.Time) bool {
	if a.Temporal.ValidFrom != nil && a.Temporal.ValidFrom.After(t) {
		return false
	}
	if a.Temporal.ValidTo != nil && !a.Temporal.ValidTo.After(t) {
		return false
	}
	return true
}

// ActiveAt reports whether the claim is currently relevant context, which is a separate
// question from whether it was ever true (AGENTS.md section 7.1 layer D).
func (a Assertion) ActiveAt(t time.Time) bool {
	if a.Temporal.ActiveFrom != nil && a.Temporal.ActiveFrom.After(t) {
		return false
	}
	if a.Temporal.ActiveUntil != nil && !a.Temporal.ActiveUntil.After(t) {
		return false
	}
	if a.Temporal.ExpiresAt != nil && !a.Temporal.ExpiresAt.After(t) {
		return false
	}
	return true
}

// Evidence links a claim to the exact source material it came from
// (AGENTS.md section 6.10).
type Evidence struct {
	ID            EvidenceID
	WorkspaceID   WorkspaceID
	AssertionID   AssertionID
	EpisodeID     EpisodeID
	ChunkID       *ChunkID
	ArtifactID    *ArtifactID
	SourceEventID SourceEventID
	QuoteStart    *int
	QuoteEnd      *int
	// ExtractedText is a short excerpt for citation. It is bounded on purpose: evidence
	// is a pointer into archived source, not a second copy of it.
	ExtractedText string
	ModelRunID    *ModelRunID
	Confidence    float64
	CreatedAt     time.Time
}

// MaxEvidenceExcerpt bounds the stored quote.
const MaxEvidenceExcerpt = 2000

func (e Evidence) Validate() error {
	const op = "domain.Evidence.Validate"

	if IsZero(e.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id is required")
	}
	if IsZero(e.EpisodeID) {
		return Errorf(CodeInvalidArgument, op, "episode_id is required: evidence must point at source material")
	}
	if len(e.ExtractedText) > MaxEvidenceExcerpt {
		return Errorf(CodeInvalidArgument, op,
			"extracted_text may be at most %d characters; evidence cites source, it does not copy it",
			MaxEvidenceExcerpt)
	}
	if e.QuoteStart != nil && e.QuoteEnd != nil && *e.QuoteEnd < *e.QuoteStart {
		return Errorf(CodeInvalidArgument, op, "quote offsets are inverted")
	}
	if e.Confidence < 0 || e.Confidence > 1 {
		return Errorf(CodeInvalidArgument, op, "confidence must be between 0 and 1")
	}
	return nil
}

// Derivation records what produced a claim that was not directly observed: which rule,
// model, or job, and which assertions it reasoned from (AGENTS.md section 6.11).
type Derivation struct {
	ID           DerivationID
	WorkspaceID  WorkspaceID
	GraphSpaceID GraphSpaceID
	// Method names the mechanism, such as consolidation or graph_inference.
	Method      string
	RuleName    string
	RuleVersion string
	ModelRunID  *ModelRunID
	// InputAssertionIDs are the claims this derivation reasoned from. Without them a
	// derived fact cannot be explained or re-evaluated when its inputs change.
	InputAssertionIDs []AssertionID
	Parameters        map[string]any
	CreatedAt         time.Time
}

func (d Derivation) Validate() error {
	const op = "domain.Derivation.Validate"

	if IsZero(d.WorkspaceID) || IsZero(d.GraphSpaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id and graph_space_id are required")
	}
	if strings.TrimSpace(d.Method) == "" {
		return Errorf(CodeInvalidArgument, op, "method is required")
	}
	return nil
}

// ConflictSet groups claims that cannot all be true and have not been resolved.
//
// Keeping both and marking the disagreement is the point: deleting one arbitrarily
// would destroy information and hide the contradiction from anyone reading the graph
// (AGENTS.md section 14.2).
type ConflictSet struct {
	ID           ConflictSetID
	WorkspaceID  WorkspaceID
	GraphSpaceID GraphSpaceID
	SubjectID    EntityID
	Predicate    string
	ScopeKey     string
	Reason       string
	Resolution   ConflictResolution
	ResolvedAt   *time.Time
	ResolvedBy   PrincipalID
	CreatedAt    time.Time
}

func (c ConflictSet) Validate() error {
	const op = "domain.ConflictSet.Validate"

	if IsZero(c.WorkspaceID) || IsZero(c.GraphSpaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id and graph_space_id are required")
	}
	if IsZero(c.SubjectID) || c.Predicate == "" {
		return Errorf(CodeInvalidArgument, op, "subject_id and predicate are required")
	}
	if _, err := ParseConflictResolution(string(c.Resolution)); err != nil {
		return err
	}
	return nil
}

// AssertionCommit is one claim together with everything committed atomically with it.
type AssertionCommit struct {
	Assertion Assertion
	Evidence  []Evidence
	// SupersedesIDs are prior claims this one corrects. They are marked superseded in
	// the same transaction, so the ledger never shows a correction without its cause.
	SupersedesIDs []AssertionID
}

// KnowledgeCommit is an atomic addition to the ledger (AGENTS.md section 27.1).
type KnowledgeCommit struct {
	Scope         Scope
	SourceEventID SourceEventID
	Entities      []Entity
	Aliases       []EntityAlias
	Derivations   []Derivation
	Assertions    []AssertionCommit
	Outbox        []OutboxEvent
	Actor         PrincipalRef
	// SupersededAt is the knowledge time stamped on claims this commit replaces.
	SupersededAt time.Time
}

// KnowledgeCommitResult reports what a commit did.
type KnowledgeCommitResult struct {
	Entities   []Entity
	Assertions []Assertion
	// Duplicates counts claims that were already present by fingerprint, which is the
	// normal outcome of a replay rather than an error.
	Duplicates  int
	Superseded  []AssertionID
	ConflictSet []ConflictSetID
}

// AssertionQuery expresses the temporal and structural filters from AGENTS.md
// sections 7.3 and 25.3.
type AssertionQuery struct {
	Scope Scope

	SubjectIDs      []EntityID
	Predicates      []string
	ObjectEntityIDs []EntityID
	ObjectKey       string
	ScopeKey        string
	MemoryKinds     []MemoryKind
	Statuses        []AssertionStatus
	SourceEventID   SourceEventID
	MinConfidence   float64

	// ProvenanceModes restricts to claims that came to exist a particular way, which is
	// how "which of these were inferred rather than observed?" is asked (AGENTS.md
	// section 38). Without it a reader cannot separate what a source said from what the
	// system concluded, and the distinction the ledger works to record goes unused.
	ProvenanceModes []ProvenanceMode

	// SourceIDs restricts to claims that arrived from these sources.
	SourceIDs []SourceID
	// MinTrustLevel restricts to claims from sources at least this authoritative — the
	// "only from audited sources" question. Trust is a property of the registered source
	// rather than of the claim, so this narrows through the event that produced it.
	MinTrustLevel TrustLevel
	// ChangedSince returns claims whose position in a source's own ordering is after
	// this one: what a downstream consumer has not seen yet.
	//
	// Ordering follows the source, never arrival (ADR 0010), so this uses the same
	// precedence as reconciliation: sequence, then version, then commit time, then
	// source time. Positions from different sources are not on one timeline, so this
	// requires exactly one source to be named.
	ChangedSince *SourcePosition
	// Classifications restricts to these sensitivity levels. Policy passes the set a
	// principal is cleared for, so the narrowing happens in the query rather than after
	// it (AGENTS.md section 22.4).
	Classifications []Classification

	// ValidAt filters world validity: what held at this world time.
	ValidAt *time.Time
	// ValidBetween filters claims whose validity overlaps the range.
	ValidBetween *TimeRange
	// KnownAt filters knowledge time: what the system believed at this instant,
	// including claims that have since been superseded.
	KnownAt *time.Time
	// EventBetween filters on when the described event occurred.
	EventBetween *TimeRange
	// ActiveAt filters context lifecycle: what is relevant now, which is not the same
	// as what is true.
	ActiveAt *time.Time

	// IncludeSuperseded returns historical belief alongside current belief. It is
	// ignored when KnownAt is set, since knowledge time already decides that.
	IncludeSuperseded bool
	Limit             int
	Offset            int
}

// TimeRange is a half-open interval [Start, End).
type TimeRange struct {
	Start time.Time
	End   time.Time
}

// DefaultAssertionLimit bounds an unspecified query. Unbounded reads are not allowed
// (AGENTS.md section 39).
const DefaultAssertionLimit = 100

// MaxAssertionLimit caps what a caller may request.
const MaxAssertionLimit = 1000

// Normalize applies defaults and bounds.
func (q AssertionQuery) Normalize() AssertionQuery {
	if q.Limit <= 0 {
		q.Limit = DefaultAssertionLimit
	}
	if q.Limit > MaxAssertionLimit {
		q.Limit = MaxAssertionLimit
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	for i, p := range q.Predicates {
		q.Predicates[i] = NormalizePredicateName(p)
	}
	return q
}

// TrustLevelsAtLeast lists the levels that meet a minimum, so a store can express an
// authority floor as set membership.
//
// The ranking lives here rather than in SQL because it is a domain judgement about which
// sources outrank which, and a second copy of it in a query would be free to disagree.
func TrustLevelsAtLeast(min TrustLevel) []TrustLevel {
	floor := min.Rank()
	if floor <= 0 {
		return nil
	}
	out := make([]TrustLevel, 0, len(trustLevels))
	for _, level := range trustLevels {
		if level.Rank() >= floor {
			out = append(out, level)
		}
	}
	return out
}

func (q AssertionQuery) Validate() error {
	const op = "domain.AssertionQuery.Validate"

	if IsZero(q.Scope.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace scope is required")
	}
	for _, r := range []*TimeRange{q.ValidBetween, q.EventBetween} {
		if r != nil && r.End.Before(r.Start) {
			return Errorf(CodeInvalidArgument, op, "time range ends before it starts")
		}
	}
	if q.MinConfidence < 0 || q.MinConfidence > 1 {
		return Errorf(CodeInvalidArgument, op, "min_confidence must be between 0 and 1")
	}
	for _, mode := range q.ProvenanceModes {
		if _, err := ParseProvenanceMode(string(mode)); err != nil {
			return err
		}
	}
	if q.MinTrustLevel != "" {
		if _, err := ParseTrustLevel(string(q.MinTrustLevel)); err != nil {
			return err
		}
	}
	if q.ChangedSince != nil {
		// One source, because a sequence from one system says nothing about a sequence
		// from another; comparing them would invent an order neither source stated.
		if len(q.SourceIDs) != 1 {
			return Errorf(CodeInvalidArgument, op,
				"changed_since requires exactly one source: positions from different "+
					"sources are not on one timeline")
		}
		if !q.ChangedSince.Comparable() {
			return Errorf(CodeInvalidArgument, op,
				"changed_since needs a sequence, version, commit time, or source time")
		}
	}
	return nil
}

// ProvenanceChain is the full walk from a claim back to the bytes that support it
// (AGENTS.md section 6.10, scenario G).
type ProvenanceChain struct {
	Assertion  Assertion
	Subject    Entity
	Links      []ProvenanceLink
	Derivation *Derivation
	// Supports are the assertions a derived claim was reasoned from.
	Supports []Assertion
}

// ProvenanceLink is one evidence record resolved through to its source.
type ProvenanceLink struct {
	Evidence    Evidence
	Chunk       *Chunk
	Episode     Episode
	Artifact    Artifact
	SourceEvent SourceEvent
	Source      Source
}

// timeKey renders an optional instant for fingerprinting.
func timeKey(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// MarshalJSON keeps the wire form of an object compact and typed.
func (o AssertionObject) MarshalJSON() ([]byte, error) {
	out := map[string]any{"kind": string(o.Kind)}
	switch o.Kind {
	case ObjectEntity:
		out["entity_id"] = string(o.EntityID)
	case ObjectString, ObjectURI, ObjectSymbol:
		out["value"] = o.Text
	case ObjectInteger:
		out["value"] = o.Integer
	case ObjectDecimal:
		out["value"] = o.Decimal
	case ObjectBoolean:
		out["value"] = o.Boolean
	case ObjectTimestamp:
		out["value"] = o.Timestamp.UTC().Format(time.RFC3339Nano)
	case ObjectDate:
		out["value"] = o.Date.UTC().Format(DateLayout)
	case ObjectDuration:
		out["value"] = o.Duration.String()
	case ObjectGeo:
		out["value"] = o.Geo
	case ObjectJSON:
		out["value"] = o.JSON
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads the wire form back into a typed object.
//
// The counterpart of MarshalJSON, and a lesson from finding a symbol arriving with no text
// after a package round trip: a type with a custom marshaller and no matching unmarshaller is
// silently lossy, and the loss shows up wherever the value is written and read back — a
// portable package, a queued job, a cached response.
func (o *AssertionObject) UnmarshalJSON(data []byte) error {
	const op = "domain.AssertionObject.UnmarshalJSON"

	var wire struct {
		Kind     string          `json:"kind"`
		EntityID string          `json:"entity_id"`
		Value    json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}

	kind, err := ParseObjectKind(wire.Kind)
	if err != nil {
		if wire.Kind == "" {
			// An absent kind is an empty object rather than a malformed one: a claim may
			// legitimately carry no object until one is set.
			*o = AssertionObject{}
			return nil
		}
		return err
	}
	rebuilt := AssertionObject{Kind: kind}

	switch kind {
	case ObjectEntity:
		rebuilt.EntityID = EntityID(wire.EntityID)
	case ObjectString, ObjectURI, ObjectSymbol:
		if err := json.Unmarshal(wire.Value, &rebuilt.Text); err != nil {
			return Wrap(err, CodeInvalidArgument, op, "malformed text object")
		}
	case ObjectInteger:
		if err := json.Unmarshal(wire.Value, &rebuilt.Integer); err != nil {
			return Wrap(err, CodeInvalidArgument, op, "malformed integer object")
		}
	case ObjectDecimal:
		if err := json.Unmarshal(wire.Value, &rebuilt.Decimal); err != nil {
			return Wrap(err, CodeInvalidArgument, op, "malformed decimal object")
		}
	case ObjectBoolean:
		if err := json.Unmarshal(wire.Value, &rebuilt.Boolean); err != nil {
			return Wrap(err, CodeInvalidArgument, op, "malformed boolean object")
		}
	case ObjectTimestamp:
		var text string
		if err := json.Unmarshal(wire.Value, &text); err != nil {
			return Wrap(err, CodeInvalidArgument, op, "malformed timestamp object")
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return Wrap(err, CodeInvalidArgument, op, "malformed timestamp object")
		}
		rebuilt.Timestamp = parsed.UTC()
	case ObjectDate:
		var text string
		if err := json.Unmarshal(wire.Value, &text); err != nil {
			return Wrap(err, CodeInvalidArgument, op, "malformed date object")
		}
		parsed, err := time.Parse(DateLayout, text)
		if err != nil {
			return Wrap(err, CodeInvalidArgument, op, "malformed date object")
		}
		rebuilt.Date = parsed.UTC()
	case ObjectDuration:
		var text string
		if err := json.Unmarshal(wire.Value, &text); err != nil {
			return Wrap(err, CodeInvalidArgument, op, "malformed duration object")
		}
		parsed, err := time.ParseDuration(text)
		if err != nil {
			return Wrap(err, CodeInvalidArgument, op, "malformed duration object")
		}
		rebuilt.Duration = parsed
	case ObjectGeo:
		if err := json.Unmarshal(wire.Value, &rebuilt.Geo); err != nil {
			return Wrap(err, CodeInvalidArgument, op, "malformed geo object")
		}
	case ObjectJSON:
		rebuilt.JSON = append(json.RawMessage(nil), wire.Value...)
	}

	*o = rebuilt
	return nil
}

// ChunkProvenance is a chunk resolved to the ingestion behind it.
//
// Enough to cite a quoted excerpt: which passage, from which episode, from which event,
// from which source, and how far that source is trusted.
type ChunkProvenance struct {
	Chunk Chunk

	EpisodeSequence int64
	EventTime       *time.Time
	ObservedAt      time.Time
	EpisodeLocator  Locator

	SourceEventID   SourceEventID
	SourceEventTime *time.Time
	RecordedAt      time.Time

	SourceID   SourceID
	SourceName string
	SourceKind SourceKind
	TrustLevel TrustLevel
}
