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
