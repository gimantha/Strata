// Package app assembles the object graph shared by every Strata process.
//
// The server, the worker, and the CLI are the same system entered through different
// doors, so they are wired here once rather than three times.
package app

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/gimantha/strata/internal/config"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/eventbus"
	"github.com/gimantha/strata/internal/identity"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/observability"
	"github.com/gimantha/strata/internal/pipeline"
	"github.com/gimantha/strata/internal/store/blob"
	"github.com/gimantha/strata/internal/store/ledger"
)

// App owns every long-lived component.
type App struct {
	Config    config.Config
	Telemetry *observability.Telemetry
	Logger    *slog.Logger
	Ledger    *ledger.Store
	Blobs     *blob.FS
	Identity  *identity.Service
	Gateway   *ingest.Gateway
	Bus       *eventbus.Outbox
	Runner    *pipeline.Runner
}

// New builds the application. It connects to PostgreSQL, applies migrations when
// configured to, and wires ingestion and processing.
func New(ctx context.Context, cfg config.Config) (*App, error) {
	telemetry, err := observability.Setup(ctx, cfg)
	if err != nil {
		return nil, err
	}
	logger := telemetry.Logger

	store, err := ledger.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}

	app := &App{
		Config:    cfg,
		Telemetry: telemetry,
		Logger:    logger,
		Ledger:    store,
	}

	if cfg.AutoMigrate {
		applied, err := store.Migrate(ctx, logger)
		if err != nil {
			app.closeLedger()
			return nil, err
		}
		if applied > 0 {
			logger.InfoContext(ctx, "schema migrated", slog.Int("migrations_applied", applied))
		}
	}

	blobs, err := blob.NewFS(cfg.BlobDir)
	if err != nil {
		app.closeLedger()
		return nil, domain.Wrap(err, domain.CodeInvalidArgument, "app.New", "cannot open blob storage")
	}
	app.Blobs = blobs

	identityService, err := identity.Load(ctx, cfg.APIKeysFile, store, logger)
	if err != nil {
		app.closeLedger()
		return nil, err
	}
	app.Identity = identityService

	app.Gateway = ingest.New(store, blobs, ingest.Options{
		PipelineVersion: cfg.PipelineVersion,
		MaxPayloadBytes: cfg.MaxBodyBytes,
		MaxAttempts:     cfg.WorkerMaxAttempts,
	}, logger, telemetry.Metrics, telemetry.Tracer)

	app.Bus = eventbus.NewOutbox(store, logger, telemetry.Metrics, telemetry.Tracer)

	app.Runner = pipeline.NewRunner(store, cfg.PipelineVersion,
		pipeline.DefaultStages(store, blobs, pipeline.StageConfig{
			ChunkMaxTokens:     cfg.ChunkMaxTokens,
			ChunkOverlapTokens: cfg.ChunkOverlapTokens,
			Tokenizer:          normalize.DefaultTokenizer,
		}),
		logger, telemetry.Metrics, telemetry.Tracer)

	// Queue depth is observed rather than pushed, so lag is visible even when nothing
	// is being processed (AGENTS.md section 30.2).
	if err := telemetry.Metrics.RegisterOutboxGauges(store.OutboxDepth); err != nil {
		logger.WarnContext(ctx, "could not register outbox gauges", slog.String("error", err.Error()))
	}

	return app, nil
}

// Close releases resources.
func (a *App) Close(ctx context.Context) error {
	a.closeLedger()
	if a.Telemetry != nil {
		return a.Telemetry.Shutdown(ctx)
	}
	return nil
}

func (a *App) closeLedger() {
	if a.Ledger != nil {
		a.Ledger.Close()
	}
}

// Subscription describes how this process consumes work.
func (a *App) Subscription() eventbus.SubscriptionSpec {
	return eventbus.SubscriptionSpec{
		Topics:       []string{domain.TopicIngestPipeline},
		Concurrency:  a.Config.WorkerConcurrency,
		BatchSize:    a.Config.WorkerBatchSize,
		Lease:        a.Config.WorkerLease,
		PollInterval: a.Config.WorkerPollInterval,
		MaxAttempts:  a.Config.WorkerMaxAttempts,
		BackoffBase:  a.Config.BackoffBase,
		BackoffMax:   a.Config.BackoffMax,
		DrainTimeout: a.Config.ShutdownTimeout,
	}
}

// RunWorker consumes work until the context is cancelled.
func (a *App) RunWorker(ctx context.Context) error {
	return a.Bus.Subscribe(ctx, a.Subscription(), a.HandleWork)
}

// HandleWork dispatches one work item.
//
// An unrecognized event type or payload version fails as invalid input rather than a
// transient error, so it dead-letters immediately instead of retrying forever against a
// consumer that will never understand it (AGENTS.md sections 28.4, 34).
func (a *App) HandleWork(ctx context.Context, event domain.OutboxEvent) error {
	const op = "app.HandleWork"

	switch event.EventType {
	case domain.EventTypeSourceEventAccepted:
		if event.SchemaVersion != domain.OutboxSchemaVersion {
			return domain.Errorf(domain.CodeInvalidArgument, op,
				"unsupported payload schema version %d for %s", event.SchemaVersion, event.EventType)
		}
		var payload domain.SourceEventAcceptedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return domain.Wrap(err, domain.CodeInvalidArgument, op, "malformed work payload")
		}
		if domain.IsZero(event.SourceEventID) {
			return domain.Errorf(domain.CodeInvalidArgument, op, "work item has no source event")
		}
		_, err := a.Runner.Process(ctx, event.WorkspaceID, event.SourceEventID, false)
		return err

	default:
		return domain.Errorf(domain.CodeInvalidArgument, op, "unknown event type %q", event.EventType)
	}
}
