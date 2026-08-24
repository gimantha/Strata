package domain

// parseEnum resolves a durable string into its enum value, returning an error for
// anything unrecognized. Unknown enum values are never silently ignored during
// durable deserialization (AGENTS.md section 34).
func parseEnum[T ~string](name, s string, allowed []T) (T, error) {
	for _, a := range allowed {
		if string(a) == s {
			return a, nil
		}
	}
	var zero T
	return zero, Errorf(CodeInvalidArgument, "domain.parseEnum", "unknown %s %q", name, s)
}

// SourceKind identifies the shape of an upstream origin (AGENTS.md section 6.2).
type SourceKind string

const (
	SourceKindChat     SourceKind = "chat"
	SourceKindFile     SourceKind = "file"
	SourceKindDocument SourceKind = "document"
	SourceKindDatabase SourceKind = "database"
	SourceKindWebhook  SourceKind = "webhook"
	SourceKindTool     SourceKind = "tool"
	SourceKindCode     SourceKind = "code"
	SourceKindAPI      SourceKind = "api"
	SourceKindStream   SourceKind = "stream"
)

var sourceKinds = []SourceKind{
	SourceKindChat, SourceKindFile, SourceKindDocument, SourceKindDatabase,
	SourceKindWebhook, SourceKindTool, SourceKindCode, SourceKindAPI, SourceKindStream,
}

func ParseSourceKind(s string) (SourceKind, error) { return parseEnum("source kind", s, sourceKinds) }

// SourceOperation is the upstream intent behind a source event
// (AGENTS.md sections 6.3, 11.2). A delete is a source tombstone, not an erasure.
type SourceOperation string

const (
	SourceOpUpsert     SourceOperation = "upsert"
	SourceOpDelete     SourceOperation = "delete"
	SourceOpAppend     SourceOperation = "append"
	SourceOpSnapshot   SourceOperation = "snapshot"
	SourceOpCorrection SourceOperation = "correction"
)

var sourceOperations = []SourceOperation{
	SourceOpUpsert, SourceOpDelete, SourceOpAppend, SourceOpSnapshot, SourceOpCorrection,
}

func ParseSourceOperation(s string) (SourceOperation, error) {
	return parseEnum("source operation", s, sourceOperations)
}

// TrustLevel is how much authority a source carries. It feeds conflict resolution
// and is one half of the poisoned-source defense (AGENTS.md section 24).
type TrustLevel string

const (
	TrustUntrusted     TrustLevel = "untrusted"
	TrustLow           TrustLevel = "low"
	TrustStandard      TrustLevel = "standard"
	TrustHigh          TrustLevel = "high"
	TrustAuthoritative TrustLevel = "authoritative"
)

var trustLevels = []TrustLevel{TrustUntrusted, TrustLow, TrustStandard, TrustHigh, TrustAuthoritative}

func ParseTrustLevel(s string) (TrustLevel, error) { return parseEnum("trust level", s, trustLevels) }

// Classification is the sensitivity label that propagates from source to episode
// to chunk to assertion (AGENTS.md section 22.3).
type Classification string

const (
	ClassificationPublic       Classification = "public"
	ClassificationInternal     Classification = "internal"
	ClassificationConfidential Classification = "confidential"
	ClassificationRestricted   Classification = "restricted"
	ClassificationSecret       Classification = "secret"
)

var classifications = []Classification{
	ClassificationPublic, ClassificationInternal, ClassificationConfidential,
	ClassificationRestricted, ClassificationSecret,
}

func ParseClassification(s string) (Classification, error) {
	return parseEnum("classification", s, classifications)
}

// rank orders classifications from least to most restrictive.
func (c Classification) rank() int {
	for i, known := range classifications {
		if known == c {
			return i + 1
		}
	}
	return 0
}

// MostRestrictive returns whichever classification protects more.
//
// Classification propagates downstream and may only be raised implicitly; lowering it
// requires an explicit policy decision, which this phase has no mechanism for. So when
// two labels meet, the stricter one wins (AGENTS.md section 22.3).
// LeastPermissive returns whichever ceiling admits less.
//
// The counterpart of MostRestrictive, which raises a label to protect data. This lowers a
// clearance to protect against over-granting: two limits on what someone may see combine to
// the tighter one, never the looser. An empty limit means "unset" and yields to the other.
func LeastPermissive(a, b Classification) Classification {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case b.rank() < a.rank():
		return b
	default:
		return a
	}
}

func MostRestrictive(a, b Classification) Classification {
	if b.rank() > a.rank() {
		return b
	}
	if a.rank() == 0 {
		return ClassificationInternal
	}
	return a
}

// MemoryKind classifies knowledge without splitting it across incompatible stores
// (AGENTS.md section 9). Declared now; assertions arrive in phase 2.
type MemoryKind string

const (
	MemoryEpisodic   MemoryKind = "episodic"
	MemorySemantic   MemoryKind = "semantic"
	MemoryProcedural MemoryKind = "procedural"
	MemoryPreference MemoryKind = "preference"
	MemoryWorking    MemoryKind = "working"
	MemoryDerived    MemoryKind = "derived"
)

var memoryKinds = []MemoryKind{
	MemoryEpisodic, MemorySemantic, MemoryProcedural, MemoryPreference, MemoryWorking, MemoryDerived,
}

func ParseMemoryKind(s string) (MemoryKind, error) { return parseEnum("memory kind", s, memoryKinds) }

// SourceEventStatus tracks how far a source event has progressed. It describes
// processing state only; it never encodes whether the knowledge is true.
type SourceEventStatus string

const (
	SourceEventAccepted    SourceEventStatus = "accepted"
	SourceEventProcessing  SourceEventStatus = "processing"
	SourceEventProcessed   SourceEventStatus = "processed"
	SourceEventFailed      SourceEventStatus = "failed"
	SourceEventQuarantined SourceEventStatus = "quarantined"
)

var sourceEventStatuses = []SourceEventStatus{
	SourceEventAccepted, SourceEventProcessing, SourceEventProcessed,
	SourceEventFailed, SourceEventQuarantined,
}

func ParseSourceEventStatus(s string) (SourceEventStatus, error) {
	return parseEnum("source event status", s, sourceEventStatuses)
}

// RunStatus is the lifecycle of a pipeline run or a single stage run
// (AGENTS.md section 10.3).
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunDead      RunStatus = "dead"
	RunCancelled RunStatus = "cancelled"
)

var runStatuses = []RunStatus{RunPending, RunRunning, RunSucceeded, RunFailed, RunDead, RunCancelled}

func ParseRunStatus(s string) (RunStatus, error) { return parseEnum("run status", s, runStatuses) }

// Terminal reports whether a run status will never change without operator action.
func (s RunStatus) Terminal() bool {
	switch s {
	case RunSucceeded, RunDead, RunCancelled:
		return true
	default:
		return false
	}
}

// PrincipalKind distinguishes humans from machines, which matters for policy and
// for audit records (AGENTS.md sections 22, 22.6).
type PrincipalKind string

const (
	PrincipalUser    PrincipalKind = "user"
	PrincipalAgent   PrincipalKind = "agent"
	PrincipalService PrincipalKind = "service"
)

var principalKinds = []PrincipalKind{PrincipalUser, PrincipalAgent, PrincipalService}

func ParsePrincipalKind(s string) (PrincipalKind, error) {
	return parseEnum("principal kind", s, principalKinds)
}

// Role is a coarse workspace role. Attribute-based policy arrives in phase 11;
// these roles are the RBAC half (AGENTS.md section 22.2).
type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleWriter Role = "writer"
	RoleReader Role = "reader"
)

var roles = []Role{RoleOwner, RoleAdmin, RoleWriter, RoleReader}

func ParseRole(s string) (Role, error) { return parseEnum("role", s, roles) }

// rank orders roles from least to most privileged.
func (r Role) rank() int {
	switch r {
	case RoleReader:
		return 1
	case RoleWriter:
		return 2
	case RoleAdmin:
		return 3
	case RoleOwner:
		return 4
	default:
		return 0
	}
}

// AtLeast reports whether this role includes the privileges of want.
func (r Role) AtLeast(want Role) bool { return r.rank() >= want.rank() && r.rank() > 0 }

// OutboxStatus is the claim lifecycle of a durable work item (AGENTS.md section 28).
type OutboxStatus string

const (
	OutboxPending   OutboxStatus = "pending"
	OutboxClaimed   OutboxStatus = "claimed"
	OutboxSucceeded OutboxStatus = "succeeded"
	OutboxDead      OutboxStatus = "dead"
	OutboxCancelled OutboxStatus = "cancelled"
)

var outboxStatuses = []OutboxStatus{
	OutboxPending, OutboxClaimed, OutboxSucceeded, OutboxDead, OutboxCancelled,
}

func ParseOutboxStatus(s string) (OutboxStatus, error) {
	return parseEnum("outbox status", s, outboxStatuses)
}

// AssertionStatus is the lifecycle of a claim. Status changes are knowledge-time
// events: an assertion's content is never edited (AGENTS.md sections 2.1, 14.3).
type AssertionStatus string

const (
	// AssertionProposed is a candidate awaiting validation.
	AssertionProposed AssertionStatus = "proposed"
	// AssertionActive is committed, current belief.
	AssertionActive AssertionStatus = "active"
	// AssertionSuperseded was replaced by later knowledge. It remains queryable as of
	// any knowledge time before it was superseded.
	AssertionSuperseded AssertionStatus = "superseded"
	// AssertionRetracted was withdrawn without a replacement.
	AssertionRetracted AssertionStatus = "retracted"
	// AssertionQuarantined failed validation or policy and is held for review.
	AssertionQuarantined AssertionStatus = "quarantined"
	// AssertionDisputed conflicts with another claim that cannot yet be resolved.
	AssertionDisputed AssertionStatus = "disputed"
)

var assertionStatuses = []AssertionStatus{
	AssertionProposed, AssertionActive, AssertionSuperseded,
	AssertionRetracted, AssertionQuarantined, AssertionDisputed,
}

func ParseAssertionStatus(s string) (AssertionStatus, error) {
	return parseEnum("assertion status", s, assertionStatuses)
}

// Believable reports whether a status represents knowledge the system currently holds.
// Disputed counts: a contested claim is still believed, just not settled.
func (s AssertionStatus) Believable() bool {
	return s == AssertionActive || s == AssertionDisputed
}

// ProvenanceMode records how a claim came to exist. Inference is never disguised as
// direct observation (AGENTS.md section 6.11).
type ProvenanceMode string

const (
	ProvenanceExtracted    ProvenanceMode = "extracted"
	ProvenanceImported     ProvenanceMode = "imported"
	ProvenanceInferred     ProvenanceMode = "inferred"
	ProvenanceDerived      ProvenanceMode = "derived"
	ProvenanceUserAsserted ProvenanceMode = "user_asserted"
)

var provenanceModes = []ProvenanceMode{
	ProvenanceExtracted, ProvenanceImported, ProvenanceInferred,
	ProvenanceDerived, ProvenanceUserAsserted,
}

func ParseProvenanceMode(s string) (ProvenanceMode, error) {
	return parseEnum("provenance mode", s, provenanceModes)
}

// RequiresDerivation reports whether this mode must name what produced the claim.
// A derived or inferred assertion without a derivation record would be an unexplainable
// fact, which the architecture does not allow.
func (p ProvenanceMode) RequiresDerivation() bool {
	return p == ProvenanceInferred || p == ProvenanceDerived
}

// ObjectKind is the type of an assertion's object. Values keep their types rather than
// being stringified (AGENTS.md section 6.9).
type ObjectKind string

const (
	ObjectEntity    ObjectKind = "entity"
	ObjectString    ObjectKind = "string"
	ObjectInteger   ObjectKind = "integer"
	ObjectDecimal   ObjectKind = "decimal"
	ObjectBoolean   ObjectKind = "boolean"
	ObjectTimestamp ObjectKind = "timestamp"
	ObjectDate      ObjectKind = "date"
	ObjectDuration  ObjectKind = "duration"
	ObjectGeo       ObjectKind = "geo"
	ObjectJSON      ObjectKind = "json"
	ObjectURI       ObjectKind = "uri"
	ObjectSymbol    ObjectKind = "symbol"
)

var objectKinds = []ObjectKind{
	ObjectEntity, ObjectString, ObjectInteger, ObjectDecimal, ObjectBoolean,
	ObjectTimestamp, ObjectDate, ObjectDuration, ObjectGeo, ObjectJSON, ObjectURI, ObjectSymbol,
}

func ParseObjectKind(s string) (ObjectKind, error) { return parseEnum("object kind", s, objectKinds) }

// TemporalPolicy describes how a predicate's values behave over time
// (AGENTS.md section 8).
type TemporalPolicy string

const (
	// TemporalPolicyStateful values hold over an interval and are replaced by later
	// values, such as an employer or an address.
	TemporalPolicyStateful TemporalPolicy = "stateful"
	// TemporalPolicyEvent values describe a moment and never expire, such as a signing.
	TemporalPolicyEvent TemporalPolicy = "event"
	// TemporalPolicyImmutable values never change once known, such as a birth date.
	TemporalPolicyImmutable TemporalPolicy = "immutable"
)

var temporalPolicies = []TemporalPolicy{
	TemporalPolicyStateful, TemporalPolicyEvent, TemporalPolicyImmutable,
}

func ParseTemporalPolicy(s string) (TemporalPolicy, error) {
	return parseEnum("temporal policy", s, temporalPolicies)
}

// ConflictPolicy decides what happens when two claims about the same subject and
// predicate cannot both hold (AGENTS.md sections 8, 14).
type ConflictPolicy string

const (
	// ConflictPolicyCoexist allows multiple simultaneous values, such as LIKES.
	ConflictPolicyCoexist ConflictPolicy = "coexist"
	// ConflictPolicyLatestWins supersedes the earlier claim when a later one arrives.
	ConflictPolicyLatestWins ConflictPolicy = "latest_wins"
	// ConflictPolicyHighestAuthority prefers the more trusted source.
	ConflictPolicyHighestAuthority ConflictPolicy = "highest_authority"
	// ConflictPolicyManual keeps both claims in a conflict set for review. Nothing is
	// deleted; the disagreement is made visible instead.
	ConflictPolicyManual ConflictPolicy = "manual"
)

var conflictPolicies = []ConflictPolicy{
	ConflictPolicyCoexist, ConflictPolicyLatestWins,
	ConflictPolicyHighestAuthority, ConflictPolicyManual,
}

func ParseConflictPolicy(s string) (ConflictPolicy, error) {
	return parseEnum("conflict policy", s, conflictPolicies)
}

// PredicateStatus distinguishes a registry entry created on the fly by extraction from
// one an operator has blessed (AGENTS.md section 8, open versus ontology-guided mode).
type PredicateStatus string

const (
	PredicateCandidate  PredicateStatus = "candidate"
	PredicateApproved   PredicateStatus = "approved"
	PredicateDeprecated PredicateStatus = "deprecated"
)

var predicateStatuses = []PredicateStatus{PredicateCandidate, PredicateApproved, PredicateDeprecated}

func ParsePredicateStatus(s string) (PredicateStatus, error) {
	return parseEnum("predicate status", s, predicateStatuses)
}

// ConflictResolution records how a conflict set ended, if it has ended.
type ConflictResolution string

const (
	ConflictOpen             ConflictResolution = "open"
	ConflictResolvedBySource ConflictResolution = "resolved_by_source"
	ConflictResolvedByHuman  ConflictResolution = "resolved_by_human"
	ConflictResolvedByPolicy ConflictResolution = "resolved_by_policy"
)

var conflictResolutions = []ConflictResolution{
	ConflictOpen, ConflictResolvedBySource, ConflictResolvedByHuman, ConflictResolvedByPolicy,
}

func ParseConflictResolution(s string) (ConflictResolution, error) {
	return parseEnum("conflict resolution", s, conflictResolutions)
}
