package eventbus_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/eventbus"
	"github.com/gimantha/strata/internal/store/ledger"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// publish enqueues n work items for a tenant.
func publish(t *testing.T, store *ledger.Store, tenant pgtest.Tenant, n int, maxAttempts int) []domain.OutboxEvent {
	t.Helper()

	events := make([]domain.OutboxEvent, 0, n)
	for i := 0; i < n; i++ {
		payload, err := json.Marshal(map[string]any{"index": i})
		if err != nil {
			t.Fatalf("encode payload: %v", err)
		}
		events = append(events, domain.OutboxEvent{
			WorkspaceID:   tenant.Workspace.ID,
			GraphSpaceID:  tenant.GraphSpace.ID,
			Topic:         domain.TopicIngestPipeline,
			EventType:     domain.EventTypeSourceEventAccepted,
			SchemaVersion: domain.OutboxSchemaVersion,
			Payload:       payload,
			DedupeKey:     fmt.Sprintf("test-work-%d", i),
			Status:        domain.OutboxPending,
			MaxAttempts:   maxAttempts,
		})
	}
	if err := store.PublishOutbox(context.Background(), events...); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return events
}

func testSpec(concurrency int) eventbus.SubscriptionSpec {
	return eventbus.SubscriptionSpec{
		Topics:       []string{domain.TopicIngestPipeline},
		Concurrency:  concurrency,
		BatchSize:    4,
		Lease:        10 * time.Second,
		PollInterval: 20 * time.Millisecond,
		MaxAttempts:  3,
		BackoffBase:  10 * time.Millisecond,
		BackoffMax:   50 * time.Millisecond,
		DrainTimeout: 5 * time.Second,
	}
}

func TestIntegrationPublishIsIdempotent(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()

	events := publish(t, f.Store, f.Primary, 3, 3)
	// Republishing the same logical work must not enqueue it twice.
	if err := f.Store.PublishOutbox(ctx, events...); err != nil {
		t.Fatalf("republish: %v", err)
	}

	pending, err := f.Store.ListOutbox(ctx, f.Primary.Workspace.ID, domain.OutboxPending, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("expected 3 work items after republishing, found %d", len(pending))
	}
}

func TestIntegrationConcurrentWorkersProcessEachItemExactlyOnce(t *testing.T) {
	f := pgtest.NewFixture(t)
	const total = 40
	publish(t, f.Store, f.Primary, total, 3)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var (
		mu        sync.Mutex
		processed = map[domain.OutboxEventID]int{}
		done      = make(chan struct{})
		closeOnce sync.Once
	)

	handler := func(ctx context.Context, event domain.OutboxEvent) error {
		mu.Lock()
		processed[event.ID]++
		if len(processed) == total {
			closeOnce.Do(func() { close(done) })
		}
		mu.Unlock()
		return nil
	}

	// Three workers share one queue. FOR UPDATE SKIP LOCKED is what keeps them from
	// handing the same item to two of them (AGENTS.md section 28.3).
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		bus := eventbus.NewOutbox(f.Store, nil, nil, nil)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := bus.Subscribe(ctx, testSpec(4), handler); err != nil {
				t.Errorf("subscribe: %v", err)
			}
		}()
	}

	select {
	case <-done:
	case <-ctx.Done():
		mu.Lock()
		count := len(processed)
		mu.Unlock()
		t.Fatalf("timed out after processing %d of %d items", count, total)
	}
	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(processed) != total {
		t.Fatalf("processed %d distinct items, want %d", len(processed), total)
	}
	for id, times := range processed {
		if times != 1 {
			t.Fatalf("item %s was processed %d times; concurrent workers must not duplicate work", id, times)
		}
	}

	succeeded, err := f.Store.ListOutbox(context.Background(), f.Primary.Workspace.ID, domain.OutboxSucceeded, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(succeeded) != total {
		t.Fatalf("expected %d items marked succeeded, found %d", total, len(succeeded))
	}
}

func TestIntegrationExpiredLeaseIsReclaimedAndCompletedOnce(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()
	publish(t, f.Store, f.Primary, 1, 3)

	// Simulate a worker that claimed an item and then died: the claim exists, nothing
	// acknowledged it, and its lease is about to expire.
	claimed, err := f.Store.ClaimOutbox(ctx, nil, "worker-that-died", time.Millisecond, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected to claim 1 item, got %d", len(claimed))
	}
	if claimed[0].Attempts != 1 {
		t.Fatalf("claiming must count an attempt, got %d", claimed[0].Attempts)
	}
	time.Sleep(50 * time.Millisecond)

	reaped, err := f.Store.ReapExpiredClaims(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("expected to reap 1 abandoned claim, got %d", reaped)
	}

	// Accepted work is never lost: a surviving worker picks it up and finishes it.
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var (
		mu    sync.Mutex
		count int
		done  = make(chan struct{})
	)
	bus := eventbus.NewOutbox(f.Store, nil, nil, nil)
	go func() {
		_ = bus.Subscribe(runCtx, testSpec(1), func(context.Context, domain.OutboxEvent) error {
			mu.Lock()
			count++
			if count == 1 {
				close(done)
			}
			mu.Unlock()
			return nil
		})
	}()

	select {
	case <-done:
	case <-runCtx.Done():
		t.Fatal("the reclaimed item was never processed; accepted work must not be lost")
	}

	// The handler signals from inside itself, so wait for the acknowledgement that
	// follows it rather than racing the worker.
	item := waitForStatus(t, f.Store, claimed[0].ID, domain.OutboxSucceeded)
	cancel()

	if item.Attempts != 2 {
		t.Fatalf("expected the retry to be the second attempt, got %d", item.Attempts)
	}

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("the reclaimed item must run exactly once, ran %d times", count)
	}
}

// waitForStatus polls until a work item reaches the expected status.
func waitForStatus(t *testing.T, store *ledger.Store, id domain.OutboxEventID, want domain.OutboxStatus) domain.OutboxEvent {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	var last domain.OutboxEvent
	for time.Now().Before(deadline) {
		item, err := store.GetOutbox(context.Background(), id)
		if err != nil {
			t.Fatalf("load item: %v", err)
		}
		if item.Status == want {
			return item
		}
		last = item
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("item %s never reached status %s (last seen %s: %s)", id, want, last.Status, last.LastError)
	return domain.OutboxEvent{}
}

func TestIntegrationRenewClaimOnlyForTheHoldingWorker(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()
	publish(t, f.Store, f.Primary, 1, 3)

	claimed, err := f.Store.ClaimOutbox(ctx, nil, "worker-a", 5*time.Second, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (%d items)", err, len(claimed))
	}
	before := claimed[0].ClaimExpiresAt

	held, err := f.Store.RenewClaim(ctx, claimed[0].ID, "worker-a", 60*time.Second)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if !held {
		t.Fatal("the holding worker must be able to extend its lease")
	}

	after, err := f.Store.GetOutbox(ctx, claimed[0].ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if before != nil && after.ClaimExpiresAt != nil && !after.ClaimExpiresAt.After(*before) {
		t.Fatal("renewal must push the lease expiry forward")
	}

	// Another worker must not be able to extend a lease it does not hold.
	held, err = f.Store.RenewClaim(ctx, claimed[0].ID, "worker-b", 60*time.Second)
	if err != nil {
		t.Fatalf("renew by another worker: %v", err)
	}
	if held {
		t.Fatal("a worker must not be able to renew another worker's claim")
	}
}

func TestIntegrationNonRetryableFailureDeadLettersImmediately(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()
	publish(t, f.Store, f.Primary, 1, 5)

	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	attempts := make(chan struct{}, 8)
	bus := eventbus.NewOutbox(f.Store, nil, nil, nil)
	go func() {
		_ = bus.Subscribe(runCtx, testSpec(1), func(context.Context, domain.OutboxEvent) error {
			attempts <- struct{}{}
			// Malformed input never becomes valid by waiting, so it must not be retried
			// against its attempt budget (AGENTS.md section 28.4).
			return domain.Errorf(domain.CodeInvalidArgument, "test", "payload is not understood")
		})
	}()

	select {
	case <-attempts:
	case <-runCtx.Done():
		t.Fatal("the handler was never invoked")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		items, err := f.Store.ListOutbox(ctx, f.Primary.Workspace.ID, domain.OutboxDead, 10)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(items) == 1 {
			if items[0].Attempts != 1 {
				t.Fatalf("non-retryable failures must not be retried, saw %d attempts", items[0].Attempts)
			}
			if items[0].ErrorClass != domain.ErrorClassInvalidSourceData {
				t.Fatalf("expected the failure to be classified as bad input, got %q", items[0].ErrorClass)
			}
			if items[0].LastError == "" {
				t.Fatal("a dead-lettered item must retain why it failed")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the item was never dead-lettered")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	// Nothing is discarded: an operator can revive it once the cause is fixed.
	revived, err := f.Store.ReviveDeadLetters(ctx, f.Primary.Workspace.ID, nil)
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	if revived != 1 {
		t.Fatalf("expected to revive 1 item, got %d", revived)
	}
	pending, err := f.Store.ListOutbox(ctx, f.Primary.Workspace.ID, domain.OutboxPending, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pending) != 1 || pending[0].Attempts != 0 {
		t.Fatalf("a revived item must return to pending with a fresh budget: %+v", pending)
	}
}

func TestIntegrationRetryableFailureIsRetriedThenDeadLettered(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()

	// Two attempts allowed, so the second failure exhausts the budget.
	publish(t, f.Store, f.Primary, 1, 2)

	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	var (
		mu    sync.Mutex
		calls int
	)
	bus := eventbus.NewOutbox(f.Store, nil, nil, nil)
	go func() {
		_ = bus.Subscribe(runCtx, testSpec(1), func(context.Context, domain.OutboxEvent) error {
			mu.Lock()
			calls++
			mu.Unlock()
			return domain.Errorf(domain.CodeProviderUnavailable, "test", "upstream is down")
		})
	}()

	deadline := time.Now().Add(15 * time.Second)
	for {
		items, err := f.Store.ListOutbox(ctx, f.Primary.Workspace.ID, domain.OutboxDead, 10)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(items) == 1 {
			if items[0].Attempts != 2 {
				t.Fatalf("expected the item to use its whole attempt budget, saw %d", items[0].Attempts)
			}
			if items[0].ErrorClass != domain.ErrorClassTransient {
				t.Fatalf("expected a transient classification, got %q", items[0].ErrorClass)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the item was never dead-lettered after exhausting its retries")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if calls < 2 {
		t.Fatalf("a transient failure must be retried, handler ran %d time(s)", calls)
	}
}

func TestIntegrationClaimRespectsTopicFilter(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()
	publish(t, f.Store, f.Primary, 1, 3)

	// A consumer subscribed to another topic must not take this work.
	claimed, err := f.Store.ClaimOutbox(ctx, []string{"some.other.topic"}, "worker-a", time.Minute, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("expected no items for an unrelated topic, got %d", len(claimed))
	}

	claimed, err = f.Store.ClaimOutbox(ctx, []string{domain.TopicIngestPipeline}, "worker-a", time.Minute, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 item for the subscribed topic, got %d", len(claimed))
	}
}

func TestIntegrationQueueDepthReportsLag(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()
	publish(t, f.Store, f.Primary, 5, 3)

	depth, err := f.Store.OutboxDepth(ctx)
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	if depth.ByStatus[string(domain.OutboxPending)] != 5 {
		t.Fatalf("expected 5 pending items, got %v", depth.ByStatus)
	}
	if depth.OldestPendingAge < 0 {
		t.Fatalf("queue age must not be negative, got %f", depth.OldestPendingAge)
	}
}
