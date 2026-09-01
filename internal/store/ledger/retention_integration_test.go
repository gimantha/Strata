package ledger_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gimantha/strata/internal/store/ledger"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

// Retention against real PostgreSQL.
//
// The behaviour under test is partition management and bulk deletion, both of which are
// PostgreSQL's semantics rather than this repository's. A fake would assert whatever its
// author believed DROP TABLE on a partition does.

func seedTrace(tb testing.TB, f *pgtest.Fixture, at time.Time) {
	tb.Helper()
	_, err := f.Store.Pool().Exec(tb.Context(), `
		INSERT INTO retrieval_traces (id, workspace_id, graph_space_id, query_hash, created_at, query_time)
		VALUES ($1, $2, $3, 'hash', $4, $4)`,
		uuid.New(), f.Primary.Workspace.ID, f.Primary.GraphSpace.ID, at)
	if err != nil {
		tb.Fatalf("seed trace at %s: %v", at.Format(time.RFC3339), err)
	}
}

func countRows(tb testing.TB, f *pgtest.Fixture, table string) int64 {
	tb.Helper()
	var n int64
	if err := f.Store.Pool().QueryRow(tb.Context(),
		fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&n); err != nil {
		tb.Fatalf("count %s: %v", table, err)
	}
	return n
}

// A partitioned table needs partitions before anything writes to it, and it needs them
// again every month forever. Nothing else creates them.
func TestIntegrationRetentionKeepsPartitionsAheadOfTheClock(t *testing.T) {
	f := pgtest.NewFixture(t)

	// Six months from now: a window the migration did not create.
	future := time.Now().UTC().AddDate(0, 6, 0)
	report, err := f.Store.Prune(t.Context(), ledger.RetentionPolicy{}, future)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(report.PartitionsCreated) == 0 {
		t.Fatal("no partitions were created for a clock six months ahead")
	}

	// A trace written at that future date must have somewhere to land other than the
	// default partition, which is the state that cannot be recovered from automatically.
	seedTrace(t, f, future)
	if n := countRows(t, f, "retrieval_traces_default"); n != 0 {
		t.Errorf("%d rows landed in the default partition; the window did not cover the clock", n)
	}
}

// Dropping a partition is the reason to partition this table. It has to actually remove the
// rows, and it has to leave everything inside the retention window alone.
func TestIntegrationExpiredTracePartitionsAreDropped(t *testing.T) {
	f := pgtest.NewFixture(t)
	now := time.Now().UTC()

	// Walk the clock forward the way a long-running deployment does, so the historical
	// partitions exist because a sweep created them rather than because the test reached
	// into the schema. A trace inserted for a month with no partition would land in the
	// default one, which is a different case and is covered below.
	for age := 6; age >= 0; age-- {
		if _, err := f.Store.Prune(t.Context(), ledger.RetentionPolicy{}, now.AddDate(0, -age, 0)); err != nil {
			t.Fatalf("prepare partitions: %v", err)
		}
	}
	for _, age := range []int{0, 1, 4, 5} {
		seedTrace(t, f, now.AddDate(0, -age, 0))
	}
	if n := countRows(t, f, "retrieval_traces_default"); n != 0 {
		t.Fatalf("%d traces landed in the default partition; the fixture is not testing partitions", n)
	}
	before := countRows(t, f, "retrieval_traces")
	if before < 4 {
		t.Fatalf("seeding failed: %d traces", before)
	}

	// Keep 90 days. The four- and five-month-old partitions are wholly outside it.
	report, err := f.Store.Prune(t.Context(), ledger.RetentionPolicy{Traces: 90 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(report.PartitionsDropped) == 0 {
		t.Fatal("nothing was dropped, though two partitions were entirely expired")
	}
	if report.TracesDropped == 0 {
		t.Error("partitions were dropped but no rows were counted as removed")
	}

	var oldest time.Time
	if err := f.Store.Pool().QueryRow(t.Context(),
		`SELECT COALESCE(min(created_at), now()) FROM retrieval_traces`).Scan(&oldest); err != nil {
		t.Fatalf("read oldest: %v", err)
	}
	// A partition is only dropped when all of it is expired, so the survivor is at most a
	// month older than the cutoff rather than exactly at it.
	if oldest.Before(now.AddDate(0, -4, 0)) {
		t.Errorf("a trace from %s outlived a 90-day policy", oldest.Format(time.RFC3339))
	}
	if countRows(t, f, "retrieval_traces") == 0 {
		t.Error("recent traces were removed along with the expired ones")
	}
}

// The default is to keep everything. An operator who has not asked for deletion does not
// get deletion.
func TestIntegrationTheDefaultPolicyDeletesNothing(t *testing.T) {
	f := pgtest.NewFixture(t)
	now := time.Now().UTC()

	for _, age := range []int{0, 6, 24} {
		seedTrace(t, f, now.AddDate(0, -age, 0))
	}
	before := countRows(t, f, "retrieval_traces")

	report, err := f.Store.Prune(t.Context(), ledger.RetentionPolicy{}, now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if report.Total() != 0 {
		t.Errorf("the zero policy removed %d rows", report.Total())
	}
	if after := countRows(t, f, "retrieval_traces"); after != before {
		t.Errorf("traces went from %d to %d under a policy that keeps everything", before, after)
	}
	if (ledger.RetentionPolicy{}).Any() {
		t.Error("the zero policy reports that it would delete something")
	}
}

// Age is not evidence that work is finished. An item still pending or claimed is live work,
// however long it has been sitting there, and deleting it would lose accepted work — the one
// thing the outbox exists to make impossible.
func TestIntegrationRetentionNeverDeletesUnfinishedWork(t *testing.T) {
	f := pgtest.NewFixture(t)
	now := time.Now().UTC()
	old := now.AddDate(0, -6, 0)

	for i, status := range []string{"pending", "claimed", "succeeded", "dead"} {
		var completed any
		if status == "succeeded" || status == "dead" {
			completed = old
		}
		_, err := f.Store.Pool().Exec(t.Context(), `
			INSERT INTO outbox_events (id, workspace_id, topic, event_type, schema_version,
				payload, dedupe_key, status, max_attempts, created_at, updated_at, completed_at)
			VALUES ($1, $2, 'work', 'test', 1, '{}'::jsonb, $3, $4, 5, $5, $5, $6)`,
			uuid.New(), f.Primary.Workspace.ID, fmt.Sprintf("dedupe-%d", i), status, old, completed)
		if err != nil {
			t.Fatalf("seed %s: %v", status, err)
		}
	}

	report, err := f.Store.Prune(t.Context(),
		ledger.RetentionPolicy{Outbox: time.Hour}, now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if report.OutboxDeleted != 2 {
		t.Errorf("deleted %d outbox rows, want the 2 terminal ones", report.OutboxDeleted)
	}

	var live int64
	if err := f.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM outbox_events WHERE status IN ('pending','claimed')`).Scan(&live); err != nil {
		t.Fatalf("count live work: %v", err)
	}
	if live != 2 {
		t.Errorf("%d unfinished items survived, want 2 — retention deleted live work", live)
	}
}

// The guarantee that matters most. Retention removes records of what the system did; it must
// never touch what the system knows or how it knows it. model_runs is the trap: it looks
// operational, and evidence and derivations reference it ON DELETE SET NULL, so pruning it
// would not error — it would silently cut a claim's link to the model interaction behind it.
func TestIntegrationRetentionNeverTouchesProvenance(t *testing.T) {
	f := pgtest.NewFixture(t)
	f.NewAssertions(t, 3)
	seedProvenance(t, f)

	provenance := []string{"assertions", "source_events", "evidence", "derivations", "model_runs", "entities"}
	before := map[string]int64{}
	for _, table := range provenance {
		before[table] = countRows(t, f, table)
		// The first version of this test asserted 0 == 0 for evidence, derivations and
		// model_runs, because the fixture creates none of them — vacuous for exactly the
		// three tables it was written to protect, model_runs most of all.
		if before[table] == 0 {
			t.Fatalf("%s has no rows; this test would prove nothing about it", table)
		}
	}

	// Every policy set to something aggressive, and a clock far in the future so that
	// everything is beyond every cutoff.
	aggressive := ledger.RetentionPolicy{
		Traces: time.Second, Outbox: time.Second, Audit: time.Second, PipelineRuns: time.Second,
	}
	if _, err := f.Store.Prune(t.Context(), aggressive, time.Now().UTC().AddDate(10, 0, 0)); err != nil {
		t.Fatalf("prune: %v", err)
	}

	for _, table := range provenance {
		if after := countRows(t, f, table); after != before[table] {
			t.Errorf("%s went from %d rows to %d: retention deleted provenance",
				table, before[table], after)
		}
	}

	// The link itself, not the row counts. Deleting a model run does not remove the evidence
	// row — the foreign key is ON DELETE SET NULL — so a count-only check would pass while
	// the trace from a claim to the model that proposed it had been cut.
	var linked int64
	if err := f.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM evidence WHERE model_run_id IS NOT NULL`).Scan(&linked); err != nil {
		t.Fatalf("check evidence links: %v", err)
	}
	if linked == 0 {
		t.Error("evidence no longer names the model run that proposed it")
	}
}

// seedProvenance writes the rows the fixture does not: a model run, and the evidence and
// derivation that point at it.
func seedProvenance(tb testing.TB, f *pgtest.Fixture) {
	tb.Helper()
	ctx := tb.Context()
	pool := f.Store.Pool()

	var assertionID, sourceEventID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM assertions LIMIT 1`).Scan(&assertionID); err != nil {
		tb.Fatalf("find an assertion: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM source_events LIMIT 1`).Scan(&sourceEventID); err != nil {
		tb.Fatalf("find a source event: %v", err)
	}

	episodeID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO episodes (id, workspace_id, graph_space_id, source_event_id, sequence,
			content, observed_at, recorded_at, classification)
		VALUES ($1, $2, $3, $4, 1, 'seeded', now(), now(), 'internal')`,
		episodeID, f.Primary.Workspace.ID, f.Primary.GraphSpace.ID, sourceEventID); err != nil {
		tb.Fatalf("seed episode: %v", err)
	}

	runID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO model_runs (id, workspace_id, provider, model, request_hash, status)
		VALUES ($1, $2, 'test', 'test-model', 'hash', 'succeeded')`,
		runID, f.Primary.Workspace.ID); err != nil {
		tb.Fatalf("seed model run: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO evidence (id, workspace_id, assertion_id, episode_id, source_event_id, model_run_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New(), f.Primary.Workspace.ID, assertionID, episodeID, sourceEventID, runID); err != nil {
		tb.Fatalf("seed evidence: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO derivations (id, workspace_id, graph_space_id, method, model_run_id)
		VALUES ($1, $2, $3, 'extraction', $4)`,
		uuid.New(), f.Primary.Workspace.ID, f.Primary.GraphSpace.ID, runID); err != nil {
		tb.Fatalf("seed derivation: %v", err)
	}
}

// Every process sweeps. Only one should do the work.
func TestIntegrationConcurrentSweepsDoNotBothRun(t *testing.T) {
	f := pgtest.NewFixture(t)
	now := time.Now().UTC()

	// Hold the lock from one connection, then sweep from another.
	conn, err := f.Store.Pool().Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(t.Context(),
		`SELECT pg_try_advisory_lock(8274490128)`).Scan(&got); err != nil || !got {
		t.Fatalf("could not hold the retention lock: %v", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(t.Context()), `SELECT pg_advisory_unlock(8274490128)`)
	}()

	report, err := f.Store.Prune(t.Context(), ledger.RetentionPolicy{Traces: time.Hour}, now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !report.Skipped {
		t.Error("a second sweep ran while another held the lock")
	}
	if report.Total() != 0 {
		t.Errorf("a skipped sweep still removed %d rows", report.Total())
	}
}

// A row in the default partition is a row no partition drop can reach. That happens when
// partition creation has fallen behind — a paused environment, a fleet scaled to zero — and
// it is exactly when nobody is watching, so retention has to clear it by deletion instead.
func TestIntegrationExpiredRowsInTheDefaultPartitionAreRemoved(t *testing.T) {
	f := pgtest.NewFixture(t)
	now := time.Now().UTC()

	// No partition exists for two years ago, so this lands in the default.
	seedTrace(t, f, now.AddDate(-2, 0, 0))
	seedTrace(t, f, now)
	if n := countRows(t, f, "retrieval_traces_default"); n != 1 {
		t.Fatalf("expected 1 row in the default partition, found %d", n)
	}

	report, err := f.Store.Prune(t.Context(),
		ledger.RetentionPolicy{Traces: 90 * 24 * time.Hour}, now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if report.TracesDropped != 1 {
		t.Errorf("removed %d traces, want the 1 expired row from the default partition",
			report.TracesDropped)
	}
	if n := countRows(t, f, "retrieval_traces_default"); n != 0 {
		t.Errorf("%d expired rows survived in the default partition", n)
	}
	if countRows(t, f, "retrieval_traces") != 1 {
		t.Error("the recent trace was removed too")
	}
}
