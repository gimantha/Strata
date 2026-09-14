package domain

import (
	"strings"
	"time"
)

// KnowledgePurgeStatus records which durable boundary a destructive purge reached.
type KnowledgePurgeStatus string

const (
	PurgePlanned          KnowledgePurgeStatus = "planned"
	PurgeCanonicalDeleted KnowledgePurgeStatus = "canonical_deleted"
	PurgeCompleted        KnowledgePurgeStatus = "completed"
	PurgeFailed           KnowledgePurgeStatus = "failed"
)

// KnowledgePurgeCounts is intentionally aggregate-only. The durable audit proves what was
// removed without copying sensitive values into a table designed to survive removal.
type KnowledgePurgeCounts struct {
	Events      int64 `json:"events"`
	Assertions  int64 `json:"assertions"`
	Evidence    int64 `json:"evidence"`
	Episodes    int64 `json:"episodes"`
	Chunks      int64 `json:"chunks"`
	Entities    int64 `json:"entities"`
	Artifacts   int64 `json:"artifacts"`
	ModelRuns   int64 `json:"model_runs"`
	Resolutions int64 `json:"resolution_decisions"`
	Blobs       int64 `json:"blobs"`
}

// KnowledgePurgeRequest selects old, inactive knowledge inside one graph space.
type KnowledgePurgeRequest struct {
	ID          KnowledgePurgeID
	Scope       Scope
	Before      time.Time
	RequestedAt time.Time
	Principal   PrincipalRef
	Reason      string
	DryRun      bool
}

func (r KnowledgePurgeRequest) Validate() error {
	const op = "domain.KnowledgePurgeRequest.Validate"
	if IsZero(r.Scope.WorkspaceID) || IsZero(r.Scope.GraphSpaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id and graph_space_id are required")
	}
	if r.Before.IsZero() || r.RequestedAt.IsZero() {
		return Errorf(CodeInvalidArgument, op, "before and requested_at are required")
	}
	if r.Before.After(r.RequestedAt) {
		return Errorf(CodeInvalidArgument, op, "before must not be in the future")
	}
	if IsZero(r.Principal.ID) {
		return Errorf(CodeInvalidArgument, op, "principal is required")
	}
	if strings.TrimSpace(r.Reason) == "" {
		return Errorf(CodeInvalidArgument, op, "reason is required")
	}
	return nil
}

// KnowledgePurgeRun is the durable, content-free audit of a purge attempt.
type KnowledgePurgeRun struct {
	ID          KnowledgePurgeID     `json:"id"`
	Scope       Scope                `json:"scope"`
	Before      time.Time            `json:"before"`
	DryRun      bool                 `json:"dry_run"`
	Status      KnowledgePurgeStatus `json:"status"`
	Reason      string               `json:"reason"`
	RequestedBy PrincipalRef         `json:"requested_by"`
	Eligible    KnowledgePurgeCounts `json:"eligible"`
	Deleted     KnowledgePurgeCounts `json:"deleted"`
	LastError   string               `json:"last_error,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	CompletedAt *time.Time           `json:"completed_at,omitempty"`
}

// KnowledgeHold excludes a graph space or one source event from old-knowledge purges.
type KnowledgeHold struct {
	ID            KnowledgeHoldID
	Scope         Scope
	SourceEventID SourceEventID
	Reason        string
	CreatedBy     PrincipalRef
	ExpiresAt     *time.Time
	CreatedAt     time.Time
}

func (h KnowledgeHold) Validate() error {
	const op = "domain.KnowledgeHold.Validate"
	if IsZero(h.Scope.WorkspaceID) || IsZero(h.Scope.GraphSpaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id and graph_space_id are required")
	}
	if IsZero(h.CreatedBy.ID) || strings.TrimSpace(h.Reason) == "" {
		return Errorf(CodeInvalidArgument, op, "created_by and reason are required")
	}
	return nil
}
