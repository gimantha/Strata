// Command contextgraphd is the Strata API server.
//
// It exposes the ingestion and administration endpoints and, when configured with
// CG_EMBEDDED_WORKER=true, also runs the outbox consumer in-process so a developer can
// run the whole system with PostgreSQL and nothing else.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/gimantha/strata/internal/api/http"
	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/config"
)

func main() {
	if err := run(); err != nil {
		// Configuration and startup failures happen before the logger exists.
		slog.Error("contextgraphd failed to start", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	// Signals are trapped before anything is started, so a shutdown request during
	// startup is honored rather than lost.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

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

	logger := application.Logger
	logger.InfoContext(ctx, "starting contextgraphd", slog.Any("config", cfg.Redacted()))

	server := http.NewServer(http.Deps{
		Config:    cfg,
		Logger:    logger,
		Metrics:   application.Telemetry.Metrics,
		Tracer:    application.Telemetry.Tracer,
		Identity:  application.Identity,
		Ledger:    application.Ledger,
		Gateway:   application.Gateway,
		Knowledge: application.Knowledge,
		Blobs:     application.Blobs,
	})

	var (
		wg        sync.WaitGroup
		serveErr  error
		workerErr error
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		serveErr = server.ListenAndServe()
	}()

	if cfg.EmbeddedWorker {
		logger.InfoContext(ctx, "running embedded outbox worker",
			slog.Int("concurrency", cfg.WorkerConcurrency))
		wg.Add(1)
		go func() {
			defer wg.Done()
			workerErr = application.RunWorker(ctx)
		}()
	}

	<-ctx.Done()
	logger.Info("shutdown requested; draining in-flight requests")

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown reported an error", slog.String("error", err.Error()))
	}

	wg.Wait()
	return errors.Join(serveErr, workerErr)
}
