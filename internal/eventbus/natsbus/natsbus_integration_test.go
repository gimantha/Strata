package natsbus_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/eventbus"
	"github.com/gimantha/strata/internal/eventbus/natsbus"
	"github.com/gimantha/strata/internal/testsupport/natstest"
)

// connect dials the test server and clears the stream, so one test's leftovers cannot be
// delivered to the next.
func connect(t *testing.T, opts natsbus.Options) *natsbus.Bus {
	t.Helper()

	url := natstest.URL(t)
	opts.URL = url
	opts.Stream = natstest.Stream(t)
	bus, err := natsbus.Connect(context.Background(), opts, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	return bus
}

func workItem(key string, partition string) domain.OutboxEvent {
	payload, _ := json.Marshal(map[string]any{"key": key})
	return domain.OutboxEvent{
		ID:            domain.OutboxEventID(domain.NewUUIDString()),
		WorkspaceID:   domain.WorkspaceID("01a00000-0000-7000-8000-000000000001"),
		Topic:         domain.TopicIngestPipeline,
		EventType:     "test.work",
		SchemaVersion: 1,
		Payload:       payload,
		DedupeKey:     key,
		PartitionKey:  partition,
		Status:        domain.OutboxPending,
		MaxAttempts:   5,
	}
}

// TestIntegrationJetStreamDeliversEachItemToOneConsumer is the distributed half of phase 14's
// first acceptance criterion: a fleet spread across processes must not duplicate work.
//
// Consumers sharing a durable name form a queue group, so JetStream delivers each message to
// exactly one of them. Counting per item rather than in aggregate, because a correct total can
// still hide one item delivered twice and another never.
func TestIntegrationJetStreamDeliversEachItemToOneConsumer(t *testing.T) {
	bus := connect(t, natsbus.Options{Durable: "scale-test"})
	ctx := t.Context()

	const items = 120
	events := make([]domain.OutboxEvent, 0, items)
	for i := range items {
		events = append(events, workItem(fmt.Sprintf("nats-%d", i), ""))
	}

	var (
		mu       sync.Mutex
		counts   = map[domain.OutboxEventID]int{}
		received atomic.Int64
		done     = make(chan struct{})
		once     sync.Once
	)

	handler := func(_ context.Context, event domain.OutboxEvent) error {
		mu.Lock()
		counts[event.ID]++
		total := len(counts)
		mu.Unlock()

		received.Add(1)
		if total == items {
			once.Do(func() { close(done) })
		}
		return nil
	}

	// Three consumers on one durable, the way three worker processes would be.
	consumerCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = bus.Subscribe(consumerCtx, eventbus.SubscriptionSpec{
				Concurrency: 4, DrainTimeout: 2 * time.Second,
			}, handler)
		}()
	}

	if err := bus.Publish(ctx, events...); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("only %d of %d items arrived", received.Load(), items)
	}

	// Let any duplicate delivery surface rather than declaring victory the moment the
	// count is reached.
	time.Sleep(500 * time.Millisecond)
	stop()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(counts) != items {
		t.Fatalf("expected %d distinct items, got %d", items, len(counts))
	}
	for id, count := range counts {
		if count != 1 {
			t.Fatalf("item %s was delivered %d times to a queue group", id, count)
		}
	}
}

// TestIntegrationJetStreamRedeliversUnacknowledgedWork is the distributed half of the second
// criterion: a worker that dies mid-flight must not take its work with it.
func TestIntegrationJetStreamRedeliversUnacknowledgedWork(t *testing.T) {
	bus := connect(t, natsbus.Options{
		Durable: "redelivery-test",
		// Short, so a test can observe the redelivery a production deployment would see
		// after its own AckWait.
		AckWait: 2 * time.Second,
	})
	ctx := t.Context()

	event := workItem("redelivered", "")

	var deliveries atomic.Int64
	redelivered := make(chan struct{})
	var once sync.Once

	// The first delivery is abandoned without acknowledgement, the way a process that dies
	// mid-handler abandons it.
	firstCtx, killFirst := context.WithCancel(ctx)
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_ = bus.Subscribe(firstCtx, eventbus.SubscriptionSpec{
			Concurrency: 1, DrainTimeout: 100 * time.Millisecond,
		}, func(handlerCtx context.Context, _ domain.OutboxEvent) error {
			deliveries.Add(1)
			<-handlerCtx.Done()
			return handlerCtx.Err()
		})
	}()

	if err := bus.Publish(ctx, event); err != nil {
		t.Fatalf("publish: %v", err)
	}

	deadline := time.After(15 * time.Second)
	for deliveries.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the first consumer never received the item")
		case <-time.After(20 * time.Millisecond):
		}
	}
	killFirst()
	<-firstDone

	// A replacement receives it again once the acknowledgement deadline passes.
	secondCtx, stopSecond := context.WithCancel(ctx)
	defer stopSecond()
	go func() {
		_ = bus.Subscribe(secondCtx, eventbus.SubscriptionSpec{
			Concurrency: 1, DrainTimeout: time.Second,
		}, func(_ context.Context, got domain.OutboxEvent) error {
			if got.ID == event.ID {
				once.Do(func() { close(redelivered) })
			}
			return nil
		})
	}()

	select {
	case <-redelivered:
	case <-time.After(30 * time.Second):
		t.Fatal("unacknowledged work was never redelivered; a dead worker took it with it")
	}
}

// TestIntegrationJetStreamPreservesTheWorkItem checks the round trip carries everything a
// handler needs, since a transport that quietly drops a field produces bugs far from here.
func TestIntegrationJetStreamPreservesTheWorkItem(t *testing.T) {
	bus := connect(t, natsbus.Options{Durable: "fidelity-test"})
	ctx := t.Context()

	sent := workItem("fidelity", domain.PartitionOf("01a00000-0000-7000-8000-000000000001", "record-1"))
	sent.TraceParent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	sent.Attempts = 2

	got := make(chan domain.OutboxEvent, 1)
	consumerCtx, stop := context.WithCancel(ctx)
	defer stop()

	go func() {
		_ = bus.Subscribe(consumerCtx, eventbus.SubscriptionSpec{
			Concurrency: 1, DrainTimeout: time.Second,
		}, func(_ context.Context, event domain.OutboxEvent) error {
			got <- event
			return nil
		})
	}()

	if err := bus.Publish(ctx, sent); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case received := <-got:
		if received.ID != sent.ID {
			t.Fatalf("id changed: %s then %s", sent.ID, received.ID)
		}
		if received.PartitionKey != sent.PartitionKey {
			t.Fatalf("partition key lost: %q", received.PartitionKey)
		}
		if received.TraceParent != sent.TraceParent {
			// Without this, a worker span starts a new trace and the ingest request it
			// belongs to becomes unfindable (AGENTS.md section 30.1).
			t.Fatalf("trace context lost: %q", received.TraceParent)
		}
		if received.Attempts != sent.Attempts {
			t.Fatalf("attempt count lost: %d", received.Attempts)
		}
		if received.DedupeKey != sent.DedupeKey {
			t.Fatalf("dedupe key lost: %q", received.DedupeKey)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the item never arrived")
	}
}

// TestIntegrationJetStreamDeduplicatesRepublishedWork covers the case a retrying publisher
// creates: the same logical work sent twice.
func TestIntegrationJetStreamDeduplicatesRepublishedWork(t *testing.T) {
	bus := connect(t, natsbus.Options{Durable: "dedupe-test"})
	ctx := t.Context()

	event := workItem("published-twice", "")

	var deliveries atomic.Int64
	consumerCtx, stop := context.WithCancel(ctx)
	defer stop()

	go func() {
		_ = bus.Subscribe(consumerCtx, eventbus.SubscriptionSpec{
			Concurrency: 1, DrainTimeout: time.Second,
		}, func(_ context.Context, _ domain.OutboxEvent) error {
			deliveries.Add(1)
			return nil
		})
	}()

	// Twice in quick succession, inside the duplicate window.
	if err := bus.Publish(ctx, event); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := bus.Publish(ctx, event); err != nil {
		t.Fatalf("republish: %v", err)
	}

	deadline := time.After(15 * time.Second)
	for deliveries.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the item never arrived")
		case <-time.After(20 * time.Millisecond):
		}
	}
	time.Sleep(time.Second)

	if got := deliveries.Load(); got != 1 {
		t.Fatalf("a republished item was delivered %d times; the dedupe key should collapse it", got)
	}
}
