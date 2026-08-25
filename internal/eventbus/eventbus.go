// Package eventbus is the work fabric: how durable work is published and consumed.
//
// The default implementation is a transactional PostgreSQL outbox. Publication happens
// inside the same transaction as the canonical mutation that requires the work, so
// there is no "commit then publish" window (AGENTS.md section 28.1). A distributed
// deployment swaps in a NATS JetStream adapter behind this same interface in phase 14
// without changing any domain package.
package eventbus

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/observability"
)

// Handler processes one work item. Returning nil acknowledges it; returning an error
// hands scheduling back to the bus, which retries or dead-letters by error class.
type Handler func(ctx context.Context, event domain.OutboxEvent) error

// SubscriptionSpec configures a consumer.
type SubscriptionSpec struct {
	// Topics filters what this consumer takes. Empty means every topic.
	Topics       []string
	Concurrency  int
	BatchSize    int
	Lease        time.Duration
	PollInterval time.Duration
	MaxAttempts  int
	BackoffBase  time.Duration
	BackoffMax   time.Duration
	// DrainTimeout bounds how long shutdown waits for in-flight work.
	DrainTimeout time.Duration

	// MaxEventsPerSecond throttles how fast one worker takes work, so a fleet cannot
	// stampede a downstream a model provider or a database it shares. Zero is unlimited.
	//
	// A limit per worker rather than per fleet: a global limiter needs coordination that
	// would itself become the bottleneck, and multiplying by the worker count is
	// arithmetic an operator can do.
	MaxEventsPerSecond float64
	// IdleBackoffMax stretches the poll interval when the queue is repeatedly empty. A
	// fleet polling an idle queue every 500ms is mostly a way to keep a database busy
	// doing nothing.
	IdleBackoffMax time.Duration
}

func (s SubscriptionSpec) withDefaults() SubscriptionSpec {
	if s.Concurrency < 1 {
		s.Concurrency = 4
	}
	if s.BatchSize < 1 {
		s.BatchSize = 16
	}
	if s.Lease <= 0 {
		s.Lease = time.Minute
	}
	if s.PollInterval <= 0 {
		s.PollInterval = 500 * time.Millisecond
	}
	if s.MaxAttempts < 1 {
		s.MaxAttempts = 8
	}
	if s.BackoffBase <= 0 {
		s.BackoffBase = time.Second
	}
	if s.BackoffMax < s.BackoffBase {
		s.BackoffMax = 5 * time.Minute
	}
	if s.DrainTimeout <= 0 {
		s.DrainTimeout = 30 * time.Second
	}
	if s.IdleBackoffMax < s.PollInterval {
		s.IdleBackoffMax = 8 * s.PollInterval
	}
	return s
}

// Bus publishes and consumes durable work.
type Bus interface {
	Publish(ctx context.Context, events ...domain.OutboxEvent) error
	// Subscribe consumes until the context is cancelled, then drains in-flight work.
	Subscribe(ctx context.Context, spec SubscriptionSpec, handler Handler) error
}

// Store is the outbox persistence this package drives.
type Store interface {
	PublishOutbox(ctx context.Context, events ...domain.OutboxEvent) error
	ClaimOutbox(ctx context.Context, topics []string, workerID string, lease time.Duration, limit int) ([]domain.OutboxEvent, error)
	RenewClaim(ctx context.Context, id domain.OutboxEventID, workerID string, lease time.Duration) (bool, error)
	CompleteOutbox(ctx context.Context, id domain.OutboxEventID) error
	RetryOutbox(ctx context.Context, id domain.OutboxEventID, retryAfter time.Duration, cause error) error
	DeadLetterOutbox(ctx context.Context, id domain.OutboxEventID, cause error) error
	ReapExpiredClaims(ctx context.Context) (int, error)
}

// Outbox is the PostgreSQL-backed bus.
type Outbox struct {
	store    Store
	workerID string
	logger   *slog.Logger
	metrics  *observability.Metrics
	tracer   trace.Tracer

	// wake cuts a poll short when something outside the loop knows work has arrived.
	// Buffered by one and never blocking: the signal only means "look now", so a
	// second signal while one is pending adds nothing.
	wake chan struct{}
}

// Notify tells a running consumer that work is available, so it polls now instead of
// sleeping out its backoff.
//
// This is how push delivery is integrated without giving up the ledger's guarantees. A
// NATS message is a hint, not a work item: the claim still happens in PostgreSQL, so
// exactly-once and partition ordering keep working exactly as they do without a bus,
// and a lost or duplicated hint costs nothing (AGENTS.md section 28.1).
func (o *Outbox) Notify() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// NewOutbox builds a bus. The worker identifier is recorded on claims so an operator
// can tell which process is holding a lease.
func NewOutbox(store Store, logger *slog.Logger, metrics *observability.Metrics, tracer trace.Tracer) *Outbox {
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer("eventbus")
	}
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return &Outbox{
		store:    store,
		workerID: fmt.Sprintf("%s/%d/%s", host, os.Getpid(), randomSuffix()),
		logger:   logger,
		metrics:  metrics,
		tracer:   tracer,
		wake:     make(chan struct{}, 1),
	}
}

// WorkerID identifies this consumer.
func (o *Outbox) WorkerID() string { return o.workerID }

// Publish writes work items in their own transaction. Producers with a canonical
// mutation to attach to publish through the ledger instead, inside that transaction.
func (o *Outbox) Publish(ctx context.Context, events ...domain.OutboxEvent) error {
	return o.store.PublishOutbox(ctx, events...)
}

// Subscribe claims and processes work until the context is cancelled.
func (o *Outbox) Subscribe(ctx context.Context, spec SubscriptionSpec, handler Handler) error {
	spec = spec.withDefaults()

	var (
		wg  sync.WaitGroup
		sem = make(chan struct{}, spec.Concurrency)
		// The poll interval grows while the queue is empty and snaps back the moment
		// there is work, so an idle fleet is quiet and a busy one is responsive.
		idle    = spec.PollInterval
		limiter = newRateLimiter(spec.MaxEventsPerSecond)
	)

	if o.logger != nil {
		o.logger.InfoContext(ctx, "outbox consumer started",
			slog.String("worker_id", o.workerID),
			slog.Int("concurrency", spec.Concurrency),
			slog.Duration("lease", spec.Lease))
	}

	for {
		// Return leases abandoned by workers that died. Running this on every poll in
		// every worker is safe because the update is idempotent, and it means recovery
		// does not depend on one designated process being alive.
		if reaped, err := o.store.ReapExpiredClaims(ctx); err != nil {
			if ctx.Err() != nil {
				break
			}
			o.warn(ctx, "could not reap expired claims", err)
		} else if reaped > 0 {
			if o.metrics != nil {
				o.metrics.OutboxReaped.Add(ctx, int64(reaped))
			}
			if o.logger != nil {
				o.logger.InfoContext(ctx, "returned expired claims to the queue",
					slog.Int("count", reaped))
			}
		}

		// Claim no more than the free capacity, so items are not held under lease
		// while waiting for a slot. This is the backpressure: a worker whose handlers
		// are all busy stops taking work rather than queueing it in memory, and the
		// items stay visible to the rest of the fleet.
		free := spec.Concurrency - len(sem)
		if free > spec.BatchSize {
			free = spec.BatchSize
		}
		if allowed := limiter.allowance(); allowed >= 0 && allowed < free {
			free = allowed
		}

		claimed := 0
		if free > 0 {
			events, err := o.store.ClaimOutbox(ctx, spec.Topics, o.workerID, spec.Lease, free)
			if err != nil {
				if ctx.Err() != nil {
					break
				}
				o.warn(ctx, "could not claim work", err)
			} else {
				claimed = len(events)
				if o.metrics != nil && claimed > 0 {
					o.metrics.OutboxClaimed.Add(ctx, int64(claimed))
				}
				limiter.consume(claimed)
				for _, event := range events {
					sem <- struct{}{}
					wg.Add(1)
					go func(event domain.OutboxEvent) {
						defer wg.Done()
						defer func() { <-sem }()
						o.process(ctx, spec, handler, event)
					}(event)
				}
			}
		}

		// A full batch means there is probably more waiting; poll again immediately
		// rather than sleeping through a backlog.
		if claimed == free && claimed > 0 {
			if ctx.Err() != nil {
				break
			}
			idle = spec.PollInterval
			continue
		}
		if claimed > 0 {
			idle = spec.PollInterval
		} else if idle < spec.IdleBackoffMax {
			idle *= 2
			if idle > spec.IdleBackoffMax {
				idle = spec.IdleBackoffMax
			}
		}

		timer := time.NewTimer(idle)
		select {
		case <-ctx.Done():
			timer.Stop()
			// Fall through to draining.
		case <-o.wake:
			// Pushed at: skip the rest of the wait and reset the backoff, because a
			// notification means the queue is no longer idle.
			timer.Stop()
			idle = spec.PollInterval
			continue
		case <-timer.C:
			continue
		}
		break
	}

	return o.drain(&wg, spec.DrainTimeout)
}

// drain waits for in-flight handlers so shutdown does not abandon claimed work.
// Anything still running when the timeout expires keeps its lease and is reclaimed by
// the reaper, so no work is lost either way.
func (o *Outbox) drain(wg *sync.WaitGroup, timeout time.Duration) error {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		if o.logger != nil {
			o.logger.Warn("shutdown timeout reached with work still in flight; " +
				"those items will be reclaimed after their lease expires")
		}
		return nil
	}
}

// process runs one item under a renewed lease and records its outcome.
func (o *Outbox) process(ctx context.Context, spec SubscriptionSpec, handler Handler, event domain.OutboxEvent) {
	// Rejoin the trace that published this work, so ingest and processing appear as
	// one trace rather than two unrelated ones (AGENTS.md section 30.1).
	workCtx := observability.ContextWithTraceParent(context.WithoutCancel(ctx), event.TraceParent)
	workCtx = observability.WithWorkspace(workCtx, string(event.WorkspaceID))

	workCtx, span := o.tracer.Start(workCtx, "eventbus.handle", trace.WithAttributes(
		attribute.String("strata.topic", event.Topic),
		attribute.String("strata.event_type", event.EventType),
		attribute.String("strata.workspace_id", string(event.WorkspaceID)),
		attribute.Int("strata.attempt", event.Attempts),
	))
	defer span.End()

	// Heartbeat the lease so long work is not reclaimed while it is progressing — but stop
	// renewing the moment this consumer is shutting down. The handler's context is
	// deliberately detached so cancellation cannot abort work mid-transaction; the lease
	// must not inherit that immortality, or a handler that never returns would hold its
	// claim forever and the item would be unreachable to every other worker.
	stopHeartbeat := o.heartbeat(ctx, workCtx, event.ID, spec.Lease)
	err := handler(workCtx, event)
	stopHeartbeat()

	// Finishing an item can make more work claimable: anything queued behind this one in
	// the same partition was passed over precisely because this was in flight. Without
	// this the successor waits for the next poll, so a partitioned stream would move at
	// one item per poll interval no matter how idle the fleet is.
	defer o.Notify()

	switch {
	case err == nil:
		if completeErr := o.store.CompleteOutbox(workCtx, event.ID); completeErr != nil {
			// The work itself succeeded; failing to record that means it will be
			// redelivered, which idempotent handlers tolerate by design.
			o.warn(workCtx, "could not mark work complete", completeErr)
		}
		o.recordOutcome(workCtx, "succeeded")

	default:
		span.RecordError(err)
		span.SetStatus(codes.Error, string(domain.CodeOf(err)))

		class := domain.ClassifyError(err)
		exhausted := event.Attempts >= event.MaxAttempts || event.Attempts >= spec.MaxAttempts

		if !class.Retryable() || exhausted {
			if deadErr := o.store.DeadLetterOutbox(workCtx, event.ID, err); deadErr != nil {
				o.warn(workCtx, "could not dead-letter work", deadErr)
			}
			o.recordOutcome(workCtx, "dead")
			if o.logger != nil {
				o.logger.ErrorContext(workCtx, "work moved to dead-letter state",
					slog.String("outbox_id", string(event.ID)),
					slog.String("event_type", event.EventType),
					slog.String("error_class", string(class)),
					slog.Int("attempts", event.Attempts),
					slog.String("error", err.Error()))
			}
			return
		}

		delay := Backoff(spec.BackoffBase, spec.BackoffMax, event.Attempts)
		if retryErr := o.store.RetryOutbox(workCtx, event.ID, delay, err); retryErr != nil {
			o.warn(workCtx, "could not reschedule work", retryErr)
		}
		o.recordOutcome(workCtx, "retried")
		if o.logger != nil {
			o.logger.WarnContext(workCtx, "work failed and will be retried",
				slog.String("outbox_id", string(event.ID)),
				slog.String("error_class", string(class)),
				slog.Int("attempts", event.Attempts),
				slog.Duration("retry_in", delay),
				slog.String("error", err.Error()))
		}
	}
}

// heartbeat renews a claim until the work finishes or the consumer shuts down.
//
// Two contexts on purpose. The lifetime context is the subscriber's: when it is cancelled,
// renewal stops and the lease is allowed to expire, so a replacement worker can reclaim the
// item. The call context is the handler's detached one, used for the renewal statements
// themselves so a shutdown does not cancel a query mid-flight.
func (o *Outbox) heartbeat(lifetime, call context.Context, id domain.OutboxEventID, lease time.Duration) func() {
	interval := lease / 3
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}

	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-lifetime.Done():
				// The consumer is shutting down. Whatever this handler is doing, its
				// lease must be allowed to lapse so the work returns to the queue rather
				// than being held by a process that is going away.
				if o.logger != nil {
					o.logger.WarnContext(call, "consumer stopped while work was in flight; "+
						"releasing the lease for another worker to reclaim",
						slog.String("outbox_id", string(id)))
				}
				return
			case <-ticker.C:
				held, err := o.store.RenewClaim(call, id, o.workerID, lease)
				if err != nil {
					o.warn(call, "could not renew claim", err)
					continue
				}
				if !held {
					// The lease was lost, most likely reaped after a stall. Stop
					// renewing: another worker may now own this item.
					if o.logger != nil {
						o.logger.WarnContext(call, "lease no longer held while work was in flight",
							slog.String("outbox_id", string(id)))
					}
					return
				}
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}

func (o *Outbox) recordOutcome(ctx context.Context, result string) {
	if o.metrics == nil {
		return
	}
	o.metrics.OutboxOutcome.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

func (o *Outbox) warn(ctx context.Context, msg string, err error) {
	if o.logger != nil {
		o.logger.WarnContext(ctx, msg, slog.String("error", err.Error()))
	}
}

// Backoff computes the delay before retrying an attempt.
//
// Equal jitter: half the exponential delay plus a random half. Pure exponential
// backoff synchronizes retries across workers after an outage, and full jitter can
// retry almost immediately; this keeps both problems bounded
// (AGENTS.md section 28.4).
func Backoff(base, max time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if base <= 0 {
		base = time.Second
	}
	if max < base {
		max = base
	}

	// Cap the exponent before shifting so a large attempt count cannot overflow.
	exp := float64(attempt - 1)
	if exp > 32 {
		exp = 32
	}
	delay := float64(base) * math.Pow(2, exp)
	if delay > float64(max) {
		delay = float64(max)
	}

	half := delay / 2
	return time.Duration(half + rand.Float64()*half)
}

func randomSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 6)
	for i := range out {
		out[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return string(out)
}

// rateLimiter is a token bucket over one worker's claim rate.
//
// Applied when claiming rather than when handling, because a claim takes a lease: throttling
// after the fact would hold work under lease while waiting, hiding it from the rest of the
// fleet and inviting the reaper to take it back.
type rateLimiter struct {
	mu       sync.Mutex
	rate     float64
	capacity float64
	tokens   float64
	last     time.Time
}

func newRateLimiter(perSecond float64) *rateLimiter {
	if perSecond <= 0 {
		return nil
	}
	// One second of burst: enough that a batch is not chopped into single items, small
	// enough that a limit of ten per second cannot become a hundred at once.
	return &rateLimiter{
		rate: perSecond, capacity: perSecond, tokens: perSecond, last: time.Now(),
	}
}

// allowance reports how many items may be claimed now, or -1 when unlimited.
func (r *rateLimiter) allowance() int {
	if r == nil {
		return -1
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	r.tokens += now.Sub(r.last).Seconds() * r.rate
	if r.tokens > r.capacity {
		r.tokens = r.capacity
	}
	r.last = now

	if r.tokens < 1 {
		return 0
	}
	return int(r.tokens)
}

// consume records what was actually claimed.
func (r *rateLimiter) consume(count int) {
	if r == nil || count <= 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.tokens -= float64(count)
	if r.tokens < 0 {
		r.tokens = 0
	}
}
