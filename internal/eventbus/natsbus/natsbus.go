// Package natsbus publishes and consumes work through NATS JetStream
// (AGENTS.md sections 4, 27.5, phase 14).
//
// The PostgreSQL outbox remains the durability boundary: work is written in the same
// transaction as the canonical mutation it belongs to, and that is not negotiable
// (section 28.1). This adapter is a *delivery* mechanism on top of it — a fleet spread across
// machines stops polling one database and gets pushed to instead, which is the difference
// between a handful of workers and a hundred.
//
// That split is deliberate. A bus that owned durability would mean two systems could disagree
// about whether work exists; here the ledger is always right and the bus is an optimization
// that can be turned off.
package natsbus

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/eventbus"
)

// StreamName is the JetStream stream this adapter owns by default.
const StreamName = "STRATA_WORK"

// SubjectPrefix namespaces every subject this adapter publishes.
const SubjectPrefix = "strata.work"

// Options configure the adapter.
type Options struct {
	// URL is the NATS server, defaulting to the local one.
	URL string
	// Name identifies this connection in NATS monitoring, which is what turns "some client
	// is misbehaving" into a hostname.
	Name string
	// Stream names the JetStream stream, defaulting to StreamName. Two deployments sharing
	// one NATS cluster need distinct names: a work-queue stream may not overlap another
	// stream's subjects, so the subject space is namespaced by this too.
	Stream string
	// Durable names the consumer group. Every worker sharing a name shares the queue, which
	// is how horizontal scaling happens without duplicate delivery.
	Durable string
	// AckWait is how long a delivery may be unacknowledged before redelivery. It plays the
	// same role as the outbox lease and should be set from the same reasoning.
	AckWait time.Duration
	// MaxDeliver bounds redelivery before an item is left for the ledger's own retry and
	// dead-letter handling, which is where failure state belongs.
	MaxDeliver int
	// MaxAckPending is the backpressure knob: JetStream stops delivering to a consumer that
	// has this many unacknowledged messages, so a slow worker slows its own intake rather
	// than accumulating a backlog in memory.
	MaxAckPending int
	// ConnectTimeout bounds startup.
	ConnectTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.URL == "" {
		o.URL = nats.DefaultURL
	}
	if o.Name == "" {
		o.Name = "strata"
	}
	if o.Stream == "" {
		o.Stream = StreamName
	}
	if o.Durable == "" {
		o.Durable = "strata-workers"
	}
	if o.AckWait <= 0 {
		o.AckWait = 60 * time.Second
	}
	if o.MaxDeliver <= 0 {
		o.MaxDeliver = 5
	}
	if o.MaxAckPending <= 0 {
		o.MaxAckPending = 256
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = 5 * time.Second
	}
	return o
}

// Bus delivers outbox work over JetStream.
type Bus struct {
	conn   *nats.Conn
	stream jetstream.JetStream
	opts   Options
	logger *slog.Logger
	// root is the subject namespace this stream owns.
	root string

	mu     sync.Mutex
	closed bool
}

// Connect dials NATS and ensures the stream exists.
func Connect(ctx context.Context, opts Options, logger *slog.Logger) (*Bus, error) {
	const op = "natsbus.Connect"

	opts = opts.withDefaults()
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	conn, err := nats.Connect(opts.URL,
		nats.Name(opts.Name),
		nats.Timeout(opts.ConnectTimeout),
		// Reconnect forever. A worker that gives up on a bus outage is a worker an
		// operator has to restart by hand, and the ledger still holds the work either way.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Warn("nats disconnected", slog.String("error", errorText(err)))
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			logger.Info("nats reconnected", slog.String("url", c.ConnectedUrl()))
		}),
	)
	if err != nil {
		return nil, domain.Wrap(err, domain.CodeProviderUnavailable, op,
			"cannot connect to NATS")
	}

	stream, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		return nil, domain.Wrap(err, domain.CodeProviderUnavailable, op,
			"cannot open JetStream")
	}

	bus := &Bus{
		conn: conn, stream: stream, opts: opts, logger: logger,
		root: SubjectPrefix + "." + sanitize(strings.ToLower(opts.Stream)),
	}
	if err := bus.ensureStream(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return bus, nil
}

// ensureStream creates or updates the work stream.
func (b *Bus) ensureStream(ctx context.Context) error {
	const op = "natsbus.ensureStream"

	_, err := b.stream.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     b.opts.Stream,
		Subjects: []string{b.root + ".>"},
		// Work-queue retention: a message leaves the stream once a consumer acknowledges
		// it. The ledger is the durable record, so keeping delivered work here as well
		// would be a second copy nobody reconciles.
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.FileStorage,
		// The ledger already deduplicates on the outbox dedupe key. This window catches
		// the narrower case of a publisher retrying the same message after a timeout.
		Duplicates: 2 * time.Minute,
		MaxAge:     24 * time.Hour,
	})
	if err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op,
			"cannot create the work stream")
	}
	return nil
}

// Publish sends work to the stream.
//
// Best effort by design: the outbox row is already committed, so a publish failure costs
// latency rather than work. A worker polling the ledger picks it up regardless, which is why
// this returns nil after logging rather than failing a caller who has already done the durable
// part correctly.
func (b *Bus) Publish(ctx context.Context, events ...domain.OutboxEvent) error {
	for _, event := range events {
		payload, err := json.Marshal(event)
		if err != nil {
			return domain.Wrap(err, domain.CodeInternal, "natsbus.Publish",
				"cannot encode a work item")
		}

		// The dedupe key doubles as the JetStream message id, so a retried publish inside
		// the duplicate window collapses instead of delivering twice.
		if _, err := b.stream.Publish(ctx, b.subjectFor(event), payload,
			jetstream.WithMsgID(event.DedupeKey)); err != nil {
			b.logger.WarnContext(ctx, "could not publish work to NATS; the ledger still holds it",
				slog.String("event_id", string(event.ID)),
				slog.String("error", err.Error()))
		}
	}
	return nil
}

// Subscribe consumes work until the context is cancelled.
//
// The handler's contract matches the outbox's: nil acknowledges, an error hands scheduling
// back. Errors are negatively acknowledged with the caller's backoff so redelivery follows the
// same policy whichever transport is in use.
func (b *Bus) Subscribe(ctx context.Context, spec eventbus.SubscriptionSpec, handler eventbus.Handler) error {
	const op = "natsbus.Subscribe"

	consumer, err := b.stream.CreateOrUpdateConsumer(ctx, b.opts.Stream, jetstream.ConsumerConfig{
		// A durable name shared by every worker makes them one queue group: each message
		// goes to exactly one of them, which is horizontal scaling without duplicate work.
		Durable:        b.opts.Durable,
		FilterSubjects: b.subjectsFor(spec.Topics),
		AckPolicy:      jetstream.AckExplicitPolicy,
		AckWait:        b.opts.AckWait,
		MaxDeliver:     b.opts.MaxDeliver,
		// Backpressure: JetStream stops delivering past this many unacknowledged messages,
		// so a slow worker throttles its own intake instead of buffering in memory.
		MaxAckPending: b.opts.MaxAckPending,
	})
	if err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op,
			"cannot create the work consumer")
	}

	var wg sync.WaitGroup
	slots := make(chan struct{}, max(spec.Concurrency, 1))

	subscription, err := consumer.Consume(func(msg jetstream.Msg) {
		slots <- struct{}{}
		wg.Add(1)

		go func() {
			defer wg.Done()
			defer func() { <-slots }()

			var event domain.OutboxEvent
			if err := json.Unmarshal(msg.Data(), &event); err != nil {
				// Undecodable means no consumer will ever succeed with it. Terminating
				// stops it consuming redelivery slots forever.
				b.logger.ErrorContext(ctx, "discarding an undecodable work item",
					slog.String("error", err.Error()))
				_ = msg.Term()
				return
			}

			if err := handler(ctx, event); err != nil {
				delay := eventbus.Backoff(spec.BackoffBase, spec.BackoffMax, event.Attempts+1)
				if nakErr := msg.NakWithDelay(delay); nakErr != nil {
					b.logger.WarnContext(ctx, "could not nak a work item",
						slog.String("event_id", string(event.ID)),
						slog.String("error", nakErr.Error()))
				}
				return
			}
			if err := msg.Ack(); err != nil {
				// The handler succeeded and the ack did not, so this item will be
				// redelivered. Handlers are idempotent by stage key, so the cost is a
				// repeat rather than a duplicate.
				b.logger.WarnContext(ctx, "could not acknowledge a work item",
					slog.String("event_id", string(event.ID)),
					slog.String("error", err.Error()))
			}
		}()
	})
	if err != nil {
		return domain.Wrap(err, domain.CodeProviderUnavailable, op, "cannot consume work")
	}

	b.logger.InfoContext(ctx, "nats consumer started",
		slog.String("stream", b.opts.Stream),
		slog.String("durable", b.opts.Durable),
		slog.Int("concurrency", spec.Concurrency),
		slog.Int("max_ack_pending", b.opts.MaxAckPending))

	<-ctx.Done()
	subscription.Stop()

	// Drain in-flight handlers so shutdown does not abandon work mid-flight. Anything
	// still running when the timeout expires stays unacknowledged and is redelivered,
	// which is the same guarantee the outbox lease gives.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(spec.DrainTimeout):
		return domain.Errorf(domain.CodeInternal, op,
			"shutdown timed out with work still in flight; it will be redelivered")
	}
}

// Close releases the connection.
func (b *Bus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil
	}
	b.closed = true

	// Drain rather than Close: it flushes pending publishes and lets in-flight callbacks
	// finish, which is the difference between a clean shutdown and a redelivery storm.
	return b.conn.Drain()
}

// subjectFor maps a work item onto a subject.
//
// The topic and the partition key are both in the subject so a deployment can route by either:
// a consumer can filter to one kind of work, and a subject-partitioned stream can keep one
// record's events in order without the consumer having to know about it.
func (b *Bus) subjectFor(event domain.OutboxEvent) string {
	parts := []string{b.root, sanitize(event.Topic)}
	if event.PartitionKey != "" {
		parts = append(parts, sanitize(event.PartitionKey))
	}
	return strings.Join(parts, ".")
}

// subjectsFor builds the consumer's filter from the requested topics.
func (b *Bus) subjectsFor(topics []string) []string {
	if len(topics) == 0 {
		return []string{b.root + ".>"}
	}
	out := make([]string, 0, len(topics))
	for _, topic := range topics {
		out = append(out, b.root+"."+sanitize(topic)+".>")
		out = append(out, b.root+"."+sanitize(topic))
	}
	return out
}

// Stream reports the stream this bus publishes to.
func (b *Bus) Stream() string { return b.opts.Stream }

// sanitize makes a string safe as one subject token.
//
// NATS subjects are dot-delimited with * and > as wildcards, so a topic or partition key
// containing any of them would silently widen a filter. Replacing rather than rejecting keeps
// an unusual key from failing an ingest that has already been committed.
func sanitize(value string) string {
	replacer := strings.NewReplacer(".", "_", "*", "_", ">", "_", " ", "_")
	cleaned := replacer.Replace(value)
	if cleaned == "" {
		return "unspecified"
	}
	return cleaned
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Ensure the adapter satisfies the bus contract the rest of the system depends on.
var _ eventbus.Bus = (*Bus)(nil)
