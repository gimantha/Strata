// Package observability wires structured logging, tracing, and metrics.
//
// Observability is part of correctness here: every ingestion and query must be
// traceable, and trace context must survive the asynchronous boundary between the
// ingest request and the worker that processes it (AGENTS.md sections 2.13, 30).
package observability

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gimantha/strata/internal/config"
)

// Telemetry holds the process-wide observability handles.
type Telemetry struct {
	Logger  *slog.Logger
	Tracer  trace.Tracer
	Meter   metric.Meter
	Metrics *Metrics

	shutdown []func(context.Context) error
}

// Setup builds logging, tracing, and metrics from configuration. When no OTLP
// endpoint is configured it installs no-op providers, so tests and local runs need
// no collector while the instrumentation calls stay identical.
func Setup(ctx context.Context, cfg config.Config) (*Telemetry, error) {
	logger := NewLogger(cfg, os.Stdout)

	// The propagator is installed unconditionally: trace context must be extracted
	// and injected even when this process is not exporting spans itself.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("opentelemetry error", slog.String("error", err.Error()))
	}))

	t := &Telemetry{Logger: logger}

	if cfg.OTLPEndpoint == "" {
		t.Tracer = tracenoop.NewTracerProvider().Tracer(cfg.ServiceName)
		t.Meter = metricnoop.NewMeterProvider().Meter(cfg.ServiceName)
	} else {
		res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.DeploymentEnvironmentNameKey.String(cfg.Env),
		))
		if err != nil {
			return nil, err
		}

		traceExp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.OTLPEndpoint))
		if err != nil {
			return nil, err
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(traceExp),
			sdktrace.WithResource(res),
		)
		otel.SetTracerProvider(tp)
		t.shutdown = append(t.shutdown, tp.Shutdown)

		metricExp, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(cfg.OTLPEndpoint))
		if err != nil {
			return nil, err
		}
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
			sdkmetric.WithResource(res),
		)
		otel.SetMeterProvider(mp)
		t.shutdown = append(t.shutdown, mp.Shutdown)

		t.Tracer = tp.Tracer(cfg.ServiceName)
		t.Meter = mp.Meter(cfg.ServiceName)
	}

	m, err := NewMetrics(t.Meter)
	if err != nil {
		return nil, err
	}
	t.Metrics = m
	return t, nil
}

// Shutdown flushes exporters. It is safe to call on a no-op setup.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	for i := len(t.shutdown) - 1; i >= 0; i-- {
		if err := t.shutdown[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// NewLogger builds a structured logger. Logs are structured only, and carry trace
// and scope correlation automatically (AGENTS.md section 30.3).
func NewLogger(cfg config.Config, w io.Writer) *slog.Logger {
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}

	return slog.New(&contextHandler{Handler: h}).With(
		slog.String("service", cfg.ServiceName),
		slog.String("env", cfg.Env),
	)
}

// contextHandler enriches every record with the correlation identifiers held in the
// context, so no call site has to remember to pass them.
type contextHandler struct{ slog.Handler }

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	if v := ctx.Value(ctxKeyRequestID{}); v != nil {
		if id, ok := v.(string); ok && id != "" {
			r.AddAttrs(slog.String("request_id", id))
		}
	}
	if v := ctx.Value(ctxKeyWorkspace{}); v != nil {
		if ws, ok := v.(string); ok && ws != "" {
			r.AddAttrs(slog.String("workspace_id", ws))
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}

type (
	ctxKeyRequestID struct{}
	ctxKeyWorkspace struct{}
)

// WithRequestID tags a context so every log line for the request is correlatable.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID{}, id)
}

// RequestIDFromContext returns the request identifier, if any.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID{}).(string); ok {
		return v
	}
	return ""
}

// WithWorkspace tags a context with the resolved workspace. Workspace identifiers
// are safe to log; source content and credentials are not.
func WithWorkspace(ctx context.Context, workspaceID string) context.Context {
	return context.WithValue(ctx, ctxKeyWorkspace{}, workspaceID)
}

// TraceParentFromContext serializes the current span into a W3C traceparent header
// value, for storage on an outbox row.
func TraceParentFromContext(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// ContextWithTraceParent restores trace context captured at publish time, so the
// worker's spans join the ingest request's trace instead of starting a new one.
func ContextWithTraceParent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}
