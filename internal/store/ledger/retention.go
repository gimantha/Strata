package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// retentionLockID serializes pruning across the fleet. Pruning is idempotent, so two
// processes running it concurrently would be correct; they would also both scan the same
// rows and one would waste the work. Advisory and try-only: a process that cannot take the
// lock skips this round rather than queueing behind one already sweeping.
const retentionLockID int64 = 8274490128

// partitionMonths is how far ahead trace partitions are created.
//
// Three, not one. A deployment where nothing runs for a few weeks — a paused environment, a
// worker fleet scaled to zero — comes back to partitions that still cover the present. A row
// with nowhere to land is not lost (the default partition takes it) but a default partition
// holding rows blocks the creation of the partitions that should have held them.
const partitionMonths = 3

// RetentionPolicy is how long each kind of operational record is kept. Zero means forever,
// which is every setting's default: deleting a customer's records is not something to start
// doing because they upgraded.
//
// Only records of what the system *did* appear here. Provenance — assertions, source events,
// evidence, derivations, model runs — is never pruned by any policy, because it is what the
// system knows and how it knows it. model_runs in particular looks operational and is not:
// evidence and derivations reference it under ON DELETE SET NULL, so deleting a run would
// quietly cut the link from a claim to the model interaction that proposed it.
type RetentionPolicy struct {
	// Traces bounds retrieval_traces, which grows with query volume and describes nothing
	// anybody knows. Usually the first one worth setting.
	Traces time.Duration
	// Outbox bounds succeeded and dead work items. Pending and claimed items are never
	// touched however old they are: age is not evidence that work is finished.
	Outbox time.Duration
	// Audit bounds audit_events. Off by default and deliberately so — an audit log is
	// frequently the subject of a retention requirement rather than a candidate for one.
	Audit time.Duration
	// PipelineRuns bounds pipeline_runs and, by cascade, their stage rows. Diagnostic
	// records of how a document was processed, not provenance of what was learned.
	PipelineRuns time.Duration
}

// Any reports whether the policy would delete anything at all.
func (p RetentionPolicy) Any() bool {
	return p.Traces > 0 || p.Outbox > 0 || p.Audit > 0 || p.PipelineRuns > 0
}

// RetentionReport is what one sweep did, for logging and for tests.
type RetentionReport struct {
	PartitionsCreated []string
	PartitionsDropped []string
	TracesDropped     int64
	OutboxDeleted     int64
	AuditDeleted      int64
	PipelineDeleted   int64
	// DefaultPartitionRows is a warning rather than a count of work done. Rows here landed
	// outside every declared range, which means partition creation had fallen behind. They
	// are not pruned by dropping partitions, and the range they occupy cannot be given a
	// partition until they are gone.
	DefaultPartitionRows int64
	// Skipped is set when another process held the lock.
	Skipped bool
}

// Total reports how many rows the sweep removed.
func (r RetentionReport) Total() int64 {
	return r.TracesDropped + r.OutboxDeleted + r.AuditDeleted + r.PipelineDeleted
}

// deleteBatch bounds a single statement so a first sweep against years of history does not
// take one enormous lock or one enormous transaction.
const deleteBatch = 5000

// Prune applies a retention policy once and extends the trace partition window.
//
// Partition maintenance runs whatever the policy says, because a table that is partitioned
// needs partitions whether or not anything is being deleted from it.
func (s *Store) Prune(ctx context.Context, policy RetentionPolicy, now time.Time) (RetentionReport, error) {
	const op = "ledger.Prune"

	var report RetentionReport

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return report, mapError(err, op, "cannot acquire a connection to prune")
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, retentionLockID).Scan(&locked); err != nil {
		return report, mapError(err, op, "cannot take the retention lock")
	}
	if !locked {
		report.Skipped = true
		return report, nil
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, retentionLockID)
	}()

	created, err := ensureTracePartitions(ctx, conn, now)
	if err != nil {
		return report, err
	}
	report.PartitionsCreated = created

	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM retrieval_traces_default`).Scan(&report.DefaultPartitionRows); err != nil {
		return report, mapError(err, op, "cannot inspect the default trace partition")
	}

	if policy.Traces > 0 {
		cutoff := now.Add(-policy.Traces)
		dropped, rows, err := dropTracePartitionsBefore(ctx, conn, cutoff)
		if err != nil {
			return report, err
		}
		report.PartitionsDropped, report.TracesDropped = dropped, rows

		// The default partition is never dropped, because the range it covers is "everything
		// else" and a future row may need it. Expired rows inside it are deleted instead.
		// Without this they would be the only records in the system that no retention policy
		// could ever remove — and a row lands there precisely when partition creation fell
		// behind, which is when nobody was watching.
		deleted, err := deleteInBatches(ctx, conn,
			`DELETE FROM retrieval_traces_default WHERE ctid IN (
				SELECT ctid FROM retrieval_traces_default WHERE created_at < $1 LIMIT $2)`, cutoff)
		if err != nil {
			return report, err
		}
		report.TracesDropped += deleted
	}

	for _, sweep := range []struct {
		keep  time.Duration
		count *int64
		sql   string
	}{
		{policy.Outbox, &report.OutboxDeleted, `
			DELETE FROM outbox_events WHERE id IN (
				SELECT id FROM outbox_events
				WHERE status IN ('succeeded', 'dead')
				  AND completed_at IS NOT NULL AND completed_at < $1
				LIMIT $2)`},
		{policy.Audit, &report.AuditDeleted, `
			DELETE FROM audit_events WHERE id IN (
				SELECT id FROM audit_events WHERE created_at < $1 LIMIT $2)`},
		{policy.PipelineRuns, &report.PipelineDeleted, `
			DELETE FROM pipeline_runs WHERE id IN (
				SELECT id FROM pipeline_runs WHERE created_at < $1 LIMIT $2)`},
	} {
		if sweep.keep <= 0 {
			continue
		}
		deleted, err := deleteInBatches(ctx, conn, sweep.sql, now.Add(-sweep.keep))
		if err != nil {
			return report, err
		}
		*sweep.count = deleted
	}

	return report, nil
}

// deleteInBatches removes rows a bounded number at a time until none are left.
func deleteInBatches(ctx context.Context, conn *pgxpool.Conn, statement string, cutoff time.Time) (int64, error) {
	const op = "ledger.prune"

	var total int64
	for {
		tag, err := conn.Exec(ctx, statement, cutoff, deleteBatch)
		if err != nil {
			return total, mapError(err, op, "cannot delete expired records")
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < deleteBatch {
			return total, nil
		}
		// Yield between batches so a long first sweep cannot monopolise a connection or
		// outrun autovacuum.
		select {
		case <-ctx.Done():
			return total, mapError(ctx.Err(), op, "pruning was cancelled")
		default:
		}
	}
}

// ensureTracePartitions creates the coming months' partitions if they are missing.
func ensureTracePartitions(ctx context.Context, conn *pgxpool.Conn, now time.Time) ([]string, error) {
	const op = "ledger.ensureTracePartitions"

	var created []string
	month := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= partitionMonths; i++ {
		start := month.AddDate(0, i, 0)
		name := fmt.Sprintf("retrieval_traces_%s", start.Format("200601"))

		var exists bool
		if err := conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)`, name).Scan(&exists); err != nil {
			return created, mapError(err, op, "cannot check for a trace partition")
		}
		if exists {
			continue
		}
		// Identifiers are formatted rather than bound because PostgreSQL does not accept a
		// parameter where a relation name belongs. Both values are derived from a clock, not
		// from input.
		statement := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF retrieval_traces FOR VALUES FROM ('%s') TO ('%s')`,
			name, start.Format("2006-01-02"), start.AddDate(0, 1, 0).Format("2006-01-02"))
		if _, err := conn.Exec(ctx, statement); err != nil {
			// A row already in the default partition for this range makes this impossible
			// until that row is gone. Report it rather than failing the whole sweep, which
			// would also stop pruning every other table.
			return created, mapError(err, op,
				"cannot create trace partition "+name+
					" (rows for this range may already be in the default partition)")
		}
		created = append(created, name)
	}
	return created, nil
}

// dropTracePartitionsBefore removes whole partitions that end at or before the cutoff.
//
// A partition is dropped only when every row in it is expired, which is why the cutoff is
// compared against the partition's upper bound. Dropping is the reason to partition this
// table at all: removing a month of traces is a catalogue change rather than millions of
// deletes and the bloat they leave behind.
func dropTracePartitionsBefore(ctx context.Context, conn *pgxpool.Conn, cutoff time.Time) ([]string, int64, error) {
	const op = "ledger.dropTracePartitionsBefore"

	rows, err := conn.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_inherits i ON c.oid = i.inhrelid
		WHERE i.inhparent = 'retrieval_traces'::regclass
		  AND c.relname <> 'retrieval_traces_default'
		ORDER BY c.relname`)
	if err != nil {
		return nil, 0, mapError(err, op, "cannot list trace partitions")
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, 0, mapError(err, op, "cannot read a trace partition name")
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, mapError(err, op, "cannot list trace partitions")
	}

	var dropped []string
	var total int64
	for _, name := range names {
		start, err := time.Parse("200601", name[len("retrieval_traces_"):])
		if err != nil {
			// Not a partition this code created. Leaving it alone is the safe reading.
			continue
		}
		if !start.AddDate(0, 1, 0).After(cutoff) {
			var count int64
			if err := conn.QueryRow(ctx,
				fmt.Sprintf(`SELECT count(*) FROM %s`, name)).Scan(&count); err != nil {
				return dropped, total, mapError(err, op, "cannot count a trace partition")
			}
			if _, err := conn.Exec(ctx, fmt.Sprintf(`DROP TABLE %s`, name)); err != nil {
				return dropped, total, mapError(err, op, "cannot drop trace partition "+name)
			}
			dropped = append(dropped, name)
			total += count
		}
	}
	return dropped, total, nil
}
