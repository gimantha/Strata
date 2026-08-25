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
	"github.com/gimantha/strata/internal/connector/cdc"
	"github.com/gimantha/strata/internal/contextblock"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding"
	"github.com/gimantha/strata/internal/embedding/hashing"
	embeddingmock "github.com/gimantha/strata/internal/embedding/mock"
	embeddingopenai "github.com/gimantha/strata/internal/embedding/openai"
	"github.com/gimantha/strata/internal/eventbus"
	"github.com/gimantha/strata/internal/eventbus/natsbus"
	"github.com/gimantha/strata/internal/extraction"
	"github.com/gimantha/strata/internal/identity"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/llm"
	"github.com/gimantha/strata/internal/llm/mock"
	"github.com/gimantha/strata/internal/llm/openai"
	"github.com/gimantha/strata/internal/memory"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/observability"
	"github.com/gimantha/strata/internal/ontology"
	"github.com/gimantha/strata/internal/pipeline"
	"github.com/gimantha/strata/internal/policy"
	"github.com/gimantha/strata/internal/portable"
	"github.com/gimantha/strata/internal/projection"
	"github.com/gimantha/strata/internal/retrieval"
	"github.com/gimantha/strata/internal/store/blob"
	blobs3 "github.com/gimantha/strata/internal/store/blob/s3"
	"github.com/gimantha/strata/internal/store/index"
	"github.com/gimantha/strata/internal/store/ledger"
)

// App owns every long-lived component.
type App struct {
	Config    config.Config
	Telemetry *observability.Telemetry
	Logger    *slog.Logger
	Ledger    *ledger.Store
	Blobs     blob.Store
	Identity  *identity.Service
	Gateway   *ingest.Gateway
	Knowledge *knowledge.Service
	Extractor *extraction.Extractor
	Projector *projection.Projector
	// Indexes is the retrieval projections as configured, so an operator can be told which
	// backend serves each and a recovery drill can iterate them rather than guessing at
	// table names.
	Indexes   index.Set
	Retriever *retrieval.Retriever
	Assembler *contextblock.Assembler
	Ontology  *ontology.Service
	Connector *cdc.Runner
	Policy    *policy.Service
	Memory    *memory.Service
	Exporter  *portable.Exporter
	Importer  *portable.Importer
	Embedder  embedding.Embedder
	Bus       *eventbus.Outbox
	Runner    *pipeline.Runner

	// NATS is the optional push path. Nil means the deployment polls the ledger, which
	// is the same behavior with more latency (AGENTS.md section 27.5).
	NATS *natsbus.Bus
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

	blobs, err := newBlobStore(ctx, cfg)
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

	// Register configured principals so an owner can grant access to someone who has
	// not authenticated yet.
	//
	// Skipped on an unmigrated database, which is the normal state when this process is
	// the one about to run the migrations: warning about a missing table that is seconds
	// away from existing would make a clean first run look broken. Authentication
	// registers principals lazily regardless.
	schemaVersion, err := store.SchemaVersion(ctx)
	if err != nil {
		logger.WarnContext(ctx, "could not read schema version", slog.String("error", err.Error()))
	}
	if schemaVersion > 0 {
		if err := identityService.SyncPrincipals(ctx); err != nil {
			logger.WarnContext(ctx, "could not register configured principals",
				slog.String("error", err.Error()))
		}
	}

	app.Gateway = ingest.New(store, blobs, ingest.Options{
		PipelineVersion: cfg.PipelineVersion,
		MaxPayloadBytes: cfg.MaxBodyBytes,
		MaxAttempts:     cfg.WorkerMaxAttempts,
	}, logger, telemetry.Metrics, telemetry.Tracer)

	app.Knowledge = knowledge.New(store, knowledge.Options{}, logger, telemetry.Tracer)

	app.Bus = eventbus.NewOutbox(store, logger, telemetry.Metrics, telemetry.Tracer)

	if cfg.NATSURL != "" {
		bus, err := natsbus.Connect(ctx, natsbus.Options{
			URL:           cfg.NATSURL,
			Name:          cfg.ServiceName,
			Stream:        cfg.NATSStream,
			Durable:       cfg.NATSDurable,
			AckWait:       cfg.NATSAckWait,
			MaxDeliver:    cfg.WorkerMaxAttempts,
			MaxAckPending: cfg.NATSMaxAckPending,
		}, logger)
		if err != nil {
			// Startup fails rather than falling back silently: an operator who
			// configured a bus and got polling instead would discover it as an
			// unexplained latency regression.
			app.closeLedger()
			return nil, err
		}
		app.NATS = bus
		// Announce committed work as soon as it exists, so a fleet reacts in
		// milliseconds instead of at its next poll.
		app.Gateway.SetNotifier(app.PublishWork)
	}

	// A model provider is optional. Without one the pipeline still ingests, segments, and
	// chunks; extraction is simply not part of it.
	provider, err := newLLMProvider(cfg)
	if err != nil {
		app.closeLedger()
		return nil, err
	}
	stageCfg := pipeline.StageConfig{
		ChunkMaxTokens:     cfg.ChunkMaxTokens,
		ChunkOverlapTokens: cfg.ChunkOverlapTokens,
		Tokenizer:          normalize.DefaultTokenizer,
	}
	// CDC needs no model provider: a row is already structured, so the mapping reads it
	// deterministically. That is why the committer is wired here rather than only in the
	// extraction branch below.
	stageCfg.Changes = store
	stageCfg.Committer = app.Knowledge

	if provider != nil {
		app.Extractor = extraction.New(provider, store, extraction.Options{}, logger, telemetry.Tracer)
		stageCfg.Extractor = app.Extractor
		logger.InfoContext(ctx, "extraction enabled",
			slog.String("provider", provider.Name()), slog.String("model", provider.Model()))
	}

	// Retrieval projections are maintained whenever the pipeline runs. An embedder is
	// optional: without one the lexical and graph projections still work, since text search
	// and traversal need no model.
	embedder, err := newEmbedder(cfg)
	if err != nil {
		app.closeLedger()
		return nil, err
	}
	app.Embedder = embedder
	// One index set, built from the ledger, shared by the writer and the reader. A
	// deployment moving a projection elsewhere replaces an entry here and nothing else.
	app.Indexes = store.Indexes()
	app.Projector = projection.New(store, store, app.Indexes, embedder,
		projection.Options{}, logger, telemetry.Tracer)
	stageCfg.Projector = app.Projector
	if embedder != nil {
		logger.InfoContext(ctx, "vector projection enabled",
			slog.String("provider", embedder.Name()),
			slog.String("model", embedder.Model()),
			slog.Int("dimensions", embedder.Dimensions()))
	}

	app.Policy = policy.New(store, policy.NewLedgerAuditor(store), policy.Options{}, logger)
	app.Ontology = ontology.New(store, logger)
	app.Memory = memory.New(store, app.Knowledge, app.Projector, memory.Options{}, logger, telemetry.Tracer)

	app.Connector = cdc.New(app.Gateway, store, cdc.Options{}, logger, telemetry.Tracer)
	app.Retriever = retrieval.New(store, app.Indexes, embedder, retrieval.Options{
		Traces:          store,
		RedactQueryText: cfg.RedactQueryText,
	}, logger, telemetry.Tracer)

	app.Assembler = contextblock.New(app.Retriever, store, contextblock.Options{}, logger, telemetry.Tracer)

	app.Runner = pipeline.NewRunner(store, cfg.PipelineVersion,
		pipeline.DefaultStages(store, blobs, stageCfg),
		logger, telemetry.Metrics, telemetry.Tracer)

	// After the runner: an import processes its own event synchronously so the claims it
	// commits can cite an episode that already exists.
	portableOpts := portable.Options{Instance: cfg.ServiceName}
	app.Exporter = portable.NewExporter(store, portableOpts, logger)
	app.Importer = portable.NewImporter(store, app.Knowledge,
		importRecorder{gateway: app.Gateway, runner: app.Runner, store: store},
		portableOpts, logger)

	// Queue depth is observed rather than pushed, so lag is visible even when nothing
	// is being processed (AGENTS.md section 30.2).
	if err := telemetry.Metrics.RegisterOutboxGauges(store.OutboxDepth); err != nil {
		logger.WarnContext(ctx, "could not register outbox gauges", slog.String("error", err.Error()))
	}

	return app, nil
}

// newLLMProvider builds the configured provider, or nil when extraction is disabled.
//
// Provider construction is the only place these packages are referenced; nothing above
// this function knows which provider is in use (AGENTS.md section 2.11).
func newLLMProvider(cfg config.Config) (llm.LLM, error) {
	switch cfg.LLMProvider {
	case "", "none":
		return nil, nil
	case "mock":
		return mock.New(), nil
	case "openai":
		return openai.New(openai.Config{
			BaseURL:    cfg.LLMBaseURL,
			APIKey:     cfg.LLMAPIKey,
			Model:      cfg.LLMModel,
			Timeout:    cfg.LLMTimeout,
			MaxRetries: cfg.LLMMaxRetries,
		})
	default:
		return nil, domain.Errorf(domain.CodeInvalidArgument, "app.newLLMProvider",
			"unknown model provider %q", cfg.LLMProvider)
	}
}

// newEmbedder builds the configured embedder, or nil when vector search is disabled.
//
// The dimension is checked here rather than at first use: a mismatch with the projection
// schema is a configuration error, and discovering it when the first document is indexed is
// far worse than refusing to start.
func newEmbedder(cfg config.Config) (embedding.Embedder, error) {
	const op = "app.newEmbedder"

	var embedder embedding.Embedder
	switch cfg.EmbeddingProvider {
	case "", "none":
		return nil, nil
	case "mock":
		embedder = embeddingmock.New()
	case "hashing":
		embedder = hashing.New()
	case "openai":
		built, err := embeddingopenai.New(embeddingopenai.Config{
			BaseURL:    cfg.EmbeddingBaseURL,
			APIKey:     cfg.EmbeddingAPIKey,
			Model:      cfg.EmbeddingModel,
			Dimensions: embedding.Dimensions,
			Timeout:    cfg.LLMTimeout,
			MaxRetries: cfg.LLMMaxRetries,
		})
		if err != nil {
			return nil, err
		}
		embedder = built
	default:
		return nil, domain.Errorf(domain.CodeInvalidArgument, op,
			"unknown embedding provider %q", cfg.EmbeddingProvider)
	}

	if embedder.Dimensions() != embedding.Dimensions {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op,
			"the embedding model produces %d dimensions but the projection schema holds %d",
			embedder.Dimensions(), embedding.Dimensions)
	}
	return embedder, nil
}

// Close releases resources.
func (a *App) Close(ctx context.Context) error {
	if a.NATS != nil {
		if err := a.NATS.Close(); err != nil && a.Logger != nil {
			a.Logger.WarnContext(ctx, "could not close the NATS connection",
				slog.String("error", err.Error()))
		}
	}
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

		MaxEventsPerSecond: a.Config.WorkerMaxEventsPerSecond,
		IdleBackoffMax:     a.Config.WorkerIdleBackoffMax,
	}
}

// RunWorker consumes work until the context is cancelled.
//
// With a bus configured, two things run: the ledger consumer, which is the only thing
// that ever claims or executes work, and a NATS subscription whose messages are turned
// into wake-ups for it. Push delivery therefore buys latency without buying a second
// path to the same work — the claim stays in PostgreSQL, so exactly-once processing and
// partition ordering hold whether or not the bus is there, and whether or not it is
// healthy (AGENTS.md section 28.1).
func (a *App) RunWorker(ctx context.Context) error {
	if a.NATS == nil {
		return a.Bus.Subscribe(ctx, a.Subscription(), a.HandleWork)
	}

	notifyCtx, stopNotify := context.WithCancel(ctx)
	defer stopNotify()

	notifications := make(chan struct{})
	go func() {
		defer close(notifications)
		spec := a.Subscription()
		// One at a time: a notification is cheap, and this consumer does no work.
		spec.Concurrency = 1
		err := a.NATS.Subscribe(notifyCtx, spec, func(_ context.Context, event domain.OutboxEvent) error {
			a.Bus.Notify()
			return nil
		})
		if err != nil && notifyCtx.Err() == nil {
			// Not fatal. Losing the hints means falling back to the poll interval,
			// which is the deployment that has no bus at all.
			a.Logger.WarnContext(ctx, "the NATS notifier stopped; falling back to polling",
				slog.String("error", err.Error()))
		}
	}()

	err := a.Bus.Subscribe(ctx, a.Subscription(), a.HandleWork)
	stopNotify()
	<-notifications
	return err
}

// PublishWork mirrors committed work onto the bus so the fleet hears about it now
// rather than at its next poll.
//
// Called after the transaction commits, never inside it: the outbox row is the durable
// record and this is a hint about it. A failure here is logged by the adapter and
// nothing more, because the work is already safe.
func (a *App) PublishWork(ctx context.Context, events ...domain.OutboxEvent) {
	if a.NATS == nil || len(events) == 0 {
		return
	}
	_ = a.NATS.Publish(ctx, events...)
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

// newBlobStore opens the configured artifact store.
//
// Both backends satisfy the same port and pass the same conformance suite, so nothing
// downstream of here knows or cares which one is running. The choice is an operational one:
// the filesystem for a single node, an object store where the bytes need versioning and
// replication of their own (AGENTS.md sections 3, 40.2).
func newBlobStore(ctx context.Context, cfg config.Config) (blob.Store, error) {
	switch cfg.BlobBackend {
	case "s3":
		return blobs3.Open(ctx, blobs3.Options{
			Bucket:          cfg.S3Bucket,
			Prefix:          cfg.S3Prefix,
			Endpoint:        cfg.S3Endpoint,
			Region:          cfg.S3Region,
			AccessKeyID:     cfg.S3AccessKey,
			SecretAccessKey: cfg.S3Secret,
			PathStyle:       cfg.S3PathStyle,
		})
	default:
		return blob.NewFS(cfg.BlobDir)
	}
}
