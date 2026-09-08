// control-plane: the Go service hosting tenancy, config lifecycle, the event
// gateway, the processing pipeline, player APIs, monetization, and workers.
//
// Boot sequence: config → DB connect + migrate → engine client → workers → HTTP.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"universalengagement/control-plane/internal/api"
	"universalengagement/control-plane/internal/config"
	"universalengagement/control-plane/internal/db"
	"universalengagement/control-plane/internal/engine"
	"universalengagement/control-plane/internal/workers"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := config.Load()

	// Database.
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("database connect failed", "error", err, "url", cfg.DatabaseURL)
		os.Exit(1)
	}
	defer pool.Close()

	if err := pool.Migrate(ctx); err != nil {
		slog.Error("migrations failed", "error", err)
		os.Exit(1)
	}
	slog.Info("database ready", "migrations_applied", true)

	// Engine client (health-checked at boot; degrades per-request if engine down).
	eng := engine.NewClient(cfg.EngineURL)
	pingCtx, pingCancel := context.WithTimeout(ctx, 3*time.Second)
	if err := eng.Healthz(pingCtx); err != nil {
		slog.Warn("engine-service not reachable at boot", "url", cfg.EngineURL, "error", err)
	}
	pingCancel()

	// Workers (outbox publisher, webhook deliveries, workflow resumption).
	runner := workers.NewRunner(pool)
	runner.Start(ctx)
	defer runner.Stop()

	// HTTP server.
	srv := &http.Server{
		Addr:              cfg.Bind,
		Handler:           api.New(pool, eng, cfg.AllowedOrigin),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}

	go func() {
		slog.Info("control-plane listening", "bind", cfg.Bind, "engine", cfg.EngineURL)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server failed", "error", err)
			cancel()
		}
	}()

	// Graceful shutdown.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		slog.Info("shutdown signal received")
	case <-ctx.Done():
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	slog.Info("control-plane stopped")
}
