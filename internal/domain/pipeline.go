package domain

import (
	"encoding/json"
	"time"
)

// PipelineRun tracks processing of one source event under one pipeline version. The
// unique key (workspace, source_event, pipeline_version) makes a replay reuse the
// existing run instead of forking a second one (AGENTS.md section 10.4).
type PipelineRun struct {
	ID              PipelineRunID
	WorkspaceID     WorkspaceID
	GraphSpaceID    GraphSpaceID
	SourceEventID   SourceEventID
	PipelineVersion int
	Status          RunStatus
	Attempts        int
	LastError       string
	ErrorClass      ErrorClass
	StartedAt       *time.Time
	FinishedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// StageRun is the durable execution record for a single pipeline stage. Its key,
// (pipeline_run, stage_name, stage_version), is the idempotency boundary: a
// succeeded stage is not re-executed unless forced.
type StageRun struct {
	ID            StageRunID
	PipelineRunID PipelineRunID
	WorkspaceID   WorkspaceID
	SourceEventID SourceEventID
	StageName     string
	StageVersion  int
	Status        RunStatus
	Attempts      int
	// OutputRef is a small summary of what the stage produced (counts, identifiers).
	// The canonical output itself lives in ledger tables, so stages remain
	// re-derivable and the ledger stays the source of truth.
	OutputRef  json.RawMessage
	LastError  string
	ErrorClass ErrorClass
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// StageKey is the durable stage execution key from AGENTS.md section 10.4.
type StageKey struct {
	WorkspaceID     WorkspaceID
	SourceEventID   SourceEventID
	PipelineVersion int
	StageName       string
	StageVersion    int
}

// String renders the stage key for logs and metrics.
func (k StageKey) String() string {
	return string(k.WorkspaceID) + "/" + string(k.SourceEventID) + "/p" +
		itoa(k.PipelineVersion) + "/" + k.StageName + "/v" + itoa(k.StageVersion)
}
