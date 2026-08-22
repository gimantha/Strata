// Package pipeline orchestrates the stages that turn a source event into knowledge.
//
// Every stage is idempotent, retryable, observable, versioned, and independently
// replayable (AGENTS.md section 10.3). Stages communicate through the canonical
// ledger rather than by passing data along in memory, so any stage can be re-run
// from durable state alone - which is what makes replay and forced reprocessing
// meaningful instead of best-effort.
package pipeline

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/observability"
)

// Stage is one unit of processing.
//
// Version is part of the durable stage key: incrementing it makes the stage re-run
// for events that already passed the previous version, which is how a fixed segmenter
// or chunker gets rolled out without touching the ledger's source events.
type Stage interface {
	Name() string
	Version() int
	Execute(ctx context.Context, in Input) (Output, error)
}

// Input is what a stage receives. Scope is already resolved and authorized.
type Input struct {
	Scope domain.Scope
	Event domain.SourceEvent
	Run   domain.PipelineRun
}

// Output is a stage's durable summary. The canonical output lives in ledger tables;
// this is the small record of what happened, stored on the stage run.
type Output struct {
	Summary any
}

// Store is the persistence the runner needs, declared by its consumer.
type Store interface {
	GetSourceEvent(ctx context.Context, ws domain.WorkspaceID, id domain.SourceEventID) (domain.SourceEvent, error)
	SetSourceEventStatus(ctx context.Context, ws domain.WorkspaceID, id domain.SourceEventID, status domain.SourceEventStatus) error
	ClaimPipelineRun(ctx context.Context, event domain.SourceEvent, pipelineVersion int) (domain.PipelineRun, error)
	FinishPipelineRun(ctx context.Context, id domain.PipelineRunID, status domain.RunStatus, cause error) error
	BeginStageRun(ctx context.Context, runID domain.PipelineRunID, ws domain.WorkspaceID, eventID domain.SourceEventID, stageName string, stageVersion int, force bool) (domain.StageRun, bool, error)
	FinishStageRun(ctx context.Context, id domain.StageRunID, status domain.RunStatus, outputRef any, cause error) error
}

// Runner executes an ordered set of stages for one source event.
type Runner struct {
	store   Store
	stages  []Stage
	version int
	logger  *slog.Logger
	metrics *observability.Metrics
	tracer  trace.Tracer
}

// NewRunner builds a runner. Stage order is the pipeline definition.
func NewRunner(store Store, version int, stages []Stage, logger *slog.Logger, metrics *observability.Metrics, tracer trace.Tracer) *Runner {
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer("pipeline")
	}
	return &Runner{
		store:   store,
		stages:  stages,
		version: version,
		logger:  logger,
		metrics: metrics,
		tracer:  tracer,
	}
}

// Version reports the pipeline version this runner executes.
func (r *Runner) Version() int { return r.version }

// Result describes one processing attempt.
type Result struct {
	Run           domain.PipelineRun
	StagesRun     int
	StagesSkipped int
}

// Process runs every stage for one source event.
//
// A stage that already succeeded under this pipeline version is skipped, so redelivery
// of the same work item costs almost nothing and creates no duplicate knowledge
// (AGENTS.md sections 2.12, 10.4). Passing force re-executes stages regardless, which
// is how an operator reprocesses an event deliberately.
func (r *Runner) Process(ctx context.Context, ws domain.WorkspaceID, eventID domain.SourceEventID, force bool) (Result, error) {
	ctx, span := r.tracer.Start(ctx, "pipeline.Process", trace.WithAttributes(
		attribute.String("strata.workspace_id", string(ws)),
		attribute.String("strata.source_event_id", string(eventID)),
		attribute.Int("strata.pipeline_version", r.version),
	))
	defer span.End()

	event, err := r.store.GetSourceEvent(ctx, ws, eventID)
	if err != nil {
		recordSpanError(span, err)
		return Result{}, err
	}

	run, err := r.store.ClaimPipelineRun(ctx, event, r.version)
	if err != nil {
		recordSpanError(span, err)
		return Result{}, err
	}
	result := Result{Run: run}

	if err := r.store.SetSourceEventStatus(ctx, ws, eventID, domain.SourceEventProcessing); err != nil {
		recordSpanError(span, err)
		return result, err
	}

	in := Input{
		Scope: domain.Scope{
			WorkspaceID:  event.WorkspaceID,
			GraphSpaceID: event.GraphSpaceID,
			CollectionID: event.CollectionID,
		},
		Event: event,
		Run:   run,
	}

	for _, stage := range r.stages {
		ran, err := r.runStage(ctx, stage, in, force)
		if err != nil {
			// Report the failure durably, then let the work fabric decide whether to
			// retry: scheduling policy belongs to the queue, not to the pipeline.
			status := domain.SourceEventFailed
			if domain.ClassifyError(err).Retryable() {
				status = domain.SourceEventProcessing
			}
			if statusErr := r.store.SetSourceEventStatus(ctx, ws, eventID, status); statusErr != nil && r.logger != nil {
				r.logger.WarnContext(ctx, "could not record source event failure",
					slog.String("error", statusErr.Error()))
			}
			if runErr := r.store.FinishPipelineRun(ctx, run.ID, domain.RunFailed, err); runErr != nil && r.logger != nil {
				r.logger.WarnContext(ctx, "could not record pipeline failure",
					slog.String("error", runErr.Error()))
			}
			recordSpanError(span, err)
			return result, err
		}
		if ran {
			result.StagesRun++
		} else {
			result.StagesSkipped++
		}
	}

	if err := r.store.FinishPipelineRun(ctx, run.ID, domain.RunSucceeded, nil); err != nil {
		recordSpanError(span, err)
		return result, err
	}
	if err := r.store.SetSourceEventStatus(ctx, ws, eventID, domain.SourceEventProcessed); err != nil {
		recordSpanError(span, err)
		return result, err
	}

	if r.logger != nil {
		r.logger.InfoContext(ctx, "source event processed",
			slog.String("source_event_id", string(eventID)),
			slog.Int("stages_run", result.StagesRun),
			slog.Int("stages_skipped", result.StagesSkipped))
	}
	return result, nil
}

// runStage executes one stage under its durable key, reporting whether it actually ran.
func (r *Runner) runStage(ctx context.Context, stage Stage, in Input, force bool) (bool, error) {
	key := domain.StageKey{
		WorkspaceID:     in.Event.WorkspaceID,
		SourceEventID:   in.Event.ID,
		PipelineVersion: r.version,
		StageName:       stage.Name(),
		StageVersion:    stage.Version(),
	}

	stageRun, alreadyDone, err := r.store.BeginStageRun(ctx, in.Run.ID, in.Event.WorkspaceID,
		in.Event.ID, stage.Name(), stage.Version(), force)
	if err != nil {
		return false, err
	}
	if alreadyDone {
		if r.logger != nil {
			r.logger.DebugContext(ctx, "stage already succeeded; reusing prior output",
				slog.String("stage", key.String()))
		}
		return false, nil
	}

	ctx, span := r.tracer.Start(ctx, "pipeline.stage."+stage.Name(), trace.WithAttributes(
		attribute.String("strata.stage", stage.Name()),
		attribute.Int("strata.stage_version", stage.Version()),
	))
	defer span.End()

	start := time.Now()
	out, execErr := stage.Execute(ctx, in)
	elapsed := time.Since(start)

	if r.metrics != nil {
		r.metrics.StageDuration.Record(ctx, elapsed.Seconds(),
			metric.WithAttributes(attribute.String("stage", stage.Name())))
	}

	if execErr != nil {
		class := domain.ClassifyError(execErr)
		// A failure that cannot succeed on retry is terminal for this stage; bad input
		// does not become valid by waiting (AGENTS.md section 28.4).
		status := domain.RunFailed
		if !class.Retryable() {
			status = domain.RunDead
		}
		if r.metrics != nil {
			r.metrics.StageFailures.Add(ctx, 1, metric.WithAttributes(
				attribute.String("stage", stage.Name()),
				attribute.String("error_class", string(class)),
			))
		}
		if err := r.store.FinishStageRun(ctx, stageRun.ID, status, nil, execErr); err != nil && r.logger != nil {
			r.logger.WarnContext(ctx, "could not record stage failure", slog.String("error", err.Error()))
		}
		recordSpanError(span, execErr)
		return false, execErr
	}

	if err := r.store.FinishStageRun(ctx, stageRun.ID, domain.RunSucceeded, out.Summary, nil); err != nil {
		recordSpanError(span, err)
		return false, err
	}
	return true, nil
}

func recordSpanError(span trace.Span, err error) {
	span.RecordError(err)
	span.SetStatus(codes.Error, string(domain.CodeOf(err)))
}
