package domain

import "time"

// Source is an upstream system or origin (AGENTS.md section 6.2).
type Source struct {
	ID          SourceID
	WorkspaceID WorkspaceID
	Kind        SourceKind
	Name        string
	URI         string
	TrustLevel  TrustLevel
	// Classification is the default sensitivity for material from this source. It
	// propagates downstream unless policy explicitly downgrades it.
	Classification Classification
	Metadata       map[string]any
	CreatedAt      time.Time
}

func (s Source) Validate() error {
	const op = "domain.Source.Validate"
	if IsZero(s.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id is required")
	}
	if s.Name == "" {
		return Errorf(CodeInvalidArgument, op, "name is required")
	}
	if _, err := ParseSourceKind(string(s.Kind)); err != nil {
		return err
	}
	if _, err := ParseTrustLevel(string(s.TrustLevel)); err != nil {
		return err
	}
	if _, err := ParseClassification(string(s.Classification)); err != nil {
		return err
	}
	return nil
}

// Artifact is original or normalized source material. Large bytes live in blob
// storage; only metadata and the content address live in the ledger
// (AGENTS.md section 6.4).
type Artifact struct {
	ID           ArtifactID
	WorkspaceID  WorkspaceID
	GraphSpaceID GraphSpaceID
	ContentHash  string
	MediaType    string
	SizeBytes    int64
	// BlobKey addresses the bytes in the configured blob store. Storage names the
	// backend so a later S3 adapter can coexist with filesystem-era rows.
	BlobKey   string
	Storage   string
	Metadata  map[string]any
	CreatedAt time.Time
}

func (a Artifact) Validate() error {
	const op = "domain.Artifact.Validate"
	if IsZero(a.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id is required")
	}
	if a.ContentHash == "" {
		return Errorf(CodeInvalidArgument, op, "content_hash is required")
	}
	if a.BlobKey == "" {
		return Errorf(CodeInvalidArgument, op, "blob_key is required")
	}
	if a.SizeBytes < 0 {
		return Errorf(CodeInvalidArgument, op, "size_bytes must not be negative")
	}
	return nil
}

// SourceEvent is the immutable record of one mutation entering the system. Every
// ingestion path produces these, batch and streaming alike (AGENTS.md sections 6.3,
// 10.1). Source events are never updated in place except for processing status.
type SourceEvent struct {
	ID             SourceEventID
	WorkspaceID    WorkspaceID
	GraphSpaceID   GraphSpaceID
	CollectionID   CollectionID
	SourceID       SourceID
	ExternalID     string
	EventType      string
	Operation      SourceOperation
	ContentHash    string
	IdempotencyKey string

	// World and source clocks as reported upstream (AGENTS.md sections 7.1, 11.1).
	EventTime        *time.Time
	SourceTime       *time.Time
	SourceCommitTime *time.Time
	SourceSequence   string
	SourceVersion    string

	// Knowledge clocks, set by this system.
	ObservedAt time.Time
	RecordedAt time.Time

	RawArtifactID  ArtifactID
	MediaType      string
	Status         SourceEventStatus
	Classification Classification
	CreatedBy      PrincipalRef
	Metadata       map[string]any
}

func (e SourceEvent) Validate() error {
	const op = "domain.SourceEvent.Validate"
	if IsZero(e.WorkspaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id is required")
	}
	if IsZero(e.GraphSpaceID) {
		return Errorf(CodeInvalidArgument, op, "graph_space_id is required")
	}
	if IsZero(e.SourceID) {
		return Errorf(CodeInvalidArgument, op, "source_id is required")
	}
	if e.ContentHash == "" {
		return Errorf(CodeInvalidArgument, op, "content_hash is required")
	}
	if e.IdempotencyKey == "" {
		return Errorf(CodeInvalidArgument, op, "idempotency_key is required")
	}
	if e.ObservedAt.IsZero() || e.RecordedAt.IsZero() {
		return Errorf(CodeInvalidArgument, op, "observed_at and recorded_at are required")
	}
	if _, err := ParseSourceOperation(string(e.Operation)); err != nil {
		return err
	}
	if _, err := ParseSourceEventStatus(string(e.Status)); err != nil {
		return err
	}
	return nil
}

// DeriveIdempotencyKey builds the stable replay key for a source event when the
// caller supplied none. It combines upstream identity with the content address, so
// re-sending identical content is a no-op while changed content for the same
// upstream version is a detectable conflict (AGENTS.md sections 6.3, 12).
func DeriveIdempotencyKey(source SourceID, externalID, sourceVersion, contentHash string) string {
	return DeriveKey("source_event", string(source), externalID, sourceVersion, contentHash)
}

// SourceEventAppend is one atomic ingestion unit: the archived artifact's metadata,
// the canonical source event, and the work items its processing requires. The ledger
// commits all of it in a single transaction (AGENTS.md sections 10.2, 28.1).
type SourceEventAppend struct {
	Artifact Artifact
	Event    SourceEvent
	Outbox   []OutboxEvent
	// PipelineVersion seeds a pending pipeline run so processing status is queryable
	// as soon as ingestion is acknowledged. Zero skips that.
	PipelineVersion int
	Actor           PrincipalID
}

// SourceEventAppendResult reports what the ledger did. Duplicate is true when the
// event already existed under the same idempotency key with identical content.
type SourceEventAppendResult struct {
	Event     SourceEvent
	Artifact  Artifact
	Duplicate bool
}

// Locator preserves positional provenance for an episode or chunk
// (AGENTS.md section 6.6). Fields are typed rather than a bare map so provenance
// stays reviewable; Extra exists for genuinely source-specific detail.
type Locator struct {
	Page         *int     `json:"page,omitempty"`
	Section      string   `json:"section,omitempty"`
	HeadingPath  []string `json:"heading_path,omitempty"`
	MessageIndex *int     `json:"message_index,omitempty"`
	Role         string   `json:"role,omitempty"`
	JSONPointer  string   `json:"json_pointer,omitempty"`
	RowKey       string   `json:"row_key,omitempty"`
	CodePath     string   `json:"code_path,omitempty"`
	Symbol       string   `json:"symbol,omitempty"`
	LineStart    *int     `json:"line_start,omitempty"`
	LineEnd      *int     `json:"line_end,omitempty"`
	// ArtifactCharStart/End locate this unit inside the normalized artifact text,
	// which is what makes an exact quote reproducible from the archived source.
	ArtifactCharStart int            `json:"artifact_char_start"`
	ArtifactCharEnd   int            `json:"artifact_char_end"`
	Extra             map[string]any `json:"extra,omitempty"`
}

// Episode is the smallest semantically meaningful ingestion unit: a conversation
// turn, a CDC row change, a document section, a tool result (AGENTS.md section 6.5).
type Episode struct {
	ID             EpisodeID
	WorkspaceID    WorkspaceID
	GraphSpaceID   GraphSpaceID
	SourceEventID  SourceEventID
	ArtifactID     ArtifactID
	Sequence       int64
	Content        string
	ContentType    string
	EventTime      *time.Time
	ObservedAt     time.Time
	RecordedAt     time.Time
	Locator        Locator
	Classification Classification
	Metadata       map[string]any
}

func (e Episode) Validate() error {
	const op = "domain.Episode.Validate"
	if IsZero(e.WorkspaceID) || IsZero(e.GraphSpaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id and graph_space_id are required")
	}
	if IsZero(e.SourceEventID) {
		return Errorf(CodeInvalidArgument, op, "source_event_id is required")
	}
	if e.Sequence < 0 {
		return Errorf(CodeInvalidArgument, op, "sequence must not be negative")
	}
	return nil
}

// Chunk is a retrieval and extraction unit, not a fact (AGENTS.md section 6.6).
// Offsets are relative to the parent episode's content; Locator carries the
// artifact-level position.
type Chunk struct {
	ID             ChunkID
	WorkspaceID    WorkspaceID
	GraphSpaceID   GraphSpaceID
	SourceEventID  SourceEventID
	EpisodeID      EpisodeID
	ArtifactID     ArtifactID
	Sequence       int64
	Content        string
	ContentType    string
	TokenCount     int
	CharStart      int
	CharEnd        int
	ByteStart      int
	ByteEnd        int
	Locator        Locator
	Classification Classification
	Metadata       map[string]any
}

func (c Chunk) Validate() error {
	const op = "domain.Chunk.Validate"
	if IsZero(c.WorkspaceID) || IsZero(c.GraphSpaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id and graph_space_id are required")
	}
	if IsZero(c.EpisodeID) {
		return Errorf(CodeInvalidArgument, op, "episode_id is required")
	}
	if c.CharEnd < c.CharStart || c.ByteEnd < c.ByteStart {
		return Errorf(CodeInvalidArgument, op, "chunk offsets are inverted")
	}
	return nil
}
