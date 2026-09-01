// Package retention bounds the operational tables.
//
// The system's append-only tables divide in two, and the division is the whole design.
//
// What the system knows, and how it knows it — assertions, source events, evidence,
// derivations, model runs — is never deleted here. That is the product. A context graph
// that forgets its own provenance to save disk has thrown away the thing that distinguishes
// it from a search index.
//
// What the system did — a trace per query, an audit row per action, an outbox row per unit
// of work, a pipeline run per document processed — grows with traffic instead. Nothing here
// describes anything anybody knows, and before this package nothing deleted any of it, so a
// deployment serving a million queries a day accumulated a million rows a day forever.
//
// Every retention setting defaults to keeping records forever. Deleting an operator's
// records is their decision, and a system whose subject is provenance should not make it on
// their behalf.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/gimantha/strata/internal/store/ledger"
)

// Pruner is the store operation this package schedules.
type Pruner interface {
	Prune(ctx context.Context, policy ledger.RetentionPolicy, now time.Time) (ledger.RetentionReport, error)
}

// Options configure the sweeper.
type Options struct {
	Policy   ledger.RetentionPolicy
	Interval time.Duration
	Logger   *slog.Logger
	// Now is injectable so tests do not have to wait for a month to pass.
	Now func() time.Time
}

// Sweeper runs retention on an interval.
type Sweeper struct {
	store    Pruner
	policy   ledger.RetentionPolicy
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time
}

// New builds a sweeper.
func New(store Pruner, opts Options) *Sweeper {
	if opts.Interval <= 0 {
		opts.Interval = time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Sweeper{
		store:    store,
		policy:   opts.Policy,
		interval: opts.Interval,
		logger:   opts.Logger,
		now:      opts.Now,
	}
}

// Run sweeps until the context is cancelled.
//
// Every process runs this. Coordination is an advisory lock in the database rather than a
// leader election, on the same reasoning the outbox reaper uses: recovery should not depend
// on one designated process being alive.
//
// It sweeps once on start rather than waiting out the first interval, because the sweep
// that matters most is the one after a deployment where retention was just switched on, and
// because trace partitions have to exist before anything writes a trace.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	s.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

// Sweep runs one round. Exported so an operator command or a test can force one.
func (s *Sweeper) Sweep(ctx context.Context) (ledger.RetentionReport, error) {
	return s.store.Prune(ctx, s.policy, s.now())
}

func (s *Sweeper) sweep(ctx context.Context) {
	report, err := s.store.Prune(ctx, s.policy, s.now())
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if s.logger != nil {
			s.logger.ErrorContext(ctx, "retention sweep failed", slog.String("error", err.Error()))
		}
		return
	}
	if report.Skipped || s.logger == nil {
		return
	}

	// Rows in the default partition mean partition creation fell behind, and the range they
	// occupy cannot be partitioned until they are gone. Worth saying out loud: it is the one
	// state here that a sweep cannot fix by itself.
	if report.DefaultPartitionRows > 0 {
		s.logger.WarnContext(ctx, "retrieval traces are landing in the default partition",
			slog.Int64("rows", report.DefaultPartitionRows),
			slog.String("effect", "partitions for those months cannot be created until they are removed"))
	}
	if len(report.PartitionsCreated) > 0 {
		s.logger.InfoContext(ctx, "created trace partitions",
			slog.Any("partitions", report.PartitionsCreated))
	}
	if report.Total() == 0 && len(report.PartitionsDropped) == 0 {
		return
	}
	s.logger.InfoContext(ctx, "retention sweep removed expired operational records",
		slog.Int64("traces", report.TracesDropped),
		slog.Any("partitions_dropped", report.PartitionsDropped),
		slog.Int64("outbox", report.OutboxDeleted),
		slog.Int64("audit", report.AuditDeleted),
		slog.Int64("pipeline_runs", report.PipelineDeleted))
}
