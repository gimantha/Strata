// Command cgworker consumes the transactional outbox and runs the ingestion pipeline.
//
// It is the separately scalable half of the system: run as many as the workload needs.
// Claims use FOR UPDATE SKIP LOCKED, so workers share one queue without duplicating
// work, and a partition key keeps successive versions of one upstream record from being
// processed out of order (AGENTS.md sections 27.5, 28).
//
// With CG_NATS_URL set, JetStream carries a notification about work that is already
// committed so the fleet reacts immediately instead of at its next poll. The claim still
// happens in PostgreSQL either way, which is why a broker outage costs latency and not
// correctness.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/config"
)

func main() {
	if err := run(); err != nil {
		slog.Error("cgworker failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// The worker does not serve HTTP; migrations belong to whichever process is
	// configured to apply them.
	cfg.EmbeddedWorker = false

	application, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()
		if err := application.Close(shutdownCtx); err != nil {
			application.Logger.Warn("shutdown reported errors", slog.String("error", err.Error()))
		}
	}()

	application.Logger.InfoContext(ctx, "starting cgworker",
		slog.String("worker_id", application.Bus.WorkerID()),
		slog.String("delivery", deliveryMode(cfg)),
		slog.Any("config", cfg.Redacted()))

	// Subscribe returns once the context is cancelled and in-flight work has drained.
	if err := application.RunWorker(ctx); err != nil {
		return err
	}
	application.Logger.Info("cgworker stopped cleanly")
	return nil
}

// deliveryMode names how this worker hears about work, so an operator can tell at a
// glance whether a latency complaint is a missing broker.
func deliveryMode(cfg config.Config) string {
	if cfg.NATSURL == "" {
		return "poll"
	}
	return "push+poll"
}
