// Package ingest is the single entry point through which all knowledge enters.
//
// Every connector, upload, batch import, and CDC stream produces the same thing: a
// SourceEvent. There is no separate batch pipeline and no separate streaming pipeline
// (AGENTS.md sections 2.4, 10.1). Batch ingestion is many events submitted
// efficiently, along the identical code path.
package ingest

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/observability"
	"github.com/gimantha/strata/internal/store/blob"
)

// Ledger is the canonical persistence the gateway needs, declared by its consumer in
// domain terms so this package carries no driver dependency (AGENTS.md sections 2.11, 5).
type Ledger interface {
	AppendSourceEvent(ctx context.Context, req domain.SourceEventAppend) (domain.SourceEventAppendResult, error)
	GetSource(ctx context.Context, ws domain.WorkspaceID, id domain.SourceID) (domain.Source, error)
	GetSourceByName(ctx context.Context, ws domain.WorkspaceID, name string) (domain.Source, error)
	GetCollection(ctx context.Context, ws domain.WorkspaceID, id domain.CollectionID) (domain.Collection, error)
}

// BlobStore archives raw payloads.
type BlobStore interface {
	Put(ctx context.Context, key string, data []byte) (blob.Info, error)
	Name() string
}

// Options configures the gateway.
type Options struct {
	PipelineVersion int
	MaxPayloadBytes int64
	MaxAttempts     int
	// Now supplies knowledge time, injectable for deterministic tests.
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now == nil {
		return time.Now().UTC()
	}
	return o.Now().UTC()
}

// Gateway accepts source events.
type Gateway struct {
	ledger  Ledger
	blobs   BlobStore
	opts    Options
	logger  *slog.Logger
	metrics *observability.Metrics
	tracer  trace.Tracer

	notify Notifier
}

// Notifier is told about work that has been committed, so a fleet can hear about it
// before its next poll.
//
// Deliberately returns nothing. The work item is already durable in the ledger by the
// time this runs; a notifier that could fail an ingest would make an optimization into
// a dependency (AGENTS.md section 28.1).
type Notifier func(ctx context.Context, events ...domain.OutboxEvent)

// SetNotifier attaches the push path. Wired after construction because the bus and the
// gateway are built in the same pass and neither should own the other.
func (g *Gateway) SetNotifier(notify Notifier) { g.notify = notify }

// New builds a gateway.
func New(ledger Ledger, blobs BlobStore, opts Options, logger *slog.Logger, metrics *observability.Metrics, tracer trace.Tracer) *Gateway {
	if opts.PipelineVersion <= 0 {
		opts.PipelineVersion = 1
	}
	if opts.MaxPayloadBytes <= 0 {
		opts.MaxPayloadBytes = 8 << 20
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 8
	}
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer("ingest")
	}
	return &Gateway{ledger: ledger, blobs: blobs, opts: opts, logger: logger, metrics: metrics, tracer: tracer}
}

// Request is one submission. Scope is already resolved from authenticated identity by
// the transport layer; the gateway never derives tenancy from caller-supplied fields
// (AGENTS.md section 22.1).
type Request struct {
	Scope     domain.Scope
	Principal domain.PrincipalRef

	// Either SourceID or SourceName identifies the registered source.
	SourceID   domain.SourceID
	SourceName string

	ExternalID     string
	EventType      string
	Operation      domain.SourceOperation
	MediaType      string
	Payload        []byte
	IdempotencyKey string
	Classification domain.Classification

	EventTime        *time.Time
	SourceTime       *time.Time
	SourceCommitTime *time.Time
	SourceSequence   string
	SourceVersion    string

	// DirectEpisode marks a payload the caller has already segmented.
	DirectEpisode bool
	Metadata      map[string]any
}

// Receipt is what a caller gets back. Acknowledgement means the event and its work are
// durably committed, not that extraction has finished (AGENTS.md section 10.2).
type Receipt struct {
	SourceEventID  domain.SourceEventID
	ArtifactID     domain.ArtifactID
	IdempotencyKey string
	ContentHash    string
	Status         domain.SourceEventStatus
	Duplicate      bool
}

// Accept durably records one source event.
//
// The order is deliberate: authorization has already happened, then the raw payload is
// archived, then the canonical event and its work item are committed together. The
// caller is not made to wait for extraction.
func (g *Gateway) Accept(ctx context.Context, req Request) (Receipt, error) {
	const op = "ingest.Accept"

	ctx, span := g.tracer.Start(ctx, "ingest.Accept", trace.WithAttributes(
		attribute.String("strata.workspace_id", string(req.Scope.WorkspaceID)),
		attribute.String("strata.graph_space_id", string(req.Scope.GraphSpaceID)),
	))
	defer span.End()

	if domain.IsZero(req.Scope.WorkspaceID) || domain.IsZero(req.Scope.GraphSpaceID) {
		return Receipt{}, domain.Errorf(domain.CodeInternal, op, "scope was not resolved before ingestion")
	}
	if len(req.Payload) == 0 {
		return Receipt{}, domain.Errorf(domain.CodeInvalidArgument, op, "payload must not be empty")
	}
	if int64(len(req.Payload)) > g.opts.MaxPayloadBytes {
		return Receipt{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"payload of %d bytes exceeds the %d byte limit", len(req.Payload), g.opts.MaxPayloadBytes)
	}

	operation := req.Operation
	if operation == "" {
		operation = domain.SourceOpUpsert
	}
	if _, err := domain.ParseSourceOperation(string(operation)); err != nil {
		return Receipt{}, err
	}

	source, err := g.resolveSource(ctx, req)
	if err != nil {
		g.countResult(ctx, "rejected")
		return Receipt{}, err
	}
	if err := g.validateCollection(ctx, req); err != nil {
		g.countResult(ctx, "rejected")
		return Receipt{}, err
	}

	mediaType := req.MediaType
	if mediaType == "" {
		mediaType = normalize.MediaTypePlain
	}

	// Sensitivity is inherited from the source and may only be raised here.
	classification := domain.MostRestrictive(source.Classification, req.Classification)

	contentHash := domain.ContentHash(req.Payload)
	blobKey, err := blob.Key(string(req.Scope.WorkspaceID), contentHash)
	if err != nil {
		return Receipt{}, domain.Wrap(err, domain.CodeInternal, op, "cannot derive blob key")
	}

	// Archive before committing: an event that references unreadable bytes would be
	// knowledge with no evidence, which this architecture does not allow. Storage is
	// content-addressed, so a retry writes the same key and an orphan left by a failed
	// transaction is harmless.
	info, err := g.blobs.Put(ctx, blobKey, req.Payload)
	if err != nil {
		g.countResult(ctx, "rejected")
		return Receipt{}, domain.Wrap(err, domain.CodeInternal, op, "cannot archive raw payload")
	}

	idempotencyKey := req.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = domain.DeriveIdempotencyKey(source.ID, req.ExternalID, req.SourceVersion, contentHash)
	}

	now := g.opts.now()
	metadata := req.Metadata
	if req.DirectEpisode {
		metadata = withFlag(metadata, normalize.MetadataDirectEpisode, true)
	}

	artifact := domain.Artifact{
		WorkspaceID: req.Scope.WorkspaceID,
		ContentHash: contentHash,
		MediaType:   mediaType,
		SizeBytes:   info.Size,
		BlobKey:     blobKey,
		Storage:     g.blobs.Name(),
	}

	event := domain.SourceEvent{
		WorkspaceID:    req.Scope.WorkspaceID,
		GraphSpaceID:   req.Scope.GraphSpaceID,
		CollectionID:   req.Scope.CollectionID,
		SourceID:       source.ID,
		ExternalID:     req.ExternalID,
		EventType:      req.EventType,
		Operation:      operation,
		ContentHash:    contentHash,
		IdempotencyKey: idempotencyKey,

		EventTime:        utc(req.EventTime),
		SourceTime:       utc(req.SourceTime),
		SourceCommitTime: utc(req.SourceCommitTime),
		SourceSequence:   req.SourceSequence,
		SourceVersion:    req.SourceVersion,

		// Knowledge time is set here, by the system, never by the caller.
		ObservedAt: now,
		RecordedAt: now,

		MediaType:      mediaType,
		Status:         domain.SourceEventAccepted,
		Classification: classification,
		CreatedBy:      req.Principal,
		Metadata:       metadata,
	}

	work, err := g.pipelineWorkItem(event)
	if err != nil {
		return Receipt{}, err
	}

	result, err := g.ledger.AppendSourceEvent(ctx, domain.SourceEventAppend{
		Artifact:        artifact,
		Event:           event,
		Outbox:          []domain.OutboxEvent{work},
		PipelineVersion: g.opts.PipelineVersion,
		Actor:           req.Principal.ID,
	})
	if err != nil {
		if domain.IsCode(err, domain.CodeSourceEventConflict) {
			g.countResult(ctx, "conflict")
		} else {
			g.countResult(ctx, "rejected")
		}
		return Receipt{}, err
	}

	if result.Duplicate {
		g.countResult(ctx, "duplicate")
	} else {
		g.countResult(ctx, "accepted")
		if g.metrics != nil {
			g.metrics.IngestBytes.Add(ctx, info.Size)
		}
		// After the commit, never inside it, and only for work that is genuinely new:
		// a duplicate created no work item to announce.
		if g.notify != nil {
			g.notify(ctx, work)
		}
	}

	span.SetAttributes(
		attribute.String("strata.source_event_id", string(result.Event.ID)),
		attribute.Bool("strata.duplicate", result.Duplicate),
	)
	if g.logger != nil {
		g.logger.InfoContext(ctx, "source event accepted",
			slog.String("source_event_id", string(result.Event.ID)),
			slog.String("source_id", string(source.ID)),
			slog.Bool("duplicate", result.Duplicate),
			slog.Int64("size_bytes", info.Size))
	}

	return Receipt{
		SourceEventID:  result.Event.ID,
		ArtifactID:     result.Artifact.ID,
		IdempotencyKey: result.Event.IdempotencyKey,
		ContentHash:    result.Event.ContentHash,
		Status:         result.Event.Status,
		Duplicate:      result.Duplicate,
	}, nil
}

// AcceptBatch submits many events through the identical single-event path, so batch
// import cannot drift from streaming behavior (AGENTS.md section 10.1).
//
// Each request succeeds or fails independently: one malformed record in a batch of a
// thousand must not discard the other 999.
func (g *Gateway) AcceptBatch(ctx context.Context, reqs []Request) ([]Receipt, []error) {
	receipts := make([]Receipt, len(reqs))
	errs := make([]error, len(reqs))
	for i, req := range reqs {
		receipts[i], errs[i] = g.Accept(ctx, req)
	}
	return receipts, errs
}

// pipelineWorkItem builds the durable work item that drives processing.
func (g *Gateway) pipelineWorkItem(event domain.SourceEvent) (domain.OutboxEvent, error) {
	const op = "ingest.pipelineWorkItem"

	payload, err := json.Marshal(domain.SourceEventAcceptedPayload{
		WorkspaceID:     event.WorkspaceID,
		GraphSpaceID:    event.GraphSpaceID,
		PipelineVersion: g.opts.PipelineVersion,
	})
	if err != nil {
		return domain.OutboxEvent{}, domain.Wrap(err, domain.CodeInternal, op, "cannot encode work payload")
	}

	return domain.OutboxEvent{
		WorkspaceID:   event.WorkspaceID,
		GraphSpaceID:  event.GraphSpaceID,
		Topic:         domain.TopicIngestPipeline,
		EventType:     domain.EventTypeSourceEventAccepted,
		SchemaVersion: domain.OutboxSchemaVersion,
		Payload:       payload,
		DedupeKey:     domain.PipelineDedupeKey(event.SourceID, event.IdempotencyKey, g.opts.PipelineVersion),
		PartitionKey:  partitionFor(event),
		Status:        domain.OutboxPending,
		MaxAttempts:   g.opts.MaxAttempts,
	}, nil
}

// partitionFor decides what this work item may not run concurrently with.
//
// Successive versions of one upstream record are ordered: processing an update alongside the
// create it supersedes lets the older content win the race and leaves the graph describing a
// state the source has already left. The CDC path takes an advisory lock for exactly this
// (internal/store/ledger/cdc.go); a partition key is the same guarantee moved into the claim,
// where it also holds across processes and across the NATS transport.
//
// Only records with an upstream identity are partitioned. An ingest with no external id is a
// one-off document that supersedes nothing, and serializing every such upload from one source
// would turn a bulk import into a queue of one.
func partitionFor(event domain.SourceEvent) string {
	if event.ExternalID == "" {
		return ""
	}
	return domain.PartitionOf(event.WorkspaceID, string(event.SourceID), event.ExternalID)
}

// resolveSource looks up the source inside the resolved workspace, which is what stops
// a caller from attributing an event to another tenant's source.
func (g *Gateway) resolveSource(ctx context.Context, req Request) (domain.Source, error) {
	const op = "ingest.resolveSource"

	switch {
	case !domain.IsZero(req.SourceID):
		if !domain.ValidUUID(req.SourceID) {
			return domain.Source{}, domain.Errorf(domain.CodeInvalidArgument, op, "malformed source id")
		}
		return g.ledger.GetSource(ctx, req.Scope.WorkspaceID, req.SourceID)
	case req.SourceName != "":
		return g.ledger.GetSourceByName(ctx, req.Scope.WorkspaceID, req.SourceName)
	default:
		return domain.Source{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"source_id or source_name is required")
	}
}

// validateCollection ensures a named collection belongs to the resolved graph space.
func (g *Gateway) validateCollection(ctx context.Context, req Request) error {
	const op = "ingest.validateCollection"

	if domain.IsZero(req.Scope.CollectionID) {
		return nil
	}
	collection, err := g.ledger.GetCollection(ctx, req.Scope.WorkspaceID, req.Scope.CollectionID)
	if err != nil {
		return err
	}
	if collection.GraphSpaceID != req.Scope.GraphSpaceID {
		return domain.Errorf(domain.CodeInvalidArgument, op,
			"collection does not belong to this graph space")
	}
	return nil
}

func (g *Gateway) countResult(ctx context.Context, result string) {
	if g.metrics == nil {
		return
	}
	g.metrics.IngestEvents.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

func withFlag(m map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[key] = value
	return out
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
