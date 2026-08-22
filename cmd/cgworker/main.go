// Command cgworker consumes the transactional outbox and runs the ingestion pipeline.
//
// It is the separately scalable half of the system: run as many as the workload needs.
// Claims use FOR UPDATE SKIP LOCKED, so workers share one queue without duplicating
// work (AGENTS.md section 28).
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
		slog.Any("config", cfg.Redacted()))

	// Subscribe returns once the context is cancelled and in-flight work has drained.
	if err := application.RunWorker(ctx); err != nil {
		return err
	}
	application.Logger.Info("cgworker stopped cleanly")
	return nil
}
