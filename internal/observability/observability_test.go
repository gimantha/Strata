package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"

	"github.com/gimantha/strata/internal/config"
)

func TestLoggerAddsCorrelationFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(config.Config{ServiceName: "strata", Env: "test", LogFormat: "json"}, &buf)

	ctx := WithRequestID(context.Background(), "req-123")
	ctx = WithWorkspace(ctx, "ws-456")
	logger.InfoContext(ctx, "something happened")

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("logs must be structured JSON: %v (%s)", err, buf.String())
	}
	// Correlation must be automatic: a call site that forgets to pass ids is the normal
	// case, and an uncorrelated log line is close to useless during an incident.
	if entry["request_id"] != "req-123" {
		t.Fatalf("request id missing from log entry: %s", buf.String())
	}
	if entry["workspace_id"] != "ws-456" {
		t.Fatalf("workspace id missing from log entry: %s", buf.String())
	}
	if entry["service"] != "strata" || entry["env"] != "test" {
		t.Fatalf("service identity missing from log entry: %s", buf.String())
	}
}

func TestLoggerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(config.Config{LogLevel: "warn", LogFormat: "json"}, &buf)

	logger.Info("should be filtered")
	if buf.Len() != 0 {
		t.Fatalf("info must be filtered at warn level, got %s", buf.String())
	}
	logger.Warn("should appear")
	if buf.Len() == 0 {
		t.Fatal("warn must be emitted at warn level")
	}
}

func TestContextHelpersReturnEmptyWhenUnset(t *testing.T) {
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Fatalf("expected no request id, got %q", got)
	}
}

func TestTraceParentRoundTripsAcrossTheAsyncBoundary(t *testing.T) {
	// Trace context must survive being written to an outbox row and read back by a
	// worker, or ingest and processing appear as two unrelated traces.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	ctx := ContextWithTraceParent(context.Background(), traceparent)

	if got := TraceParentFromContext(ctx); got != traceparent {
		t.Fatalf("trace context did not survive the round trip: %q", got)
	}
	// An empty header must leave the context untouched rather than fabricating a trace.
	if got := TraceParentFromContext(ContextWithTraceParent(context.Background(), "")); got != "" {
		t.Fatalf("expected no trace context, got %q", got)
	}
}

func TestNewMetricsCreatesEveryInstrument(t *testing.T) {
	m, err := NewMetrics(metricnoop.NewMeterProvider().Meter("test"))
	if err != nil {
		t.Fatalf("create metrics: %v", err)
	}
	// Instruments are built up front so a recording site can never fail mid-request.
	if m.IngestEvents == nil || m.StageDuration == nil || m.OutboxOutcome == nil || m.HTTPDuration == nil {
		t.Fatal("every instrument must be constructed")
	}
	if err := m.RegisterOutboxGauges(func(context.Context) (OutboxDepthSnapshot, error) {
		return OutboxDepthSnapshot{ByStatus: map[string]int64{"pending": 1}}, nil
	}); err != nil {
		t.Fatalf("register gauges: %v", err)
	}
}

func TestSetupWithoutCollectorUsesNoOpProviders(t *testing.T) {
	// Running locally or in tests must not require a collector, while the
	// instrumentation calls stay identical.
	telemetry, err := Setup(context.Background(), config.Config{
		ServiceName: "strata", Env: "test", LogFormat: "json", LogLevel: "error",
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if telemetry.Tracer == nil || telemetry.Meter == nil || telemetry.Metrics == nil {
		t.Fatal("setup must always yield usable handles")
	}
	if err := telemetry.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown of a no-op setup must succeed: %v", err)
	}
}
