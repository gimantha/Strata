package observability

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics holds the instruments listed in AGENTS.md section 30.2 that this phase can
// actually populate. Later phases add their own without changing these.
type Metrics struct {
	IngestEvents  metric.Int64Counter
	IngestBytes   metric.Int64Counter
	StageDuration metric.Float64Histogram
	StageFailures metric.Int64Counter
	OutboxClaimed metric.Int64Counter
	OutboxOutcome metric.Int64Counter
	OutboxReaped  metric.Int64Counter
	HTTPRequests  metric.Int64Counter
	HTTPDuration  metric.Float64Histogram

	meter metric.Meter
}

// NewMetrics creates every instrument up front so a recording site can never fail.
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{meter: meter}
	var errs []error
	collect := func(err error) { errs = append(errs, err) }

	var err error
	m.IngestEvents, err = meter.Int64Counter("strata.ingest.events",
		metric.WithDescription("Source events accepted, by result (accepted, duplicate, conflict)"))
	collect(err)
	m.IngestBytes, err = meter.Int64Counter("strata.ingest.bytes",
		metric.WithDescription("Raw payload bytes archived"), metric.WithUnit("By"))
	collect(err)
	m.StageDuration, err = meter.Float64Histogram("strata.pipeline.stage.duration",
		metric.WithDescription("Pipeline stage execution time"), metric.WithUnit("s"))
	collect(err)
	m.StageFailures, err = meter.Int64Counter("strata.pipeline.stage.failures",
		metric.WithDescription("Pipeline stage failures, by stage and error class"))
	collect(err)
	m.OutboxClaimed, err = meter.Int64Counter("strata.outbox.claimed",
		metric.WithDescription("Outbox work items claimed by a worker"))
	collect(err)
	m.OutboxOutcome, err = meter.Int64Counter("strata.outbox.outcome",
		metric.WithDescription("Outbox work item outcomes, by result (succeeded, retried, dead)"))
	collect(err)
	m.OutboxReaped, err = meter.Int64Counter("strata.outbox.reaped",
		metric.WithDescription("Expired claims returned to pending after a worker died"))
	collect(err)
	m.HTTPRequests, err = meter.Int64Counter("strata.http.requests",
		metric.WithDescription("HTTP requests, by route and status class"))
	collect(err)
	m.HTTPDuration, err = meter.Float64Histogram("strata.http.duration",
		metric.WithDescription("HTTP request duration"), metric.WithUnit("s"))
	collect(err)

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return m, nil
}

// OutboxDepthSnapshot is the queue-lag view an operator needs: how much work is
// waiting, and how long the oldest waiting item has been waiting.
type OutboxDepthSnapshot struct {
	ByStatus         map[string]int64
	OldestPendingAge float64 // seconds
}

// RegisterOutboxGauges attaches observable gauges to a live source of queue depth.
// Queue lag is the primary early warning that projections are falling behind, so it
// is polled by the metrics pipeline rather than computed on the request path.
func (m *Metrics) RegisterOutboxGauges(observe func(context.Context) (OutboxDepthSnapshot, error)) error {
	depth, err := m.meter.Int64ObservableGauge("strata.outbox.depth",
		metric.WithDescription("Outbox work items by status"))
	if err != nil {
		return err
	}
	age, err := m.meter.Float64ObservableGauge("strata.outbox.oldest_pending_age",
		metric.WithDescription("Age of the oldest pending outbox item"), metric.WithUnit("s"))
	if err != nil {
		return err
	}

	_, err = m.meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		snap, err := observe(ctx)
		if err != nil {
			return err
		}
		for status, count := range snap.ByStatus {
			o.ObserveInt64(depth, count, metric.WithAttributes(attribute.String("status", status)))
		}
		o.ObserveFloat64(age, snap.OldestPendingAge)
		return nil
	}, depth, age)
	return err
}
