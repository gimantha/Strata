package extraction

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/llm"
)

// ModelRunStore records model interactions.
type ModelRunStore interface {
	RecordModelRun(ctx context.Context, run domain.ModelRun) (domain.ModelRun, error)
}

// Extractor asks a model for candidate knowledge and checks what comes back.
type Extractor struct {
	provider llm.LLM
	runs     ModelRunStore
	now      func() time.Time
	logger   *slog.Logger
	tracer   trace.Tracer
}

// Options configures the extractor.
type Options struct {
	Now func() time.Time
}

// New builds an extractor.
func New(provider llm.LLM, runs ModelRunStore, opts Options, logger *slog.Logger, tracer trace.Tracer) *Extractor {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer("extraction")
	}
	return &Extractor{provider: provider, runs: runs, now: now, logger: logger, tracer: tracer}
}

// Request is one extraction call.
type Request struct {
	Scope         domain.Scope
	SourceEventID domain.SourceEventID
	Units         []SourceUnit
}

// Result is what extraction produced, along with the model run that produced it.
type Result struct {
	Candidates domain.ExtractionResult
	Rejections []Rejection
	ModelRun   domain.ModelRun
}

// Extract runs one extraction and records the interaction.
//
// A model run is written whatever happens - success, invalid output, or provider failure.
// The run that produced unusable output is the one an operator most needs to see, so it is
// never the one that goes unrecorded (AGENTS.md section 13.2).
func (e *Extractor) Extract(ctx context.Context, req Request) (Result, error) {
	const op = "extraction.Extract"

	ctx, span := e.tracer.Start(ctx, "extraction.Extract", trace.WithAttributes(
		attribute.String("strata.workspace_id", string(req.Scope.WorkspaceID)),
		attribute.String("strata.provider", e.provider.Name()),
		attribute.String("strata.model", e.provider.Model()),
		attribute.Int("strata.units", len(req.Units)),
	))
	defer span.End()

	prompt, err := BuildPrompt(req.Units)
	if err != nil {
		return Result{}, domain.Wrap(err, domain.CodeInvalidArgument, op, "cannot build the extraction prompt")
	}

	run := domain.ModelRun{
		WorkspaceID:    req.Scope.WorkspaceID,
		GraphSpaceID:   req.Scope.GraphSpaceID,
		Provider:       e.provider.Name(),
		Model:          e.provider.Model(),
		PromptTemplate: PromptTemplate,
		PromptVersion:  PromptVersion,
		RequestHash:    llm.HashRequest(prompt.Request),
		SourceEventID:  req.SourceEventID,
	}

	started := e.now()
	response, callErr := e.provider.GenerateStructured(ctx, prompt.Request)
	run.Latency = e.now().Sub(started)

	if callErr != nil {
		run.Status = domain.ModelRunFailed
		run.ValidationError = domainMessage(callErr)
		recorded, recordErr := e.record(ctx, run)
		if recordErr != nil {
			return Result{}, recordErr
		}
		span.RecordError(callErr)
		// The provider's error class is preserved, so a rate limit retries and a refusal
		// does not.
		return Result{ModelRun: recorded}, callErr
	}

	run.ResponseHash = llm.HashResponse(response.Raw)
	run.PromptTokens = response.Usage.PromptTokens
	run.CompletionTokens = response.Usage.CompletionTokens
	run.TotalTokens = response.Usage.TotalTokens
	if response.Model != "" {
		// Record what actually served the request, which may be a pinned snapshot of the
		// model that was asked for.
		run.Model = response.Model
	}

	validated, validationErr := Validate(response.Raw, prompt.Source)
	if validationErr != nil {
		run.Status = domain.ModelRunInvalid
		run.ValidationError = domainMessage(validationErr)
		run.ResponseExcerpt = excerpt(string(response.Raw))

		recorded, recordErr := e.record(ctx, run)
		if recordErr != nil {
			return Result{}, recordErr
		}
		if e.logger != nil {
			e.logger.WarnContext(ctx, "model output failed validation and was discarded",
				slog.String("model_run_id", string(recorded.ID)),
				slog.String("error", run.ValidationError))
		}
		span.RecordError(validationErr)
		// Malformed output is a fault in the response, not the request: it must never be
		// retried indefinitely, and nothing from it may be committed.
		return Result{ModelRun: recorded}, validationErr
	}

	run.Status = domain.ModelRunSucceeded
	recorded, err := e.record(ctx, run)
	if err != nil {
		return Result{}, err
	}

	if e.logger != nil && len(validated.Rejections) > 0 {
		e.logger.WarnContext(ctx, "some extracted candidates were discarded",
			slog.String("model_run_id", string(recorded.ID)),
			slog.Int("rejected", len(validated.Rejections)),
			slog.Int("accepted", len(validated.Result.Assertions)))
	}
	span.SetAttributes(
		attribute.Int("strata.candidates", len(validated.Result.Assertions)),
		attribute.Int("strata.rejected", len(validated.Rejections)),
	)

	return Result{
		Candidates: validated.Result,
		Rejections: validated.Rejections,
		ModelRun:   recorded,
	}, nil
}

func (e *Extractor) record(ctx context.Context, run domain.ModelRun) (domain.ModelRun, error) {
	// Use a detached context so a cancelled request still leaves its audit trail.
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return e.runs.RecordModelRun(recordCtx, run)
}

// excerpt bounds rejected output kept for debugging.
func excerpt(s string) string {
	if len(s) <= domain.MaxResponseExcerpt {
		return s
	}
	return s[:domain.MaxResponseExcerpt] + "...(truncated)"
}
