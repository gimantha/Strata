// Package cdc consumes upstream row changes and turns them into source events
// (AGENTS.md sections 10.1 and 11).
//
// The contract is one interface — a stream of domain.ChangeEvent — and everything else in
// the system is downstream of it. A PostgreSQL logical-decoding adapter, a Kafka consumer,
// and the JSONL replay adapter in this package all differ only in where the events come
// from, which is the point of having a contract at all.
//
// Nothing here interprets a row. The runner records what the upstream said; the pipeline
// stage decides what it means, using a mapping stored with the stream. That split is what
// lets a mapping change without re-reading the upstream, and lets a replay produce the same
// events whatever the current mapping happens to be.
package cdc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
)

// Stream yields change events in upstream order.
//
// Next returns io.EOF when the stream is exhausted. A live adapter blocks instead; a replay
// adapter ends. Both are legitimate, which is why the runner treats EOF as completion rather
// than as failure.
type Stream interface {
	// Next returns the next change, or io.EOF.
	Next(ctx context.Context) (domain.ChangeEvent, error)
	// Name identifies the adapter in logs and traces.
	Name() string
}

// Gateway is the ingestion surface a connector writes through, declared by its consumer.
//
// A connector has no privileged path into the ledger. It goes through the same gateway as an
// HTTP upload, which is what keeps idempotency, archival, and the outbox commit identical
// for every source (AGENTS.md section 10.1).
type Gateway interface {
	Accept(ctx context.Context, req ingest.Request) (ingest.Receipt, error)
}

// Store is the checkpoint surface.
type Store interface {
	GetCDCStream(ctx context.Context, ws domain.WorkspaceID, source domain.SourceID, stream string) (domain.CDCStream, error)
	SaveStreamCheckpoint(ctx context.Context, checkpoint domain.StreamCheckpoint) error
}

// Options configure a runner.
type Options struct {
	// CheckpointEvery bounds how many events may be reprocessed after a crash. Saving
	// after every event is correct and slow; saving rarely is fast and replays more.
	CheckpointEvery int
	Clock           func() time.Time
}

// DefaultCheckpointEvery is the batch size between checkpoint writes.
const DefaultCheckpointEvery = 50

// Runner drives a stream into the ledger.
type Runner struct {
	gateway Gateway
	store   Store
	opts    Options
	logger  *slog.Logger
	tracer  trace.Tracer
}

func New(gateway Gateway, store Store, opts Options, logger *slog.Logger, tracer trace.Tracer) *Runner {
	if opts.CheckpointEvery <= 0 {
		opts.CheckpointEvery = DefaultCheckpointEvery
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer("cdc")
	}
	return &Runner{gateway: gateway, store: store, opts: opts, logger: logger, tracer: tracer}
}

// Request is one connector run.
type Request struct {
	Scope     domain.Scope
	Principal domain.PrincipalRef
	SourceID  domain.SourceID
	// Stream restricts the run to one registered stream. A stream carrying events for
	// several tables is fine; each event names its own.
	Stream string
	// Limit stops after this many events. Zero runs to exhaustion.
	Limit int
	// ResumeFromCheckpoint skips events at or before the stored offset. Off means replay
	// everything the stream offers, which idempotency makes safe and which is how a
	// rebuild from an archive works.
	ResumeFromCheckpoint bool
}

// Result reports what a run did.
type Result struct {
	Consumed   int
	Accepted   int
	Duplicates int
	Skipped    int
	LastOffset string
	Events     []domain.SourceEventID
}

// Run consumes a stream until it ends, the limit is reached, or the context is cancelled.
//
// Checkpoints advance only after events are durably accepted. A crash between accepting and
// checkpointing therefore replays those events, and replaying them is free: the gateway keys
// on the change's idempotency key and returns the original event. The opposite ordering —
// checkpoint first — would silently lose changes, which is the one outcome a CDC pipeline
// cannot recover from (AGENTS.md sections 10.2 and 11.1).
func (r *Runner) Run(ctx context.Context, req Request, stream Stream) (Result, error) {
	const op = "cdc.Run"

	ctx, span := r.tracer.Start(ctx, "cdc.Run", trace.WithAttributes(
		attribute.String("strata.workspace_id", string(req.Scope.WorkspaceID)),
		attribute.String("strata.source_id", string(req.SourceID)),
		attribute.String("strata.stream", req.Stream),
		attribute.String("strata.adapter", stream.Name()),
	))
	defer span.End()

	if domain.IsZero(req.Scope.WorkspaceID) || domain.IsZero(req.Scope.GraphSpaceID) {
		return Result{}, domain.Errorf(domain.CodeInternal, op,
			"scope was not resolved before running a connector")
	}
	if domain.IsZero(req.SourceID) {
		return Result{}, domain.Errorf(domain.CodeInvalidArgument, op, "a source is required")
	}

	resumeFrom := ""
	if req.ResumeFromCheckpoint && req.Stream != "" {
		registered, err := r.store.GetCDCStream(ctx, req.Scope.WorkspaceID, req.SourceID, req.Stream)
		if err != nil {
			return Result{}, err
		}
		resumeFrom = registered.Checkpoint.LastOffset
	}

	var (
		result    Result
		pending   int
		lastEvent domain.ChangeEvent
	)

	for {
		if req.Limit > 0 && result.Consumed >= req.Limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}

		event, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Flush what is already durable before giving up, so a transient upstream
			// failure does not cost the work that succeeded before it.
			r.flush(ctx, req, lastEvent, &pending, &result)
			return result, err
		}
		result.Consumed++

		if err := event.Validate(); err != nil {
			return result, domain.Wrap(err, domain.CodeInvalidArgument, op,
				"change event at offset "+event.Offset+" is malformed")
		}
		if req.Stream != "" && event.Stream != req.Stream {
			result.Skipped++
			continue
		}
		if resumeFrom != "" && event.Offset != "" && event.Offset <= resumeFrom {
			// Already durable. Re-accepting would be harmless and pointless.
			result.Skipped++
			continue
		}

		receipt, err := r.accept(ctx, req, event)
		if err != nil {
			r.flush(ctx, req, lastEvent, &pending, &result)
			return result, err
		}

		result.Events = append(result.Events, receipt.SourceEventID)
		if receipt.Duplicate {
			result.Duplicates++
		} else {
			result.Accepted++
		}
		result.LastOffset = event.Offset
		lastEvent = event
		pending++

		if pending >= r.opts.CheckpointEvery {
			r.flush(ctx, req, lastEvent, &pending, &result)
		}
	}

	r.flush(ctx, req, lastEvent, &pending, &result)

	span.SetAttributes(
		attribute.Int("strata.consumed", result.Consumed),
		attribute.Int("strata.accepted", result.Accepted),
		attribute.Int("strata.duplicates", result.Duplicates),
	)
	r.logger.InfoContext(ctx, "cdc run finished",
		slog.String("adapter", stream.Name()),
		slog.String("stream", req.Stream),
		slog.Int("consumed", result.Consumed),
		slog.Int("accepted", result.Accepted),
		slog.Int("duplicates", result.Duplicates),
		slog.Int("skipped", result.Skipped))
	return result, nil
}

// accept turns one change into a source event.
func (r *Runner) accept(ctx context.Context, req Request, event domain.ChangeEvent) (ingest.Receipt, error) {
	const op = "cdc.accept"

	payload, err := json.Marshal(event)
	if err != nil {
		return ingest.Receipt{}, domain.Wrap(err, domain.CodeInternal, op,
			"cannot encode the change event")
	}

	// The whole change is archived, not just the row image. A before image, a transaction
	// id, and an offset are what make a disputed fact explicable a year later, and they
	// cost nothing to keep.
	return r.gateway.Accept(ctx, ingest.Request{
		Scope:            req.Scope,
		Principal:        req.Principal,
		SourceID:         req.SourceID,
		ExternalID:       event.RecordID(),
		EventType:        domain.EventTypeChangeRow,
		Operation:        event.Operation.SourceOperation(),
		MediaType:        domain.MediaTypeChangeRow,
		Payload:          payload,
		IdempotencyKey:   event.IdempotencyKey(),
		EventTime:        event.EventTime,
		SourceTime:       event.CommitTime,
		SourceCommitTime: event.CommitTime,
		SourceSequence:   event.Sequence,
		SourceVersion:    event.Sequence,
		Metadata: map[string]any{
			"stream":         event.Stream,
			"offset":         event.Offset,
			"transaction":    event.Transaction,
			"schema_version": event.SchemaVersion,
			"operation":      string(event.Operation),
		},
	})
}

// flush writes the checkpoint for everything accepted so far.
func (r *Runner) flush(ctx context.Context, req Request, last domain.ChangeEvent, pending *int, result *Result) {
	if *pending == 0 || req.Stream == "" {
		*pending = 0
		return
	}

	checkpoint := domain.StreamCheckpoint{
		WorkspaceID:    req.Scope.WorkspaceID,
		SourceID:       req.SourceID,
		Stream:         req.Stream,
		LastOffset:     last.Offset,
		LastSequence:   last.Sequence,
		LastCommitTime: last.CommitTime,
		EventsConsumed: int64(*pending),
	}
	if err := r.store.SaveStreamCheckpoint(ctx, checkpoint); err != nil {
		// A checkpoint that fails to save costs a replay, not correctness. Failing the
		// run here would turn a recoverable inefficiency into lost progress.
		r.logger.WarnContext(ctx, "cannot save cdc checkpoint",
			slog.String("stream", req.Stream), slog.String("error", err.Error()))
	}
	_ = result
	*pending = 0
}
