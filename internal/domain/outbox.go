package domain

import (
	"encoding/json"
	"time"
)

// Topic and event type names for the work fabric. Durable event schemas are
// versioned so a consumer can reject payloads it does not understand
// (AGENTS.md section 34).
const (
	TopicIngestPipeline = "ingest.pipeline"

	EventTypeSourceEventAccepted = "source_event.accepted"

	// OutboxSchemaVersion is the version of the payload shapes in this package.
	OutboxSchemaVersion = 1
)

// OutboxEvent is a durable work item. It is written in the same transaction as the
// canonical mutation that requires it, which is what makes "commit then publish and
// hope" impossible (AGENTS.md section 28.1).
type OutboxEvent struct {
	ID            OutboxEventID
	WorkspaceID   WorkspaceID
	GraphSpaceID  GraphSpaceID
	SourceEventID SourceEventID
	Topic         string
	EventType     string
	SchemaVersion int
	Payload       json.RawMessage
	// DedupeKey makes publication idempotent: re-running the producer for the same
	// logical work never enqueues a second item.
	DedupeKey string

	Status      OutboxStatus
	Attempts    int
	MaxAttempts int
	VisibleAt   time.Time
	// ClaimedBy and ClaimExpiresAt implement leasing. A worker that dies leaves an
	// expired lease, which the reaper returns to pending rather than losing.
	ClaimedBy      string
	ClaimExpiresAt *time.Time
	LastError      string
	ErrorClass     ErrorClass
	// TraceParent carries W3C trace context across the asynchronous boundary so one
	// trace spans ingest and processing (AGENTS.md section 30.1).
	TraceParent string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

func (o OutboxEvent) Validate() error {
	const op = "domain.OutboxEvent.Validate"
	if IsZero(o.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id is required")
	}
	if o.Topic == "" || o.EventType == "" {
		return Errorf(CodeInvalidArgument, op, "topic and event_type are required")
	}
	if o.DedupeKey == "" {
		return Errorf(CodeInvalidArgument, op, "dedupe_key is required")
	}
	if o.SchemaVersion <= 0 {
		return Errorf(CodeInvalidArgument, op, "schema_version must be positive")
	}
	if o.MaxAttempts <= 0 {
		return Errorf(CodeInvalidArgument, op, "max_attempts must be positive")
	}
	if _, err := ParseOutboxStatus(string(o.Status)); err != nil {
		return err
	}
	return nil
}

// SourceEventAcceptedPayload is the versioned payload for the only work item this
// phase produces. It carries identifiers only: the consumer re-reads canonical
// state from the ledger rather than trusting a possibly stale copy in the payload.
type SourceEventAcceptedPayload struct {
	SourceEventID   SourceEventID `json:"source_event_id"`
	WorkspaceID     WorkspaceID   `json:"workspace_id"`
	GraphSpaceID    GraphSpaceID  `json:"graph_space_id"`
	PipelineVersion int           `json:"pipeline_version"`
}

// PipelineDedupeKey is the outbox dedupe key for processing one source event under
// one pipeline version.
func PipelineDedupeKey(eventID SourceEventID, pipelineVersion int) string {
	return DeriveKey("outbox.pipeline", string(eventID), itoa(pipelineVersion))
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
