package eventbus_test

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
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

// fleet runs several independent consumers against one queue, the way separate worker
// processes would.
type fleet struct {
	buses  []*eventbus.Outbox
	cancel context.CancelFunc
	wg     sync.WaitGroup
	errs   chan error
}

// startFleet launches n consumers, each with its own worker identity.
func startFleet(t *testing.T, store eventbus.Store, n int, spec eventbus.SubscriptionSpec, handler eventbus.Handler) *fleet {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	f := &fleet{cancel: cancel, errs: make(chan error, n)}

	for range n {
		// A separate Outbox per member: each gets its own worker id, which is what makes
		// these behave like distinct processes rather than one process with goroutines.
		bus := eventbus.NewOutbox(store, nil, nil, nil)
		f.buses = append(f.buses, bus)

		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			if err := bus.Subscribe(ctx, spec, handler); err != nil && ctx.Err() == nil {
				f.errs <- err
			}
		}()
	}
	return f
}

func (f *fleet) stop() {
	f.cancel()
	f.wg.Wait()
	close(f.errs)
}

// TestIntegrationFleetProcessesEachItemExactlyOnce is phase 14's first acceptance criterion:
// components scale horizontally without duplicate knowledge.
//
// Eight consumers, two hundred items. Duplicate delivery is the failure this exists to
// exclude, so the test counts per item rather than in aggregate — a total of two hundred could
// still mean one item twice and another never.
func TestIntegrationFleetProcessesEachItemExactlyOnce(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := t.Context()

	const items = 200
	published := make([]domain.OutboxEvent, 0, items)
	for i := range items {
		published = append(published, workItem(f, fmt.Sprintf("scale-%d", i), ""))
	}
	if err := f.Store.PublishOutbox(ctx, published...); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var (
		mu     sync.Mutex
		counts = map[domain.OutboxEventID]int{}
		done   = make(chan struct{})
		seen   atomic.Int64
	)

	fleet := startFleet(t, f.Store, 8, eventbus.SubscriptionSpec{
		Concurrency: 4, BatchSize: 8, PollInterval: 20 * time.Millisecond,
		Lease: 30 * time.Second,
	}, func(_ context.Context, event domain.OutboxEvent) error {
		mu.Lock()
		counts[event.ID]++
		mu.Unlock()

		if seen.Add(1) == items {
			close(done)
		}
		return nil
	})
	defer fleet.stop()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("only %d of %d items were processed", seen.Load(), items)
	}

	// Give any duplicate delivery a moment to show up rather than declaring victory the
	// instant the count is reached.
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	if len(counts) != items {
		t.Fatalf("expected %d distinct items, got %d", items, len(counts))
	}
	for id, count := range counts {
		if count != 1 {
			t.Fatalf("item %s was processed %d times; a fleet must not duplicate work",
				id, count)
		}
	}
}

// TestIntegrationPartitionedWorkIsNeverConcurrentWithItself covers AGENTS.md section 28.3.
//
// This is the general form of a bug phase 10 hit with CDC and patched with an advisory lock
// inside one stage: two events about the same record, processed at once, each seeing the state
// before the other. The partition key fixes it in the claim, for every kind of work.
func TestIntegrationPartitionedWorkIsNeverConcurrentWithItself(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := t.Context()

	const (
		partitions = 5
		perKey     = 8
	)

	var published []domain.OutboxEvent
	for p := range partitions {
		key := domain.PartitionOf(f.Primary.Workspace.ID, fmt.Sprintf("record-%d", p))
		for i := range perKey {
			published = append(published,
				workItem(f, fmt.Sprintf("part-%d-%d", p, i), key))
		}
	}
	if err := f.Store.PublishOutbox(ctx, published...); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var (
		mu       sync.Mutex
		inFlight = map[string]bool{}
		overlaps atomic.Int64
		order    = map[string][]string{}
		seen     atomic.Int64
		done     = make(chan struct{})
	)

	fleet := startFleet(t, f.Store, 6, eventbus.SubscriptionSpec{
		Concurrency: 4, BatchSize: 8, PollInterval: 20 * time.Millisecond,
		Lease: 30 * time.Second,
	}, func(_ context.Context, event domain.OutboxEvent) error {
		mu.Lock()
		if inFlight[event.PartitionKey] {
			overlaps.Add(1)
		}
		inFlight[event.PartitionKey] = true
		order[event.PartitionKey] = append(order[event.PartitionKey], event.DedupeKey)
		mu.Unlock()

		// Long enough that an unserialized claim would overlap visibly rather than by
		// luck of scheduling.
		time.Sleep(15 * time.Millisecond)

		mu.Lock()
		inFlight[event.PartitionKey] = false
		mu.Unlock()

		if seen.Add(1) == partitions*perKey {
			close(done)
		}
		return nil
	})
	defer fleet.stop()

	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatalf("only %d of %d items were processed", seen.Load(), partitions*perKey)
	}

	if got := overlaps.Load(); got != 0 {
		t.Fatalf("%d overlapping executions within a partition; ordering was not enforced", got)
	}

	// And within a partition, publication order was preserved: a later event must not
	// overtake an earlier one from the same record.
	mu.Lock()
	defer mu.Unlock()
	for key, processed := range order {
		if len(processed) != perKey {
			t.Fatalf("partition %s processed %d items, expected %d", key, len(processed), perKey)
		}
		for i, dedupe := range processed {
			expected := fmt.Sprintf("-%d", i)
			if len(dedupe) < len(expected) || dedupe[len(dedupe)-len(expected):] != expected {
				t.Fatalf("partition %s ran out of order: %v", key, processed)
			}
		}
	}
}

// TestIntegrationUnpartitionedWorkStaysParallel is the counterweight: ordering must not be
// bought by making the fleet serial.
func TestIntegrationUnpartitionedWorkStaysParallel(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := t.Context()

	const items = 40
	var published []domain.OutboxEvent
	for i := range items {
		published = append(published, workItem(f, fmt.Sprintf("parallel-%d", i), ""))
	}
	if err := f.Store.PublishOutbox(ctx, published...); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var (
		mu        sync.Mutex
		running   int
		peak      int
		processed atomic.Int64
		done      = make(chan struct{})
	)

	fleet := startFleet(t, f.Store, 4, eventbus.SubscriptionSpec{
		Concurrency: 4, BatchSize: 8, PollInterval: 20 * time.Millisecond,
		Lease: 30 * time.Second,
	}, func(_ context.Context, _ domain.OutboxEvent) error {
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		mu.Unlock()

		time.Sleep(20 * time.Millisecond)

		mu.Lock()
		running--
		mu.Unlock()

		if processed.Add(1) == items {
			close(done)
		}
		return nil
	})
	defer fleet.stop()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("only %d of %d items were processed", processed.Load(), items)
	}

	mu.Lock()
	defer mu.Unlock()
	if peak < 2 {
		t.Fatalf("unpartitioned work never ran concurrently (peak %d); the partition rule "+
			"is serializing work that has no ordering requirement", peak)
	}
}

// TestIntegrationRestartsDoNotLoseAcceptedWork is phase 14's second acceptance criterion.
//
// A worker is killed while holding leases — the ordinary case of a pod being rescheduled —
// and a replacement finishes the work. Nothing accepted may be lost, which is the one failure
// a durable queue exists to prevent.
func TestIntegrationRestartsDoNotLoseAcceptedWork(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := t.Context()

	const items = 30
	var published []domain.OutboxEvent
	for i := range items {
		published = append(published, workItem(f, fmt.Sprintf("restart-%d", i), ""))
	}
	if err := f.Store.PublishOutbox(ctx, published...); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var (
		mu        sync.Mutex
		completed = map[domain.OutboxEventID]int{}
		abandoned atomic.Int64
	)

	// The first worker takes work and dies partway through: its handler blocks until the
	// context is cancelled, so those items stay claimed with nothing to finish them. A
	// short lease is what lets the reaper recover them inside a test's patience.
	firstCtx, killFirst := context.WithCancel(context.Background())
	firstDone := make(chan struct{})
	first := eventbus.NewOutbox(f.Store, nil, nil, nil)

	go func() {
		defer close(firstDone)
		_ = first.Subscribe(firstCtx, eventbus.SubscriptionSpec{
			Concurrency: 2, BatchSize: 4, PollInterval: 20 * time.Millisecond,
			Lease: 2 * time.Second, DrainTimeout: 100 * time.Millisecond,
		}, func(handlerCtx context.Context, _ domain.OutboxEvent) error {
			abandoned.Add(1)
			<-handlerCtx.Done()
			// Never acknowledged: this is a process that died mid-flight, not one that
			// failed cleanly.
			return handlerCtx.Err()
		})
	}()

	// Wait until it is genuinely holding work before killing it.
	deadline := time.After(20 * time.Second)
	for abandoned.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("the first worker never claimed anything")
		case <-time.After(20 * time.Millisecond):
		}
	}
	killFirst()
	<-firstDone

	// The replacement fleet finishes everything, including whatever the dead worker was
	// holding — recovered by the reaper once its lease expired.
	var (
		seen atomic.Int64
		done = make(chan struct{})
	)
	replacement := startFleet(t, f.Store, 3, eventbus.SubscriptionSpec{
		Concurrency: 4, BatchSize: 8, PollInterval: 20 * time.Millisecond,
		Lease: 30 * time.Second,
	}, func(_ context.Context, event domain.OutboxEvent) error {
		mu.Lock()
		completed[event.ID]++
		total := len(completed)
		mu.Unlock()

		if total == items && seen.Add(1) >= 1 {
			select {
			case <-done:
			default:
				close(done)
			}
		}
		return nil
	})
	defer replacement.stop()

	select {
	case <-done:
	case <-time.After(90 * time.Second):
		mu.Lock()
		got := len(completed)
		mu.Unlock()
		t.Fatalf("only %d of %d items survived the restart", got, items)
	}

	mu.Lock()
	processed := len(completed)
	mu.Unlock()
	if processed != items {
		t.Fatalf("expected every item to be processed, got %d of %d", processed, items)
	}

	// Every item is accounted for in the ledger too, not merely in the handler's memory.
	//
	// Polled rather than read once: the handler returns before the bus has recorded the
	// outcome, so the last item is legitimately still claimed for a moment. Asserting
	// immediately would be testing how fast this machine is.
	settle := time.After(30 * time.Second)
	for {
		stuck := 0
		for _, status := range []domain.OutboxStatus{domain.OutboxPending, domain.OutboxClaimed} {
			remaining, err := f.Store.ListOutbox(ctx, f.Primary.Workspace.ID, status, 100)
			if err != nil {
				t.Fatalf("list outbox: %v", err)
			}
			stuck += len(remaining)
		}
		if stuck == 0 {
			return
		}
		select {
		case <-settle:
			t.Fatalf("%d items were still unfinished in the queue after the restart", stuck)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestIntegrationRateLimitThrottlesOneWorker checks the backpressure knob does something
// measurable, since a limiter that silently does nothing is worse than none.
func TestIntegrationRateLimitThrottlesOneWorker(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := t.Context()

	const items = 40
	var published []domain.OutboxEvent
	for i := range items {
		published = append(published, workItem(f, fmt.Sprintf("rate-%d", i), ""))
	}
	if err := f.Store.PublishOutbox(ctx, published...); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var processed atomic.Int64
	bus := eventbus.NewOutbox(f.Store, nil, nil, nil)

	runCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = bus.Subscribe(runCtx, eventbus.SubscriptionSpec{
		Concurrency: 8, BatchSize: 16, PollInterval: 10 * time.Millisecond,
		Lease: 30 * time.Second, DrainTimeout: time.Second,
		// Ten per second with a one-second burst: in two seconds a compliant worker takes
		// roughly twenty, and an unthrottled one would take all forty immediately.
		MaxEventsPerSecond: 10,
	}, func(_ context.Context, _ domain.OutboxEvent) error {
		processed.Add(1)
		return nil
	})

	got := processed.Load()
	if got == 0 {
		t.Fatal("the rate limiter stopped all work rather than throttling it")
	}
	if got > 30 {
		t.Fatalf("a limit of 10/s let %d items through in two seconds", got)
	}
}

// TestIntegrationPartitionedWorkDoesNotWaitForThePollInterval is the counterpart to the
// ordering guarantee: serializing a partition must not also slow it down.
//
// Items queued behind an in-flight one are deliberately passed over by the claim, so nothing
// else would make the worker look again until its next poll. That would move a partitioned
// stream at one item per poll interval however idle the fleet is — a real deployment with a
// several-minute interval would appear to have stalled.
func TestIntegrationPartitionedWorkDoesNotWaitForThePollInterval(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := t.Context()

	const items = 5
	partition := domain.PartitionOf(f.Primary.Workspace.ID, "record-serial")

	var published []domain.OutboxEvent
	for i := range items {
		published = append(published, workItem(f, fmt.Sprintf("serial-%d", i), partition))
	}
	if err := f.Store.PublishOutbox(ctx, published...); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var processed atomic.Int64
	done := make(chan struct{})
	var once sync.Once

	bus := eventbus.NewOutbox(f.Store, nil, nil, nil)
	runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	go func() {
		_ = bus.Subscribe(runCtx, eventbus.SubscriptionSpec{
			Concurrency: 4, BatchSize: 8,
			// Long enough that draining the partition on poll timing alone would take
			// well over two minutes.
			PollInterval: 30 * time.Second,
			Lease:        60 * time.Second, DrainTimeout: time.Second,
		}, func(_ context.Context, _ domain.OutboxEvent) error {
			if processed.Add(1) == items {
				once.Do(func() { close(done) })
			}
			return nil
		})
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("only %d of %d partitioned items were processed; the queue moves at one "+
			"item per poll interval", processed.Load(), items)
	}
}

// TestIntegrationNotifyWakesAnIdleConsumer covers the push path phase 14 adds.
//
// A worker that has backed off to a long idle interval must still start promptly when
// something tells it work has arrived. This is the whole value of putting NATS in front of
// the ledger: the claim stays in PostgreSQL, and the bus only shortens the wait. If the
// notification did nothing, a distributed deployment would be slower than a single node.
func TestIntegrationNotifyWakesAnIdleConsumer(t *testing.T) {
	f := pgtest.NewFixture(t)

	started := make(chan struct{})
	bus := eventbus.NewOutbox(f.Store, nil, nil, nil)

	runCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bus.Subscribe(runCtx, eventbus.SubscriptionSpec{
			Concurrency: 2, BatchSize: 4,
			// A poll interval long enough that finishing quickly can only mean the
			// notification worked, not that a poll happened to land.
			PollInterval: 30 * time.Second,
			Lease:        30 * time.Second, DrainTimeout: time.Second,
		}, func(_ context.Context, _ domain.OutboxEvent) error {
			close(started)
			return nil
		})
	}()

	// Let the consumer reach its first long sleep, so the wake-up is what ends it.
	time.Sleep(300 * time.Millisecond)

	if err := f.Store.PublishOutbox(t.Context(), workItem(f, "notified", "")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	bus.Notify()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("a notified consumer still slept through its poll interval")
	}
	cancel()
	<-done
}

// TestIntegrationIdleBackoffStretchesThePollInterval checks the other half: a fleet with
// nothing to do must stop hammering the ledger.
//
// Without this, load on the database is proportional to fleet size rather than to work, which
// is precisely the thing that stops a deployment from scaling out (AGENTS.md section 27.5).
func TestIntegrationIdleBackoffStretchesThePollInterval(t *testing.T) {
	f := pgtest.NewFixture(t)

	counter := &countingStore{Store: f.Store}
	bus := eventbus.NewOutbox(counter, nil, nil, nil)

	runCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_ = bus.Subscribe(runCtx, eventbus.SubscriptionSpec{
		Concurrency: 2, BatchSize: 4,
		PollInterval: 10 * time.Millisecond,
		Lease:        30 * time.Second, DrainTimeout: time.Second,
		// Doubling from 10ms, an idle worker reaches the cap within the first second.
		IdleBackoffMax: 500 * time.Millisecond,
	}, func(_ context.Context, _ domain.OutboxEvent) error { return nil })

	// Doubling to the configured 500ms gives roughly eight polls in two seconds. Twenty
	// would still catch a worker that ignored the setting and fell back to the default
	// cap, and two hundred is what no backoff at all looks like.
	if polls := counter.claims.Load(); polls > 20 {
		t.Fatalf("an idle worker polled %d times in two seconds; the backoff is not applied", polls)
	}
}

// countingStore counts claim attempts, which is what an idle worker costs the database.
type countingStore struct {
	eventbus.Store
	claims atomic.Int64
}

func (s *countingStore) ClaimOutbox(ctx context.Context, topics []string, worker string,
	lease time.Duration, limit int) ([]domain.OutboxEvent, error) {
	s.claims.Add(1)
	return s.Store.ClaimOutbox(ctx, topics, worker, lease, limit)
}

// workItem builds one publishable unit of work.
func workItem(f *pgtest.Fixture, key, partition string) domain.OutboxEvent {
	payload, _ := json.Marshal(map[string]any{"key": key})
	return domain.OutboxEvent{
		ID:            domain.OutboxEventID(domain.NewUUIDString()),
		WorkspaceID:   f.Primary.Workspace.ID,
		GraphSpaceID:  f.Primary.GraphSpace.ID,
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
