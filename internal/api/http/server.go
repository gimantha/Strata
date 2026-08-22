package http

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gimantha/strata/internal/config"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/identity"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/observability"
	"github.com/gimantha/strata/internal/store/ledger"
)

// Server is the HTTP API.
type Server struct {
	cfg      config.Config
	logger   *slog.Logger
	metrics  *observability.Metrics
	tracer   trace.Tracer
	identity *identity.Service
	ledger   *ledger.Store
	gateway  *ingest.Gateway
	blobs    healthChecker
	http     *http.Server
}

// healthChecker is the readiness contract for a dependency.
type healthChecker interface {
	Healthy(ctx context.Context) error
}

// Deps are the server's collaborators.
type Deps struct {
	Config   config.Config
	Logger   *slog.Logger
	Metrics  *observability.Metrics
	Tracer   trace.Tracer
	Identity *identity.Service
	Ledger   *ledger.Store
	Gateway  *ingest.Gateway
	Blobs    healthChecker
}

// NewServer wires the API.
func NewServer(deps Deps) *Server {
	tracer := deps.Tracer
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer("api.http")
	}
	s := &Server{
		cfg:      deps.Config,
		logger:   deps.Logger,
		metrics:  deps.Metrics,
		tracer:   tracer,
		identity: deps.Identity,
		ledger:   deps.Ledger,
		gateway:  deps.Gateway,
		blobs:    deps.Blobs,
	}
	s.http = &http.Server{
		Addr:              deps.Config.HTTPAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       deps.Config.RequestTimeout,
		WriteTimeout:      deps.Config.RequestTimeout,
		IdleTimeout:       2 * time.Minute,
	}
	return s
}

// Handler builds the router with its middleware chain.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health endpoints are unauthenticated by design: an orchestrator must be able to
	// probe them, and they expose no tenant data.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)

	// Scope administration.
	mux.HandleFunc("POST /v1/workspaces", s.authenticated(s.handleCreateWorkspace))
	mux.HandleFunc("GET /v1/workspaces", s.authenticated(s.handleListWorkspaces))
	mux.HandleFunc("POST /v1/workspaces/{workspace_id}/graph-spaces", s.authenticated(s.handleCreateGraphSpace))
	mux.HandleFunc("GET /v1/workspaces/{workspace_id}/graph-spaces", s.authenticated(s.handleListGraphSpaces))
	mux.HandleFunc("POST /v1/workspaces/{workspace_id}/sources", s.authenticated(s.handleCreateSource))
	mux.HandleFunc("GET /v1/workspaces/{workspace_id}/sources", s.authenticated(s.handleListSources))
	mux.HandleFunc("POST /v1/workspaces/{workspace_id}/grants", s.authenticated(s.handleCreateGrant))
	mux.HandleFunc("POST /v1/graph-spaces/{graph_space_id}/collections", s.authenticated(s.handleCreateCollection))

	// Ingestion. All three routes converge on one source-event path.
	mux.HandleFunc("POST /v1/graph-spaces/{graph_space_id}/events", s.authenticated(s.handleIngestEvents))
	mux.HandleFunc("POST /v1/graph-spaces/{graph_space_id}/documents", s.authenticated(s.handleIngestDocument))
	mux.HandleFunc("POST /v1/graph-spaces/{graph_space_id}/episodes", s.authenticated(s.handleIngestEpisode))

	// Processing status.
	mux.HandleFunc("GET /v1/events/{event_id}/status", s.authenticated(s.handleEventStatus))

	var h http.Handler = mux
	h = s.withObservability(h)
	h = s.withRecovery(h)
	return h
}

// ListenAndServe starts the server.
func (s *Server) ListenAndServe() error {
	s.logger.Info("http server listening", slog.String("addr", s.cfg.HTTPAddr))
	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return domain.Wrap(err, domain.CodeInternal, "http.ListenAndServe", "server stopped")
	}
	return nil
}

// Shutdown stops accepting connections and waits for in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}
