package eventbus

import (
	"testing"
	"time"
)

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	const (
		base = time.Second
		max  = 30 * time.Second
	)

	// Equal jitter keeps every delay within [d/2, d) of the exponential schedule, so
	// retries neither stampede together nor fire almost immediately.
	for attempt := 1; attempt <= 12; attempt++ {
		delay := Backoff(base, max, attempt)
		if delay <= 0 {
			t.Fatalf("attempt %d produced a non-positive delay", attempt)
		}
		if delay > max {
			t.Fatalf("attempt %d exceeded the cap: %s > %s", attempt, delay, max)
		}
	}

	// Later attempts must wait longer on average than early ones.
	var early, late time.Duration
	for i := 0; i < 200; i++ {
		early += Backoff(base, max, 1)
		late += Backoff(base, max, 5)
	}
	if late <= early*2 {
		t.Fatalf("backoff does not grow with attempts: attempt 1 averaged %s, attempt 5 averaged %s",
			early/200, late/200)
	}
}

func TestBackoffIsJittered(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 50; i++ {
		seen[Backoff(time.Second, time.Minute, 4)] = true
	}
	// Identical delays across workers would synchronize retries after an outage.
	if len(seen) < 10 {
		t.Fatalf("expected jittered delays, got only %d distinct values", len(seen))
	}
}

func TestBackoffHandlesDegenerateInput(t *testing.T) {
	if d := Backoff(0, 0, 0); d <= 0 {
		t.Fatalf("zero configuration must fall back to a positive delay, got %s", d)
	}
	// A very large attempt count must not overflow into a negative or absurd delay.
	if d := Backoff(time.Second, time.Minute, 1_000_000); d <= 0 || d > time.Minute {
		t.Fatalf("large attempt counts must stay within the cap, got %s", d)
	}
	// A max below base is treated as the base rather than producing an inverted range.
	if d := Backoff(10*time.Second, time.Second, 1); d <= 0 || d > 10*time.Second {
		t.Fatalf("inverted bounds must be normalized, got %s", d)
	}
}

func TestSubscriptionSpecDefaults(t *testing.T) {
	spec := SubscriptionSpec{}.withDefaults()
	if spec.Concurrency < 1 || spec.BatchSize < 1 || spec.MaxAttempts < 1 {
		t.Fatalf("defaults must be usable: %+v", spec)
	}
	if spec.Lease <= spec.PollInterval {
		t.Fatal("the default lease must outlast the poll interval or claims would expire mid-flight")
	}
	if spec.BackoffMax < spec.BackoffBase {
		t.Fatal("default backoff bounds must not be inverted")
	}
}
